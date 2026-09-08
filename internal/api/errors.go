package api

import (
	"encoding/json"
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
		// marshal, which leaves a broken connection as the only cause.
		h.logger.Error("write response",
			slog.Any("error", err),
			slog.String("request_id", requestIDFrom(r.Context())),
		)
	}
}

// writeInternal answers a failure that is ours and not the client's: the cause
// goes to the log, the client gets the code alone.
//
// This is where raw errors stop, the way raw upstream errors stop in the
// worker. Everything below this layer -- a driver's message, a statement, a
// wrapped chain naming the operations it passed through -- is written for
// whoever runs the service, not for whoever called it.
func (h *handler) writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("request failed",
		slog.Any("error", err),
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)

	writeErrorJSON(w, http.StatusInternalServerError, codeInternalError, internalErrorMessage)
}
