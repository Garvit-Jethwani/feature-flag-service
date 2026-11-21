// Package store persists flags in PostgreSQL. Every query is parameterized.
//
// Every write runs in one transaction that locks the flag, applies the
// change, and inserts the audit entry, so a change and its audit entry can
// never disagree. Writes also report which flags' cached evaluation results
// are now out of date, for the caller to invalidate after the commit.
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

// querier is the part of pgx shared by a connection pool and a transaction,
// so the same helper can run inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

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

// scanFlag reads one row selected with flagColumns into a Flag.
func scanFlag(row pgx.Row) (flag.Flag, error) {
	var f flag.Flag
	err := row.Scan(&f.Name, &f.Description, &f.Enabled, &f.RolloutPercentage,
		&f.CreatedAt, &f.UpdatedAt, &f.Prerequisites)
	return f, err
}

// hasCode reports whether err is a Postgres error with the given code, such
// as a unique or foreign key violation.
func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// lockFlag reads a flag inside a transaction and locks its row until the
// transaction ends. Concurrent changes to the same flag therefore run one
// after another, and each sees the result of the one before it. It returns
// flag.ErrNotFound if the flag does not exist.
func lockFlag(ctx context.Context, tx pgx.Tx, name string) (flag.Flag, error) {
	const query = `SELECT ` + flagColumns + ` FROM flags f WHERE f.name = $1 FOR UPDATE OF f`
	f, err := scanFlag(tx.QueryRow(ctx, query, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return flag.Flag{}, flag.ErrNotFound
	}
	if err != nil {
		return flag.Flag{}, fmt.Errorf("lock flag: %w", err)
	}
	return f, nil
}

// affectedFlags returns the flag itself plus every flag that requires it,
// directly or through other flags. These are the flags whose cached
// evaluation results a change to this flag can alter. The depth bound keeps
// the query finite even if the stored data contained a cycle.
func affectedFlags(ctx context.Context, q querier, name string) ([]string, error) {
	const query = `
		WITH RECURSIVE dependents (name, depth) AS (
			SELECT p.flag_name, 1 FROM flag_prerequisites p WHERE p.requires_flag = $1
			UNION
			SELECT p.flag_name, d.depth + 1
			FROM dependents d
			JOIN flag_prerequisites p ON p.requires_flag = d.name
			WHERE d.depth < $2
		)
		SELECT DISTINCT name FROM dependents`
	rows, err := q.Query(ctx, query, name, flag.MaxPrerequisiteDepth+1)
	if err != nil {
		return nil, fmt.Errorf("select dependent flags: %w", err)
	}
	dependents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read dependent flags: %w", err)
	}
	return append([]string{name}, dependents...), nil
}

// CreateFlag inserts a flag and its audit entry. It returns
// flag.ErrAlreadyExists if the name is taken. Nothing needs invalidating:
// no result is ever cached for a flag that does not exist.
func (s *Store) CreateFlag(ctx context.Context, actor, name, description string, enabled bool, rolloutPercentage int) (flag.Flag, error) {
	const query = `
		INSERT INTO flags AS f (name, description, enabled, rollout_percentage)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + flagColumns

	var created flag.Flag
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		f, err := scanFlag(tx.QueryRow(ctx, query, name, description, enabled, rolloutPercentage))
		if hasCode(err, pgUniqueViolation) {
			return flag.ErrAlreadyExists
		}
		if err != nil {
			return fmt.Errorf("insert flag: %w", err)
		}
		created = f
		return insertAudit(ctx, tx, actor, name, flag.ActionFlagCreated, nil, f.State())
	})
	if err != nil {
		return flag.Flag{}, err
	}
	return created, nil
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
	if f.Overrides, err = listOverrides(ctx, s.pool, name); err != nil {
		return flag.Flag{}, err
	}
	return f, nil
}

// listOverrides returns every per-user override of a flag, ordered by user.
func listOverrides(ctx context.Context, q querier, name string) ([]flag.Override, error) {
	const query = `
		SELECT user_id, enabled, updated_at
		FROM flag_overrides
		WHERE flag_name = $1
		ORDER BY user_id`
	rows, err := q.Query(ctx, query, name)
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

// UpdateFlag applies a partial update and returns the flag with its
// overrides, plus the flags whose cached results must be invalidated.
//
// If the patch would change nothing, the flag is returned as it is, no audit
// entry is written, and no flags are reported for invalidation: the audit
// trail records state changes, and there was none. It returns
// flag.ErrNotFound if the flag does not exist. A prerequisite change can
// also fail with a validation error (a required flag does not exist),
// flag.ErrDependencyCycle, or flag.ErrDependencyTooDeep; nothing is saved
// in those cases.
func (s *Store) UpdateFlag(ctx context.Context, actor, name string, patch flag.Patch) (flag.Flag, []string, error) {
	const query = `
		UPDATE flags AS f
		SET description = $2, enabled = $3, rollout_percentage = $4, updated_at = now()
		WHERE f.name = $1
		RETURNING ` + flagColumns

	var updated flag.Flag
	var affected []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Taken before the row lock so that two dependency edits never
		// wait on each other's rows; see lockDependencyEdits.
		if patch.Prerequisites != nil {
			if err := lockDependencyEdits(ctx, tx); err != nil {
				return err
			}
		}
		previous, err := lockFlag(ctx, tx, name)
		if err != nil {
			return err
		}

		// Work out the values after the patch; a nil field keeps its value.
		description, enabled, rollout := previous.Description, previous.Enabled, previous.RolloutPercentage
		if patch.Description != nil {
			description = *patch.Description
		}
		if patch.Enabled != nil {
			enabled = *patch.Enabled
		}
		if patch.RolloutPercentage != nil {
			rollout = *patch.RolloutPercentage
		}
		prerequisitesChange := patch.Prerequisites != nil && !sameNames(previous.Prerequisites, *patch.Prerequisites)

		updated = previous
		if description != previous.Description || enabled != previous.Enabled ||
			rollout != previous.RolloutPercentage || prerequisitesChange {
			// Prerequisites are written first so that the UPDATE below
			// returns the flag with its new list.
			if prerequisitesChange {
				if err := replacePrerequisites(ctx, tx, name, *patch.Prerequisites); err != nil {
					return err
				}
			}
			updated, err = scanFlag(tx.QueryRow(ctx, query, name, description, enabled, rollout))
			if err != nil {
				return fmt.Errorf("update flag: %w", err)
			}
			if err := insertAudit(ctx, tx, actor, name, flag.ActionFlagUpdated, previous.State(), updated.State()); err != nil {
				return err
			}
			if affected, err = affectedFlags(ctx, tx, name); err != nil {
				return err
			}
		}

		updated.Overrides, err = listOverrides(ctx, tx, name)
		return err
	})
	if err != nil {
		return flag.Flag{}, nil, err
	}
	return updated, affected, nil
}

// DeleteFlag removes a flag and its overrides, records the deletion, and
// returns the flags whose cached results must be invalidated. It returns
// flag.ErrNotFound if the flag does not exist and flag.ErrHasDependents if
// other flags require it.
func (s *Store) DeleteFlag(ctx context.Context, actor, name string) ([]string, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		previous, err := lockFlag(ctx, tx, name)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM flags WHERE name = $1`, name)
		if hasCode(err, pgForeignKeyViolation) {
			return flag.ErrHasDependents
		}
		if err != nil {
			return fmt.Errorf("delete flag: %w", err)
		}
		return insertAudit(ctx, tx, actor, name, flag.ActionFlagDeleted, previous.State(), nil)
	})
	if err != nil {
		return nil, err
	}
	// A flag can only be deleted when nothing requires it, so only its own
	// cached results are affected.
	return []string{name}, nil
}

