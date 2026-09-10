package domain

import (
	"time"

	"github.com/google/uuid"
)

// Status values are exactly those the CHECK constraint on
// quote_updates.status allows.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	// Terminal and not retried: a client that still wants the rate posts a new
	// update.
	StatusFailed Status = "failed"
)

type UpdateTask struct {
	ID   uuid.UUID
	Pair Pair
	// The state when the row was read; no lock is held on it afterwards.
	Status Status
	// A claim is the only thing that increments this, so a finalising worker
	// proves the task is still its own by matching the value.
	Attempts  int
	Error     string
	CreatedAt time.Time
	// By the database clock. For StatusInProgress it is the claim time, which
	// is what the recovery pass measures staleness against.
	UpdatedAt time.Time
}

type TaskDetails struct {
	Task UpdateTask
	// Nil while the task has not completed successfully.
	Quote *Quote
}
