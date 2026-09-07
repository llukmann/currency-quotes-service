package domain

import (
	"time"

	"github.com/google/uuid"
)

// Status is the state of an update task. The values are exactly those allowed
// by the CHECK constraint on quote_updates.status.
type Status string

const (
	// StatusPending is a task waiting to be claimed by a worker.
	StatusPending Status = "pending"
	// StatusInProgress is a task some worker has claimed and not yet finished.
	StatusInProgress Status = "in_progress"
	// StatusDone is a task whose rate was fetched and stored.
	StatusDone Status = "done"
	// StatusFailed is a task that will not be retried. Terminal: a client that
	// still wants the rate posts a new update.
	StatusFailed Status = "failed"
)

// QuoteUpdate is one row of the update queue: a request to refresh a pair,
// driven to a terminal status by the background worker.
type QuoteUpdate struct {
	ID   uuid.UUID
	Pair Pair
	// Status is the state at the time the row was read; a worker holds no lock
	// on it afterwards.
	Status Status
	// Attempts counts how many times the task has been claimed, so it is 1 for
	// a task processed on the first try. It bounds the recovery loop, and it
	// doubles as the identity of a single claim: since a claim is the only
	// thing that increments it, a finalising worker proves the task is still
	// its own by matching this value. See Repository.Complete.
	Attempts int
	// Error is the reason a task failed, empty for every other status.
	Error     string
	CreatedAt time.Time
	// UpdatedAt is the last status change. For StatusInProgress it is the
	// claim time, which is what the recovery pass measures staleness against,
	// and it comes from the database clock rather than the application's.
	UpdatedAt time.Time
}

// UpdateDetails is a task together with the rate it produced, which exists
// only once the task reached StatusDone. It answers GET /quotes/updates/{id}
// in a single query.
type UpdateDetails struct {
	Update QuoteUpdate
	// Quote is nil while the task has not completed successfully.
	Quote *Quote
}
