// Package provider fetches reference exchange rates from an upstream service.
//
// It holds a client for the frankfurter API and a decorator that retries it.
// Neither touches the database, and neither decides what a client of this
// service is told: they report what the upstream said and whether the failure
// is worth another attempt. Turning that into the message stored on a failed
// task belongs to the worker, which is also where raw upstream errors stop and
// are logged.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Rate is a rate as the upstream reported it.
//
// It carries no fetch time. When this service received a value is a fact about
// this service rather than about the upstream, and the worker is what records
// it, so that a retried call cannot leave behind the timestamp of an attempt
// that failed.
type Rate struct {
	// Value is how many units of the quote currency one unit of the base
	// currency buys.
	Value decimal.Decimal
	// Date is the day the rate is valid for. The upstream publishes daily
	// reference rates and no publication instant at all, so only the day means
	// anything; the value is midnight UTC.
	Date time.Time
}

// RateProvider fetches the current rate for a pair.
//
// The interface is declared beside its implementations rather than at the
// consumer because one of its consumers is here: Retrier wraps a RateProvider.
// The worker declares the narrow interface it needs where it uses it.
type RateProvider interface {
	FetchRate(ctx context.Context, pair domain.Pair) (Rate, error)
}

// ErrTransient marks a failure that another attempt may not meet: a network
// error, a timeout, a 429 or a 5xx. Retrier repeats exactly the errors wrapping
// it, so everything left unmarked -- a currency the upstream rejects, a body it
// cannot have meant -- fails on the first attempt instead of being asked three
// times for the same answer.
//
// A cancelled or expired context is not marked either. That is the caller
// leaving rather than the upstream failing, and sleeping before retrying it
// would spend a budget that has already run out.
var ErrTransient = errors.New("transient provider failure")
