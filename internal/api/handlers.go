package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// maxRequestBody bounds the body of a post. The only body this API reads holds
// a currency pair, so anything approaching this size is a mistake or an
// attempt, and either is better refused than buffered.
const maxRequestBody = 4 << 10

// headerIdempotencyKey names one intent of one client. Optional: a post without
// it is still deduplicated against unfinished work on the same pair, but only a
// key can tell a retry of one request from a fresh request for the same thing.
const headerIdempotencyKey = "Idempotency-Key"

// quoteService is the business layer as seen from here: three calls, two of
// them taking the pair as the client wrote it. Declared at the consumer, as in
// the worker, and for the same second reason -- it is the surface a
// hand-written fake stands in for when the handlers are tested in step 7.
type quoteService interface {
	CreateTask(ctx context.Context, pair string, key *uuid.UUID) (domain.UpdateTask, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error)
	GetLatestQuote(ctx context.Context, pair string) (domain.Quote, error)
}

// handler serves the business endpoints.
type handler struct {
	svc    quoteService
	logger *slog.Logger
}

// createTask backs POST /quotes/updates: it queues a refresh and answers with
// the identifier to poll, never with a rate. Fetching one is the worker's, and
// the asynchronous contract is exactly this handler not doing it.
//
// Answers 202, 400 for a body that is not JSON, a pair that is not supported or
// a key that is not a UUID, and 409 for a key already spent on another pair.
//
// A 202 here may well describe a task this request did not create: a repeat
// carrying a live key is answered with the task the first one made, and a post
// arriving while the pair is already being refreshed is answered with that
// task. The status in the body is therefore the one the row carries now, which
// can be any of the four -- a client that retried after a failure sees failed
// and knows to send a fresh key rather than poll a task that will never move.
func (h *handler) createTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// One answer for a malformed body, a truncated one and an oversized
		// one alike: all three say the request could not be read, and which of
		// them it was helps nobody holding a client.
		writeErrorJSON(w, http.StatusBadRequest, codeInvalidRequest, "request body is not valid JSON")

		return
	}

	key, err := idempotencyKey(r)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, codeInvalidRequest, "Idempotency-Key is not a valid UUID")

		return
	}

	// A missing "pair" arrives here as an empty string and is refused by the
	// same rule as an unsupported one, with the same code: both mean the
	// request named no pair this service can quote.
	task, err := h.svc.CreateTask(r.Context(), req.Pair, key)

	switch {
	case errors.Is(err, domain.ErrInvalidPair):
		writeErrorJSON(w, http.StatusBadRequest, codeInvalidPair, err.Error())
	case errors.Is(err, domain.ErrKeyConflict):
		// 409 and not 400: the request is faultless in itself -- valid body,
		// supported pair, well formed key -- and what refuses it is the history
		// behind that key. The two need different fixes, and a client reading
		// the code alone has to be able to tell them apart.
		writeErrorJSON(w, http.StatusConflict, codeKeyConflict, err.Error())
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusAccepted, newAcceptedResponse(task))
	}
}

// idempotencyKey reads the Idempotency-Key header, returning nil when the
// client sent none: the header is optional, and without it a post is
// deduplicated by pair alone.
//
// A value that is not a UUID is refused rather than taken as an opaque string.
// The contract asks for a UUID, and holding it to that is what bounds the size
// of something a client controls before it reaches a column.
func idempotencyKey(r *http.Request) (*uuid.UUID, error) {
	raw := r.Header.Get(headerIdempotencyKey)
	if raw == "" {
		return nil, nil
	}

	key, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}

	return &key, nil
}

// getTask backs GET /quotes/updates/{id}, the endpoint a client polls.
//
// Answers 200 in every status the task can be in, 400 for an identifier that is
// not a UUID and 404 for one that names no task. A failed task is a 200 as
// well: the request worked, and what it found is that the refresh did not.
func (h *handler) getTask(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, codeInvalidRequest, "update id is not a valid UUID")

		return
	}

	details, err := h.svc.GetTask(r.Context(), id)

	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, codeNotFound, "no update with this id")
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusOK, newTaskResponse(details))
	}
}

// getLatestQuote backs GET /quotes/latest: the last rate stored for a pair,
// whichever update produced it and however old it is. It reads from the
// database alone, so a pair nobody has ever posted an update for has no
// answer here.
//
// Answers 200, 400 for a missing or unsupported pair, and 404 for a supported
// one that has never been quoted. The two are told apart on purpose: the first
// will never work, the second will once an update completes.
func (h *handler) getLatestQuote(w http.ResponseWriter, r *http.Request) {
	// Query unescapes for us, so ?pair=EUR/MXN and ?pair=EUR%2FMXN arrive the
	// same. A missing parameter yields an empty string, which the service
	// refuses as an invalid pair.
	pair := r.URL.Query().Get("pair")

	quote, err := h.svc.GetLatestQuote(r.Context(), pair)

	switch {
	case errors.Is(err, domain.ErrInvalidPair):
		writeErrorJSON(w, http.StatusBadRequest, codeInvalidPair, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, codeNotFound, "this pair has not been quoted yet")
	case err != nil:
		h.writeInternal(w, r, err)
	default:
		h.writeJSON(w, r, http.StatusOK, newQuoteResponse(quote))
	}
}
