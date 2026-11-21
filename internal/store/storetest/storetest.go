// Package storetest gives database-backed tests an isolated, migrated
// Postgres schema.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/store"
)

// NewPool returns a pool whose connections use a fresh schema with all
// migrations applied. The schema is dropped when the test ends.
//
// The database comes from TEST_DATABASE_URL. When it is unset the test is
// skipped, unless CI is set, in which case the test fails so that CI can
// never silently skip database coverage.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is not set; database-backed tests must run in CI")
		}
		t.Skip("TEST_DATABASE_URL is not set; skipping database-backed test (run `make test`)")
	}

	ctx := context.Background()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	// Schema names cannot be query parameters. This one is generated here,
	// not taken from input, and is quoted by pgx.
	schema := pgx.Identifier{"test_" + hex.EncodeToString(suffix)}.Sanitize()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		_ = admin.Close(ctx)
	})

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return pool
}
