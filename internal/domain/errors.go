// Package domain holds the business entities of the service. It depends on
// nothing else in the project.
package domain

import "errors"

var (
	ErrInvalidPair = errors.New("invalid currency pair")
	ErrNotFound    = errors.New("not found")
	ErrKeyConflict = errors.New("idempotency key already used with a different pair")

	// The task stopped being the caller's, so whatever it fetched has to be
	// discarded rather than written.
	ErrStaleClaim = errors.New("stale claim")
)
