package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Separate from DATABASE_URL on purpose: these tests truncate every table
// between cases, and a variable shared with the running service would make
// that a plausible accident. Unset, every test here skips.
const testDatabaseURL = "TEST_DATABASE_URL"

// Long enough that nothing expires by the clock during a run: the tests about
// expiry move a row's timestamp instead of waiting for one.
const testKeyTTL = time.Hour

// Shared, since connecting per test would dominate the runtime. The tests do
// not run in parallel and each starts from an empty schema.
var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv(testDatabaseURL)

	if url == "" {
		// Said out loud, on stderr, because the alternative is a run that
		// looks complete and is not: the skips below are invisible without
		// -v, and `go test ./...` prints ok for this package either way --
		// while the tests it skipped are the only ones that ever execute the
		// SQL. One line, once per package, in the output of a plain run.
		fmt.Fprintf(os.Stderr,
			"%s is not set: the storage tests are SKIPPED and no SQL is exercised.\n"+
				"Point it at a database of its own to run them, see the Tests section of README.md.\n",
			testDatabaseURL)

		os.Exit(m.Run())
	}

	ctx := context.Background()

	// Failing loudly rather than skipping, here and below. The variable was set
	// deliberately, so a database that cannot be reached is a broken run and
	// not an absent one -- silently skipping is how a suite stops covering
	// anything without anybody noticing.
	p, err := pgxpool.New(ctx, url)
	if err == nil {
		err = p.Ping(ctx)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s is set but the database could not be reached: %v\n", testDatabaseURL, err)
		os.Exit(1)
	}

	// Applying the migrations is not this suite's job: they are applied by the
	// migrate service of docker compose, which is the only thing in the project
	// that applies them. All that is checked here is that somebody did, since
	// the alternative is every test in the package failing on a missing table.
	if err := requireSchema(ctx, p); err != nil {
		fmt.Fprintf(os.Stderr,
			"%s is set but the schema is not there: %v\n"+
				"Apply the migrations to it first, see the Tests section of README.md.\n",
			testDatabaseURL, err)
		os.Exit(1)
	}

	pool = p
	code := m.Run()
	pool.Close()

	os.Exit(code)
}

func requireSchema(ctx context.Context, p *pgxpool.Pool) error {
	const query = `SELECT to_regclass('quote_updates'), to_regclass('quotes'), to_regclass('idempotency_keys')`

	var updates, quotes, keys *string
	if err := p.QueryRow(ctx, query).Scan(&updates, &quotes, &keys); err != nil {
		return err
	}

	if updates == nil || quotes == nil || keys == nil {
		return errors.New("one of quote_updates, quotes, idempotency_keys is missing")
	}

	return nil
}

func newRepo(t *testing.T) *Repository {
	t.Helper()

	if pool == nil {
		t.Skipf("%s is not set", testDatabaseURL)
	}

	// Between cases rather than after them, so that a failed test leaves its
	// rows behind to be looked at.
	_, err := pool.Exec(t.Context(), `TRUNCATE quotes, idempotency_keys, quote_updates`)
	require.NoError(t, err)

	return &Repository{pool: pool, keyTTL: testKeyTTL}
}

func createTask(t *testing.T, r *Repository, pair string, key *uuid.UUID) domain.UpdateTask {
	t.Helper()

	task, err := r.CreateTask(t.Context(), domain.Pair(pair), key)
	require.NoError(t, err)

	return task
}

func claimTask(t *testing.T, r *Repository) domain.UpdateTask {
	t.Helper()

	task, ok, err := r.ClaimTask(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "the queue was empty")

	return task
}

func finish(t *testing.T, r *Repository, task domain.UpdateTask) {
	t.Helper()

	claim := claimTask(t, r)
	require.Equal(t, task.ID, claim.ID)

	err := r.CompleteTask(
		t.Context(), claim,
		decimal.RequireFromString("19.6552"), today(), time.Now().UTC(),
	)
	require.NoError(t, err)
}

func countRows(t *testing.T, table string) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n))

	return n
}

// Moves a row's clock back by d, written directly because age is measured by
// the database clock everywhere in the repository.
func age(t *testing.T, table, column string, id uuid.UUID, d time.Duration) {
	t.Helper()

	_, err := pool.Exec(t.Context(),
		fmt.Sprintf(`UPDATE %s SET %s = now() - make_interval(secs => $2) WHERE %s = $1`, table, column, idColumn(table)),
		id, d.Seconds())
	require.NoError(t, err)
}

func idColumn(table string) string {
	if table == "idempotency_keys" {
		return "key"
	}

	return "id"
}

// What a post leaves in the table. The identifier and both timestamps come
// from the database, so what comes back has to be the row a client will later
// be shown.
func TestCreateTask(t *testing.T) {
	repo := newRepo(t)

	task := createTask(t, repo, "EUR/MXN", nil)

	require.NotEqual(t, uuid.Nil, task.ID)
	require.Equal(t, domain.Pair("EUR/MXN"), task.Pair)
	require.Equal(t, domain.StatusPending, task.Status)
	// Counts claims, and nothing has claimed it yet.
	require.Zero(t, task.Attempts)
	require.Empty(t, task.Error)
	require.False(t, task.CreatedAt.IsZero())
	require.False(t, task.UpdatedAt.IsZero())
	require.Equal(t, 1, countRows(t, "quote_updates"))
}

