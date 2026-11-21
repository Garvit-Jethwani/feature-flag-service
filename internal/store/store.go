// Package store persists flags in PostgreSQL. Every query is parameterized.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// Postgres error codes handled explicitly.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// flagColumns is the fixed column list read for a flag aliased as f. It is a
// compile-time constant, not built from input.
const flagColumns = `
	f.name, f.description, f.enabled, f.rollout_percentage, f.created_at, f.updated_at,
	ARRAY(
		SELECT p.requires_flag FROM flag_prerequisites p
		WHERE p.flag_name = f.name ORDER BY p.requires_flag
	)`

// Store reads and writes flags.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func scanFlag(row pgx.Row) (flag.Flag, error) {
	var f flag.Flag
	err := row.Scan(&f.Name, &f.Description, &f.Enabled, &f.RolloutPercentage,
		&f.CreatedAt, &f.UpdatedAt, &f.Prerequisites)
	return f, err
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// CreateFlag inserts a flag. It returns flag.ErrAlreadyExists if the name is
// taken.
func (s *Store) CreateFlag(ctx context.Context, name, description string, enabled bool) (flag.Flag, error) {
	const query = `
		INSERT INTO flags AS f (name, description, enabled)
		VALUES ($1, $2, $3)
		RETURNING ` + flagColumns
	f, err := scanFlag(s.pool.QueryRow(ctx, query, name, description, enabled))
	if hasCode(err, pgUniqueViolation) {
		return flag.Flag{}, flag.ErrAlreadyExists
	}
	if err != nil {
		return flag.Flag{}, fmt.Errorf("insert flag: %w", err)
	}
	return f, nil
}

