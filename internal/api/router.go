// Package api is the HTTP layer: router, middleware, handlers and the
// translation between the wire and the domain.
//
// The shapes on the wire are not written here. They are generated from
// api/openapi.yaml into internal/api/contract, and the handlers below satisfy
// the interface generated with them, so a route, a parameter or a field that
// changes in the spec and not in this package stops the build.
//
// Nothing below this package knows about HTTP, and nothing above it sees a
// domain type. This is also where errors stop carrying their causes -- what a
// client is told and what the log is told part company in writeInternal.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
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

		// Registered on this group rather than through the Middlewares option
		// of the generated server: that option wraps the handler alone, leaving
		// the parameter binding -- and every answer writeParamError gives for
		// it -- outside the chain, without a request id and unseen by the
		// access log.
		contract.HandlerWithOptions(h, contract.ChiServerOptions{
			BaseRouter:       r,
			ErrorHandlerFunc: h.writeParamError,
		})
	})

	return r
}