// The half of the mechanism that needs no key: at most one task per pair may
// be waiting or running, so a second post lands on the first task rather than
// queueing another refresh of the same thing.
func TestCreateTaskDeduplicatesUnfinishedWork(t *testing.T) {
	repo := newRepo(t)

	first := createTask(t, repo, "EUR/MXN", nil)
	second := createTask(t, repo, "EUR/MXN", nil)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, countRows(t, "quote_updates"))

	// A different pair is different work and gets its own task.
	other := createTask(t, repo, "USD/MXN", nil)

	require.NotEqual(t, first.ID, other.ID)
	require.Equal(t, 2, countRows(t, "quote_updates"))
}

// The predicate covers in_progress as well as pending. Were it pending alone,
// a post arriving during the claim would be handed a second update of the same
// pair.
func TestCreateTaskDeduplicatesOntoAClaimedTask(t *testing.T) {
	repo := newRepo(t)

	first := createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)
	require.Equal(t, first.ID, claim.ID)

	second := createTask(t, repo, "EUR/MXN", nil)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, domain.StatusInProgress, second.Status)
	require.Equal(t, 1, countRows(t, "quote_updates"))
}

// The reason the conflicting insert updates a column with its own value. For
// an in_progress row updated_at is the claim time the recovery pass measures
// staleness against, and posts on a busy pair would otherwise push the
// threshold ahead of itself for as long as they kept arriving -- leaving a
// task abandoned by a dead worker in progress forever.
func TestCreateTaskDoesNotTouchTheRecoveryClock(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	age(t, "quote_updates", "updated_at", claim.ID, time.Hour)

	before := taskByID(t, claim.ID)
	createTask(t, repo, "EUR/MXN", nil)
	after := taskByID(t, claim.ID)

	require.Equal(t, before.UpdatedAt, after.UpdatedAt, "the post moved the recovery clock")
}

// Finished history never blocks new work: the unique index covers the
// unfinished statuses only.
func TestCreateTaskAfterATerminalStatus(t *testing.T) {
	tests := []struct {
		name  string
		close func(t *testing.T, r *Repository, claim domain.UpdateTask)
	}{
		{
			name: "after a completed task",
			close: func(t *testing.T, r *Repository, claim domain.UpdateTask) {
				require.NoError(t, r.CompleteTask(
					t.Context(), claim, decimal.RequireFromString("19.6552"), today(), time.Now()))
			},
		},
		{
			name: "after a failed task",
			close: func(t *testing.T, r *Repository, claim domain.UpdateTask) {
				require.NoError(t, r.FailTask(t.Context(), claim, "provider unavailable after 3 attempts"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)

			first := createTask(t, repo, "EUR/MXN", nil)
			tt.close(t, repo, claimTask(t, repo))

			second := createTask(t, repo, "EUR/MXN", nil)

			require.NotEqual(t, first.ID, second.ID)
			require.Equal(t, domain.StatusPending, second.Status)
			require.Equal(t, 2, countRows(t, "quote_updates"))
		})
	}
}

// The case the whole mechanism exists for: one client sending the same request
// twice, usually long after the first task finished, when nothing else would
// deduplicate it.
func TestCreateTaskWithAKey(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	first := createTask(t, repo, "EUR/MXN", &key)
	second := createTask(t, repo, "EUR/MXN", &key)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, countRows(t, "quote_updates"))
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
}

// The window the key covers and the pair index does not. The answer carries
// the status the row holds now, which is how a client that retried after a
// failure learns to send a fresh key rather than poll a task that will never
// move.
func TestCreateTaskReplaysAKeyAfterTheTaskFinished(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	first := createTask(t, repo, "EUR/MXN", &key)
	require.NoError(t, repo.FailTask(t.Context(), claimTask(t, repo), "provider unavailable after 3 attempts"))

	second := createTask(t, repo, "EUR/MXN", &key)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, domain.StatusFailed, second.Status)
	// No second refresh was queued: the key answered the post.
	require.Equal(t, 1, countRows(t, "quote_updates"))
}

// Several keys can point at one task, which is why the bindings live in a
// table of their own: a post that deduplicates onto unfinished work binds its
// own key to that same row, and a column on the task would hold only the
// first.
func TestCreateTaskBindsASecondKeyToOneTask(t *testing.T) {
	repo := newRepo(t)

	firstKey, secondKey := uuid.New(), uuid.New()

	first := createTask(t, repo, "EUR/MXN", &firstKey)
	second := createTask(t, repo, "EUR/MXN", &secondKey)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, countRows(t, "quote_updates"))
	require.Equal(t, 2, countRows(t, "idempotency_keys"))
}

