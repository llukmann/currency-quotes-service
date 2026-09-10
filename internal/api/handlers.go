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

// The only body this API reads holds a currency pair, so anything approaching
// this size is a mistake or an attempt, and either is better refused than
// buffered.
const maxRequestBody = 4 << 10

type quoteService interface {
	CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error)
	GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error)
}

// Implements contract.ServerInterface, so the methods below are named by the
// operation ids of the spec rather than by this package.
type handler struct {
	svc    quoteService
	logger *slog.Logger
}

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

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