// GetFlag returns a flag with its overrides and prerequisites, or
// flag.ErrNotFound.
func (s *Store) GetFlag(ctx context.Context, name string) (flag.Flag, error) {
	const query = `SELECT ` + flagColumns + ` FROM flags f WHERE f.name = $1`
	f, err := scanFlag(s.pool.QueryRow(ctx, query, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return flag.Flag{}, flag.ErrNotFound
	}
	if err != nil {
		return flag.Flag{}, fmt.Errorf("select flag: %w", err)
	}
	if f.Overrides, err = s.listOverrides(ctx, name); err != nil {
		return flag.Flag{}, err
	}
	return f, nil
}

func (s *Store) listOverrides(ctx context.Context, name string) ([]flag.Override, error) {
	const query = `
		SELECT user_id, enabled, updated_at
		FROM flag_overrides
		WHERE flag_name = $1
		ORDER BY user_id`
	rows, err := s.pool.Query(ctx, query, name)
	if err != nil {
		return nil, fmt.Errorf("select overrides: %w", err)
	}
	overrides, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (flag.Override, error) {
		var o flag.Override
		err := row.Scan(&o.UserID, &o.Enabled, &o.UpdatedAt)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	return overrides, nil
}

// ListFlags returns up to limit flags whose names sort after the given name,
// in name order. Overrides are not loaded.
func (s *Store) ListFlags(ctx context.Context, after string, limit int) ([]flag.Flag, error) {
	const query = `
		SELECT ` + flagColumns + `
		FROM flags f
		WHERE f.name > $1
		ORDER BY f.name
		LIMIT $2`
	rows, err := s.pool.Query(ctx, query, after, limit)
	if err != nil {
		return nil, fmt.Errorf("select flags: %w", err)
	}
	flags, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (flag.Flag, error) {
		return scanFlag(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read flags: %w", err)
	}
	return flags, nil
}

// UpdateFlag applies a partial update and returns the updated flag with its
// overrides, or flag.ErrNotFound.
func (s *Store) UpdateFlag(ctx context.Context, name string, patch flag.Patch) (flag.Flag, error) {
	const query = `
		UPDATE flags AS f
		SET description = COALESCE($2, f.description),
		    enabled     = COALESCE($3, f.enabled),
		    updated_at  = now()
		WHERE f.name = $1
		RETURNING ` + flagColumns
	f, err := scanFlag(s.pool.QueryRow(ctx, query, name, patch.Description, patch.Enabled))
	if errors.Is(err, pgx.ErrNoRows) {
		return flag.Flag{}, flag.ErrNotFound
	}
	if err != nil {
		return flag.Flag{}, fmt.Errorf("update flag: %w", err)
	}
	if f.Overrides, err = s.listOverrides(ctx, name); err != nil {
		return flag.Flag{}, err
	}
	return f, nil
}

// DeleteFlag removes a flag and its overrides. It returns flag.ErrNotFound
// if the flag does not exist and flag.ErrHasDependents if other flags
// require it.
func (s *Store) DeleteFlag(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM flags WHERE name = $1`, name)
	if hasCode(err, pgForeignKeyViolation) {
		return flag.ErrHasDependents
	}
	if err != nil {
		return fmt.Errorf("delete flag: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return flag.ErrNotFound
	}
	return nil
}

// SetOverride creates or replaces a user's override. It returns
// flag.ErrNotFound if the flag does not exist.
func (s *Store) SetOverride(ctx context.Context, name, userID string, enabled bool) (flag.Override, error) {
	const query = `
		INSERT INTO flag_overrides (flag_name, user_id, enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (flag_name, user_id)
		DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()
		RETURNING user_id, enabled, updated_at`
	var o flag.Override
	err := s.pool.QueryRow(ctx, query, name, userID, enabled).Scan(&o.UserID, &o.Enabled, &o.UpdatedAt)
	if hasCode(err, pgForeignKeyViolation) {
		return flag.Override{}, flag.ErrNotFound
	}
	if err != nil {
		return flag.Override{}, fmt.Errorf("upsert override: %w", err)
	}
	return o, nil
}

// DeleteOverride removes a user's override. It returns flag.ErrNotFound if
// the flag does not exist and flag.ErrOverrideNotFound if the flag exists
// but has no override for the user.
func (s *Store) DeleteOverride(ctx context.Context, name, userID string) error {
	const query = `DELETE FROM flag_overrides WHERE flag_name = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, query, name, userID)
	if err != nil {
		return fmt.Errorf("delete override: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	var exists bool
	const flagExists = `SELECT EXISTS (SELECT 1 FROM flags WHERE name = $1)`
	if err := s.pool.QueryRow(ctx, flagExists, name).Scan(&exists); err != nil {
		return fmt.Errorf("check flag exists: %w", err)
	}
	if !exists {
		return flag.ErrNotFound
	}
	return flag.ErrOverrideNotFound
}

// EvaluationSnapshot loads, in one query, what is needed to evaluate a flag
// for a user: the flag, its transitive prerequisites, and the user's
// overrides on any of them. It returns flag.ErrNotFound if the flag does not
// exist.
func (s *Store) EvaluationSnapshot(ctx context.Context, name, userID string) (flag.Snapshot, error) {
	// The depth bound keeps the recursion finite even if the stored data
	// contained a cycle. It is one past the evaluator's limit so that the
	// evaluator, not a truncated snapshot, reports a chain that is too deep.
	const query = `
		WITH RECURSIVE needed (name, depth) AS (
			SELECT f.name, 0 FROM flags f WHERE f.name = $1
			UNION
			SELECT p.requires_flag, n.depth + 1
			FROM needed n
			JOIN flag_prerequisites p ON p.flag_name = n.name
			WHERE n.depth < $3
		)
		SELECT f.name, f.enabled, f.rollout_percentage,
			ARRAY(
				SELECT p.requires_flag FROM flag_prerequisites p
				WHERE p.flag_name = f.name ORDER BY p.requires_flag
			),
			o.enabled
		FROM flags f
		LEFT JOIN flag_overrides o ON o.flag_name = f.name AND o.user_id = $2
		WHERE f.name IN (SELECT name FROM needed)`
	rows, err := s.pool.Query(ctx, query, name, userID, flag.MaxPrerequisiteDepth+1)
	if err != nil {
		return flag.Snapshot{}, fmt.Errorf("select evaluation snapshot: %w", err)
	}
	defer rows.Close()

	snap := flag.Snapshot{Flags: map[string]flag.Flag{}, Overrides: map[string]bool{}}
	for rows.Next() {
		var f flag.Flag
		var override *bool
		if err := rows.Scan(&f.Name, &f.Enabled, &f.RolloutPercentage, &f.Prerequisites, &override); err != nil {
			return flag.Snapshot{}, fmt.Errorf("read evaluation snapshot: %w", err)
		}
		snap.Flags[f.Name] = f
		if override != nil {
			snap.Overrides[f.Name] = *override
		}
	}
	if err := rows.Err(); err != nil {
		return flag.Snapshot{}, fmt.Errorf("read evaluation snapshot: %w", err)
	}
	if _, ok := snap.Flags[name]; !ok {
		return flag.Snapshot{}, flag.ErrNotFound
	}
	return snap, nil
}