// The refusal a 409 is built on. Answering with the bound task instead would
// hand the client an update of a pair it never asked for.
func TestCreateTaskKeyConflict(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()
	first := createTask(t, repo, "EUR/MXN", &key)

	_, err := repo.CreateTask(t.Context(), domain.Pair("USD/MXN"), &key)

	require.ErrorIs(t, err, domain.ErrKeyConflict)
	// The refused post left nothing behind: its task and its binding rolled
	// back together.
	require.Equal(t, 1, countRows(t, "quote_updates"))
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
	require.Equal(t, domain.StatusPending, taskByID(t, first.ID).Status)
}

// The test the transaction exists for. Twenty posts sharing a key are
// serialised by the primary key of the bindings table -- by a constraint
// rather than by anything the service checks -- and every one of them has to
// come back with the same task.
func TestCreateTaskConcurrentWithOneKey(t *testing.T) {
	repo := newRepo(t)

	const posts = 20

	key := uuid.New()

	ids := make([]uuid.UUID, posts)
	errs := make([]error, posts)

	var wg sync.WaitGroup

	for i := range posts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			task, err := repo.CreateTask(context.Background(), domain.Pair("EUR/MXN"), &key)
			ids[i], errs[i] = task.ID, err
		}()
	}

	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "post %d", i)
		require.Equalf(t, ids[0], ids[i], "post %d was answered with another task", i)
	}

	require.Equal(t, 1, countRows(t, "quote_updates"))
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
}

// The other arbiter under the same pressure: with no key at all, the partial
// unique index on the pair is what keeps twenty simultaneous posts down to one
// refresh.
func TestCreateTaskConcurrentWithoutAKey(t *testing.T) {
	repo := newRepo(t)

	const posts = 20

	ids := make([]uuid.UUID, posts)
	errs := make([]error, posts)

	var wg sync.WaitGroup

	for i := range posts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			task, err := repo.CreateTask(context.Background(), domain.Pair("EUR/MXN"), nil)
			ids[i], errs[i] = task.ID, err
		}()
	}

	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "post %d", i)
		require.Equalf(t, ids[0], ids[i], "post %d was answered with another task", i)
	}

	require.Equal(t, 1, countRows(t, "quote_updates"))
}

// A lost race leaves nothing behind. Each post writes its task before it finds
// out whether its key is free, so the losers have a task in hand when they are
// refused -- and the rollback has to take it with them, or the table fills
// with updates whose identifier reached nobody.
func TestCreateTaskConcurrentWithOneKeyOverManyPairs(t *testing.T) {
	repo := newRepo(t)

	pairs := []domain.Pair{"EUR/MXN", "USD/MXN", "EUR/USD", "MXN/EUR", "USD/EUR"}

	const perPair = 4

	key := uuid.New()

	type answer struct {
		task domain.UpdateTask
		err  error
	}

	answers := make([]answer, len(pairs)*perPair)

	var wg sync.WaitGroup

	for i := range answers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			task, err := repo.CreateTask(context.Background(), pairs[i%len(pairs)], &key)
			answers[i] = answer{task: task, err: err}
		}()
	}

	wg.Wait()

	var (
		accepted int
		winner   domain.UpdateTask
	)

	for i, got := range answers {
		if got.err == nil {
			accepted++
			winner = got.task

			continue
		}

		// The only failure allowed here: whoever asked for another pair is
		// refused, never answered with somebody else's update.
		require.ErrorIsf(t, got.err, domain.ErrKeyConflict, "post %d", i)
	}

	// One pair won the key, and everybody who asked for that same pair was
	// answered with the one task.
	require.Equal(t, perPair, accepted)
	for i, got := range answers {
		if got.err == nil {
			require.Equalf(t, winner.ID, got.task.ID, "post %d", i)
		}
	}

	require.Equal(t, 1, countRows(t, "quote_updates"), "a refused post left an orphaned task behind")
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
}

// The single query that answers the endpoint a client polls, in the two shapes
// it has: a task with no quote yet, and a completed one whose rate is joined
// in.
func TestGetTask(t *testing.T) {
	repo := newRepo(t)

	queued := createTask(t, repo, "EUR/MXN", nil)

	details, err := repo.GetTask(t.Context(), queued.ID)
	require.NoError(t, err)
	require.Equal(t, queued.ID, details.Task.ID)
	require.Equal(t, domain.StatusPending, details.Task.Status)
	require.Nil(t, details.Quote, "a task that has not completed has no quote")

	claim := claimTask(t, repo)
	rate := decimal.RequireFromString("19.6552123456")
	fetchedAt := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, repo.CompleteTask(t.Context(), claim, rate, today(), fetchedAt))

	details, err = repo.GetTask(t.Context(), queued.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusDone, details.Task.Status)
	require.Empty(t, details.Task.Error)
	require.NotNil(t, details.Quote)
	require.True(t, rate.Equal(details.Quote.Rate), "stored %s, read back %s", rate, details.Quote.Rate)
	require.Equal(t, queued.ID, details.Quote.UpdateID)
	require.Equal(t, domain.Pair("EUR/MXN"), details.Quote.Pair)
	require.Equal(t, today(), details.Quote.RateDate.UTC())
	require.Equal(t, fetchedAt, details.Quote.FetchedAt.UTC())
}

