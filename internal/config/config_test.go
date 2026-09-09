package config

import (
	"cmp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigCheckTimeouts pins the four invariants the service refuses to
// start without. They are worth a test of their own now that Load no longer
// runs them: the check is one call in main away from being forgotten, and it is
// cheap to state here what it must reject. The numbers are the configured
// defaults, so a change to them shows up as a failure with a name on it.
func TestConfigCheckTimeouts(t *testing.T) {
	tests := []struct {
		name           string
		writeTimeout   time.Duration
		providerBudget time.Duration
		taskTimeout    time.Duration
		stuckTimeout   time.Duration
		// The sweep of idempotency keys and the lifetime it enforces. Left at
		// zero by every case that is about one of the other three rules, which
		// the fixture then fills with a valid pair: a case states the rule it
		// is about and stays silent on the rest.
		cleanupInterval time.Duration
		idempotencyTTL  time.Duration
		// wantErr is the variable the failure has to name; empty means the
		// combination must be accepted.
		wantErr string
	}{
		{
			name:           "the configured defaults hold",
			writeTimeout:   10 * time.Second,
			providerBudget: 9900 * time.Millisecond,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
		},
		{
			// Equal leaves a handler deadline of zero, which time.Context
			// treats as already expired rather than as unlimited.
			name:           "a write timeout equal to the margin leaves no deadline",
			writeTimeout:   handlerTimeoutMargin,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "HTTP_WRITE_TIMEOUT",
		},
		{
			name:           "the smallest write timeout above the margin is accepted",
			writeTimeout:   handlerTimeoutMargin + time.Millisecond,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
		},
		{
			name:           "a deadline shorter than the budget is refused",
			writeTimeout:   10 * time.Second,
			providerBudget: 10 * time.Second,
			taskTimeout:    9 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "WORKER_TASK_TIMEOUT",
		},
		{
			// Equal is not enough: the deadline covers the finalising
			// transaction as well as the call.
			name:           "a deadline equal to the budget leaves nothing for the commit",
			writeTimeout:   10 * time.Second,
			providerBudget: 10 * time.Second,
			taskTimeout:    10 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "WORKER_TASK_TIMEOUT",
		},
		{
			name:           "a staleness threshold without a margin is refused",
			writeTimeout:   10 * time.Second,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   29 * time.Second,
			wantErr:        "WORKER_STUCK_TIMEOUT",
		},
		{
			name:           "twice the deadline is the smallest margin accepted",
			writeTimeout:   10 * time.Second,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   30 * time.Second,
		},
		{
			// Equal is already wrong: the sweep is the only thing that ends a
			// binding, so an interval that matches the lifetime doubles it.
			name:            "a sweep as rare as the lifetime it enforces is refused",
			writeTimeout:    10 * time.Second,
			providerBudget:  5 * time.Second,
			taskTimeout:     15 * time.Second,
			stuckTimeout:    time.Minute,
			cleanupInterval: time.Hour,
			idempotencyTTL:  time.Hour,
			wantErr:         "IDEMPOTENCY_CLEANUP_INTERVAL",
		},
		{
			name:            "the configured sweep and lifetime hold",
			writeTimeout:    10 * time.Second,
			providerBudget:  5 * time.Second,
			taskTimeout:     15 * time.Second,
			stuckTimeout:    time.Minute,
			cleanupInterval: defaultIdempotencyCleanupInterval,
			idempotencyTTL:  defaultIdempotencyTTL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				HTTPWriteTimeout:   tt.writeTimeout,
				WorkerTaskTimeout:  tt.taskTimeout,
				WorkerStuckTimeout: tt.stuckTimeout,
				// A valid pair stands in wherever a case did not care, so that
				// the rule under test is the only one that can fail it.
				IdempotencyCleanupInterval: cmp.Or(tt.cleanupInterval, time.Minute),
				IdempotencyTTL:             cmp.Or(tt.idempotencyTTL, time.Hour),
			}

			err := cfg.CheckTimeouts(tt.providerBudget)

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestConfigHandlerTimeout states the relation the request deadline is derived
// by, since nothing downstream of it can tell a deadline that is too generous
// from one that is right: the difference only shows as a connection dropped
// mid-answer under load.
func TestConfigHandlerTimeout(t *testing.T) {
	cfg := Config{HTTPWriteTimeout: 10 * time.Second}

	require.Equal(t, 10*time.Second-handlerTimeoutMargin, cfg.HandlerTimeout())
	require.Less(t, cfg.HandlerTimeout(), cfg.HTTPWriteTimeout)
}
