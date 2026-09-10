package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// internalErrorMessage is all a client is told about a failure of ours. What
// actually went wrong goes to the log, with the request id beside it.
const internalErrorMessage = "internal error"

const contentTypeJSON = "application/json"

func writeErrorJSON(w http.ResponseWriter, status int, code contract.ErrorCode, message string) {
	var body contract.Error
	body.Error.Code = code
	body.Error.Message = message

	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// The message is the sentence ParsePair wrote, which names what is wrong with
// this particular pair rather than restating the code.
func writePairError(w http.ResponseWriter, err error) {
	writeErrorJSON(w, http.StatusBadRequest, contract.ErrorCodeInvalidPair, err.Error())
}

// Keeps the answers of the generated binder, which turns a request away before
// any handler sees it, inside the envelope. A pair that was not sent is
// answered as a pair that cannot be parsed -- the spec makes missing, malformed
// and unsupported one answer -- and the message comes from ParsePair so the two
// paths cannot drift apart.
func (h *handler) writeParamError(w http.ResponseWriter, r *http.Request, err error) {
	var missing *contract.RequiredParamError
	if errors.As(err, &missing) && missing.ParamName == "pair" {
		_, perr := domain.ParsePair("")
		writePairError(w, perr)

		return
	}

	var format *contract.InvalidParamFormatError
	if errors.As(err, &format) {
		switch format.ParamName {
		case "id":
			writeErrorJSON(w, http.StatusBadRequest, contract.ErrorCodeInvalidRequest, "update id is not a valid UUID")

			return
		case "Idempotency-Key":
			writeErrorJSON(w, http.StatusBadRequest, contract.ErrorCodeInvalidRequest, "Idempotency-Key is not a valid UUID")

			return
		}
	}

	var tooMany *contract.TooManyValuesForParamError
	if errors.As(err, &tooMany) {
		writeErrorJSON(w, http.StatusBadRequest, contract.ErrorCodeInvalidRequest, tooMany.ParamName+" must be sent once")

		return
	}

	// Nothing the spec of this service can produce: every parameter it declares
	// is covered above. Logged with the cause rather than guessed at.
	h.writeInternal(w, r, err)
}

func (h *handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already out, so there is no answer left to change;
		// all that is left is to say why the body is short.
		if clientGone(r.Context()) {
			h.logAbandoned(r, err)

			return
		}

		h.logger.Error("write response",
			slog.Any("error", err),
			slog.String("request_id", requestIDFrom(r.Context())),
		)
	}
}

func clientGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// Info rather than error: a client that hangs up is not a failure of this
// service.
func (h *handler) logAbandoned(r *http.Request, err error) {
	h.logger.Info("request abandoned by the client",
		slog.Any("error", err),
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
}

func (h *handler) writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(r.Context()) {
		h.logAbandoned(r, err)

		return
	}

	h.logger.Error("request failed",
		slog.Any("error", err),
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)

	writeErrorJSON(w, http.StatusInternalServerError, contract.ErrorCodeInternalError, internalErrorMessage)
}