// A well formed identifier naming no task is a fact rather than a failure: the
// API turns this into a 404 and everything else into a 500.
func TestGetTaskNotFound(t *testing.T) {
	repo := newRepo(t)

	_, err := repo.GetTask(t.Context(), uuid.New())

	require.ErrorIs(t, err, domain.ErrNotFound)
}

// A failed task answers with the reason the worker wrote, which is what a
// client is shown.
func TestGetTaskCarriesTheFailureReason(t *testing.T) {
	repo := newRepo(t)

	task := createTask(t, repo, "EUR/MXN", nil)
	require.NoError(t, repo.FailTask(t.Context(), claimTask(t, repo), "provider unavailable after 3 attempts"))

	details, err := repo.GetTask(t.Context(), task.ID)

	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, details.Task.Status)
	require.Equal(t, "provider unavailable after 3 attempts", details.Task.Error)
	require.Nil(t, details.Quote)
}

// The invariant the whole decimal choice rests on. The values below are chosen
// to be exactly the ones binary floating point cannot hold: a rate that
// survived a float64 would be a coincidence.
func TestRateSurvivesTheRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		rate string
	}{
		{name: "a rate as the provider publishes it", rate: "19.6552"},
		{name: "every place the column holds", rate: "19.6552123456"},
		{name: "the smallest value the scale allows", rate: "0.0000000001"},
		{name: "the whole precision of the column", rate: "1234567890.1234567890"},
		{name: "a value with no exact binary form", rate: "0.1000000003"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)

			createTask(t, repo, "EUR/MXN", nil)

			rate := decimal.RequireFromString(tt.rate)
			require.NoError(t, repo.CompleteTask(t.Context(), claimTask(t, repo), rate, today(), time.Now()))

			quote, err := repo.GetLatestQuote(t.Context(), domain.Pair("EUR/MXN"))
			require.NoError(t, err)

			// Compared by value and then by rendering: the scale matters as
			// much as the number, since the API serves the column's ten places
			// rather than an abbreviation of them.
			require.True(t, rate.Equal(quote.Rate), "stored %s, read back %s", rate, quote.Rate)
			require.Equal(t, rate.StringFixed(10), quote.Rate.StringFixed(10))
		})
	}
}

// The ordering the endpoint depends on. It is by fetched_at rather than
// rate_date because a weekend leaves several rows sharing one rate_date, and
// their order would then be undefined.
func TestGetLatestQuote(t *testing.T) {
	repo := newRepo(t)

	rateDate := today()
	older := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newer := time.Now().UTC().Truncate(time.Second)

	// Two quotes for one pair, sharing a rate_date as they would over a
	// weekend, and inserted oldest last so that insertion order cannot be what
	// answers.
	first := createTask(t, repo, "EUR/MXN", nil)
	require.NoError(t, repo.CompleteTask(
		t.Context(), claimTask(t, repo), decimal.RequireFromString("19.6552"), rateDate, newer))

	second := createTask(t, repo, "EUR/MXN", nil)
	require.NoError(t, repo.CompleteTask(
		t.Context(), claimTask(t, repo), decimal.RequireFromString("18.1111"), rateDate, older))

	require.NotEqual(t, first.ID, second.ID)

	// A quote for another pair, which must not be reachable through this one.
	createTask(t, repo, "USD/MXN", nil)
	require.NoError(t, repo.CompleteTask(
		t.Context(), claimTask(t, repo), decimal.RequireFromString("17.0000"), rateDate, newer))

	quote, err := repo.GetLatestQuote(t.Context(), domain.Pair("EUR/MXN"))

	require.NoError(t, err)
	require.Equal(t, first.ID, quote.UpdateID)
	require.Equal(t, domain.Pair("EUR/MXN"), quote.Pair)
	require.True(t, decimal.RequireFromString("19.6552").Equal(quote.Rate))
	require.Equal(t, newer, quote.FetchedAt.UTC())
	require.Equal(t, rateDate, quote.RateDate.UTC())
}

// The answer for a supported pair nobody has quoted yet, which the API tells
// apart from an unsupported one: this pair will have a rate as soon as an
// update completes.
func TestGetLatestQuoteNotFound(t *testing.T) {
	repo := newRepo(t)

	// A queued task is not a quote: the endpoint reads what was fetched.
	createTask(t, repo, "EUR/MXN", nil)

	_, err := repo.GetLatestQuote(t.Context(), domain.Pair("EUR/MXN"))

	require.ErrorIs(t, err, domain.ErrNotFound)
}

// What a claim does to the row: the status and the attempt counter move
// together, and the counter is what identifies this claim to the finalisation
// later.
func TestClaimTask(t *testing.T) {
	repo := newRepo(t)

	queued := createTask(t, repo, "EUR/MXN", nil)

	claim := claimTask(t, repo)

	require.Equal(t, queued.ID, claim.ID)
	require.Equal(t, domain.Pair("EUR/MXN"), claim.Pair)
	require.Equal(t, domain.StatusInProgress, claim.Status)
	require.Equal(t, 1, claim.Attempts)
	require.False(t, claim.UpdatedAt.Before(queued.UpdatedAt), "the claim did not move updated_at")
}

