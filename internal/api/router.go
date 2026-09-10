// Package api is the HTTP layer and the translation between the wire and the
// domain. The shapes on the wire are generated from api/openapi.yaml into
// internal/api/contract, and the handlers satisfy the interface generated with
// them, so a route or a field that changes in the spec and not here stops the
// build.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
)

func NewRouter(svc quoteService, logger *slog.Logger, handlerTimeout time.Duration) http.Handler {
	h := &handler{svc: svc, logger: logger}

	r := chi.NewRouter()

	// Outside the group, so none of the middleware applies: the compose
	// healthcheck calls it every five seconds, and seventeen thousand log lines
	// a day would bury anything worth reading.
	r.Get("/healthz", handleHealth)

	r.Group(func(r chi.Router) {
		// Ordered, not merely listed. The identifier is set first so every line
		// after it can carry one; the access log wraps the writer the two below
		// it depend on; the deadline is set outside the recovery so that a
		// handler killed by it still answers through the envelope; and the
		// recovery sits innermost, where the panic it catches becomes a status
		// the access log can still report.
		r.Use(
			requestID,
			accessLog(logger),
			requestTimeout(handlerTimeout),
			recoverPanic(logger),
		)

		// Registered on this group rather than through the Middlewares option of
		// the generated server: that option wraps the handler alone, leaving the
		// parameter binding -- and every answer writeParamError gives for it --
		// outside the chain, without a request id and unseen by the access log.
		contract.HandlerWithOptions(h, contract.ChiServerOptions{
			BaseRouter:       r,
			ErrorHandlerFunc: h.writeParamError,
		})
	})

	return r
}
