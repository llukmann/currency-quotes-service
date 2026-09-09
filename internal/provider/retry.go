package provider

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// jitter is how far a pause may be drawn from its nominal length, as a
// fraction of it: half to one and a half times.
//
// Doubling alone spaces out the attempts of one caller but not those of
// several: a pool of workers that met the same 429 would step back and return
// in unison, rebuilding the burst that produced the limit. The spread is what
// breaks the group apart, so it is set here rather than inherited from a
// default that happens to agree with it today.
const jitter = 0.5

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
// from a window around its nominal length, see jitter.
//
// The three settings arrive as arguments rather than being read from the
// environment here: the caller that constructs a provider is the one holding
// the configuration, and this package has no business knowing which variables
// exist.
//
// attempts below one is raised to one. Passed on as it stands it would mean no
// limit at all, and an upstream that is down would be asked until the task
// deadline ran out.
func NewRetrier(next RateProvider, attempts int, backoff time.Duration) *Retrier {
	if attempts < 1 {
		attempts = 1
	}

	return &Retrier{next: next, attempts: attempts, backoff: backoff}
}

// FetchRate calls the wrapped provider until it answers, an error turns out not
// to be transient, or the attempts run out. The last transient error is what is
// returned in that final case, still wrapping ErrTransient, so a caller can
// tell an upstream that never answered from one that refused.
func (r *Retrier) FetchRate(ctx context.Context, pair domain.Pair) (Rate, error) {
	// Built per call, not held on the Retrier: the schedule carries the
	// interval it has reached, and one Retrier is shared by the whole pool.
	schedule := backoff.NewExponentialBackOff()
	schedule.InitialInterval = r.backoff
	schedule.Multiplier = 2
	schedule.RandomizationFactor = jitter

	return backoff.Retry(ctx, func() (Rate, error) {
		rate, err := r.next.FetchRate(ctx, pair)

		// Everything not marked transient would be answered the same way next
		// time, so it ends the run instead of spending the attempts left. A
		// context that is done ends it too, and the library checks that
		// itself, both between attempts and during a pause.
		if err != nil && !errors.Is(err, ErrTransient) {
			return Rate{}, backoff.Permanent(err)
		}

		return rate, err
	},
		backoff.WithBackOff(schedule),
		backoff.WithMaxTries(uint(r.attempts)),
		// The attempt count is the only thing that ends a run. Left alone this
		// would be a second limit of fifteen minutes, invisible in the
		// configuration and in the logs.
		backoff.WithMaxElapsedTime(0),
	)
}