// An idle queue is a state and not an error: it is what a worker sees most of
// the time.
func TestClaimTaskEmptyQueue(t *testing.T) {
	repo := newRepo(t)

	task, ok, err := repo.ClaimTask(t.Context())

	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, task)
}

// The ordering of the queue, so that a steady stream of posts cannot leave an
// early task waiting indefinitely.
func TestClaimTaskTakesTheOldestFirst(t *testing.T) {
	repo := newRepo(t)

	first := createTask(t, repo, "EUR/MXN", nil)
	second := createTask(t, repo, "USD/MXN", nil)
	third := createTask(t, repo, "MXN/EUR", nil)

	require.Equal(t, first.ID, claimTask(t, repo).ID)
	require.Equal(t, second.ID, claimTask(t, repo).ID)
	require.Equal(t, third.ID, claimTask(t, repo).ID)

	_, ok, err := repo.ClaimTask(t.Context())
	require.NoError(t, err)
	require.False(t, ok)
}

// Not a test of SKIP LOCKED, which the one below covers: this passes without
// it too, since a claim that waits for the lock and then finds the row taken
// moves on. Waiting is the cost SKIP LOCKED removes, not a wrong answer it
// prevents.
func TestClaimTaskHandsEachTaskToOneWorker(t *testing.T) {
	repo := newRepo(t)

	pairs := []domain.Pair{"EUR/MXN", "USD/MXN", "MXN/EUR", "EUR/USD", "USD/EUR", "MXN/USD"}
	for _, pair := range pairs {
		createTask(t, repo, string(pair), nil)
	}

	const workers = 8

	claims := make([]uuid.UUID, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup

	for i := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// Recorded rather than asserted here: a failed assertion stops the
			// goroutine it runs in, and testing only allows that in the one
			// running the test.
			task, ok, err := repo.ClaimTask(context.Background())
			errs[i] = err

			if ok {
				claims[i] = task.ID
			}
		}()
	}

	wg.Wait()

	seen := make(map[uuid.UUID]int)

	for i, err := range errs {
		require.NoErrorf(t, err, "worker %d", i)

		if claims[i] != uuid.Nil {
			seen[claims[i]]++
		}
	}

	require.Len(t, seen, len(pairs), "not every task was claimed")
	for id, times := range seen {
		require.Equalf(t, 1, times, "task %s was claimed twice", id)
	}
}

// The oldest task is locked by a transaction deliberately left open, so the
// claim has to step over it rather than wait. The deadline is what makes the
// difference visible: waiting would run into it instead of returning the
// second task.
func TestClaimTaskDoesNotWaitForALockedRow(t *testing.T) {
	repo := newRepo(t)

	first := createTask(t, repo, "EUR/MXN", nil)
	second := createTask(t, repo, "USD/MXN", nil)

	// A connection of its own: the lock has to be held by somebody else while
	// the claim below runs.
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)

	defer func() { _ = holder.Rollback(t.Context()) }()

	var locked uuid.UUID
	require.NoError(t,
		holder.QueryRow(t.Context(), `SELECT id FROM quote_updates WHERE id = $1 FOR UPDATE`, first.ID).Scan(&locked))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	claim, ok, err := repo.ClaimTask(ctx)

	require.NoError(t, err, "the claim waited for a row somebody else had locked")
	require.True(t, ok)
	require.Equal(t, second.ID, claim.ID, "the claim did not step over the locked task")
}

// The finalisation, which has to be all or nothing: a quote whose task is not
// finished would be served by GET /quotes/latest under a task that still
// claims to be running.
func TestCompleteTask(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	rate := decimal.RequireFromString("19.6552")
	fetchedAt := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, repo.CompleteTask(t.Context(), claim, rate, today(), fetchedAt))

	task := taskByID(t, claim.ID)
	require.Equal(t, domain.StatusDone, task.Status)
	require.Empty(t, task.Error)
	require.Equal(t, 1, countRows(t, "quotes"))

	quote, err := repo.GetLatestQuote(t.Context(), domain.Pair("EUR/MXN"))
	require.NoError(t, err)
	// Copied from the claim, which is the only source of it: the denormalised
	// column cannot drift from the task it belongs to.
	require.Equal(t, claim.Pair, quote.Pair)
	require.Equal(t, claim.ID, quote.UpdateID)
}

