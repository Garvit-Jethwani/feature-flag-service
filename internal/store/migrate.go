package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the Postgres advisory lock key that serialises
// migrations when several instances start at once. The value is arbitrary.
const migrationLockID int64 = 7061743901

// Migrate applies every embedded migration that has not been applied yet, in
// file name order. Each migration runs in its own transaction together with
// the record that it was applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migrations: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// A fresh context, so the lock is released even if ctx has ended.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	const createTable = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`
	if _, err := conn.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, entry := range entries { // ReadDir returns entries sorted by name
		version := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(version, ".sql") {
			continue
		}

		var applied bool
		const isApplied = `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`
		if err := conn.QueryRow(ctx, isApplied, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}

		statements, err := migrationFiles.ReadFile("migrations/" + version)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// Migration files are static SQL compiled into the binary; they
			// take no parameters and contain no external input.
			if _, err := tx.Exec(ctx, string(statements)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
	}
	return nil
}
