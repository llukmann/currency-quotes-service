package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// reasonAbandoned is stored on a task that has been claimed as often as it is
// allowed to be and is still not finished. Like the other reasons it names the
// outcome and not the mechanism: what a client can act on is that this update
// will not happen, so a new one has to be posted.
const reasonAbandoned = "abandoned after %d attempts"

// recoveryRepository is the one statement this pass needs. It is separate from
// the worker's repository because the two have nothing in common: a worker acts
// on the task it holds, this acts on tasks nobody holds any more.
type recoveryRepository interface {
	ReleaseStuckTasks(ctx context.Context, olderThan time.Duration, maxAttempts int, reason string) (released, failed int, err error)
}

// RecoverySettings are the numbers the pass runs by, from the configuration
// through main.
type RecoverySettings struct {
	// Interval is how often the pass runs. It bounds how long a task sits
	// unattended after its worker died, on top of StuckTimeout.
	Interval time.Duration
	// StuckTimeout is how long a task may stay in_progress before it counts as
	// abandoned. It is measured by the database clock against the claim time,
	// while a worker's deadline is measured by the worker's own process, so
	// this has to carry a margin over that deadline -- the configuration
	// refuses to start without one.
	StuckTimeout time.Duration
	// MaxAttempts is how many claims a task gets before the pass stops
	// releasing it and closes it as failed instead. Without a limit a task
	// that reliably kills whoever picks it up would be handed round forever.
	MaxAttempts int
}

// Recovery returns tasks that no live worker holds to the queue.
//
// Nothing releases them on their own: the row lock a claim takes is gone the
// moment the claiming transaction commits, so a process that dies afterwards
// leaves a row that says in_progress and a worker that no longer exists. The
// database cannot tell those apart from a worker still at work -- it has no
// such knowledge -- so age is what stands in for it.
type Recovery struct {
	repo     recoveryRepository
	settings RecoverySettings
	logger   *slog.Logger
}

// NewRecovery returns a recovery pass over repo.
func NewRecovery(repo recoveryRepository, settings RecoverySettings, logger *slog.Logger) *Recovery {
	return &Recovery{repo: repo, settings: settings, logger: logger}
}

// Run makes a pass on every tick until ctx is done, which is a shutdown and so
// returns nil.
//
// The first pass waits out an interval rather than running at startup. A task
// left behind by the previous process is not stale until StuckTimeout has
// passed anyway, and by then the tick will have come round.
func (r *Recovery) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.settings.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.pass(ctx)
		}
	}
}

// pass releases what it can and closes what it cannot, reporting the two
// separately: they say different things about the service. Releases mean
// workers are dying, or that the threshold is tight enough to be taking tasks
// from workers that are still alive; an abandonment means one task keeps
// killing whoever picks it up.
func (r *Recovery) pass(ctx context.Context) {
	reason := fmt.Sprintf(reasonAbandoned, r.settings.MaxAttempts)

	released, failed, err := r.repo.ReleaseStuckTasks(ctx, r.settings.StuckTimeout, r.settings.MaxAttempts, reason)
	if err != nil {
		// A pass cut short by the shutdown has nothing to report: the tasks it
		// would have released are stale by age, and the next process to start
		// finds them exactly as they are.
		if ctx.Err() != nil {
			return
		}

		r.logger.Error("release stuck tasks", slog.Any("error", err))

		return
	}

	if released > 0 {
		r.logger.Warn("stuck tasks released", slog.Int("count", released))
	}

	if failed > 0 {
		r.logger.Warn("stuck tasks abandoned", slog.Int("count", failed), slog.String("reason", reason))
	}
}
