package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Quote is a rate that was fetched and stored successfully.
//
// It is a read model. Callers never assemble one to be written: the
// finalisation path takes the values it stores from the claim it holds, so
// that the pair recorded here cannot disagree with the task it belongs to.
type Quote struct {
	// UpdateID is the task that produced this rate, and the identity of the
	// row: one quote per update.
	UpdateID uuid.UUID
	Pair     Pair
	// Rate is how many units of the quote currency one unit of the base
	// currency buys. Decimal rather than a float: a rate that survives a round
	// trip through binary floating point is a coincidence, not a guarantee.
	Rate decimal.Decimal
	// RateDate is the day the rate is valid for, as reported by the provider.
	// Only the date part is meaningful -- the provider publishes no instant --
	// so the value is midnight UTC and is formatted without a time.
	RateDate time.Time
	// FetchedAt is when this service received the rate. It can be days after
	// RateDate: reference rates are published on working days only, and a
	// weekend request returns the last of them.
	FetchedAt time.Time
}