// The proof of ownership a finalisation has to carry. The task belongs to
// someone else by now, so storing the rate would attach it to an update that
// is queued or running again -- and the rate is discarded instead, leaving no
// row in quotes.
func TestCompleteTaskStaleClaim(t *testing.T) {
	tests := []struct {
		name string
		// stale turns the claim into one that is no longer current, the way
		// the recovery pass would.
		stale func(t *testing.T, r *Repository, claim domain.UpdateTask) domain.UpdateTask
	}{
		{
			// The status check alone would pass here: the task is in progress
			// again, just not ours.
			name: "the task was released and claimed by somebody else",
			stale: func(t *testing.T, r *Repository, claim domain.UpdateTask) domain.UpdateTask {
				require.NoError(t, r.ReleaseTask(t.Context(), claim))
				claimTask(t, r)

				return claim
			},
		},
		{
			name: "the task was closed as failed by the recovery pass",
			stale: func(t *testing.T, r *Repository, claim domain.UpdateTask) domain.UpdateTask {
				require.NoError(t, r.FailTask(t.Context(), claim, "abandoned"))

				return claim
			},
		},
		{
			name: "the claim was never current",
			stale: func(_ *testing.T, _ *Repository, claim domain.UpdateTask) domain.UpdateTask {
				claim.Attempts++

				return claim
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)

			createTask(t, repo, "EUR/MXN", nil)
			claim := tt.stale(t, repo, claimTask(t, repo))

			err := repo.CompleteTask(
				t.Context(), claim, decimal.RequireFromString("19.6552"), today(), time.Now())

			require.ErrorIs(t, err, domain.ErrStaleClaim)
			require.Zero(t, countRows(t, "quotes"), "a lost claim left a rate behind")
		})
	}
}

// The terminal status a client is shown a reason with.
func TestFailTask(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	require.NoError(t, repo.FailTask(t.Context(), claim, "provider unavailable after 3 attempts"))

	task := taskByID(t, claim.ID)
	require.Equal(t, domain.StatusFailed, task.Status)
	require.Equal(t, "provider unavailable after 3 attempts", task.Error)
	require.Zero(t, countRows(t, "quotes"))
}

// The guard in front of a constraint the schema states in one direction only:
// an empty string satisfies "a failed task has a reason" while telling a
// client nothing.
func TestFailTaskEmptyReason(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	require.Error(t, repo.FailTask(t.Context(), claim, ""))
	require.Equal(t, domain.StatusInProgress, taskByID(t, claim.ID).Status)
}

// A worker cannot report a failure of a task that is no longer its own, which
// would overwrite the state of whoever holds it now.
func TestFailTaskStaleClaim(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)
	require.NoError(t, repo.ReleaseTask(t.Context(), claim))

	current := claimTask(t, repo)

	err := repo.FailTask(t.Context(), claim, "provider unavailable after 3 attempts")

	require.ErrorIs(t, err, domain.ErrStaleClaim)
	require.Equal(t, domain.StatusInProgress, taskByID(t, current.ID).Status)
}

// The way out for a worker that is giving a task up rather than finishing it:
// on shutdown nobody is at fault, so failing the task would tell a client the
// provider was.
func TestReleaseTask(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	require.NoError(t, repo.ReleaseTask(t.Context(), claim))

	task := taskByID(t, claim.ID)
	require.Equal(t, domain.StatusPending, task.Status)
	// Not decremented: it counts how many times processing was started, and one
	// start did happen.
	require.Equal(t, 1, task.Attempts)

	// Back in the queue, and the next claim moves the counter on.
	require.Equal(t, 2, claimTask(t, repo).Attempts)
}

// A worker leaving late cannot take the task away from whoever claimed it
// after the recovery pass released it.
func TestReleaseTaskStaleClaim(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)
	require.NoError(t, repo.ReleaseTask(t.Context(), claim))
	current := claimTask(t, repo)

	err := repo.ReleaseTask(t.Context(), claim)

	require.ErrorIs(t, err, domain.ErrStaleClaim)
	require.Equal(t, domain.StatusInProgress, taskByID(t, current.ID).Status)
}

// The pass that deals with tasks left in progress by a worker that died:
// nothing releases those on their own, since the row lock disappeared along
// with the process. Both branches matter -- one returns work to the queue, the
// other stops a task that reliably kills its worker from cycling between claim
// and release forever.
func TestReleaseStuckTasks(t *testing.T) {
	const (
		threshold   = time.Minute
		maxAttempts = 3
		reason      = "abandoned after 3 attempts"
	)

	tests := []struct {
		name string
		// attempts is how many claims the task has behind it when the pass
		// runs.
		attempts int
		// stuckFor is how long it has been in progress.
		stuckFor time.Duration

		wantReleased int
		wantFailed   int
		wantStatus   domain.Status
	}{
		{
			name:       "a task claimed a moment ago is left alone",
			attempts:   1,
			stuckFor:   time.Second,
			wantStatus: domain.StatusInProgress,
		},
		{
			name:         "a stuck task below the limit goes back to the queue",
			attempts:     1,
			stuckFor:     2 * time.Minute,
			wantReleased: 1,
			wantStatus:   domain.StatusPending,
		},
		{
			name:       "a stuck task at the limit is closed as failed",
			attempts:   maxAttempts,
			stuckFor:   2 * time.Minute,
			wantFailed: 1,
			wantStatus: domain.StatusFailed,
		},
		{
			name:       "a task at the limit that is not stuck is left alone",
			attempts:   maxAttempts,
			stuckFor:   time.Second,
			wantStatus: domain.StatusInProgress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)

			createTask(t, repo, "EUR/MXN", nil)

			var claim domain.UpdateTask
			for range tt.attempts {
				claim = claimTask(t, repo)
				if claim.Attempts < tt.attempts {
					require.NoError(t, repo.ReleaseTask(t.Context(), claim))
				}
			}

			require.Equal(t, tt.attempts, claim.Attempts)

			age(t, "quote_updates", "updated_at", claim.ID, tt.stuckFor)

			released, failed, err := repo.ReleaseStuckTasks(t.Context(), threshold, maxAttempts, reason)

			require.NoError(t, err)
			require.Equal(t, tt.wantReleased, released)
			require.Equal(t, tt.wantFailed, failed)

			task := taskByID(t, claim.ID)
			require.Equal(t, tt.wantStatus, task.Status)
			// The counter is left alone by both branches: it counts claims, and
			// the next claim is what moves it on.
			require.Equal(t, tt.attempts, task.Attempts)

			if tt.wantStatus == domain.StatusFailed {
				require.Equal(t, reason, task.Error)
			}
		})
	}
}

