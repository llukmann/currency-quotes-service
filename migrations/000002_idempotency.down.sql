-- The receipts first: they reference quote_updates, which this migration leaves
-- alone.
DROP TABLE IF EXISTS idempotency_keys;
DROP INDEX IF EXISTS quote_updates_unfinished_pair_idx;
