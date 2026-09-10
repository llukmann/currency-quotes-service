// Package service holds the business operations the HTTP layer offers. Nothing
// here contacts the rate provider: posting an update queues a task and signals
// a worker, and both reads answer from the database alone.
package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

type repository interface {
	CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error)
	GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error)
}

type Service struct {
	repo repository
	wake chan<- struct{}
}

func New(repo repository, wake chan<- struct{}) *Service {
	return &Service{repo: repo, wake: wake}
}

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

// The channel says that work arrived, not how much: a worker that wakes drains
// what it finds, so a signal already pending is dropped.
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	return s.repo.GetTask(ctx, id)
}

func (s *Service) GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	return s.repo.GetLatestQuote(ctx, pair)
}
