package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Going through the constructor rather than converting a literal keeps the
// tests honest about where a Pair comes from: Currencies below only works
// because of it.
func mustParsePair(t *testing.T, s string) domain.Pair {
	t.Helper()

	p, err := domain.ParsePair(s)
	require.NoError(t, err)

	return p
}

// The client against a stand-in upstream. The cases that matter are the ones a
// live request cannot produce: a null rate, a body that does not parse, a 429,
// a 500.
func TestClientFetchRate(t *testing.T) {
	// The day every successful case below reports.
	wantDate := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// status defaults to 200 when zero.
		status int
		body   string
		// wantRate is the decimal the call must return; empty means the call
		// must fail instead.
		wantRate string
		// wantTransient says whether the failure is one worth repeating, and is
		// checked on every case that fails. It is what the retrier branches on,
		// so a permanent error that started wrapping ErrTransient would be
		// caught here -- and so would a transient one that stopped.
		wantTransient bool
	}{
		{
			name:     "rate and date of a normal answer",
			body:     `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":19.6552}}`,
			wantRate: "19.6552",
		},
		{
			// The reason latestResponse types the map as decimal.Decimal: read
			// through float64, this value comes back 20.139450000000000073896.
			name:     "every digit of the rate survives decoding",
			body:     `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":20.13945000000000000123456789}}`,
			wantRate: "20.13945000000000000123456789",
		},
		{
			name:     "rate quoted as a string",
			body:     `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":"19.6552"}}`,
			wantRate: "19.6552",
		},
		{
			// A null unmarshals into a zero decimal with no error and with the
			// key present, so only the positivity check stands between it and a
			// task closed as done carrying a rate of zero.
			name: "null rate",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":null}}`,
		},
		{
			name: "zero rate",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":0}}`,
		},
		{
			name: "negative rate",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":-19.6552}}`,
		},
		{
			name: "rate for a currency that was not asked for",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"USD":1.17}}`,
		},
		{
			name: "no rates at all",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{}}`,
		},
		{
			name: "truncated body",
			body: `{"amount":1.0,"base":"EUR","date":`,
		},
		{
			name: "rate that is not a number",
			body: `{"amount":1.0,"base":"EUR","date":"2026-09-07","rates":{"MXN":"not a number"}}`,
		},
		{
			name: "date in another format",
			body: `{"amount":1.0,"base":"EUR","date":"07.09.2026","rates":{"MXN":19.6552}}`,
		},
		{
			name: "no date",
			body: `{"amount":1.0,"base":"EUR","rates":{"MXN":19.6552}}`,
		},
		{
			// What the live API answers for a currency it does not know. Asking
			// again would be answered identically.
			name:   "currency the upstream rejects",
			status: http.StatusNotFound,
			body:   `{"message":"not found"}`,
		},
		{
			name:   "request the upstream refuses",
			status: http.StatusBadRequest,
			body:   `{"message":"bad request"}`,
		},
		{
			// The upstream giving up on a slow request, which the next attempt
			// may well not meet.
			name:          "upstream timed out waiting for the request",
			status:        http.StatusRequestTimeout,
			body:          `{"message":"request timeout"}`,
			wantTransient: true,
		},
		{
			name:          "rate limited",
			status:        http.StatusTooManyRequests,
			body:          `{"message":"too many requests"}`,
			wantTransient: true,
		},
		{
			name:          "upstream failure",
			status:        http.StatusInternalServerError,
			body:          `{"message":"internal server error"}`,
			wantTransient: true,
		},
		{
			name:          "upstream unavailable",
			status:        http.StatusServiceUnavailable,
			wantTransient: true,
		},
	}

	pair := mustParsePair(t, "EUR/MXN")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}

			var (
				gotPath  string
				gotQuery url.Values
			)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.Path, r.URL.Query()

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			rate, err := NewClient(srv.URL, time.Minute).FetchRate(t.Context(), pair)

			// Every case asks the upstream the same way: the pair arrives split
			// into the two parameters the API names, and the path is the
			// client's own rather than part of the configured host.
			require.Equal(t, latestPath, gotPath)
			require.Equal(t, "EUR", gotQuery.Get("base"))
			require.Equal(t, "MXN", gotQuery.Get("symbols"))

			if tt.wantRate == "" {
				require.Error(t, err)
				require.Equal(t, tt.wantTransient, errors.Is(err, ErrTransient))

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantRate, rate.Value.String())
			require.Equal(t, wantDate, rate.Date)
		})
	}
}

// The upstream that promises a body and then goes away half way through it.
// The answer never becomes a status the client can classify, so the failure
// has to be marked transient here, in the read -- an upstream that died
// mid-sentence is exactly the kind another attempt may not meet.
//
// Not the same case as the truncated body in the table above: that one is a
// complete HTTP response carrying JSON that does not parse, and it fails a
// dozen lines later with an answer of its own.
func TestClientFetchRateBodyCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Written straight onto the connection: net/http would otherwise
		// correct the length to what was actually sent, which is the one thing
		// this test needs to be wrong.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}

		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{\"rates\":"))
		_ = conn.Close()
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))

	require.ErrorIs(t, err, ErrTransient)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// The other half of the rule TestClientFetchRateContextCancelled covers. A
