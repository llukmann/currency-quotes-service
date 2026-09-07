package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// Repository is the whole of the service's persistence: the queue of update
// tasks and the quotes they produce. The two tables sit behind one type
// because the quotes table is written only by the finalisation of a task, and
// because every statement that moves a task between statuses then lives in one
// file and can be read as a set -- each of them has to carry
// updated_at = now(), and nothing in the schema enforces that.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository backed by pool. The pool is owned by the
// caller, which also closes it.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// CreateTask adds a refresh of pair to the queue and returns the task as
// stored. The identifier and the timestamps come from the database, so what is
// returned is the row a client will later be shown.
func (r *Repository) CreateTask(ctx context.Context, pair domain.Pair) (domain.UpdateTask, error) {
	const query = `
		INSERT INTO quote_updates (pair)
		VALUES ($1)
		RETURNING id, pair, status, attempts, COALESCE(error, ''), created_at, updated_at`

	t, err := scanTask(r.pool.QueryRow(ctx, query, pair))
	if err != nil {
		return domain.UpdateTask{}, fmt.Errorf("create task: %w", err)
	}

	return t, nil
}

// GetTask returns the task and, once it has completed successfully, the rate it
// produced. It backs GET /quotes/updates/{id}, the endpoint a client polls, and
// that is its only caller: a worker carries its task in hand and never reads it
// back. The join keeps the answer to a single round trip, and a quote row
// exists for precisely the tasks in StatusDone, since the finalisation writes
// both in one transaction.
//
// Returns domain.ErrNotFound if no such task exists.
func (r *Repository) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	const query = `
		SELECT u.id, u.pair, u.status, u.attempts, COALESCE(u.error, ''), u.created_at, u.updated_at,
		       q.update_id, q.pair, q.rate::text, q.rate_date, q.fetched_at
		  FROM quote_updates u
		  LEFT JOIN quotes q ON q.update_id = u.id
		 WHERE u.id = $1`

	var (
		d domain.TaskDetails
		// Null for every task that has not completed, hence the pointers.
		quoteID   *uuid.UUID
		quotePair *domain.Pair
		rawRate   *string
		rateDate  *time.Time
		fetchedAt *time.Time
	)

	err := r.pool.QueryRow(ctx, query, id).Scan(
		&d.Task.ID, &d.Task.Pair, &d.Task.Status, &d.Task.Attempts,
		&d.Task.Error, &d.Task.CreatedAt, &d.Task.UpdatedAt,
		&quoteID, &quotePair, &rawRate, &rateDate, &fetchedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TaskDetails{}, fmt.Errorf("get task %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.TaskDetails{}, fmt.Errorf("get task %s: %w", id, err)
	}

	if quoteID == nil {
		return d, nil
	}

	rate, err := decimal.NewFromString(*rawRate)
	if err != nil {
		return domain.TaskDetails{}, fmt.Errorf("get task %s: parse rate %q: %w", id, *rawRate, err)
	}

	d.Quote = &domain.Quote{
		UpdateID:  *quoteID,
		Pair:      *quotePair,
		Rate:      rate,
		RateDate:  *rateDate,
		FetchedAt: *fetchedAt,
	}

	return d, nil
}

// GetLatestQuote returns the most recently fetched rate for pair, whichever
// task produced it.
//
// Returns domain.ErrNotFound if the pair has never been quoted.
func (r *Repository) GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	// Ordered by fetched_at rather than rate_date: a weekend leaves several
	// rows sharing one rate_date, and their order would be undefined. Both the
	// filter and the ordering are served by quotes_pair_fetched_at_idx.
	const query = `
		SELECT update_id, pair, rate::text, rate_date, fetched_at
		  FROM quotes
		 WHERE pair = $1
		 ORDER BY fetched_at DESC
		 LIMIT 1`

	var (
		q       domain.Quote
		rawRate string
	)

	err := r.pool.QueryRow(ctx, query, pair).Scan(&q.UpdateID, &q.Pair, &rawRate, &q.RateDate, &q.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Quote{}, fmt.Errorf("get latest quote for %s: %w", pair, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Quote{}, fmt.Errorf("get latest quote for %s: %w", pair, err)
	}

	if q.Rate, err = decimal.NewFromString(rawRate); err != nil {
		return domain.Quote{}, fmt.Errorf("get latest quote for %s: parse rate %q: %w", pair, rawRate, err)
	}

	return q, nil
}

