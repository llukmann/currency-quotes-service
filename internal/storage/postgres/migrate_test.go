package postgres

import (
	"context"
	"log/slog"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestMigrateReportsAnUnreachableDatabase covers the first thing that can go
// wrong at startup, and the only test here that needs no database of its own:
// nothing listens on the port below.
//
// sql.Open is lazy, so the failure has to come from the ping that follows it.
// Without that ping the error would surface later, from the migration driver,
// with a message about the schema rather than about the connection.
func TestMigrateReportsAnUnreachableDatabase(t *testing.T) {
	const nowhere = "postgres://quotes:quotes@127.0.0.1:1/quotes?sslmode=disable"

	err := Migrate(t.Context(), nowhere, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	require.ErrorContains(t, err, "connect to database")
}

// TestMigrateIsIdempotent checks the ordinary case rather than an exceptional
// one: the service runs this on every start, and in compose it starts again
// whenever the container is recreated. A schema already at the latest version
// has to be a normal outcome, not an error that keeps the process from coming
// up.
func TestMigrateIsIdempotent(t *testing.T) {
	requireDatabase(t)

	logger := slog.New(slog.DiscardHandler)

	// TestMain has already migrated once, so both of these find nothing to do.
	require.NoError(t, Migrate(t.Context(), dsn, logger))
	require.NoError(t, Migrate(t.Context(), dsn, logger))
}

// TestMigrateRefusesADirtySchema covers the one failure the code rewrites in
// its own words. A schema left dirty by a migration that failed part way is in
// an unknown state, and golang-migrate says so with "Fix and force version.",
// which assumes the reader already knows the tool. Every start would repeat
// that line, so it is replaced with what happened and what to do about it.
//
// It runs against a database of its own rather than the shared one. A dirty
// mark is exactly what stops this package from starting, so a test that set it
// on the shared schema and then died -- interrupted, panicking, killed --
// would leave every later run refusing to begin, with a message about a
// migration that never failed.
func TestMigrateRefusesADirtySchema(t *testing.T) {
	requireDatabase(t)

	scratch := scratchDatabase(t, "quotes_test_dirty")
	logger := slog.New(slog.DiscardHandler)

	require.NoError(t, Migrate(t.Context(), scratch, logger))

	scratchPool, err := pgxpool.New(t.Context(), scratch)
	require.NoError(t, err)

	_, err = scratchPool.Exec(t.Context(), `UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)
	scratchPool.Close()

	err = Migrate(t.Context(), scratch, logger)

	require.Error(t, err)
	require.ErrorContains(t, err, "dirty")
	// The way out, named in the message rather than left to the reader.
	require.ErrorContains(t, err, "docker compose down -v")
}

// scratchDatabase creates an empty database beside the test one and returns a
// connection string for it, dropping it again when the test ends.
//
// Dropped first as well as last: a run that died before its cleanup leaves the
// database behind, and the next one has to start from an empty schema rather
// than from whatever that run made of it.
func scratchDatabase(t *testing.T, name string) string {
	t.Helper()

	drop := func(ctx context.Context) {
		_, err := pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		require.NoError(t, err)
	}

	drop(t.Context())

	_, err := pool.Exec(t.Context(), `CREATE DATABASE `+name)
	require.NoError(t, err)

	// A context of its own: the test's is already cancelled by the time a
	// cleanup runs.
	t.Cleanup(func() { drop(context.Background()) })

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)

	parsed.Path = "/" + name

	return parsed.String()
}

// requireDatabase skips a test that cannot run without one. Separate from
// newRepo, which also empties the tables: these tests are about the schema
// rather than about what is in it.
func requireDatabase(t *testing.T) {
	t.Helper()

	if pool == nil {
		t.Skipf("%s is not set", testDatabaseURL)
	}
}