// cancellation can land after the headers have arrived as easily as before
// them, and the read has to tell the two apart the same way the request does:
// the caller leaving is not the upstream failing, and marked transient it
// would buy a backoff nobody is waiting out.
func TestClientFetchRateContextCancelledMidBody(t *testing.T) {
	reached := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A body promised and begun, so the client is inside the read rather
		// than still waiting for a status.
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"rates":`)

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		close(reached)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		<-reached

		// The handler has flushed its headers, but the client may not have
		// finished parsing them yet, and cancelling in that instant would end
		// the request rather than the read. The pause is not what makes the
		// assertion hold -- both paths answer the same way, which is the point
		// -- it is what keeps the test on the path it was written for.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := NewClient(srv.URL, time.Minute).FetchRate(ctx, mustParsePair(t, "EUR/MXN"))

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrTransient)
}

// The body below is a valid answer with the rate placed beyond the cap, so the
// truncation is what the assertion rests on: read whole it would parse, read
// to the cap it cannot.
//
// The padding is sized from the constant, so this checks that a cap is
// enforced and not what it is set to.
func TestClientFetchRateReadsNoMoreThanTheCap(t *testing.T) {
	padding := strings.Repeat("x", maxBodySize)
	body := `{"padding":"` + padding + `","date":"2026-09-07","rates":{"MXN":19.6552}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTransient)
	require.Greater(t, len(body), maxBodySize, "the body has to be larger than the cap for this to prove anything")
}

// The other half of that bound. The error below is written on every failed
// attempt of every task, and the body it describes is capped at 64 KB -- which
// is 64 KB per line in the log unless it is cut here.
func TestClientFetchRateQuotesOnlyASnippetOfTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", maxBodySize))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))

	require.ErrorIs(t, err, ErrTransient)
	// The message carries the pair, the status and a snippet; what it must not
	// carry is the body.
	require.Less(t, len(err.Error()), snippetSize*2)
}

// Where the cut falls. The boundary is the whole of what this function
// decides, and an off-by-one either way is invisible in the messages it
// appears in.
func TestSnippet(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an empty body",
			body: "",
			want: "",
		},
		{
			// Bodies arrive with a trailing newline more often than not, and it
			// buys nothing in the middle of a log line.
			name: "surrounding whitespace is dropped",
			body: "  {\"message\":\"not found\"}\n",
			want: `{"message":"not found"}`,
		},
		{
			name: "a body exactly at the limit is left whole",
			body: strings.Repeat("x", snippetSize),
			want: strings.Repeat("x", snippetSize),
		},
		{
			name: "one byte past the limit is cut",
			body: strings.Repeat("x", snippetSize+1),
			want: strings.Repeat("x", snippetSize) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, snippet([]byte(tt.body)))
		})
	}
}

// The one failure that happens before anything is sent. The host is
// configuration, so this is the shape a typo in PROVIDER_BASE_URL takes, and
// it is deliberately not transient: retrying a URL that cannot be parsed
// spends the budget on the same answer three times.
func TestClientFetchRateMalformedBaseURL(t *testing.T) {
	_, err := NewClient("http://exa mple.com", time.Minute).
		FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTransient)
}

// The one thing the constructor does to its argument. A host written with a
// slash is the likelier spelling of the two, and left alone it would produce a
// double slash in the path -- which the upstream is under no obligation to
// treat as the same endpoint.
func TestNewClientTrimsATrailingSlash(t *testing.T) {
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"date":"2026-09-07","rates":{"MXN":19.6552}}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL+"/", time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))

	require.NoError(t, err)
	require.Equal(t, latestPath, gotPath)
}

// The failure that never reaches a status code at all.
func TestClientFetchRateUnreachableUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	// Nothing listens on that port from here on.
	srv.Close()

	_, err := NewClient(addr, time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))
	require.ErrorIs(t, err, ErrTransient)
}

// The per-attempt timeout, which is what stops an upstream that accepts a
// connection and then says nothing from holding a worker for the whole budget
// of its task.
func TestClientFetchRateTimesOut(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	_, err := NewClient(srv.URL, 50*time.Millisecond).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))
	require.ErrorIs(t, err, ErrTransient)
}

// A cancelled context comes back as itself and unmarked. Reported as transient
// it would be retried, and the retrier would sleep out a backoff on behalf of
// a caller that has already gone.
func TestClientFetchRateContextCancelled(t *testing.T) {
	reached := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(reached)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		<-reached
		cancel()
	}()

	_, err := NewClient(srv.URL, time.Minute).FetchRate(ctx, mustParsePair(t, "EUR/MXN"))
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrTransient)
}
