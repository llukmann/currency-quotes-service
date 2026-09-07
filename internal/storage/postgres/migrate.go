// Package postgres holds everything that talks to PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Registers the "pgx" driver used by sql.Open below.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/llukmann/currency-quotes-service/migrations"
)

// How long to wait for the database to answer before giving up on startup.
// Compose already gates the service on the postgres healthcheck, so this only
// has to cover an unreachable or misconfigured host, not a slow boot.
const pingTimeout = 10 * time.Second

// Migrate applies every pending migration and returns once the schema is up to
// date. It opens a connection of its own and closes it before returning: no
// other code needs the database yet.
//
// Concurrent instances are safe -- the driver takes an advisory lock for the
// duration of the run.
func Migrate(ctx context.Context, databaseURL string, logger *slog.Logger) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Warn("closing migration connection", slog.Any("error", err))
		}
	}()

	// sql.Open is lazy, so this is the first call that actually reaches the
	// server and the only one that honours the caller's context: migrate.Up
	// below takes no context in golang-migrate v4.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("init migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}

	// m is deliberately not closed: closing it would close the *sql.DB that the
	// deferred call above already owns, and the embedded source holds nothing to
	// release.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		// Up refuses to run against a schema left dirty by an earlier failed
		// attempt, and its own message ("Fix and force version.") assumes the
		// reader already knows the tool. Every start would repeat it, so say
		// what happened and what to do about it instead.
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return fmt.Errorf(
				"schema is marked dirty at version %d: an earlier migration failed part way "+
					"and the schema is in an unknown state; reset the database with "+
					"'docker compose down -v', or inspect it and force the version once it matches: %w",
				dirty.Version, err)
		}

		return fmt.Errorf("apply migrations: %w", err)
	}

	version, _, err := m.Version()
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	logger.Info("migrations applied", slog.Uint64("version", uint64(version)))

	return nil
}
