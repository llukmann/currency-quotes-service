package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Quote struct {
	UpdateID uuid.UUID
	Pair     Pair
	Rate     decimal.Decimal
	// Always midnight UTC: parsed from a bare date, stored in a date column.
	RateDate time.Time
	// Can be days after RateDate: reference rates are published on working
	// days only.
	FetchedAt time.Time
}
