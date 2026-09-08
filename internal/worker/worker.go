// Package worker drains the queue of update tasks.
//
// This is where the two halves of the service meet: a worker claims a task,
// asks the provider for the rate and finalises the task in the database. It is
// also where raw upstream errors stop. The provider reports what happened and
// whether another attempt could help; deciding what a client is told about it,
// and what only the log gets to see, belongs here.
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
	// reasonUnavailable is for an upstream that never answered -- every
	// attempt met a network failure, a timeout, a 429 or a 5xx.
	reasonUnavailable = "provider unavailable after %d attempts"
	// reasonUnusableResponse is for an upstream that did answer, with
	// something this service cannot use: a rejected request, a body that does
	// not parse, a rate that is not a rate. Asking again would produce the
	// same answer, so it never was.
	reasonUnusableResponse = "provider returned an unusable response"
)

// repository is the part of the storage layer this package uses. It is
// declared here rather than beside the implementation because the consumer is
// what knows which statements it needs -- and because this is the surface a
// hand-written mock stands in for.
type repository interface {
	ClaimTask(ctx context.Context) (domain.UpdateTask, bool, error)
	CompleteTask(ctx context.Context, claim domain.UpdateTask, rate decimal.Decimal, rateDate, fetchedAt time.Time) error
	FailTask(ctx context.Context, claim domain.UpdateTask, reason string) error
	ReleaseTask(ctx context.Context, claim domain.UpdateTask) error
}

// releaseTimeout bounds handing a claim back on shutdown. That work runs on a
// context which has already been cancelled and therefore needs a deadline of
// its own; one UPDATE takes milliseconds, and if even this is not enough the
// task is not lost -- it stays in progress and the recovery pass has it.
const releaseTimeout = 5 * time.Second

// claimFailureReportInterval is how often a worker repeats itself while the
// queue cannot be reached at all. See Run.
const claimFailureReportInterval = time.Minute

// rateProvider is the provider as seen from here: one call, no retry policy of
// its own to expose. Both the bare client and the retrying decorator satisfy
// it, and a worker cannot tell which one it holds.
type rateProvider interface {
	FetchRate(ctx context.Context, pair domain.Pair) (provider.Rate, error)
}

// Settings are the numbers a worker runs by. They arrive from the
// configuration through main: this package neither reads the environment nor
// knows which variables exist.
type Settings struct {
	// TaskTimeout is the deadline of one task, covering the provider call with
	// all of its retries and the finalisation after it. The configuration
	// refuses to start unless it exceeds the provider's own budget.
	TaskTimeout time.Duration
	// PollInterval is how long to wait before asking an empty queue again.
	PollInterval time.Duration
	// ProviderAttempts is how many attempts that provider makes. It is here
	// only to phrase reasonUnavailable: the retrying itself is the provider's
	// business, and a worker does not count the tries.
	ProviderAttempts int
}

// Worker processes tasks one at a time. It holds nothing that changes, so the
// pool runs several goroutines over a single instance.
type Worker struct {
	repo     repository
	rates    rateProvider
	settings Settings
	logger   *slog.Logger
}

// New returns a worker reading from repo and rates.
func New(repo repository, rates rateProvider, settings Settings, logger *slog.Logger) *Worker {
	return &Worker{repo: repo, rates: rates, settings: settings, logger: logger}
}

// Run claims and processes tasks until ctx is done, which is a shutdown rather
// than a failure and so returns nil.
//
// A processed task is followed by another claim straight away: the queue is
// only asked to wait when it turns out to be empty. Polling at all is what the
// interval is for, and step 5 adds the signal that shortens it when a task has
// just been posted.
func (w *Worker) Run(ctx context.Context) error {
	// A database that is briefly unreachable is not worth stopping the service
	// for: the loop waits out the interval and asks again. Saying so every time
	// would bury the incident under its own symptom, since every worker polls --
	// four of them at a one second interval write four lines a second for as
	// long as the outage lasts. The first failure is reported, then one a
	// minute, and the recovery counts what was lost, which is the figure that
	// says how long it went on.
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
		case <-time.After(w.settings.PollInterval):
		}
	}

	return nil
}

