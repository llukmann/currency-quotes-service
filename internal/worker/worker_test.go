package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
	"github.com/llukmann/currency-quotes-service/internal/provider"
)

// completion is one call of CompleteTask, kept whole so a test can check that
// the rate stored is the rate fetched and that the pair comes from the claim.
type completion struct {
	claim     domain.UpdateTask
	rate      decimal.Decimal
	rateDate  time.Time
	fetchedAt time.Time
}

// failure is one call of FailTask.
type failure struct {
	claim  domain.UpdateTask
	reason string
}

// stubRepo is the queue as the worker sees it: one task on offer, scripted
// answers to the three finalising statements, and a record of what was called.
// Written by hand -- the interface is small, and the hooks below are what the
// tests are actually about.
type stubRepo struct {
	task     domain.UpdateTask
	hasTask  bool
	claimErr error

	completeErr error
	failErr     error
	releaseErr  error

	// beforeComplete runs inside CompleteTask, before it answers, so a test can
	// have the context cancelled while the finalisation is in flight.
	beforeComplete func()

	claims    int
	completed []completion
	failed    []failure
	released  []domain.UpdateTask
}

func (s *stubRepo) ClaimTask(context.Context) (domain.UpdateTask, bool, error) {
	s.claims++

	if s.claimErr != nil {
		return domain.UpdateTask{}, false, s.claimErr
	}
	if !s.hasTask {
		return domain.UpdateTask{}, false, nil
	}

	s.hasTask = false

	return s.task, true, nil
}

func (s *stubRepo) CompleteTask(
	_ context.Context,
	claim domain.UpdateTask,
	rate decimal.Decimal,
	rateDate, fetchedAt time.Time,
) error {
	if s.beforeComplete != nil {
		s.beforeComplete()
	}

	s.completed = append(s.completed, completion{claim: claim, rate: rate, rateDate: rateDate, fetchedAt: fetchedAt})

	return s.completeErr
}

func (s *stubRepo) FailTask(_ context.Context, claim domain.UpdateTask, reason string) error {
	s.failed = append(s.failed, failure{claim: claim, reason: reason})

	return s.failErr
}

func (s *stubRepo) ReleaseTask(_ context.Context, claim domain.UpdateTask) error {
	s.released = append(s.released, claim)

	return s.releaseErr
}

// stubProvider answers with one scripted result and can run a hook first. The
// hook is given the task's own context, which is what lets a test cancel it
// from inside the call or wait for its deadline to fire rather than guess when
// that happens.
type stubProvider struct {
	rate   provider.Rate
	err    error
	before func(ctx context.Context)

	pairs []domain.Pair
}

func (s *stubProvider) FetchRate(ctx context.Context, pair domain.Pair) (provider.Rate, error) {
	s.pairs = append(s.pairs, pair)

	if s.before != nil {
		s.before(ctx)
	}

	return s.rate, s.err
}

