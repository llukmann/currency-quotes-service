package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// maxRequestBody bounds the body of a post. The only body this API reads holds
// a currency pair, so anything approaching this size is a mistake or an
// attempt, and either is better refused than buffered.
const maxRequestBody = 4 << 10

// quoteService is the business layer as seen from here: three calls, two of
// them taking a pair this package has already parsed. Declared at the
// consumer, as in the worker.
type quoteService interface {
	CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error)
	GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error)
}

// handler implements contract.ServerInterface, so the three methods below are
// named by the operation ids of the spec rather than by this package.
type handler struct {
	svc    quoteService
	logger *slog.Logger
}

// CreateQuoteUpdate queues a refresh of a pair. It never contacts the provider:
// all it does is write the task a worker will pick up.
func (h *handler) CreateQuoteUpdate(w http.ResponseWriter, r *http.Request, params contract.CreateQuoteUpdateParams) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req contract.CreateUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, contract.ErrorCodeInvalidRequest, "request body is not valid JSON")

		return
	}

	// Parsed here rather than deeper: this is the boundary the untrusted string
	// arrives at, and past it the service takes a domain.Pair, which cannot be
	// built any other way.
	pair, err := domain.ParsePair(req.Pair)
	if err != nil {
		writePairError(w, err)

		return
	}

	task, err := h.svc.CreateTask(r.Context(), pair, params.IdempotencyKey)

	switch {
	case errors.Is(err, domain.ErrKeyConflict):
		writeErrorJSON(w, http.StatusConflict, contract.ErrorCodeKeyConflict, err.Error())
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusAccepted, newAcceptedResponse(task))
	}
}

// GetQuoteUpdate reports the state of an update. An unfinished task answers 200
// with the status it carries, not 404: the identifier is known, the work is not
// done.
func (h *handler) GetQuoteUpdate(w http.ResponseWriter, r *http.Request, id contract.UpdateIdPath) {
	details, err := h.svc.GetTask(r.Context(), id)

	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, contract.ErrorCodeNotFound, "no update with this id")
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusOK, newTaskResponse(details))
	}
}

// GetLatestQuote reads the last rate stored for a pair. It reads the database
// alone, so a supported pair nobody has updated yet has no answer here.
func (h *handler) GetLatestQuote(w http.ResponseWriter, r *http.Request, params contract.GetLatestQuoteParams) {
	pair, err := domain.ParsePair(params.Pair)
	if err != nil {
		writePairError(w, err)

		return
	}

	quote, err := h.svc.GetLatestQuote(r.Context(), pair)

	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, contract.ErrorCodeNotFound, "this pair has not been quoted yet")
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusOK, newQuoteResponse(quote))
	}
}

// handleHealth backs the docker compose healthcheck. It is not part of the
// business API and is not described in the OpenAPI spec.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