// ClaimTask takes the oldest waiting task, marks it in progress and returns it.
// The second result is false when there is nothing to do, which is the normal
// state of an idle worker rather than an error.
//
// Selecting and marking are one statement on purpose: read the row first and
// mark it second, and another worker reads it in between, so the provider is
// called twice for one task. SKIP LOCKED is what makes a pool of workers worth
// having -- without it they would queue up behind the same oldest row and run
// as one.
//
// The row lock lasts only until this statement's transaction commits, so from
// then on the only thing saying the task is taken is its status. That is why
// the recovery pass has to exist, and why CompleteTask and FailTask have to
// prove the claim they hold is still current.
func (r *Repository) ClaimTask(ctx context.Context) (domain.UpdateTask, bool, error) {
	// attempts is incremented here rather than on failure: it counts how many
	// times processing was started, which is what bounds the recovery loop, and
	// it identifies this particular claim to the finalisation below.
	const query = `
		UPDATE quote_updates
		   SET status     = 'in_progress',
		       attempts   = attempts + 1,
		       updated_at = now()
		 WHERE id = (
		       SELECT id
		         FROM quote_updates
		        WHERE status = 'pending'
		        ORDER BY created_at
		        LIMIT 1
		          FOR UPDATE SKIP LOCKED
		 )
		RETURNING id, pair, status, attempts, COALESCE(error, ''), created_at, updated_at`

	t, err := scanTask(r.pool.QueryRow(ctx, query))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UpdateTask{}, false, nil
	}
	if err != nil {
		return domain.UpdateTask{}, false, fmt.Errorf("claim task: %w", err)
	}

	return t, true, nil
}

