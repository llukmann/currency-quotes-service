// Package api holds the HTTP layer: router, middleware, handlers and the DTOs
// they answer with.
//
// Nothing below this package knows about HTTP, and nothing above it sees a
// domain type: the DTOs here are what translate between the two vocabularies,
// so that renaming a field of an entity is not a change of contract.
//
// That split runs through the names in this package. The Go identifiers follow
// the domain, where the queued entity is an UpdateTask -- hence createTask,
// getTask and taskResponse -- while the JSON keys keep the word a client knows
// it by, update_id under /quotes/updates, which is what api.md fixes. Only the
// DTOs are allowed to hold both. This is
// also where errors stop carrying their causes -- what a client is told and
// what the log is told part company in writeInternal.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// NewRouter wires up the service routes.
//
// handlerTimeout is the deadline given to each business request; it comes from
// the configuration, which derives it from the write timeout of the server.
func NewRouter(svc quoteService, logger *slog.Logger, handlerTimeout time.Duration) http.Handler {
	h := &handler{svc: svc, logger: logger}

	r := chi.NewRouter()

	// Registered outside the group below, so none of the middleware applies to
	// it. That is not a shortcut but the same line the rest of the project
	// draws: /healthz is not part of the business API and is not described in
	// the OpenAPI spec either. The reason it matters here is the compose
	// healthcheck, which calls it every five seconds -- seventeen thousand
	// lines a day, in which anything worth reading would be lost.
	//
	// What it gives up is small. A panic in a handler that writes one constant
	// string would go to net/http's own recovery instead of ours, and the
	// probe would see a closed connection, which is what an unhealthy
	// container should see anyway.
	r.Get("/healthz", handleHealth)

	r.Group(func(r chi.Router) {
		// Ordered, not merely listed. The identifier is set first so every
		// line after it can carry one; the access log wraps the writer the two
		// below it depend on; the deadline is set outside the recovery so that
		// a handler killed by it still answers through the envelope; and the
		// recovery sits innermost, where the panic it catches becomes a status
		// the access log can still report.
		r.Use(
			requestID,
			accessLog(logger),
			requestTimeout(handlerTimeout),
			recoverPanic(logger),
		)

		r.Post("/quotes/updates", h.createTask)
		r.Get("/quotes/updates/{id}", h.getTask)
		r.Get("/quotes/latest", h.getLatestQuote)
	})

	return r
}

// handleHealth backs the docker compose healthcheck. It is not part of the
// business API and is not described in the OpenAPI spec.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
