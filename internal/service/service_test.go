package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// creation is one call of CreateTask, kept whole so a test can check that the
// pair reached storage normalised and that the key crossed unchanged.
type creation struct {
	pair domain.Pair
	key  *uuid.UUID
}

// stubRepo is storage as this package sees it: scripted answers to the three
// statements and a record of what was asked of it. What the tests are actually
// about is that record -- whether the repository was reached at all, and with
// what.
type stubRepo struct {
	task      domain.UpdateTask
	createErr error

	details domain.TaskDetails
	getErr  error

	quote    domain.Quote
	quoteErr error

	// onCreate runs inside CreateTask, before it answers, so a test can look
	// at the world as it stands while the row is still being written.
	onCreate func()

	created []creation
	fetched []uuid.UUID
	looked  []domain.Pair
}

func (s *stubRepo) CreateTask(_ context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error) {
	s.created = append(s.created, creation{pair: pair, key: key})

	if s.onCreate != nil {
		s.onCreate()
	}

	if s.createErr != nil {
		return domain.UpdateTask{}, s.createErr
	}

	return s.task, nil
}

func (s *stubRepo) GetTask(_ context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	s.fetched = append(s.fetched, id)

	if s.getErr != nil {
		return domain.TaskDetails{}, s.getErr
	}

	return s.details, nil
}

func (s *stubRepo) GetLatestQuote(_ context.Context, pair domain.Pair) (domain.Quote, error) {
	s.looked = append(s.looked, pair)

	if s.quoteErr != nil {
		return domain.Quote{}, s.quoteErr
	}

	return s.quote, nil
}

// TestServiceCreateTask covers the two decisions this method owns: a pair is
// validated and normalised before anything reaches storage, and a worker is
// woken only for a task that is actually waiting to be claimed.
func TestServiceCreateTask(t *testing.T) {
	key := uuid.New()
	conflict := domain.ErrKeyConflict
	unreachable := errors.New("connection refused")

	tests := []struct {
		name       string
		pair       string
		key        *uuid.UUID
		repoStatus domain.Status
		createErr  error

		// wantPair is the pair storage should have been called with, empty
		// when it should not have been called at all.
		wantPair domain.Pair
		wantErr  error
		wantWake bool
	}{
		{
			name:       "a queued task wakes a worker",
			pair:       "EUR/MXN",
			repoStatus: domain.StatusPending,
			wantPair:   domain.Pair("EUR/MXN"),
			wantWake:   true,
		},
		{
			// The pair storage sees is the canonical one, which is what keeps
			// two spellings of one pair from becoming two rows and what an
			// idempotency key is later compared against.
			name:       "the pair is normalised before it reaches storage",
			pair:       "eur/mxn",
			repoStatus: domain.StatusPending,
			wantPair:   domain.Pair("EUR/MXN"),
			wantWake:   true,
		},
		{
			name:       "the idempotency key crosses unchanged",
			pair:       "USD/MXN",
			key:        &key,
			repoStatus: domain.StatusPending,
			wantPair:   domain.Pair("USD/MXN"),
			wantWake:   true,
		},
		{
			// Deduplicated onto work already running: a woken worker would
			// find nothing to claim.
			name:       "a task already in progress wakes nobody",
			pair:       "EUR/MXN",
			repoStatus: domain.StatusInProgress,
			wantPair:   domain.Pair("EUR/MXN"),
		},
		{
			// A key replayed long after its task finished.
			name:       "a finished task wakes nobody",
			pair:       "EUR/MXN",
			key:        &key,
			repoStatus: domain.StatusDone,
			wantPair:   domain.Pair("EUR/MXN"),
		},
		{
			name:       "a failed task wakes nobody",
			pair:       "EUR/MXN",
			key:        &key,
			repoStatus: domain.StatusFailed,
			wantPair:   domain.Pair("EUR/MXN"),
		},
		{
			// The rule the whitelist exists for: nothing in the schema would
			// have caught this, so a pair refused here is a pair that never
			// reaches a row.
			name:    "an unsupported pair never reaches storage",
			pair:    "EUR/RUB",
			wantErr: domain.ErrInvalidPair,
		},
		{
			name:    "a malformed pair never reaches storage",
			pair:    "EURMXN",
			wantErr: domain.ErrInvalidPair,
		},
		{
			name:      "a key already spent on another pair is reported as such",
			pair:      "EUR/MXN",
			key:       &key,
			createErr: conflict,
			wantPair:  domain.Pair("EUR/MXN"),
			wantErr:   domain.ErrKeyConflict,
		},
		{
			// Nothing was queued, so there is nothing to wake anybody for.
			name:      "a failure of storage is passed up and wakes nobody",
			pair:      "EUR/MXN",
			createErr: unreachable,
			wantPair:  domain.Pair("EUR/MXN"),
			wantErr:   unreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &stubRepo{
				task: domain.UpdateTask{
					ID:     uuid.New(),
					Pair:   domain.Pair("EUR/MXN"),
					Status: tt.repoStatus,
				},
				createErr: tt.createErr,
			}

			wake := make(chan struct{}, 1)

			task, err := New(repo, wake).CreateTask(t.Context(), tt.pair, tt.key)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Empty(t, task)
			} else {
				require.NoError(t, err)
				require.Equal(t, repo.task, task)
			}

			if tt.wantPair == "" {
				require.Empty(t, repo.created)
			} else {
				require.Len(t, repo.created, 1)
				require.Equal(t, tt.wantPair, repo.created[0].pair)
				require.Equal(t, tt.key, repo.created[0].key)
			}

			require.Len(t, wake, boolToInt(tt.wantWake))
		})
	}
}

