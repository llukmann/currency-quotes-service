package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	released  []release
}

// release is one call of ReleaseTask together with the state of the context it
// arrived on, read while the call was in progress.
//
// Read rather than kept: the context is cancelled by a deferred call the moment
// release returns, so a test asking afterwards would find every one of them
// done and learn nothing.
type release struct {
	claim       domain.UpdateTask
	ctxErr      error
	deadline    time.Time
	hasDeadline bool
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

func (s *stubRepo) ReleaseTask(ctx context.Context, claim domain.UpdateTask) error {
	deadline, hasDeadline := ctx.Deadline()

	s.released = append(s.released, release{
		claim:       claim,
		ctxErr:      ctx.Err(),
		deadline:    deadline,
		hasDeadline: hasDeadline,
	})

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
				require.Equal(t, claim, got.claim)
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

// logEntry is one structured record, decoded from what the logger wrote. Only
// the fields the tests below ask about.
type logEntry struct {
	Level       string `json:"level"`
	Msg         string `json:"msg"`
	Error       string `json:"error"`
	Reason      string `json:"reason"`
	Count       int    `json:"count"`
	FailedPolls int    `json:"failed_polls"`
	UpdateID    string `json:"update_id"`
}

func (e logEntry) level() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(e.Level)); err != nil {
		return slog.LevelInfo
	}

	return level
}

// captureLogger returns a logger writing structured records and a function
// reading back what it has written. It is safe to read while the loops below
// are still running, which is why the buffer is behind a lock.
func captureLogger() (*slog.Logger, func() []logEntry) {
	buf := &lockedBuffer{}

	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, func() []logEntry {
		var entries []logEntry

		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}

			var entry logEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}

			entries = append(entries, entry)
		}

		return entries
	}
}

// lockedBuffer lets a test read the log of a loop that is still writing it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// entriesWith returns every record carrying this message.
func entriesWith(entries []logEntry, msg string) []logEntry {
	var found []logEntry

	for _, entry := range entries {
		if entry.Msg == msg {
			found = append(found, entry)
		}
	}

	return found
}

// requireEntry finds the one record with this message and fails the test when
// there is none.
func requireEntry(t *testing.T, entries []logEntry, msg string) logEntry {
	t.Helper()

	found := entriesWith(entries, msg)
	require.Lenf(t, found, 1, "expected exactly one %q record among %d written", msg, len(entries))

	return found[0]
}

// TestWorkerReleaseOutlivesTheCancellationThatCausedIt covers the one context
// in this package that must not be the caller's.
//
// A claim is handed back precisely because the task's context is done -- the
// service is shutting down -- so a statement issued on that context would fail
// before it was sent, in the single case the whole path exists for. The task
// would then stay in progress until the recovery pass noticed, which is the
// delay releasing it is there to avoid.
func TestWorkerReleaseOutlivesTheCancellationThatCausedIt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	claim := domain.UpdateTask{
		ID:       uuid.New(),
		Pair:     domain.Pair("EUR/MXN"),
		Status:   domain.StatusInProgress,
		Attempts: 1,
	}

	repo := &stubRepo{task: claim, hasTask: true}
	rates := &stubProvider{before: func(context.Context) { cancel() }, err: context.Canceled}

	claimed, err := New(repo, rates, nil, testSettings(5*time.Second), discardLogger()).claimAndProcess(ctx)

	require.NoError(t, err)
	require.True(t, claimed)
	require.Len(t, repo.released, 1)

	released := repo.released[0]

	require.NoError(t, released.ctxErr, "the release was issued on a context that was already done")

	// Bounded all the same: this runs during a shutdown, and a statement with
	// no deadline would hold it open for as long as the database stayed quiet.
	require.True(t, released.hasDeadline, "the release was issued without a deadline")
	require.False(t, released.deadline.After(time.Now().Add(releaseTimeout)))
}