// CompleteTask stores the fetched rate and closes the task in one transaction:
// a quote whose task is not finished would be served by GET /quotes/latest
// under a task that still claims to be running.
//
// claim must be the task as ClaimTask returned it. Its attempts value
// identifies that claim, and the statement matches on it, so a worker whose
// task was taken away by the recovery pass -- and possibly claimed by someone
// else since, which restores the status and leaves a status check alone
// useless -- changes nothing and gets domain.ErrStaleClaim. This match is what
// carries the guarantee; the staleness threshold only decides how often the
// case arises, since it weighs a budget measured by this process against wall
// time measured by the database.
//
// The task is closed before the quote is inserted, so that a lost claim leaves
// no row behind in quotes.
func (r *Repository) CompleteTask(
	ctx context.Context,
	claim domain.UpdateTask,
	rate decimal.Decimal,
	rateDate time.Time,
	fetchedAt time.Time,
) error {
	const closeTask = `
		UPDATE quote_updates
		   SET status     = 'done',
		       error      = NULL,
		       updated_at = now()
		 WHERE id = $1 AND status = 'in_progress' AND attempts = $2`

	// pair is copied from the claim, which is the only source of it here: the
	// denormalised column cannot drift from the task it belongs to. The rate
	// crosses as text, so no binary float is involved on the way into numeric.
	const insertQuote = `
		INSERT INTO quotes (update_id, pair, rate, rate_date, fetched_at)
		VALUES ($1, $2, $3::numeric, $4, $5)`

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("complete task %s: begin: %w", claim.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, closeTask, claim.ID, claim.Attempts)
	if err != nil {
		return fmt.Errorf("complete task %s: close task: %w", claim.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("complete task %s: %w", claim.ID, domain.ErrStaleClaim)
	}

	if _, err := tx.Exec(ctx, insertQuote, claim.ID, claim.Pair, rate.String(), rateDate, fetchedAt); err != nil {
		return fmt.Errorf("complete task %s: insert quote: %w", claim.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete task %s: commit: %w", claim.ID, err)
	}

	return nil
}

// FailTask closes the task as failed with reason, which is served to clients as
// is: it has to be a normalised message, never a raw provider error or an
// upstream URL. The caller owns that wording, since what a client reads is a
// decision of the layer above this one.
//
// claim carries the same proof of ownership as in CompleteTask, and a lost
// claim is reported the same way. Failing a task that is no longer ours would
// overwrite the state of whoever holds it now.
func (r *Repository) FailTask(ctx context.Context, claim domain.UpdateTask, reason string) error {
	const query = `
		UPDATE quote_updates
		   SET status     = 'failed',
		       error      = $3,
		       updated_at = now()
		 WHERE id = $1 AND status = 'in_progress' AND attempts = $2`

	// The schema requires a failed task to carry a reason; an empty string
	// satisfies that constraint while telling a client nothing.
	if reason == "" {
		return fmt.Errorf("fail task %s: reason is empty", claim.ID)
	}

	tag, err := r.pool.Exec(ctx, query, claim.ID, claim.Attempts, reason)
	if err != nil {
		return fmt.Errorf("fail task %s: %w", claim.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("fail task %s: %w", claim.ID, domain.ErrStaleClaim)
	}

	return nil
}

// ReleaseStuckTasks deals with tasks left in progress by a worker that died
// before finalising them: nothing releases those on their own, since the row
// lock disappeared along with the process. A task counts as stuck once it has
// been in progress for longer than olderThan.
//
// Tasks below maxAttempts go back to the queue; those that have reached it are
// closed as failed with reason, which is what stops a task that reliably kills
// its worker from cycling between claim and release forever. All three are
// settings rather than constants, and they arrive as arguments because the
// caller is the one holding the configuration: how long to wait, how many
// claims to allow and what to tell a client are not decisions for storage.
//
// The two counts are returned apart because they mean different things to
// whoever logs them. A steady trickle of releases says workers are dying or
// that the threshold is too tight; a failure says one task keeps killing
// whoever picks it up. A single total would hide both.
//
// The two statements share one transaction so that both partition the table at
// the same instant: now() is the transaction's start time, not the statement's.
// Age is measured by the database clock throughout -- a cutoff computed here
// would weigh the database's timestamps against this container's clock.
func (r *Repository) ReleaseStuckTasks(
	ctx context.Context,
	olderThan time.Duration,
	maxAttempts int,
	reason string,
) (released, failed int, err error) {
	// The cutoff is now() minus an interval built from $1 seconds. Written this
	// way the column stays bare on the left and the right side is one value for
	// the whole transaction, so the partial index on updated_at serves the
	// condition as a range. Turned around -- extracting each row's age and
	// comparing that to $1 -- it would be an expression over the column, and
	// every row would have to be computed.
	const releaseQuery = `
		UPDATE quote_updates
		   SET status     = 'pending',
		       updated_at = now()
		 WHERE status = 'in_progress'
		   AND attempts < $2
		   AND updated_at < now() - make_interval(secs => $1)`

	// attempts is left alone: it counts claims, and the next claim increments
	// it.
	const abandonQuery = `
		UPDATE quote_updates
		   SET status     = 'failed',
		       error      = $3,
		       updated_at = now()
		 WHERE status = 'in_progress'
		   AND attempts >= $2
		   AND updated_at < now() - make_interval(secs => $1)`

	if reason == "" {
		return 0, 0, errors.New("release stuck tasks: reason is empty")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("release stuck tasks: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	seconds := olderThan.Seconds()

	releaseTag, err := tx.Exec(ctx, releaseQuery, seconds, maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("release stuck tasks: release: %w", err)
	}

	abandonTag, err := tx.Exec(ctx, abandonQuery, seconds, maxAttempts, reason)
	if err != nil {
		return 0, 0, fmt.Errorf("release stuck tasks: abandon: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("release stuck tasks: commit: %w", err)
	}

	return int(releaseTag.RowsAffected()), int(abandonTag.RowsAffected()), nil
}

// scanTask reads the columns of quote_updates in the order the queries above
// list them.
func scanTask(row pgx.Row) (domain.UpdateTask, error) {
	var t domain.UpdateTask

	if err := row.Scan(&t.ID, &t.Pair, &t.Status, &t.Attempts, &t.Error, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return domain.UpdateTask{}, err
	}

	return t, nil
}
