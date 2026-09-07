package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// mustParsePair builds the pair the tests ask for. Going through the
// constructor rather than converting a literal keeps the tests honest about
// where a Pair comes from: Currencies below only works because of it.
func mustParsePair(t *testing.T, s string) domain.Pair {
	t.Helper()

	p, err := domain.ParsePair(s)
	require.NoError(t, err)

	return p
}

// TestClientFetchRate drives the client against a stand-in upstream. The cases
// that matter are the ones a live request cannot produce: a null rate, a body
// that does not parse, a 429, a 500.
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
		// wantTransient says whether the failure is one worth repeating. It is
		// asserted for successes and failures alike, so a permanent error that
		// started wrapping ErrTransient would be caught here.
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

// TestClientFetchRateUnreachableUpstream covers the failure that never reaches
// a status code at all.
func TestClientFetchRateUnreachableUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	// Nothing listens on that port from here on.
	srv.Close()

	_, err := NewClient(addr, time.Minute).FetchRate(t.Context(), mustParsePair(t, "EUR/MXN"))
	require.ErrorIs(t, err, ErrTransient)
}

// TestClientFetchRateTimesOut checks the per-attempt timeout, which is what
// stops an upstream that accepts a connection and then says nothing from
// holding a worker for the whole budget of its task.
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

// TestClientFetchRateContextCancelled checks that a cancelled context comes
// back as itself and unmarked. Reported as transient it would be retried, and
// the retrier would sleep out a backoff on behalf of a caller that has already
// gone.
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
