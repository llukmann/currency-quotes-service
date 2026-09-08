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
)

// requestIDHeader carries the identifier back to the client, which is the only
// way it can quote one when reporting a problem.
const requestIDHeader = "X-Request-Id"

// requestIDContextKey keys the identifier in the request context. An
// unexported empty struct rather than a string: nothing outside this package
// can name the type, so nothing can collide with it.
type requestIDContextKey struct{}

// requestID gives every request an identifier and puts it where the log lines
// of that request can find it.
//
// An incoming X-Request-Id is ignored rather than adopted. Honouring it would
// put a string of the client's choosing, of any length and any content, into
// every log line of the request -- and buy nothing here, since there is no
// second service of ours upstream to correlate with. The identifier is a UUID
// for the same reason update_id is: two of them side by side in a log line
// read as the same kind of thing.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set(requestIDHeader, id)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)))
	})
}

// requestIDFrom returns the identifier of the request ctx belongs to, or an
// empty string outside a request.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)

	return id
}

// accessLog writes one line per request, after it has been answered.
//
// It records what was asked and how it ended, never why: a request that failed
// through no fault of the client is logged with its cause by writeInternal,
// where the error is still in hand. Keeping the two apart is what lets this
// line stay at info for every status, including 500 -- the level of a line
// nobody has to act on.
//
// The response writer is wrapped here so that the status can be read back
// afterwards. The wrapper is chi's rather than ours: a hand-rolled one is a
// dozen lines, and they are exactly the dozen where the pass-through of Flush
// and ReaderFrom is got wrong.
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

// requestTimeout bounds everything a handler does. Without it a query against
// a database that has stopped answering holds a goroutine and a pooled
// connection for as long as the outage lasts: the server's write timeout ends
// the response but does not cancel the request.
//
// It only sets the deadline and writes nothing itself. What the handler was
// doing then fails with a context error, which is served as internal_error --
// a code the contract describes, through the envelope the contract describes.
// chi's own middleware.Timeout was not used for both halves of that: it answers
// with a bare 504, which api.md lists for no endpoint, and it writes that 504
// from a deferred call without checking whether the handler has already
// answered, so a request finishing just as the deadline lands gets a second
// WriteHeader on top of a response that was already correct.
func requestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// recoverPanic turns a panic in a handler into the 500 the contract promises.
//
// Without it the panic reaches net/http, which recovers it too -- the process
// survives either way -- but logs the stack through its own logger and closes
// the connection without answering. A client would see a dropped connection
// where the contract shows an envelope.
//
// It sits inside accessLog rather than around it, so that the panic is turned
// into a status before the deferred log line reads one. The other way round the
// line would report a status of zero and the 500 would appear nowhere.
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

				// Only if the handler has not answered yet. A panic after a
				// status has gone out cannot be reported to the client at all:
				// a second WriteHeader is refused with a complaint of its own,
				// and the envelope would land inside a body that is already
				// valid. The wrapper accessLog installed is what makes this
				// answerable, and is the second reason it is there.
				if wrapped, ok := w.(middleware.WrapResponseWriter); ok && wrapped.Status() != 0 {
					return
				}

				writeErrorJSON(w, http.StatusInternalServerError, codeInternalError, internalErrorMessage)
			}(r.Context())

			next.ServeHTTP(w, r)
		})
	}
}
