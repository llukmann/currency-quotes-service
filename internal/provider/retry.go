package provider

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Half to one and a half times the nominal pause. Doubling spaces out the
// attempts of one caller but not those of several: a pool of workers that met
// the same 429 would step back and return in unison, rebuilding the burst that
// produced the limit.
const jitter = 0.5

type Retrier struct {
	next     RateProvider
	attempts int
	backoff  time.Duration
}

var _ RateProvider = (*Retrier)(nil)

// attempts is the total number of tries, not the number of retries after the
// first. Below one it is raised to one: passed on as it stands it would mean no
// limit at all.
func NewRetrier(next RateProvider, attempts int, backoff time.Duration) *Retrier {
	if attempts < 1 {
		attempts = 1
	}

	return &Retrier{next: next, attempts: attempts, backoff: backoff}
}

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
		// context that is done ends it too, which the library checks itself.
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
