package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// stubResult is one scripted answer of stubProvider.
type stubResult struct {
	rate Rate
	err  error
}

// stubProvider answers from a script, one entry per call, and records what it
// was asked. Written by hand rather than generated: the interface has one
// method, and the generator would need more configuration than this.
type stubProvider struct {
	results []stubResult
	pairs   []domain.Pair
}

func (s *stubProvider) FetchRate(_ context.Context, pair domain.Pair) (Rate, error) {
	s.pairs = append(s.pairs, pair)

	if len(s.pairs) > len(s.results) {
		return Rate{}, fmt.Errorf("provider called %d times, script has %d answers", len(s.pairs), len(s.results))
	}

	r := s.results[len(s.pairs)-1]

	return r.rate, r.err
}

func TestRetrierFetchRate(t *testing.T) {
	// A failure marked the way the client marks one, and one it leaves bare.
	transient := fmt.Errorf("upstream status 503: %w", ErrTransient)
	permanent := errors.New("upstream status 404")

	answer := Rate{
		Value: decimal.RequireFromString("19.6552"),
		Date:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name     string
		results  []stubResult
		attempts int
		// wantErr is the error the call must carry; nil means it must succeed.
		wantErr error
	}{
		{
			name:     "answers on the first attempt",
			results:  []stubResult{{rate: answer}},
			attempts: 3,
		},
		{
			name:     "repeats a transient failure until it clears",
			results:  []stubResult{{err: transient}, {err: transient}, {rate: answer}},
			attempts: 3,
		},
		{
			name:     "gives up once the attempts run out",
			results:  []stubResult{{err: transient}, {err: transient}, {err: transient}},
			attempts: 3,
			wantErr:  ErrTransient,
		},
		{
			name:     "does not repeat a permanent failure",
			results:  []stubResult{{err: permanent}},
			attempts: 3,
			wantErr:  permanent,
		},
		{
			name:     "stops as soon as a failure turns out to be permanent",
			results:  []stubResult{{err: transient}, {err: permanent}},
			attempts: 3,
			wantErr:  permanent,
		},
		{
			name:     "a single attempt means no repeating at all",
			results:  []stubResult{{err: transient}},
			attempts: 1,
			wantErr:  ErrTransient,
		},
		{
			// Normalised by the constructor. Left as it is, the loop would not
			// run and the caller would get a zero rate with a nil error.
			name:     "an attempt count below one is raised to one",
			results:  []stubResult{{err: transient}},
			attempts: 0,
			wantErr:  ErrTransient,
		},
	}

	pair := mustParsePair(t, "EUR/MXN")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubProvider{results: tt.results}

			rate, err := NewRetrier(stub, tt.attempts, time.Millisecond).FetchRate(t.Context(), pair)

			// The script is written to be consumed exactly: an unused answer
			// means the retrier stopped early, and one call too many is
			// reported by the stub as an error of its own.
			require.Len(t, stub.pairs, len(tt.results))
			for _, got := range stub.pairs {
				require.Equal(t, pair, got)
			}

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.results[len(tt.results)-1].rate, rate)
		})
	}
}

// TestBudget pins the worst case to concrete numbers, because that is what the
// startup invariant on WORKER_TASK_TIMEOUT is compared against. Widening the
// jitter window or changing how the pauses grow moves these figures, and this
// is where that has to be noticed: the check would otherwise keep passing on a
// budget that had become too small.
func TestBudget(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		timeout  time.Duration
		backoff  time.Duration
		want     time.Duration
	}{
		{
			name:     "a single attempt is the timeout alone",
			attempts: 1,
			timeout:  3 * time.Second,
			backoff:  200 * time.Millisecond,
			want:     3 * time.Second,
		},
		{
			// The configured default: three requests of three seconds, plus
			// pauses of at most 300ms and 600ms.
			name:     "the configured schedule",
			attempts: 3,
			timeout:  3 * time.Second,
			backoff:  200 * time.Millisecond,
			want:     9900 * time.Millisecond,
		},
		{
			name:     "pauses double and each is taken at the top of its window",
			attempts: 4,
			timeout:  time.Second,
			backoff:  100 * time.Millisecond,
			want:     5050 * time.Millisecond,
		},
		{
			name:     "an attempt count below one is raised to one",
			attempts: 0,
			timeout:  3 * time.Second,
			backoff:  200 * time.Millisecond,
			want:     3 * time.Second,
		},
		{
			name:     "no backoff leaves only the requests",
			attempts: 3,
			timeout:  time.Second,
			backoff:  0,
			want:     3 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Budget(tt.attempts, tt.timeout, tt.backoff))
		})
	}
}

// TestBackoffSchedule pins the shape of the pauses, which Budget only ever
// checks as a sum: a schedule that stopped doubling, or one that started from
// the wrong end, would add up differently but a schedule with the same total
// in a different order would not.
//
// What is still covered by inspection alone is the indexing in FetchRate. It
// walks this list one entry per attempt, and a version taking the first pause
// every time would pass every test here -- telling the two apart means timing
// a run against a jitter window wide enough to swallow the difference.
func TestBackoffSchedule(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		backoff  time.Duration
		want     []time.Duration
	}{
		{
			// One attempt is one request and nothing to wait for.
			name:     "a single attempt has no pauses at all",
			attempts: 1,
			backoff:  100 * time.Millisecond,
			want:     []time.Duration{},
		},
		{
			name:     "the configured schedule",
			attempts: 3,
			backoff:  200 * time.Millisecond,
			want:     []time.Duration{200 * time.Millisecond, 400 * time.Millisecond},
		},
		{
			name:     "each pause is twice the one before it",
			attempts: 4,
			backoff:  100 * time.Millisecond,
			want: []time.Duration{
				100 * time.Millisecond,
				200 * time.Millisecond,
				400 * time.Millisecond,
			},
		},
		{
			// Doubling nothing is still nothing: the attempts follow one
			// another as fast as the upstream answers.
			name:     "no backoff means pauses of zero, not no pauses",
			attempts: 3,
			backoff:  0,
			want:     []time.Duration{0, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, backoffSchedule(tt.attempts, tt.backoff))
		})
	}
}

// TestJitteredStaysInItsWindow pins the formula rather than the draw. The
// window is what the spread is for: a factor that could come out at or below
// zero would collapse the backoff, and one drawn from [0,1) would only ever
// shorten it, which is the mistake this guards against.
func TestJitteredStaysInItsWindow(t *testing.T) {
	const nominal = time.Second

	low := time.Duration(float64(nominal) * jitterMin)
	high := time.Duration(float64(nominal) * (jitterMin + jitterSpan))

	for range 1000 {
		got := jittered(nominal)

		require.GreaterOrEqual(t, got, low)
		require.Less(t, got, high)
	}
}

// TestRetrierBackoffIsInterruptible checks that the pause between attempts ends
// with the context rather than with the timer. The backoff is an hour: if the
// wait were not interruptible this test would not fail, it would hang.
func TestRetrierBackoffIsInterruptible(t *testing.T) {
	stub := &stubProvider{results: []stubResult{{err: fmt.Errorf("upstream status 503: %w", ErrTransient)}}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewRetrier(stub, 3, time.Hour).FetchRate(ctx, mustParsePair(t, "EUR/MXN"))

	require.ErrorIs(t, err, context.Canceled)
	// The first attempt happens before any pause; the second never starts.
	require.Len(t, stub.pairs, 1)
}
