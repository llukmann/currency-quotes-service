package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Machine-readable error codes. The set is closed: a client is meant to branch
// on these rather than on the message beside them, which is written for a
// person and may be reworded.
const (
	codeInvalidPair    = "invalid_pair"
	codeInvalidRequest = "invalid_request"
	codeNotFound       = "not_found"
	codeInternalError  = "internal_error"
)

// internalErrorMessage is all a client is told about a failure of ours. What
// actually happened is logged instead, with the request id beside it: the
// wording of a database error, a driver's or a connection string's, says
// nothing to whoever made the request and something to whoever did not.
const internalErrorMessage = "internal error"

// contentTypeJSON is the only content type this API produces, errors included.
const contentTypeJSON = "application/json"

// errorResponse is the single error envelope of the API. Every failure of
// every endpoint is shaped like this, so a client parses one thing.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeErrorJSON serves one error envelope.
//
// A plain function rather than a method: the panic middleware needs it too,
// and there is no handler to hang it off there.
func writeErrorJSON(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)

	// Nothing left to do with a failure here -- the status line is already on
	// the wire -- and the only way to reach one is a connection that has gone
	// away, which the access log records anyway.
	_ = json.NewEncoder(w).Encode(errorResponse{Error: errorBody{Code: code, Message: message}})
}

// writeJSON serves a successful body.
func (h *handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status and the headers have gone out already, so the client
		// cannot be told. None of the three response types can fail to
		// marshal, which leaves a broken connection as the only cause -- and
		// the commonest one is the client hanging up mid-body, which is not a
		// failure of ours either.
		if clientGone(r.Context()) {
			h.logAbandoned(r)

			return
		}

		h.logger.Error("write response",
			slog.Any("error", err),
			slog.String("request_id", requestIDFrom(r.Context())),
		)
	}
}

// clientGone reports whether the work stopped because the client went away
// rather than because anything of ours failed.
//
// Asked of the context rather than of the error, as the worker asks it: what
// went wrong is the error's to say, but why the work stopped is the context's.
// It is also the only reading that does not depend on how a driver chooses to
// wrap a cancellation on the way back up.
//
// The two reasons cannot be confused here, which is what makes the question
// answerable at all. requestTimeout sets a deadline, so a limit of ours always
// arrives as context.DeadlineExceeded; a cancellation can only have come from
// net/http closing the request context when the connection went away.
func clientGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// logAbandoned records a request whose client left before it was answered.
//
// At info, deliberately. Nothing failed: there is no answer to write and
// nobody to write it to. Logged at error it would be a false alarm sitting in
// the error log beside a real outage, to be told apart by hand at exactly the
// moment nobody has time for it -- a closed tab and a database that has
// stopped answering would look alike.
func (h *handler) logAbandoned(r *http.Request) {
	h.logger.Info("request abandoned by the client",
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
}

// writeInternal answers a failure that is ours and not the client's: the cause
// goes to the log, the client gets the code alone.
//
// This is where raw errors stop, the way raw upstream errors stop in the
// worker. Everything below this layer -- a driver's message, a statement, a
// wrapped chain naming the operations it passed through -- is written for
// whoever runs the service, not for whoever called it.
//
// A request the client abandoned does not come through here as a failure. Its
// error is real -- everything in flight fails when the context is cancelled --
// but it is the consequence of the client leaving, not a fault of ours, and
// answering it would write a status into a connection that is already gone.
func (h *handler) writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(r.Context()) {
		h.logAbandoned(r)

		return
	}

	h.logger.Error("request failed",
		slog.Any("error", err),
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)

	writeErrorJSON(w, http.StatusInternalServerError, codeInternalError, internalErrorMessage)
}