func testSettings(taskTimeout time.Duration) Settings {
	return Settings{TaskTimeout: taskTimeout, PollInterval: time.Millisecond, ProviderAttempts: 3}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestWorkerClaimAndProcess covers the branches a task can leave process by.
// Two of them are the point of the whole design: a task is closed only when the
// answer is final, and a claim is handed back only when this service is the one
// walking away.
func TestWorkerClaimAndProcess(t *testing.T) {
	answer := provider.Rate{
		Value: decimal.RequireFromString("19.6552"),
		Date:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}

	transient := fmt.Errorf("upstream status 503: %w", provider.ErrTransient)
	unusable := errors.New("upstream status 404")

	tests := []struct {
		name        string
		taskTimeout time.Duration
		fetchErr    error
		completeErr error
		// cancelDuringFetch has the context cancelled while the provider is
		// being called, which is what a shutdown looks like from here.
		cancelDuringFetch bool
		// cancelDuringComplete cancels it one step later, while the finalising
		// statement is in flight.
		cancelDuringComplete bool
		// waitForDeadline holds the provider call until the task's deadline
		// fires, so the expiry is a fact rather than a race with a timer.
		waitForDeadline bool

		wantCompleted int
		wantReason    string
		wantReleased  int
	}{
		{
			name:          "a rate that arrives is stored and closes the task",
			taskTimeout:   5 * time.Second,
			wantCompleted: 1,
		},
		{
			name:        "an upstream that never answered is reported with the attempt count",
			taskTimeout: 5 * time.Second,
			fetchErr:    transient,
			wantReason:  "provider unavailable after 3 attempts",
		},
		{
			name:        "an unusable answer is reported without quoting the upstream",
			taskTimeout: 5 * time.Second,
			fetchErr:    unusable,
			wantReason:  "provider returned an unusable response",
		},
		{
			// The task belongs to someone else by now: storing the rate would
			// attach it to an update that is queued or running again.
			name:          "a lost claim discards the rate",
			taskTimeout:   5 * time.Second,
			completeErr:   fmt.Errorf("complete task: %w", domain.ErrStaleClaim),
			wantCompleted: 1,
		},
		{
			name:              "a cancelled fetch hands the claim back",
			taskTimeout:       5 * time.Second,
			cancelDuringFetch: true,
			fetchErr:          context.Canceled,
			wantReleased:      1,
		},
		{
			// The window the first version left open: cancelled one step later,
			// with the finalising statement already in flight. Same cause, so
			// the same answer.
			name:                 "a cancellation during finalisation hands the claim back",
			taskTimeout:          5 * time.Second,
			cancelDuringComplete: true,
			completeErr:          fmt.Errorf("complete task: %w", context.Canceled),
			wantCompleted:        1,
			wantReleased:         1,
		},
		{
			// Deliberately not released: requeued, it would be claimed again at
			// once and never grow stale enough for the recovery pass to give up
			// on it.
			name:            "an expired deadline leaves the task in progress",
			taskTimeout:     5 * time.Millisecond,
			waitForDeadline: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			claim := domain.UpdateTask{
				ID:       uuid.New(),
				Pair:     domain.Pair("EUR/MXN"),
				Status:   domain.StatusInProgress,
				Attempts: 1,
			}

			repo := &stubRepo{task: claim, hasTask: true, completeErr: tt.completeErr}
			rates := &stubProvider{rate: answer, err: tt.fetchErr}

			if tt.cancelDuringFetch {
				rates.before = func(context.Context) { cancel() }
			}
			if tt.waitForDeadline {
				rates.before = func(ctx context.Context) { <-ctx.Done() }
			}
			if tt.cancelDuringComplete {
				repo.beforeComplete = cancel
			}

			before := time.Now().UTC()

			claimed, err := New(repo, rates, nil, testSettings(tt.taskTimeout), discardLogger()).claimAndProcess(ctx)

			require.NoError(t, err)
			require.True(t, claimed)

			require.Len(t, repo.completed, tt.wantCompleted)
			require.Len(t, repo.released, tt.wantReleased)

			if tt.wantReason == "" {
				require.Empty(t, repo.failed)
			} else {
				require.Len(t, repo.failed, 1)
				require.Equal(t, tt.wantReason, repo.failed[0].reason)
				require.Equal(t, claim, repo.failed[0].claim)
			}

			for _, got := range repo.released {
				require.Equal(t, claim, got)
			}

			if tt.wantCompleted > 0 {
				stored := repo.completed[0]

				require.Equal(t, claim, stored.claim)
				require.True(t, answer.Value.Equal(stored.rate), "stored %s, fetched %s", stored.rate, answer.Value)
				require.Equal(t, answer.Date, stored.rateDate)
				// Stamped when the answer arrived, by this service rather than
				// by the database.
				require.False(t, stored.fetchedAt.Before(before))
				require.False(t, stored.fetchedAt.After(time.Now().UTC()))
			}
		})
	}
}

// TestWorkerClaimAndProcessEmptyQueue checks that an idle queue is a state and
// not an error: it is what a worker sees most of the time.
func TestWorkerClaimAndProcessEmptyQueue(t *testing.T) {
	repo := &stubRepo{}
	rates := &stubProvider{}

	claimed, err := New(repo, rates, nil, testSettings(time.Second), discardLogger()).claimAndProcess(t.Context())

	require.NoError(t, err)
	require.False(t, claimed)
	require.Empty(t, rates.pairs)
	require.Empty(t, repo.completed)
	require.Empty(t, repo.failed)
}

// TestWorkerClaimAndProcessClaimFails checks that a queue that cannot be
// reached is reported upwards and touches nothing else. The provider is never
// called: there is no task to fetch a rate for.
func TestWorkerClaimAndProcessClaimFails(t *testing.T) {
	repo := &stubRepo{claimErr: errors.New("connection refused")}
	rates := &stubProvider{}

	claimed, err := New(repo, rates, nil, testSettings(time.Second), discardLogger()).claimAndProcess(t.Context())

	require.Error(t, err)
	require.False(t, claimed)
	require.Empty(t, rates.pairs)
}

// TestWorkerRunStopsWithTheContext checks that a shutdown ends the loop and is
// not reported as a failure: the group waits on this return value, and an error
// here would cancel everything else on the way out.
func TestWorkerRunStopsWithTheContext(t *testing.T) {
	repo := &stubRepo{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, New(repo, &stubProvider{}, nil, testSettings(time.Second), discardLogger()).Run(ctx))
}