// SetOverride creates or replaces a user's override, records the change,
// and returns the flags whose cached results must be invalidated. Setting an
// override to the value it already has changes nothing and is not audited.
// It returns flag.ErrNotFound if the flag does not exist.
func (s *Store) SetOverride(ctx context.Context, actor, name, userID string, enabled bool) (flag.Override, []string, error) {
	const selectCurrent = `
		SELECT user_id, enabled, updated_at
		FROM flag_overrides
		WHERE flag_name = $1 AND user_id = $2`
	const upsert = `
		INSERT INTO flag_overrides (flag_name, user_id, enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (flag_name, user_id)
		DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()
		RETURNING user_id, enabled, updated_at`

	var override flag.Override
	var affected []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockFlag(ctx, tx, name); err != nil {
			return err
		}

		// previous stays nil when the user had no override before.
		var previous any
		var current flag.Override
		err := tx.QueryRow(ctx, selectCurrent, name, userID).Scan(&current.UserID, &current.Enabled, &current.UpdatedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("select override: %w", err)
		case current.Enabled == enabled:
			override = current
			return nil // nothing changes
		default:
			previous = flag.OverrideState{UserID: userID, Enabled: current.Enabled}
		}

		err = tx.QueryRow(ctx, upsert, name, userID, enabled).Scan(&override.UserID, &override.Enabled, &override.UpdatedAt)
		if err != nil {
			return fmt.Errorf("upsert override: %w", err)
		}
		next := flag.OverrideState{UserID: userID, Enabled: enabled}
		if err := insertAudit(ctx, tx, actor, name, flag.ActionOverrideSet, previous, next); err != nil {
			return err
		}
		affected, err = affectedFlags(ctx, tx, name)
		return err
	})
	if err != nil {
		return flag.Override{}, nil, err
	}
	return override, affected, nil
}

// DeleteOverride removes a user's override, records the change, and returns
// the flags whose cached results must be invalidated. It returns
// flag.ErrNotFound if the flag does not exist and flag.ErrOverrideNotFound
// if the flag exists but has no override for the user.
func (s *Store) DeleteOverride(ctx context.Context, actor, name, userID string) ([]string, error) {
	const query = `
		DELETE FROM flag_overrides
		WHERE flag_name = $1 AND user_id = $2
		RETURNING enabled`

	var affected []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockFlag(ctx, tx, name); err != nil {
			return err
		}
		var wasEnabled bool
		err := tx.QueryRow(ctx, query, name, userID).Scan(&wasEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return flag.ErrOverrideNotFound
		}
		if err != nil {
			return fmt.Errorf("delete override: %w", err)
		}
		previous := flag.OverrideState{UserID: userID, Enabled: wasEnabled}
		if err := insertAudit(ctx, tx, actor, name, flag.ActionOverrideRemoved, previous, nil); err != nil {
			return err
		}
		affected, err = affectedFlags(ctx, tx, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return affected, nil
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
		var override *bool // nil when the user has no override on this flag
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
