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
	// ctxs holds the context each call arrived with, so a test can ask whether
	// it is the caller's own rather than one made up on the way down.
	ctxs []context.Context
}

func (s *stubRepo) CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error) {
	s.ctxs = append(s.ctxs, ctx)
	s.created = append(s.created, creation{pair: pair, key: key})

	if s.onCreate != nil {
		s.onCreate()
	}

	if s.createErr != nil {
		return domain.UpdateTask{}, s.createErr
	}

	return s.task, nil
}

func (s *stubRepo) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	s.ctxs = append(s.ctxs, ctx)
	s.fetched = append(s.fetched, id)

	if s.getErr != nil {
		return domain.TaskDetails{}, s.getErr
	}

	return s.details, nil
}

func (s *stubRepo) GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	s.ctxs = append(s.ctxs, ctx)
	s.looked = append(s.looked, pair)

	if s.quoteErr != nil {
		return domain.Quote{}, s.quoteErr
	}

	return s.quote, nil
}

// The one decision this method owns: a worker is woken only for a task that is
// actually waiting to be claimed. The pair arrives already parsed, so there is
// nothing left here to refuse.
func TestServiceCreateTask(t *testing.T) {
	key := uuid.New()
	conflict := domain.ErrKeyConflict
	unreachable := errors.New("connection refused")

	tests := []struct {
		name       string
		pair       domain.Pair
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

// The ordering the signal depends on. A worker woken by it claims from the
// table, so a signal sent ahead of the insert would find an empty queue and be
// spent for nothing.
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

// A response never waits on a worker. The task is committed by the time the
// signal is sent, and a worker that misses it still polls -- so a full or
// unread channel has to cost nothing rather than hold the request open.
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

// The signal says the queue is worth looking at rather than how much is in it:
// a worker that wakes goes on claiming until the queue is empty, so a burst of
// posts costs one wakeup.
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

// The lookup a client polls with. Every status is an answer, failed included,
// so the only error it can produce is one storage raised.
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

// A missing task is reported as such rather than as a failure: the API turns
// this one into a 404 and everything else into a 500.
func TestServiceGetTaskNotFound(t *testing.T) {
	repo := &stubRepo{getErr: domain.ErrNotFound}

	_, err := New(repo, nil).GetTask(t.Context(), uuid.New())

	require.ErrorIs(t, err, domain.ErrNotFound)
}

// The read: the pair reaches storage as it was handed in, and what storage
// answers is what the caller gets.
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
		pair domain.Pair

		quoteErr error
		wantErr  error
	}{
		{
			name: "a stored rate is returned as it stands",
			pair: domain.Pair("EUR/MXN"),
		},
		{
			name:     "a supported pair nobody has quoted is not found",
			pair:     domain.Pair("USD/MXN"),
			quoteErr: domain.ErrNotFound,
			wantErr:  domain.ErrNotFound,
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

			require.Equal(t, []domain.Pair{tt.pair}, repo.looked)
		})
	}
}

// One signal or none.
func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// callerKey marks a context as the one a test handed in, so that a context
// made up somewhere below can be told from the one that was passed down.
type callerKey struct{}

// The context has to reach the driver, or a client that goes away leaves work
// running against a connection nobody is waiting on.
//
// Two things are asked of it: the value says the context was inherited rather
// than made, and the cancellation says what was inherited is still the
// caller's -- a context.WithoutCancel would carry the value across and drop
// the only part that matters.
func TestServicePassesTheCallersContext(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, svc *Service) error
	}{
		{
			name: "creating a task",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.CreateTask(ctx, "EUR/MXN", nil)

				return err
			},
		},
		{
			name: "reading a task",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.GetTask(ctx, uuid.New())

				return err
			},
		},
		{
			name: "reading the latest quote",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.GetLatestQuote(ctx, "EUR/MXN")

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &stubRepo{
				task: domain.UpdateTask{ID: uuid.New(), Pair: domain.Pair("EUR/MXN"), Status: domain.StatusPending},
			}

			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), callerKey{}, "the caller's"))
			// Cancelled before the call rather than during it: what is being
			// checked is which context arrived, not what anybody does about it.
			cancel()

			require.NoError(t, tt.call(ctx, New(repo, make(chan struct{}, 1))))

			require.Len(t, repo.ctxs, 1)
			got := repo.ctxs[0]

			require.Equal(t, "the caller's", got.Value(callerKey{}), "storage was handed a context of its own")
			require.ErrorIs(t, got.Err(), context.Canceled, "storage was handed a context that outlives the caller")
		})
	}
}

// A database that has stopped answering is reported as itself. The API turns
// the domain errors into 400 and 404 and everything else into a 500, so an
// error invented here would be a 500 with a cause nobody wrote down.
func TestServiceGettersPassStorageFailuresUp(t *testing.T) {
	unreachable := errors.New("connection refused")

	t.Run("reading a task", func(t *testing.T) {
		repo := &stubRepo{getErr: unreachable}

		details, err := New(repo, nil).GetTask(t.Context(), uuid.New())

		require.ErrorIs(t, err, unreachable)
		require.Empty(t, details)
	})

	t.Run("reading the latest quote", func(t *testing.T) {
		repo := &stubRepo{quoteErr: unreachable}

		quote, err := New(repo, nil).GetLatestQuote(t.Context(), "EUR/MXN")

		require.ErrorIs(t, err, unreachable)
		require.Empty(t, quote)
		// The pair was valid, so the lookup did happen: this is storage
		// failing rather than the request being refused.
		require.Equal(t, []domain.Pair{domain.Pair("EUR/MXN")}, repo.looked)
	})
}
