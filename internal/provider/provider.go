// Package provider fetches reference exchange rates from an upstream service.
// It decides nothing about what a client is told: it reports what the upstream
// said and whether the failure is worth another attempt.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Carries no fetch time: when this service received a value is a fact about
// this service, and the worker records it, so a retried call cannot leave
// behind the timestamp of an attempt that failed.
type Rate struct {
	Value decimal.Decimal
	// Midnight UTC: the upstream publishes daily reference rates and no
	// publication instant at all.
	Date time.Time
}

// Declared beside its implementations rather than at the consumer because one
// of its consumers is here: Retrier wraps a RateProvider.
type RateProvider interface {
	FetchRate(ctx context.Context, pair domain.Pair) (Rate, error)
}

// Retrier repeats exactly the errors wrapping this one, so a currency the
// upstream rejects fails on the first attempt instead of being asked three
// times for the same answer. A cancelled context is not marked either: that is
// the caller leaving rather than the upstream failing.
var ErrTransient = errors.New("transient provider failure")
