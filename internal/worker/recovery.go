package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Names the outcome and not the mechanism: what a client can act on is that
// this update will not happen, so a new one has to be posted.
const reasonAbandoned = "abandoned after %d attempts"

// The one statement this pass needs, separate from the worker's repository
// because the two have nothing in common: a worker acts on the task it holds,
// this acts on tasks nobody holds any more.
type recoveryRepository interface {
	ReleaseStuckTasks(ctx context.Context, olderThan time.Duration, maxAttempts int, reason string) (released, failed int, err error)
}

type RecoverySettings struct {
	// Bounds how long a task sits unattended after its worker died, on top of
	// StuckTimeout.
	Interval time.Duration
	// Measured by the database clock against the claim time, while a worker's
	// deadline is measured by the worker's own process, so this has to carry a
	// margin over that deadline -- the configuration refuses to start without
	// one.
	StuckTimeout time.Duration
	// Without a limit a task that reliably kills whoever picks it up would be
	// handed round forever.
	MaxAttempts int
}

// Nothing releases stuck tasks on their own: the row lock a claim takes is
// gone the moment the claiming transaction commits, so a process that dies
// afterwards leaves a row saying in_progress and a worker that no longer
// exists. The database cannot tell that from a worker still at work, so age
// stands in for it.
type Recovery struct {
	repo     recoveryRepository
	settings RecoverySettings
	logger   *slog.Logger
}

func NewRecovery(repo recoveryRepository, settings RecoverySettings, logger *slog.Logger) *Recovery {
	return &Recovery{repo: repo, settings: settings, logger: logger}
}

// The first pass waits out an interval rather than running at startup: a task
// left behind by the previous process is not stale until StuckTimeout has
// passed anyway.
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

// The two counts are reported separately because they mean different things:
// releases say workers are dying, or that the threshold is tight enough to be
// taking tasks from workers still alive, while an abandonment says one task
// keeps killing whoever picks it up.
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