// TestServiceCreateTaskSignalsAfterTheRowExists checks the ordering the signal
// depends on. A worker woken by it claims from the table, so a signal sent
// ahead of the insert would find an empty queue and be spent for nothing.
func TestServiceCreateTaskSignalsAfterTheRowExists(t *testing.T) {
	wake := make(chan struct{}, 1)

	var signalledEarly bool

	repo := &stubRepo{
		task:     domain.UpdateTask{ID: uuid.New(), Pair: domain.Pair("EUR/MXN"), Status: domain.StatusPending},
		onCreate: func() { signalledEarly = len(wake) > 0 },
	}

	_, err := New(repo, wake).CreateTask(t.Context(), "EUR/MXN", nil)

	require.NoError(t, err)
	require.False(t, signalledEarly, "a worker was woken before the task existed")
	require.Len(t, wake, 1)
}

// TestServiceCreateTaskDoesNotWaitOnTheSignal checks that a response never
// waits on a worker. The task is committed by the time the signal is sent, and
// a worker that misses it still polls -- so a full or unread channel has to
// cost nothing rather than hold the request open.
func TestServiceCreateTaskDoesNotWaitOnTheSignal(t *testing.T) {
	repo := &stubRepo{
		task: domain.UpdateTask{ID: uuid.New(), Pair: domain.Pair("EUR/MXN"), Status: domain.StatusPending},
	}

	// Unbuffered and unread: every worker is busy and none is at the select.
	svc := New(repo, make(chan struct{}))

	done := make(chan error, 1)

	// The error is carried out rather than asserted inside: a failed assertion
	// stops the goroutine it runs in, and testing only allows that in the one
	// running the test.
	go func() {
		_, err := svc.CreateTask(context.Background(), "EUR/MXN", nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("CreateTask blocked on the wakeup signal")
	}
}

// TestServiceCreateTaskQueuesOneSignalForABurst checks that the signal says the
// queue is worth looking at rather than how much is in it: a worker that wakes
// goes on claiming until the queue is empty, so a burst of posts costs one
// wakeup.
func TestServiceCreateTaskQueuesOneSignalForABurst(t *testing.T) {
	repo := &stubRepo{
		task: domain.UpdateTask{ID: uuid.New(), Pair: domain.Pair("EUR/MXN"), Status: domain.StatusPending},
	}

	wake := make(chan struct{}, 1)
	svc := New(repo, wake)

	for range 5 {
		_, err := svc.CreateTask(t.Context(), "EUR/MXN", nil)
		require.NoError(t, err)
	}

	require.Len(t, repo.created, 5)
	require.Len(t, wake, 1)
}

// TestServiceGetTask checks the lookup a client polls with. Every status is an
// answer, failed included, so the only error it can produce is one storage
// raised.
func TestServiceGetTask(t *testing.T) {
	id := uuid.New()

	details := domain.TaskDetails{
		Task: domain.UpdateTask{ID: id, Pair: domain.Pair("EUR/MXN"), Status: domain.StatusDone, Attempts: 1},
		Quote: &domain.Quote{
			UpdateID:  id,
			Pair:      domain.Pair("EUR/MXN"),
			Rate:      decimal.RequireFromString("19.6552"),
			RateDate:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
			FetchedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
		},
	}

	repo := &stubRepo{details: details}

	got, err := New(repo, nil).GetTask(t.Context(), id)

	require.NoError(t, err)
	require.Equal(t, details, got)
	require.Equal(t, []uuid.UUID{id}, repo.fetched)
}

// TestServiceGetTaskNotFound checks that a missing task is reported as such
// rather than as a failure: the API turns this one into a 404 and everything
// else into a 500.
func TestServiceGetTaskNotFound(t *testing.T) {
	repo := &stubRepo{getErr: domain.ErrNotFound}

	_, err := New(repo, nil).GetTask(t.Context(), uuid.New())

	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestServiceGetLatestQuote covers the second endpoint that validates a pair.
// The refusal matters as much as the answer: an unsupported pair has to be
// refused outright rather than answered with a 404, which would suggest it
// might exist once somebody posts an update for it.
func TestServiceGetLatestQuote(t *testing.T) {
	quote := domain.Quote{
		UpdateID:  uuid.New(),
		Pair:      domain.Pair("EUR/MXN"),
		Rate:      decimal.RequireFromString("19.6552"),
		RateDate:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		FetchedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name string
		pair string

		wantPair domain.Pair
		quoteErr error
		wantErr  error
	}{
		{
			name:     "a stored rate is returned as it stands",
			pair:     "EUR/MXN",
			wantPair: domain.Pair("EUR/MXN"),
		},
		{
			name:     "the pair is normalised before it reaches storage",
			pair:     "eur/mxn",
			wantPair: domain.Pair("EUR/MXN"),
		},
		{
			name:     "a supported pair nobody has quoted is not found",
			pair:     "USD/MXN",
			wantPair: domain.Pair("USD/MXN"),
			quoteErr: domain.ErrNotFound,
			wantErr:  domain.ErrNotFound,
		},
		{
			name:    "an unsupported pair never reaches storage",
			pair:    "EUR/RUB",
			wantErr: domain.ErrInvalidPair,
		},
		{
			name:    "a missing pair never reaches storage",
			pair:    "",
			wantErr: domain.ErrInvalidPair,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &stubRepo{quote: quote, quoteErr: tt.quoteErr}

			got, err := New(repo, nil).GetLatestQuote(t.Context(), tt.pair)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Empty(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, quote, got)
			}

			if tt.wantPair == "" {
				require.Empty(t, repo.looked)
			} else {
				require.Equal(t, []domain.Pair{tt.wantPair}, repo.looked)
			}
		})
	}
}

// boolToInt renders an expectation about the wakeup channel as the length it
// should have: one signal or none.
func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}