// claimAndProcess takes the next task, if there is one, and reports whether it
// found work. Only the claim can fail here: what happens to a claimed task is
// recorded on the task itself, not returned to the loop.
func (w *Worker) claimAndProcess(ctx context.Context) (bool, error) {
	claim, ok, err := w.repo.ClaimTask(ctx)
	if err != nil || !ok {
		return false, err
	}

	// The deadline starts here rather than at the claim, so it measures the
	// work and not the wait. The database has been timing the task since the
	// claim committed, which is part of why its staleness threshold has to
	// carry a margin over this deadline.
	taskCtx, cancel := context.WithTimeout(ctx, w.settings.TaskTimeout)
	defer cancel()

	w.process(taskCtx, claim)

	return true, nil
}

// process drives one claimed task to a terminal status, or leaves it alone for
// the recovery pass when finalising it would be wrong.
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

	// Asked of the context rather than of the error, because the two cannot be
	// told apart by looking at what came back: an attempt that hit the
	// provider's own timeout wraps context.DeadlineExceeded just as a task
	// whose deadline ran out does.
	//
	// Either way nothing can be written with a context that is done, and which
	// of the two it was decides what happens to the task.
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			// The service is shutting down. The task did not fail -- we are the
			// ones leaving -- so the claim goes back and the next process finds
			// the task queued rather than waiting out the staleness threshold.
			w.release(ctx, log, claim)

			return
		}

		// The deadline ran out. Left in progress on purpose: released here it
		// would be claimed again at once, and a pair the upstream reliably
		// takes too long over would cycle between claim and release forever,
		// always too fresh for the recovery pass to give up on. Leaving it
		// makes it stale, and staleness is what the attempt limit is counted
		// against.
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

// fail closes a task the provider could not answer, storing the normalised
// reason and logging the raw one.
func (w *Worker) fail(ctx context.Context, log *slog.Logger, claim domain.UpdateTask, fetchErr error) {
	reason := w.failureReason(fetchErr)

	// The raw error carries the upstream URL, its status and its own words.
	// Only this line ever sees them.
	log.Error("fetch rate", slog.Any("error", fetchErr), slog.String("reason", reason))

	if err := w.repo.FailTask(ctx, claim, reason); err != nil {
		w.finalisationFailed(ctx, log, claim, "fail task", err)
	}
}

// finalisationFailed deals with a finalising statement that did not go
// through, whichever of the two it was.
//
// A lost claim is the end of it: the task belongs to someone else, and what
// this worker was about to record about it is void.
//
// Anything else leaves the task in progress, and then it matters why. A
// cancelled context means the write failed because the service is shutting
// down, which is the same situation as a cancellation during the provider call
// and deserves the same answer: the claim goes back. Releasing is safe even if
// the finalisation did commit and only its answer was lost -- ReleaseTask
// matches on status and on the claim token, so a task already closed is left
// alone and reported as a stale claim.
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

// release hands a claim back to the queue during shutdown.
//
// The write outlives the cancellation that caused it: derived from the task's
// context so that the connection settings and values on it are kept, but
// without its cancellation, which has already fired. A failure here is not
// worth holding the shutdown for -- the task simply stays in progress, which
// is the state the recovery pass exists for.
func (w *Worker) release(ctx context.Context, log *slog.Logger, claim domain.UpdateTask) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := w.repo.ReleaseTask(releaseCtx, claim); err != nil {
		log.Warn("task left in progress", slog.Any("error", err))

		return
	}

	log.Info("task released back to the queue")
}

// failureReason maps a provider error to the sentence a client is shown.
func (w *Worker) failureReason(err error) string {
	if errors.Is(err, provider.ErrTransient) {
		return fmt.Sprintf(reasonUnavailable, w.settings.ProviderAttempts)
	}

	return reasonUnusableResponse
}
