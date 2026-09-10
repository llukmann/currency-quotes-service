-- Driven to a terminal status by the background worker, never by the handler.
CREATE TABLE quote_updates (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    pair       text        NOT NULL CHECK (pair ~ '^[A-Z]{3}/[A-Z]{3}$'),
    status     text        NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'in_progress', 'done', 'failed')),
    -- Incremented when the task is claimed, so it counts how many times
    -- processing was started. It bounds the recovery loop: without it a task
    -- that reliably kills its worker would be released back forever.
    attempts   smallint    NOT NULL DEFAULT 0,
    -- Served to clients, so raw provider errors and upstream URLs must not
    -- reach it.
    error      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Maintained by the service, not by the schema: every statement that
    -- changes status must also set updated_at = now(), and the default below
    -- covers INSERT only. The value has to come from now(), since staleness is
    -- measured against the database clock; for 'in_progress' rows it is
    -- therefore the claim time.
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Stated in one direction only, which leaves .error. free to hold a reason
    -- on a task that has not reached a terminal status.
    CONSTRAINT quote_updates_failed_has_reason
        CHECK (status <> 'failed' OR error IS NOT NULL)
);

-- Claim path. Partial, so it stays small however much finished history the
-- table accumulates.
CREATE INDEX quote_updates_pending_idx
    ON quote_updates (created_at)
    WHERE status = 'pending';

-- Recovery path: tasks left behind by a worker that died between the claim
-- commit and the finalisation. Nothing releases those rows on their own -- the
-- row lock was already gone when the process disappeared.
CREATE INDEX quote_updates_in_progress_idx
    ON quote_updates (updated_at)
    WHERE status = 'in_progress';

-- 'pair' is a denormalised copy of quote_updates.pair. Without it the latest
-- quote lookup would filter on one table and sort on the other, which no single
-- index can serve. The copies cannot drift: both are immutable after insert and
-- are written from the same value by the finalisation, which is this table's
-- only writer.
CREATE TABLE quotes (
    update_id  uuid           PRIMARY KEY REFERENCES quote_updates (id),
    pair       text           NOT NULL,
    rate       numeric(20,10) NOT NULL CHECK (rate > 0),
    -- Reference rates are published on working days only, so this can lag
    -- fetched_at by several days; a date because the provider exposes no
    -- publication instant at all.
    rate_date  date           NOT NULL,
    -- Generated at the moment the provider call returned, not by now(), which
    -- would be the start of the transaction that opens afterwards.
    fetched_at timestamptz    NOT NULL
);

-- Serves GET /quotes/latest: the predicate and the ordering both come from this
-- index, so the answer is its first entry regardless of history size.
CREATE INDEX quotes_pair_fetched_at_idx
    ON quotes (pair, fetched_at DESC);
