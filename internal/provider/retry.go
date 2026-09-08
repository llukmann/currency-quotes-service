package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Each pause is multiplied by a factor drawn uniformly from
// [jitterMin, jitterMin+jitterSpan). Doubling alone spaces out the attempts of
// one caller but not those of several: a pool of workers that met the same 429
// would step back and return in unison, rebuilding the burst that produced the
// limit. The spread is what breaks the group apart, which is why it belongs
// here rather than in a later round of tuning.
const (
	jitterMin  = 0.5
	jitterSpan = 1.0
)

// Retrier repeats a fetch that failed in a way the next attempt may not meet.
// It is a RateProvider itself, so whoever holds one cannot tell whether the
// rate arrived on the first try or the third.
type Retrier struct {
	next     RateProvider
	attempts int
	backoff  time.Duration
}

var _ RateProvider = (*Retrier)(nil)

// NewRetrier wraps next.
//
// attempts is the total number of tries, not the number of retries after the
// first, so one means no retrying at all. backoff is the pause before the
// second attempt and doubles before each one after it; every pause is drawn
// from a window around its nominal length, see jittered.
//
// The three settings arrive as arguments rather than being read from the
// environment here: the caller that constructs a provider is the one holding
// the configuration, and this package has no business knowing which variables
// exist.
//
// attempts below one is raised to one instead of rejected. A zero would leave
// the loop below unentered and hand the caller a zero rate with a nil error,
// and that failure is worse than anything a constructor returning an error
// would have prevented.
func NewRetrier(next RateProvider, attempts int, backoff time.Duration) *Retrier {
	if attempts < 1 {
		attempts = 1
	}

	return &Retrier{next: next, attempts: attempts, backoff: backoff}
}

// Budget is the longest a Retrier built with these arguments can take: every
// attempt spending its whole timeout, and every pause drawn at the top of its
// jitter window.
//
// It lives here because the number belongs to the retry schedule, and the
// schedule includes a jitter window this package owns. The caller that needs
// it is the configuration, which has to refuse a task deadline too short to
// hold a full run: computed there, the window would be either duplicated or
// quietly left out, and a budget that came out too small would let the check
// pass exactly when it should not.
//
// The loop mirrors the one in FetchRate so the two cannot drift apart.
func Budget(attempts int, timeout, backoff time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}

	total := time.Duration(attempts) * timeout

	delay := backoff
	for attempt := 2; attempt <= attempts; attempt++ {
		total += time.Duration(float64(delay) * (jitterMin + jitterSpan))
		delay *= 2
	}

	return total
}

// FetchRate calls the wrapped provider until it answers, an error turns out not
// to be transient, or the attempts run out. The last transient error is what is
// returned in that final case, still wrapping ErrTransient, so a caller can
// tell an upstream that never answered from one that refused.
func (r *Retrier) FetchRate(ctx context.Context, pair domain.Pair) (Rate, error) {
	var lastErr error

	delay := r.backoff

	for attempt := 1; attempt <= r.attempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, jittered(delay)); err != nil {
				return Rate{}, fmt.Errorf("fetch rate %s: %w", pair, err)
			}

			// The undistorted delay is what doubles, so the schedule keeps its
			// shape instead of drifting with whatever each draw happened to be.
			delay *= 2
		}

		rate, err := r.next.FetchRate(ctx, pair)
		if err == nil {
			return rate, nil
		}

		// Everything not marked transient would be answered the same way next
		// time, and a context that is done has no next time at all.
		if !errors.Is(err, ErrTransient) {
			return Rate{}, err
		}

		lastErr = err
	}

	return Rate{}, fmt.Errorf("after %d attempts: %w", r.attempts, lastErr)
}

// jittered spreads d over a window around itself.
//
// The generator of math/rand/v2 is safe for concurrent use and needs no
// seeding, and both matter here: one Retrier is shared by the whole pool, and
// those workers are exactly the callers that must not draw the same pause.
func jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (jitterMin + rand.Float64()*jitterSpan))
}

// sleep waits for d or for ctx to be done, whichever comes first.
//
// A pause that ignored cancellation would hold a worker past the deadline of
// the task it carries, so the time actually spent would exceed the budget that
// deadline was computed from -- and on shutdown it would keep the process alive
// for a backoff nobody is waiting for any more.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