// The pass touches nothing that has finished. A done task carries a quote, and
// reopening it would put a second rate under one update.
func TestReleaseStuckTasksLeavesTerminalTasksAlone(t *testing.T) {
	repo := newRepo(t)

	done := createTask(t, repo, "EUR/MXN", nil)
	require.NoError(t, repo.CompleteTask(
		t.Context(), claimTask(t, repo), decimal.RequireFromString("19.6552"), today(), time.Now()))

	failed := createTask(t, repo, "USD/MXN", nil)
	require.NoError(t, repo.FailTask(t.Context(), claimTask(t, repo), "provider unavailable after 3 attempts"))

	queued := createTask(t, repo, "MXN/EUR", nil)

	for _, id := range []uuid.UUID{done.ID, failed.ID, queued.ID} {
		age(t, "quote_updates", "updated_at", id, time.Hour)
	}

	released, abandoned, err := repo.ReleaseStuckTasks(t.Context(), time.Minute, 3, "abandoned")

	require.NoError(t, err)
	require.Zero(t, released)
	require.Zero(t, abandoned)
	require.Equal(t, domain.StatusDone, taskByID(t, done.ID).Status)
	require.Equal(t, domain.StatusFailed, taskByID(t, failed.ID).Status)
	require.Equal(t, domain.StatusPending, taskByID(t, queued.ID).Status)
}

// The same guard FailTask carries: the pass writes a failure a client will
// read.
func TestReleaseStuckTasksEmptyReason(t *testing.T) {
	repo := newRepo(t)

	_, _, err := repo.ReleaseStuckTasks(t.Context(), time.Minute, 3, "")

	require.Error(t, err)
}

// The whole of what ends a binding now that nothing deletes one: the lookup
// stops seeing the row, and the post that finds it expired writes its own task
// over it.
func TestExpiredKeyIsTakenOver(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	first := createTask(t, repo, "EUR/MXN", &key)
	require.Equal(t, 1, countRows(t, "idempotency_keys"))

	// The task has to leave the queue as well, or the partial index would
	// answer the second post with the same task whatever the key did.
	finish(t, repo, first)

	age(t, "idempotency_keys", "created_at", key, 2*testKeyTTL)

	second := createTask(t, repo, "EUR/MXN", &key)

	// A new task, bound to the same key, in the same row: taking a binding over
	// is an update, not a second receipt.
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, 1, countRows(t, "idempotency_keys"))

	// And the key answers for the new task from here on.
	replayed := createTask(t, repo, "EUR/MXN", &key)
	require.Equal(t, second.ID, replayed.ID)
}

// The other half of a takeover: an expired binding holds nothing back, so the
// key is free to name a different pair than the one it was spent on.
func TestExpiredKeyIsTakenOverAcrossPairs(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	first := createTask(t, repo, "EUR/MXN", &key)
	finish(t, repo, first)

	// While it holds, the same key on another pair is a conflict.
	_, err := repo.CreateTask(t.Context(), domain.Pair("USD/MXN"), &key)
	require.ErrorIs(t, err, domain.ErrKeyConflict)

	age(t, "idempotency_keys", "created_at", key, 2*testKeyTTL)

	second, err := repo.CreateTask(t.Context(), domain.Pair("USD/MXN"), &key)
	require.NoError(t, err)
	require.Equal(t, domain.Pair("USD/MXN"), second.Pair)
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
}

// The case the condition on the takeover exists for: a binding within its
// lifetime answers with the task it holds, and no row is written.
func TestLiveKeyIsNotTakenOver(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	first := createTask(t, repo, "EUR/MXN", &key)
	finish(t, repo, first)

	second := createTask(t, repo, "EUR/MXN", &key)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, countRows(t, "idempotency_keys"))
	require.Equal(t, 1, countRows(t, "quote_updates"))
}

func taskByID(t *testing.T, id uuid.UUID) domain.UpdateTask {
	t.Helper()

	const query = `
		SELECT id, pair, status, attempts, COALESCE(error, ''), created_at, updated_at
		  FROM quote_updates
		 WHERE id = $1`

	task, err := scanTask(pool.QueryRow(t.Context(), query, id))
	require.NoError(t, err)

	return task
}

