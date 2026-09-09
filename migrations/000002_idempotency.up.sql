-- Deduplication of unfinished work: at most one task per pair may be waiting or
-- running at any one moment. The predicate covers both unfinished statuses
-- rather than 'pending' alone -- a task already claimed has to deduplicate too,
-- or a client posting during the claim would be handed a second update of the
-- same pair.
--
-- Terminal rows fall outside the predicate, so accumulated history never blocks
-- a new task and the index stays as small as the queue. Nothing but an insert
-- can violate it: of the six statements that move a task between statuses,
-- three travel inside this set (a claim, and the two that return a task to
-- 'pending') and three leave it, while none enters -- terminal statuses are
-- terminal and nothing reopens a task. The one door in is the insert, which
-- resolves the conflict itself.
CREATE UNIQUE INDEX quote_updates_unfinished_pair_idx
    ON quote_updates (pair)
    WHERE status IN ('pending', 'in_progress');

-- Receipts for POST /quotes/updates: the task a client's Idempotency-Key was
-- answered with, so that a repeat of the same request is answered the same way
-- instead of queueing a second update of the pair.
--
-- A table of its own rather than a column on quote_updates, because several
-- keys can point at one task: a post that deduplicates onto an unfinished task
-- binds its own key to that same row, and a column would hold only the first.
CREATE TABLE idempotency_keys (
    -- The value the client sent. The API requires it to be a UUID, which is
    -- also what bounds the size of something arriving in a header. This
    -- constraint is the arbiter of concurrent posts sharing a key: they are
    -- serialised here rather than by anything the service does.
    key        uuid        PRIMARY KEY,
    update_id  uuid        NOT NULL REFERENCES quote_updates (id),
    -- When the binding was made, which is not the task's own created_at: a post
    -- that deduplicates binds its key to a task created before it, and the
    -- lifetime has to be measured from the binding. Every read of a key weighs
    -- this against the lifetime, and that is the whole of the enforcement:
    -- nothing deletes an expired row, a lookup stops seeing it and the next
    -- post carrying that key takes it over.
    --
    -- Deliberately unindexed. It is only ever read beside key, which is the
    -- primary key here, so the row is found before the age is looked at, while
    -- an index would cost on every insert -- the hot path of a post.
    created_at timestamptz NOT NULL DEFAULT now()
);
