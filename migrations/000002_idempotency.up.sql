-- At most one task per pair may be waiting or running at any one moment. The
-- predicate covers both unfinished statuses rather than 'pending' alone: a task
-- already claimed has to deduplicate too, or a client posting during the claim
-- would be handed a second update of the same pair.
--
-- Terminal rows fall outside the predicate, so accumulated history never blocks
-- a new task, and only an insert can violate it: nothing reopens a task.
CREATE UNIQUE INDEX quote_updates_unfinished_pair_idx
    ON quote_updates (pair)
    WHERE status IN ('pending', 'in_progress');

-- Receipts for POST /quotes/updates: the task a client's Idempotency-Key was
-- answered with. A table rather than a column on quote_updates because several
-- keys can point at one task -- a post that deduplicates onto an unfinished
-- task binds its own key to that same row.
CREATE TABLE idempotency_keys (
    -- The API requires a UUID, which is also what bounds the size of something
    -- arriving in a header. This constraint is the arbiter of concurrent posts
    -- sharing a key: they are serialised here rather than by anything the
    -- service does.
    key        uuid        PRIMARY KEY,
    update_id  uuid        NOT NULL REFERENCES quote_updates (id),
    -- When the binding was made, which is not the task's own created_at: a post
    -- that deduplicates binds its key to a task created before it. Every read
    -- weighs this against the lifetime, and that is the whole of the
    -- enforcement: nothing deletes an expired row, a lookup stops seeing it and
    -- the next post carrying that key takes it over.
    --
    -- Deliberately unindexed. It is only ever read beside key, which is the
    -- primary key here, while an index would cost on every insert -- the hot
    -- path of a post.
    created_at timestamptz NOT NULL DEFAULT now()
);
