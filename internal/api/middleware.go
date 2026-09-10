package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
)

// requestIDHeader carries the identifier back to the client, which is the only
// way it can quote one when reporting a problem.
const requestIDHeader = "X-Request-Id"

// requestIDContextKey keys the identifier in the request context. An
// unexported empty struct rather than a string: nothing outside this package
// can name the type, so nothing can collide with it.
type requestIDContextKey struct{}

// An incoming X-Request-Id is ignored rather than adopted: honouring it would
// put a string of the client's choosing, of any length and any content, into
// every log line of the request, and there is no upstream service of ours to
// correlate with.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set(requestIDHeader, id)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)))
	})
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)

	return id
}

// Never why: the cause of a failure is logged by writeInternal, which is what
// lets this line stay at info for every status. A status of zero means the
// handler wrote nothing at all.
//
// The writer is wrapped so the status can be read back.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			started := time.Now()

			// The context is handed in rather than reached for through the
			// request, which is what keeps this closure a function of what it
			// logs and lets the linter see that it carries one.
			defer func(ctx context.Context) {
				logger.Info("request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", wrapped.Status()),
					slog.Int64("duration_ms", time.Since(started).Milliseconds()),
					slog.String("request_id", requestIDFrom(ctx)),
				)
			}(r.Context())

			next.ServeHTTP(wrapped, r)
		})
	}
}

// The server's write timeout ends the response but does not cancel the request,
// so without this a query against a stopped database holds a goroutine and a
// pooled connection for the length of the outage.
func requestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// net/http recovers a panic too, but logs the stack through its own logger and
// closes the connection without answering, where the contract shows an
// envelope.
//
// Inside accessLog rather than around it, so the panic becomes a status before
// the deferred log line reads one.
func recoverPanic(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func(ctx context.Context) {
				recovered := recover()
				if recovered == nil {
					return
				}

				// The documented way for a handler to abandon a response
				// without a word. net/http raises it and expects to catch it
				// again, so it is passed on rather than answered.
				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}

				logger.Error("panic in handler",
					slog.Any("panic", recovered),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("request_id", requestIDFrom(ctx)),
				)

				// Only if the handler has not answered yet: a second WriteHeader is
				// refused, and the envelope would land inside a body that is already
				// valid.
				if wrapped, ok := w.(middleware.WrapResponseWriter); ok && wrapped.Status() != 0 {
					return
				}

				writeErrorJSON(w, http.StatusInternalServerError, contract.ErrorCodeInternalError, internalErrorMessage)
			}(r.Context())

			next.ServeHTTP(w, r)
		})
	}
}
