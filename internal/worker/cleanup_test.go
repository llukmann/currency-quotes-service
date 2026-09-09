package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sweepCall is one call of DeleteExpiredKeys.
type sweepCall struct {
	olderThan time.Duration
	ctxErr    error
}

// stubCleanupRepo is the one statement this sweep needs, with a record of how
// it was called. Behind a lock: the loop runs in a goroutine of its own.
type stubCleanupRepo struct {
	mu      sync.Mutex
	calls   []sweepCall
	deleted int
	err     error
}

func (s *stubCleanupRepo) DeleteExpiredKeys(ctx context.Context, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, sweepCall{olderThan: olderThan, ctxErr: ctx.Err()})

	return s.deleted, s.err
}

func (s *stubCleanupRepo) sweeps() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

func testCleanupSettings() CleanupSettings {
	return CleanupSettings{Interval: time.Hour, TTL: 24 * time.Hour}
}

// TestCleanupPassSweepsByTheLifetime checks which of the two durations reaches
// the statement. They are the lifetime of a binding and the period of the
// sweep, and passing one where the other belongs compiles: bindings would then
// be removed by the tick, which for the configured defaults means six times too
// early.
func TestCleanupPassSweepsByTheLifetime(t *testing.T) {
	repo := &stubCleanupRepo{}
	settings := testCleanupSettings()

	NewCleanup(repo, settings, discardLogger()).pass(t.Context())

	require.Len(t, repo.calls, 1)
	require.Equal(t, settings.TTL, repo.calls[0].olderThan)
}

// TestCleanupPassReportsAtDebug pins the level, which is the decision here. A
// count of expired bindings says nothing about the health of the service --
// they expire because time passed -- so it belongs below the level anybody
// watches, and a sweep that found nothing says nothing at all.
func TestCleanupPassReportsAtDebug(t *testing.T) {
	t.Run("a sweep that removed nothing is silent", func(t *testing.T) {
		logger, records := captureLogger()

		NewCleanup(&stubCleanupRepo{}, testCleanupSettings(), logger).pass(t.Context())

		require.Empty(t, records())
	})

	t.Run("a sweep that removed bindings reports how many", func(t *testing.T) {
		logger, records := captureLogger()

		NewCleanup(&stubCleanupRepo{deleted: 7}, testCleanupSettings(), logger).pass(t.Context())

		entry := requireEntry(t, records(), "expired idempotency keys deleted")
		require.Equal(t, slog.LevelDebug, entry.level())
		require.Equal(t, 7, entry.Count)
	})
}

// TestCleanupPassFailure covers the same distinction the recovery pass draws. A
// sweep failing is not an incident -- the next tick repeats it, and until then
// bindings merely outlive their term -- but it is worth a line; a sweep cut
// short by the shutdown is not even that, and reported it would put an error in
// the log of every clean stop.
func TestCleanupPassFailure(t *testing.T) {
	tests := []struct {
		name      string
		cancelled bool

		wantLogged bool
	}{
		{
			name:       "a sweep that could not reach the database is reported",
			wantLogged: true,
		},
		{
			name:      "a sweep cut short by the shutdown is not",
			cancelled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, records := captureLogger()

			repo := &stubCleanupRepo{err: errors.New("connection refused")}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.cancelled {
				cancel()
			}

			NewCleanup(repo, testCleanupSettings(), logger).pass(ctx)

			if !tt.wantLogged {
				require.Empty(t, records(), "a shutdown was reported as a failure")

				return
			}

			entry := requireEntry(t, records(), "delete expired idempotency keys")
			require.Equal(t, slog.LevelError, entry.level())
			require.Contains(t, entry.Error, "connection refused")
		})
	}
}

// TestCleanupRunWaitsOutTheFirstInterval checks that the sweep does not run at
// startup. Nothing left by the previous process is expired any sooner than the
// lifetime says, and the tick will have come round by then.
func TestCleanupRunWaitsOutTheFirstInterval(t *testing.T) {
	repo := &stubCleanupRepo{}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- NewCleanup(repo, testCleanupSettings(), discardLogger()).Run(ctx) }()

	require.Never(t, func() bool { return repo.sweeps() > 0 }, 100*time.Millisecond, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

// TestCleanupRunSweepsOnEveryTick covers the loop itself, and that a shutdown
// is a stop rather than a failure: the group main waits on takes this return
// value, and an error here would bring down everything else on the way out.
func TestCleanupRunSweepsOnEveryTick(t *testing.T) {
	repo := &stubCleanupRepo{}
	settings := CleanupSettings{Interval: time.Millisecond, TTL: 24 * time.Hour}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- NewCleanup(repo, settings, discardLogger()).Run(ctx) }()

	require.Eventually(t, func() bool { return repo.sweeps() >= 3 }, 5*time.Second, time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a shutdown was reported as a failure")
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not stop with its context")
	}
}
