// Package service holds the business operations the HTTP layer offers.
//
// It is deliberately thin: the pair a client sent has already been parsed by
// the time a call gets here, so what is left is the ordering that matters --
// queueing a task and signalling a worker as one step -- and a pair of reads
// that exist so that every handler reaches storage the same way.
//
// Nothing here contacts the rate provider. Posting an update queues a task and
// signals a worker; both reads answer from the database alone.
package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// repository is the part of the storage layer this package uses, declared here
// because the consumer is what knows which statements it needs. The worker
// declares its own, narrower set for the same reason: the two have no method
// in common, since one drains the queue and the other only fills and reads it.
type repository interface {
	CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error)
	GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error)
}

// Service answers the three business endpoints. It holds nothing that changes
// and is safe for concurrent use.
type Service struct {
	repo repository
	wake chan<- struct{}
}

// New returns a service backed by repo, signalling wake whenever it queues a
// task. Only the sending half is held here: this package puts work into the
// queue and never takes any out.
func New(repo repository, wake chan<- struct{}) *Service {
	return &Service{repo: repo, wake: wake}
}

// CreateTask queues a refresh of pair and returns the task as stored, which is
// not always a new one: an unfinished task for the same pair, or a live
// idempotency key, is answered with the task that already exists.
//
// It does not perform the refresh. That is the worker's, and the asynchronous
// contract rests on this path never reaching the provider.
func (s *Service) CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error) {
	task, err := s.repo.CreateTask(ctx, pair, key)
	if err != nil {
		return domain.UpdateTask{}, err
	}

	// Only for a task that is actually waiting. One answered from an
	// idempotency key may be long finished, and a task already in progress has
	// a worker on it: waking one for either would be a poll of an empty queue.
	if task.Status == domain.StatusPending {
		s.notify()
	}

	return task, nil
}

// notify tells an idle worker the queue is worth a look, and drops the signal
// if one is already pending. The channel says that work arrived, not how much:
// a worker that wakes drains what it finds.
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// GetTask returns an update and whatever it has produced so far.
func (s *Service) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	return s.repo.GetTask(ctx, id)
}

// GetLatestQuote returns the last rate stored for pair.
func (s *Service) GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	return s.repo.GetLatestQuote(ctx, pair)
}
