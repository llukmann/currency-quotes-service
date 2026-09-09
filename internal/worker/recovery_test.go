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

// stuckCall is one call of ReleaseStuckTasks, kept whole so a test can check
// that the numbers the pass runs by are the ones the statement receives.
type stuckCall struct {
	olderThan   time.Duration
	maxAttempts int
	reason      string
	// ctxErr is the state of the context at the time of the call, read here
	// because a pass cut short by a shutdown answers differently.
	ctxErr error
}

// stubRecoveryRepo is the one statement this pass needs, with a record of how
// it was called. Behind a lock: the loop runs in a goroutine of its own.
type stubRecoveryRepo struct {
	mu       sync.Mutex
	calls    []stuckCall
	released int
	failed   int
	err      error
}

func (s *stubRecoveryRepo) ReleaseStuckTasks(
	ctx context.Context,
	olderThan time.Duration,
	maxAttempts int,
	reason string,
) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, stuckCall{
		olderThan:   olderThan,
		maxAttempts: maxAttempts,
		reason:      reason,
		ctxErr:      ctx.Err(),
	})

	return s.released, s.failed, s.err
}

func (s *stubRecoveryRepo) passes() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

func testRecoverySettings() RecoverySettings {
	return RecoverySettings{Interval: time.Hour, StuckTimeout: 90 * time.Second, MaxAttempts: 3}
}

// TestRecoveryPassUsesItsSettings checks that the numbers reach the statement.
// Two of them are durations of very different meaning -- how often the pass
// runs and how long a task may sit before it counts as abandoned -- and passing
// the first where the second belongs compiles, leaving the threshold silently
// set to the tick.
//
// The third is the attempt limit, which is used twice: once as the limit and
// once inside the sentence a client is shown. They have to be the same number,
// or the task says it was abandoned after a count nobody applied.
func TestRecoveryPassUsesItsSettings(t *testing.T) {
	repo := &stubRecoveryRepo{}
	settings := testRecoverySettings()

	NewRecovery(repo, settings, discardLogger()).pass(t.Context())

	require.Len(t, repo.calls, 1)

	got := repo.calls[0]
	require.Equal(t, settings.StuckTimeout, got.olderThan)
	require.Equal(t, settings.MaxAttempts, got.maxAttempts)
	require.Equal(t, "abandoned after 3 attempts", got.reason)
}

// TestRecoveryPassReportsWhatItDid covers the two counts and why they are kept
// apart. A steady trickle of releases says workers are dying, or that the
// threshold is tight enough to be taking tasks from workers still at work; an
// abandonment says one task keeps killing whoever picks it up. A single total
// would hide both.
//
// A pass that found nothing says nothing, which is what most passes do.
func TestRecoveryPassReportsWhatItDid(t *testing.T) {
	tests := []struct {
		name     string
		released int
		failed   int

		wantMsgs []string
	}{
		{
			name: "a pass with nothing to do is silent",
		},
		{
			name:     "released tasks are reported",
			released: 2,
			wantMsgs: []string{"stuck tasks released"},
		},
		{
			name:     "abandoned tasks are reported",
			failed:   1,
			wantMsgs: []string{"stuck tasks abandoned"},
		},
		{
			name:     "both are reported separately",
			released: 2,
			failed:   1,
			wantMsgs: []string{"stuck tasks released", "stuck tasks abandoned"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, records := captureLogger()

			repo := &stubRecoveryRepo{released: tt.released, failed: tt.failed}

			NewRecovery(repo, testRecoverySettings(), logger).pass(t.Context())

			entries := records()
			require.Len(t, entries, len(tt.wantMsgs))

			for _, msg := range tt.wantMsgs {
				entry := requireEntry(t, entries, msg)

				// Warn rather than error: neither is a failure of this pass,
				// and both are worth somebody's attention.
				require.Equal(t, slog.LevelWarn, entry.level())
			}

			if tt.released > 0 {
				require.Equal(t, tt.released, requireEntry(t, entries, "stuck tasks released").Count)
			}

			if tt.failed > 0 {
				abandoned := requireEntry(t, entries, "stuck tasks abandoned")
				require.Equal(t, tt.failed, abandoned.Count)
				// The sentence the abandoned tasks now carry, so that the log
				// and the rows agree about what happened.
				require.Equal(t, "abandoned after 3 attempts", abandoned.Reason)
			}
		})
	}
}

// TestRecoveryPassFailure covers the difference between a pass that failed and
// a pass that was interrupted. The second happens on every shutdown, and
// reported as a failure it would put an error in the log of every clean stop --
// the tasks it would have released are stale by age, and the next process to
// start finds them exactly as they are.
func TestRecoveryPassFailure(t *testing.T) {
	tests := []struct {
		name      string
		cancelled bool

		wantLogged bool
	}{
		{
			name:       "a pass that could not reach the database is reported",
			wantLogged: true,
		},
		{
			name:      "a pass cut short by the shutdown is not",
			cancelled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, records := captureLogger()

			repo := &stubRecoveryRepo{err: errors.New("connection refused")}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.cancelled {
				cancel()
			}

			NewRecovery(repo, testRecoverySettings(), logger).pass(ctx)

			if !tt.wantLogged {
				require.Empty(t, records(), "a shutdown was reported as a failure")

				return
			}

			entry := requireEntry(t, records(), "release stuck tasks")
			require.Equal(t, slog.LevelError, entry.level())
			require.Contains(t, entry.Error, "connection refused")
		})
	}
}

// TestRecoveryRunWaitsOutTheFirstInterval checks that the pass does not run at
// startup. Nothing left behind by the previous process is stale before
// StuckTimeout has passed anyway, and by then the tick will have come round.
func TestRecoveryRunWaitsOutTheFirstInterval(t *testing.T) {
	repo := &stubRecoveryRepo{}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	// Longer than this test is prepared to wait, so any pass at all is one
	// that did not wait for a tick.
	go func() { done <- NewRecovery(repo, testRecoverySettings(), discardLogger()).Run(ctx) }()

	require.Never(t, func() bool { return repo.passes() > 0 }, 100*time.Millisecond, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

// TestRecoveryRunPassesOnEveryTick covers the loop itself, and that a shutdown
// is a stop rather than a failure: the group main waits on takes this return
// value, and an error here would bring down everything else on the way out.
func TestRecoveryRunPassesOnEveryTick(t *testing.T) {
	repo := &stubRecoveryRepo{}
	settings := RecoverySettings{Interval: time.Millisecond, StuckTimeout: 90 * time.Second, MaxAttempts: 3}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- NewRecovery(repo, settings, discardLogger()).Run(ctx) }()

	require.Eventually(t, func() bool { return repo.passes() >= 3 }, 5*time.Second, time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a shutdown was reported as a failure")
	case <-time.After(5 * time.Second):
		t.Fatal("the recovery pass did not stop with its context")
	}
}
