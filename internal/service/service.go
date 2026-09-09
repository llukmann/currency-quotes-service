// Package service holds the business operations the HTTP layer offers.
//
// It is deliberately thin. Two of its three methods do one thing the handlers
// must not be trusted with -- turn a string from a client into a validated
// domain.Pair -- and the third is a lookup that exists so that every handler
// reaches storage the same way. Posting an update then carries an idempotency
// key through to storage, which is where a key is answered: the ordering that
// matters, parsing the pair before anything compares one, lives here.
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

// CreateTask validates pair and queues a refresh of it, returning the task as
// stored. It does not perform the refresh: that is the worker's, and the
// asynchronous contract rests on this handler path never reaching the
// provider.
//
// The pair arrives as the raw string a client sent, and parsing it is this
// method's own work rather than the handler's. The whitelist lives in domain
// and nothing in the schema enforces it -- the CHECK constraint knows only the
// shape of a pair and would accept EUR/RUB -- so a forgotten ParsePair would
// have nothing left to catch it. With the call here there is one caller per
// rule instead of one per endpoint.
//
// A failure of ParsePair is returned unwrapped. The sentence it carries names
// the currency at fault and is written to be read by a client, which is what
// the handler serves as the message of an invalid_pair; wrapping it here would
// prepend an operation name that means nothing outside this process.
//
// key is the client's Idempotency-Key, or nil if it sent none. It is passed
// on rather than acted on here: what a key means is a question about rows --
// which one it is bound to, and whether that binding still exists -- and the
// answer is arbitrated by a constraint rather than by anything this layer could
// check. Parsing comes first all the same, so that the pair a key is compared
// against is the normalised one and "eur/mxn" does not conflict with "EUR/MXN".
//
// Returns domain.ErrKeyConflict if the key was used before with another pair.
func (s *Service) CreateTask(ctx context.Context, pair string, key *uuid.UUID) (domain.UpdateTask, error) {
	p, err := domain.ParsePair(pair)
	if err != nil {
		return domain.UpdateTask{}, err
	}

	task, err := s.repo.CreateTask(ctx, p, key)
	if err != nil {
		return domain.UpdateTask{}, err
	}

	// After the row exists, never before: a worker woken by this signal claims
	// from the table, so a signal sent ahead of the insert would find nothing
	// and be spent.
	//
	// Only for a task actually waiting to be claimed. Deduplication and key
	// replays answer with tasks in every status, and a task already in progress
	// or long finished gives a woken worker nothing to do.
	if task.Status == domain.StatusPending {
		s.notify()
	}

	return task, nil
}

// notify tells an idle worker there is something to claim, so that a posted
// task is picked up at once instead of at the next poll.
//
// The send never blocks and the signal is never queued more than once. Both
// follow from what the signal means: it says the queue is worth looking at,
// not how many tasks are in it, and a worker that wakes goes on claiming until
// the queue is empty. A burst of posts therefore costs one wakeup rather than
// one each, and a post that arrives while every worker is busy costs nothing
// at all -- they will find the task without being told.
//
// A response must never wait on this, which is the other half of why the send
// is non-blocking: the task is already committed by the time we get here, and
// a worker that missed the signal still polls.
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// GetTask returns the state of one update together with the rate it produced,
// which exists only once it has completed. Every status is an answer,
// including failed: a task that could not be refreshed is a fact about the
// task, not a failure of the request asking about it.
func (s *Service) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	return s.repo.GetTask(ctx, id)
}

// GetLatestQuote returns the most recent rate stored for pair, whichever
// update produced it. Like CreateTask it takes the raw string and validates it
// here, so that an unsupported pair is refused rather than answered with a 404
// that would suggest it might exist later.
func (s *Service) GetLatestQuote(ctx context.Context, pair string) (domain.Quote, error) {
	p, err := domain.ParsePair(pair)
	if err != nil {
		return domain.Quote{}, err
	}

	return s.repo.GetLatestQuote(ctx, p)
}
