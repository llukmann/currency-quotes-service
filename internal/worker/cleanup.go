package worker

import (
	"context"
	"log/slog"
	"time"
)

// cleanupRepository is the one statement this pass needs. Separate from the
// worker's and the recovery pass's for the same reason as theirs: it shares no
// method with either, since it touches neither the queue nor the tasks in it.
type cleanupRepository interface {
	DeleteExpiredKeys(ctx context.Context, olderThan time.Duration) (int, error)
}

// CleanupSettings are the numbers the pass runs by, from the configuration
// through main.
type CleanupSettings struct {
	// Interval is how often the pass runs.
	Interval time.Duration
	// TTL is how old a binding has to be to be swept. Because the sweep is the
	// only thing that ends one, this is the lifetime itself rather than a
	// tidying threshold behind it, and a binding lives between TTL and TTL plus
	// Interval. The configuration refuses to start unless the latter is the
	// smaller of the two.
	TTL time.Duration
}

// Cleanup removes idempotency bindings that have outlived their purpose.
//
// A binding exists to answer a client that repeats a request without knowing
// whether the first one arrived, which happens within seconds. Past that it
// only stands between a client and a fresh rate: a post carrying a live key is
// answered with the earlier task rather than queueing an update. So the sweep
// is not housekeeping for the sake of disk -- the table is tiny -- it is what
// keeps the endpoint doing what it says.
type Cleanup struct {
	repo     cleanupRepository
	settings CleanupSettings
	logger   *slog.Logger
}

// NewCleanup returns a sweep over repo.
func NewCleanup(repo cleanupRepository, settings CleanupSettings, logger *slog.Logger) *Cleanup {
	return &Cleanup{repo: repo, settings: settings, logger: logger}
}

// Run sweeps on every tick until ctx is done, which is a shutdown and so
// returns nil.
//
// The first sweep waits out an interval rather than running at startup, as the
// recovery pass does: nothing left by the previous process is expired any
// sooner than the lifetime says, and the tick will have come round by then.
func (c *Cleanup) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.settings.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.pass(ctx)
		}
	}
}

// pass sweeps once, reporting at debug: a count of expired bindings says
// nothing about the health of the service, and the sweep failing outright is
// not an incident either -- the next tick repeats it, and until then bindings
// merely outlive their term.
func (c *Cleanup) pass(ctx context.Context) {
	deleted, err := c.repo.DeleteExpiredKeys(ctx, c.settings.TTL)
	if err != nil {
		// A pass cut short by the shutdown has nothing to report: the rows it
		// would have removed are expired by age, and the next process to start
		// finds them exactly as they are.
		if ctx.Err() != nil {
			return
		}

		c.logger.Error("delete expired idempotency keys", slog.Any("error", err))

		return
	}

	if deleted > 0 {
		c.logger.Debug("expired idempotency keys deleted", slog.Int("count", deleted))
	}
}