// A bare date, which is what the provider publishes and what the column holds.
func today() time.Time {
	now := time.Now().UTC()

	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Every method has a second answer that is not an error -- no such row, or the
// claim is no longer yours -- and both are only meaningful once the database
// returned something. A statement that never ran must not be reported as
// either.
//
// A cancelled context fails them all at the same place: before the query is
// sent.
func TestRepositoryReportsAFailingDatabaseAsItself(t *testing.T) {
	repo := newRepo(t)

	// Built while the context still works, so the calls below fail for the one
	// reason under test rather than for want of a row.
	task := createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)
	key := uuid.New()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{
			name: "creating a task",
			call: func(ctx context.Context) error {
				_, err := repo.CreateTask(ctx, domain.Pair("USD/MXN"), nil)

				return err
			},
		},
		{
			name: "creating a task with a key",
			call: func(ctx context.Context) error {
				_, err := repo.CreateTask(ctx, domain.Pair("USD/MXN"), &key)

				return err
			},
		},
		{
			// Called past the lookup that answers a repeat, so that the
			// transaction itself is what fails: with a key, the read above
			// comes first and would otherwise be the only thing this reaches.
			name: "creating a task inside the key transaction",
			call: func(ctx context.Context) error {
				_, err := repo.createTaskWithKey(ctx, domain.Pair("USD/MXN"), key)

				return err
			},
		},
		{
			name: "reading a task",
			call: func(ctx context.Context) error {
				_, err := repo.GetTask(ctx, task.ID)

				return err
			},
		},
		{
			name: "reading the latest quote",
			call: func(ctx context.Context) error {
				_, err := repo.GetLatestQuote(ctx, domain.Pair("EUR/MXN"))

				return err
			},
		},
		{
			name: "claiming a task",
			call: func(ctx context.Context) error {
				_, _, err := repo.ClaimTask(ctx)

				return err
			},
		},
		{
			name: "completing a task",
			call: func(ctx context.Context) error {
				return repo.CompleteTask(ctx, claim, decimal.RequireFromString("19.6552"), today(), time.Now())
			},
		},
		{
			name: "failing a task",
			call: func(ctx context.Context) error {
				return repo.FailTask(ctx, claim, "provider unavailable after 3 attempts")
			},
		},
		{
			name: "releasing a task",
			call: func(ctx context.Context) error {
				return repo.ReleaseTask(ctx, claim)
			},
		},
		{
			name: "releasing stuck tasks",
			call: func(ctx context.Context) error {
				_, _, err := repo.ReleaseStuckTasks(ctx, time.Minute, 3, "abandoned")

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(ctx)

			require.Error(t, err)
			require.ErrorIs(t, err, context.Canceled)

			// None of the three may stand in for a database that did not
			// answer: each of them is a fact about a row, and no row was read.
			require.NotErrorIs(t, err, domain.ErrNotFound)
			require.NotErrorIs(t, err, domain.ErrStaleClaim)
			require.NotErrorIs(t, err, domain.ErrKeyConflict)
		})
	}

	// Nothing was decided about the task either: it is still held by the claim
	// whose finalisation could not be sent.
	require.Equal(t, domain.StatusInProgress, taskByID(t, claim.ID).Status)
}

// The finalising transaction from the side the happy path cannot show. The
// task is closed first and the quote inserted second, so an insert the column
// refuses has to take the closure with it -- otherwise the task reads done
// while no rate exists, and GET /quotes/updates/{id} answers with a completed
// update carrying nothing.
//
// The rate below is one the provider would never pass on; the constraint is
// the backstop behind that, and this is the only test that asks it anything.
func TestCompleteTaskRollsBackARefusedQuote(t *testing.T) {
	repo := newRepo(t)

	createTask(t, repo, "EUR/MXN", nil)
	claim := claimTask(t, repo)

	err := repo.CompleteTask(t.Context(), claim, decimal.Zero, today(), time.Now())

	require.Error(t, err)
	require.NotErrorIs(t, err, domain.ErrStaleClaim)

	require.Equal(t, domain.StatusInProgress, taskByID(t, claim.ID).Status, "the task was closed without a rate")
	require.Zero(t, countRows(t, "quotes"))
}

// The branch that needs the two statements to read the clock at two instants:
// a binding that still held when the insert refused it, and was expired by the
// time of the read that follows.
//
// Called directly rather than raced into, since what is worth pinning is not
// how to get there but what happens: it is reported. Answered as a conflict it
// would tell a client its key belonged to another pair, and answered as
// success it would hand back an empty task.
func TestReplayByKeyWithoutABinding(t *testing.T) {
	repo := newRepo(t)

	key := uuid.New()

	task, err := repo.replayByKey(t.Context(), key, domain.Pair("EUR/MXN"))

	require.Error(t, err)
	require.ErrorContains(t, err, key.String())
	require.NotErrorIs(t, err, domain.ErrKeyConflict)
	require.Empty(t, task)
}