// TestWorkerLogsTheRawProviderFailure covers the boundary this package is here
// to hold. The client is told a normalised sentence that names the outcome and
// not the mechanism, which is only acceptable because the upstream's own words,
// its URL and its status are written down somewhere -- and this is the one line
// that ever sees them.
//
// Remove it and nothing outside changes: the task fails with the same reason,
// the same status, the same answer to the client. Only the ability to find out
// why goes.
func TestWorkerLogsTheRawProviderFailure(t *testing.T) {
	const raw = "fetch rate EUR/MXN: upstream status 503: {\"message\":\"service unavailable\"}"

	logger, records := captureLogger()

	claim := domain.UpdateTask{
		ID:       uuid.New(),
		Pair:     domain.Pair("EUR/MXN"),
		Status:   domain.StatusInProgress,
		Attempts: 1,
	}

	repo := &stubRepo{task: claim, hasTask: true}
	rates := &stubProvider{err: fmt.Errorf("%s: %w", raw, provider.ErrTransient)}

	_, err := New(repo, rates, nil, testSettings(5*time.Second), logger).claimAndProcess(t.Context())
	require.NoError(t, err)

	entry := requireEntry(t, records(), "fetch rate")

	require.Equal(t, slog.LevelError, entry.level())
	require.Contains(t, entry.Error, raw)
	require.Equal(t, "provider unavailable after 3 attempts", entry.Reason)
	require.Equal(t, claim.ID.String(), entry.UpdateID)

	// What was stored is the normalised sentence alone: the two must not be the
	// same string, or the boundary is not being held.
	require.Len(t, repo.failed, 1)
	require.Equal(t, "provider unavailable after 3 attempts", repo.failed[0].reason)
	require.NotContains(t, repo.failed[0].reason, "503")
}

// TestWorkerFinalisationFailures covers what happens when the statement that
// was supposed to close a task does not go through. None of these can be
// reported to a client -- the request that queued the task was answered long
// ago -- so the log is the whole of the outcome.
func TestWorkerFinalisationFailures(t *testing.T) {
	unreachable := errors.New("connection refused")

	tests := []struct {
		name string
		// fetchErr decides which finalising statement is reached.
		fetchErr    error
		completeErr error
		failErr     error
		releaseErr  error

		wantMsg   string
		wantLevel slog.Level
	}{
		{
			// The task belongs to somebody else now, and what this worker was
			// about to record about it is void.
			name:        "a lost claim ends it",
			completeErr: fmt.Errorf("complete task: %w", domain.ErrStaleClaim),
			wantMsg:     "claim lost, result discarded",
			wantLevel:   slog.LevelWarn,
		},
		{
			name:        "a completion that did not go through is reported",
			completeErr: unreachable,
			wantMsg:     "complete task",
			wantLevel:   slog.LevelError,
		},
		{
			// The provider failed and so did the statement recording that.
			name:      "a failure that could not be stored is reported",
			fetchErr:  errors.New("upstream status 404"),
			failErr:   unreachable,
			wantMsg:   "fail task",
			wantLevel: slog.LevelError,
		},
		{
			name:      "a failure whose claim was lost ends it",
			fetchErr:  errors.New("upstream status 404"),
			failErr:   fmt.Errorf("fail task: %w", domain.ErrStaleClaim),
			wantMsg:   "claim lost, result discarded",
			wantLevel: slog.LevelWarn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, records := captureLogger()

			claim := domain.UpdateTask{
				ID:       uuid.New(),
				Pair:     domain.Pair("EUR/MXN"),
				Status:   domain.StatusInProgress,
				Attempts: 1,
			}

			repo := &stubRepo{
				task:        claim,
				hasTask:     true,
				completeErr: tt.completeErr,
				failErr:     tt.failErr,
				releaseErr:  tt.releaseErr,
			}

			rates := &stubProvider{
				rate: provider.Rate{
					Value: decimal.RequireFromString("19.6552"),
					Date:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
				},
				err: tt.fetchErr,
			}

			_, err := New(repo, rates, nil, testSettings(5*time.Second), logger).claimAndProcess(t.Context())
			require.NoError(t, err)

			entry := requireEntry(t, records(), tt.wantMsg)
			require.Equal(t, tt.wantLevel, entry.level())
			require.Equal(t, claim.ID.String(), entry.UpdateID)

			// Nothing was handed back: the context is live, so this is not a
			// shutdown and the task stays where the failed statement left it.
			require.Empty(t, repo.released)
		})
	}
}

// TestWorkerReleaseFailureIsNotFatal covers the last thing that can go wrong
// on the way out. The shutdown is not held up for it: the task simply stays in
// progress, which is the state the recovery pass exists for.
func TestWorkerReleaseFailureIsNotFatal(t *testing.T) {
	logger, records := captureLogger()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	claim := domain.UpdateTask{
		ID:       uuid.New(),
		Pair:     domain.Pair("EUR/MXN"),
		Status:   domain.StatusInProgress,
		Attempts: 1,
	}

	repo := &stubRepo{task: claim, hasTask: true, releaseErr: errors.New("connection refused")}
	rates := &stubProvider{before: func(context.Context) { cancel() }, err: context.Canceled}

	claimed, err := New(repo, rates, nil, testSettings(5*time.Second), logger).claimAndProcess(ctx)

	require.NoError(t, err)
	require.True(t, claimed)

	entry := requireEntry(t, records(), "task left in progress")
	require.Equal(t, slog.LevelWarn, entry.level())
}
