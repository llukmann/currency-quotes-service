// Package worker drains the queue of update tasks. It is where raw upstream
// errors stop: deciding what a client is told about a failure, and what only
// the log sees, belongs here.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
	"github.com/llukmann/currency-quotes-service/internal/provider"
)

// Reasons stored in quote_updates.error and served to clients. The set is
// deliberately short and fixed: it says what happened to the task, never how
// this service is built. The upstream's own words, its URL and its status code
// stay in the log.
const (
	reasonUnavailable      = "provider unavailable after %d attempts"
	reasonUnusableResponse = "provider returned an unusable response"
)

type repository interface {
	ClaimTask(ctx context.Context) (domain.UpdateTask, bool, error)
	CompleteTask(ctx context.Context, claim domain.UpdateTask, rate decimal.Decimal, rateDate, fetchedAt time.Time) error
	FailTask(ctx context.Context, claim domain.UpdateTask, reason string) error
	ReleaseTask(ctx context.Context, claim domain.UpdateTask) error
}

// Handing a claim back runs on a context that has already been cancelled and
// therefore needs a deadline of its own. If even this is not enough the task is
// not lost: it stays in progress and the recovery pass has it.
const releaseTimeout = 5 * time.Second

const claimFailureReportInterval = time.Minute

// The provider as seen from here: one call, no retry policy of its own. The
// retrier is a RateProvider too, so a worker cannot tell whether the rate
// arrived on the first attempt or the third.
type rateProvider interface {
	FetchRate(ctx context.Context, pair domain.Pair) (provider.Rate, error)
}

// The numbers arrive from the configuration through main: this package neither
// reads the environment nor knows which variables exist.
type Settings struct {
	TaskTimeout  time.Duration
	PollInterval time.Duration
	// Here only to phrase reasonUnavailable: the retrying itself is the
	// provider's business, and a worker does not count the tries.
	ProviderAttempts int
}

type Worker struct {
	repo     repository
	rates    rateProvider
	wake     <-chan struct{}
	settings Settings
	logger   *slog.Logger
}

// A nil wake channel is legal and means the worker only polls.
func New(repo repository, rates rateProvider, wake <-chan struct{}, settings Settings, logger *slog.Logger) *Worker {
	return &Worker{repo: repo, rates: rates, wake: wake, settings: settings, logger: logger}
}

// An idle worker waits for whichever comes first, the poll interval or a signal
// that a task has just been posted. The signal is what makes the service feel
// immediate, the interval is what makes it correct: a task posted by another
// process, or one handed back by the recovery pass, has nobody to signal.
func (w *Worker) Run(ctx context.Context) error {
	// A briefly unreachable database is not worth stopping for, and saying so
	// every time would bury the incident under its own symptom: four workers at
	// a one second interval write four lines a second. First failure, then one
	// a minute.
	var failures int
	var lastReport time.Time

	for ctx.Err() == nil {
		claimed, err := w.claimAndProcess(ctx)

		switch {
		case err != nil:
			failures++
			if failures == 1 || time.Since(lastReport) >= claimFailureReportInterval {
				w.logger.Error("claim task", slog.Any("error", err), slog.Int("failed_polls", failures))
				lastReport = time.Now()
			}
		case failures > 0:
			w.logger.Info("claim recovered", slog.Int("failed_polls", failures))
			failures = 0
		}

		if claimed {
			continue
		}

		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(w.settings.PollInterval):
		}
	}

	return nil
}

func (w *Worker) claimAndProcess(ctx context.Context) (bool, error) {
	claim, ok, err := w.repo.ClaimTask(ctx)
	if err != nil || !ok {
		return false, err
	}

	// The deadline starts here rather than at the claim, so it measures the
	// work and not the wait. The database has been timing the task since the
	// claim committed, which is why its staleness threshold carries a margin
	// over this deadline.
	taskCtx, cancel := context.WithTimeout(ctx, w.settings.TaskTimeout)
	defer cancel()

	w.process(taskCtx, claim)

	return true, nil
}

func (w *Worker) process(ctx context.Context, claim domain.UpdateTask) {
	log := w.logger.With(
		slog.String("update_id", claim.ID.String()),
		slog.String("pair", string(claim.Pair)),
		slog.Int("attempt", claim.Attempts),
	)

	rate, fetchErr := w.rates.FetchRate(ctx, claim.Pair)
	// Stamped from the answer, not from the database: when this service
	// received a rate is a fact about this service, and now() would be the
	// start of a transaction that has not opened yet.
	fetchedAt := time.Now().UTC()

	// Asked of the context rather than of the error: an attempt that hit the
	// provider's own timeout wraps context.DeadlineExceeded just as a task
	// whose deadline ran out does, so the two cannot be told apart by what came
	// back.
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			// We are the ones leaving, so the claim goes back and the next
			// process finds the task queued rather than waiting out the
			// staleness threshold.
			w.release(ctx, log, claim)

			return
		}

		// Left in progress on purpose: released here it would be claimed again
		// at once, and a pair that reliably times out would cycle forever,
		// always too fresh for the recovery pass. Leaving it makes it stale,
		// which is what the attempt limit is counted against.
		log.Warn("task deadline exceeded, left in progress",
			slog.Any("error", err),
			slog.Any("provider_error", fetchErr),
		)

		return
	}

	if fetchErr != nil {
		w.fail(ctx, log, claim, fetchErr)

		return
	}

	if err := w.repo.CompleteTask(ctx, claim, rate.Value, rate.Date, fetchedAt); err != nil {
		w.finalisationFailed(ctx, log, claim, "complete task", err)

		return
	}

	log.Info("task done",
		slog.String("rate", rate.Value.String()),
		slog.String("rate_date", rate.Date.Format(time.DateOnly)),
	)
}

func (w *Worker) fail(ctx context.Context, log *slog.Logger, claim domain.UpdateTask, fetchErr error) {
	reason := w.failureReason(fetchErr)

	// The raw error carries the upstream URL, its status and its own words.
	// Only this line ever sees them.
	log.Error("fetch rate", slog.Any("error", fetchErr), slog.String("reason", reason))

	if err := w.repo.FailTask(ctx, claim, reason); err != nil {
		w.finalisationFailed(ctx, log, claim, "fail task", err)
	}
}

// A lost claim is the end of it: what this worker was about to record is void.
// Anything else leaves the task in progress, and a cancelled context means the
// service is shutting down, so the claim goes back -- safe even if the
// finalisation did commit and only its answer was lost, since ReleaseTask
// matches on status and on the claim token.
func (w *Worker) finalisationFailed(ctx context.Context, log *slog.Logger, claim domain.UpdateTask, op string, err error) {
	if errors.Is(err, domain.ErrStaleClaim) {
		log.Warn("claim lost, result discarded", slog.String("op", op), slog.Any("error", err))

		return
	}

	log.Error(op, slog.Any("error", err))

	if errors.Is(ctx.Err(), context.Canceled) {
		w.release(ctx, log, claim)
	}
}

// A failure here is not worth holding the shutdown for: the task stays in
// progress, which is the state the recovery pass exists for.
func (w *Worker) release(ctx context.Context, log *slog.Logger, claim domain.UpdateTask) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := w.repo.ReleaseTask(releaseCtx, claim); err != nil {
		log.Warn("task left in progress", slog.Any("error", err))

		return
	}

	log.Info("task released back to the queue")
}

func (w *Worker) failureReason(err error) string {
	if errors.Is(err, provider.ErrTransient) {
		return fmt.Sprintf(reasonUnavailable, w.settings.ProviderAttempts)
	}

	return reasonUnusableResponse
}
