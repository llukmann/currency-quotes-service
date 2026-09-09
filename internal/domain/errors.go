// Package domain holds the business entities of the service and the errors
// they are described by. It depends on nothing else in the project.
package domain

import "errors"

var (
	// ErrInvalidPair reports a pair that is malformed or built from currencies
	// outside the supported set. It is raised before anything reaches the
	// provider or the database.
	ErrInvalidPair = errors.New("invalid currency pair")

	// ErrNotFound reports a row that does not exist. Both lookups return it,
	// since the API answers 404 for a missing update and for a pair that has
	// never been quoted alike.
	ErrNotFound = errors.New("not found")

	// ErrKeyConflict reports an idempotency key already bound to a task for a
	// different pair. The request itself is well formed -- what objects is the
	// history, so the API answers 409 rather than 400, and returning the bound
	// task instead would hand the client an update of a pair it never asked
	// for.
	ErrKeyConflict = errors.New("idempotency key already used with a different pair")

	// ErrStaleClaim reports that the task is no longer held by the caller: the
	// recovery pass released or closed it, and another worker may have claimed
	// it since. Whatever the caller fetched has to be discarded -- the task did
	// not fail, it merely stopped being ours, and reporting it as failed would
	// overwrite the state of whoever owns it now.
	ErrStaleClaim = errors.New("stale claim")
)
