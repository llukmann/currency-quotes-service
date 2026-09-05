// Package api holds the HTTP layer: router, handlers and DTOs.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter wires up the service routes.
func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", handleHealth)

	return r
}

// handleHealth backs the docker compose healthcheck. It is not part of the
// business API and is not described in the OpenAPI spec.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
