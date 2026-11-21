package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// dependencyLockKey identifies the Postgres advisory lock that serializes
// prerequisite changes. The number itself has no meaning; it only has to
// differ from the migration lock.
const dependencyLockKey int64 = 7411002

// lockDependencyEdits makes prerequisite changes run one at a time, until
// the transaction ends.
//
// The cycle check below looks at the whole dependency graph. Without this
// lock, "A requires B" and "B requires A" submitted at the same moment
// could each pass the check (neither sees the other's uncommitted row) and
// together form a cycle. Dependency edits are rare admin actions, so one
// global lock costs nothing in practice.
func lockDependencyEdits(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, dependencyLockKey); err != nil {
		return fmt.Errorf("lock dependency edits: %w", err)
	}
	return nil
}

// sameNames reports whether two lists hold the same names, in any order.
func sameNames(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// replacePrerequisites sets the full list of flags that the flag called
// name requires, then checks the resulting graph. The caller holds the
// dependency lock and the flag's row lock. If it returns an error the
// caller's transaction must roll back, which undoes the change.
func replacePrerequisites(ctx context.Context, tx pgx.Tx, name string, prerequisites []string) error {
	// Report a missing flag by name. The foreign key would catch it too,
	// but only with a generic error.
	const missingQuery = `
		SELECT n FROM unnest($1::text[]) AS n
		WHERE NOT EXISTS (SELECT 1 FROM flags f WHERE f.name = n)
		ORDER BY n LIMIT 1`
	var missing string
	err := tx.QueryRow(ctx, missingQuery, prerequisites).Scan(&missing)
	switch {
	case err == nil:
		return &flag.ValidationError{Field: "prerequisites", Message: fmt.Sprintf("flag %q does not exist", missing)}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("check prerequisites exist: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM flag_prerequisites WHERE flag_name = $1`, name); err != nil {
		return fmt.Errorf("delete prerequisites: %w", err)
	}
	const insert = `
		INSERT INTO flag_prerequisites (flag_name, requires_flag)
		SELECT $1, unnest($2::text[])`
	_, err = tx.Exec(ctx, insert, name, prerequisites)
	if hasCode(err, pgForeignKeyViolation) {
		// A required flag was deleted between the check above and here.
		return &flag.ValidationError{Field: "prerequisites", Message: "every prerequisite must be an existing flag"}
	}
	if err != nil {
		return fmt.Errorf("insert prerequisites: %w", err)
	}
	return checkDependencyGraph(ctx, tx, name)
}

// checkDependencyGraph verifies the graph around a flag after its
// prerequisites changed. It returns flag.ErrDependencyCycle if the flag now
// requires itself through any path, and flag.ErrDependencyTooDeep if the
// longest chain passing through the flag has more levels than the evaluator
// follows.
func checkDependencyGraph(ctx context.Context, tx pgx.Tx, name string) error {
	// "down" walks from the flag to everything it requires; "up" walks to
	// everything that requires it. Each row carries how many steps away it
	// is. The depth bound keeps both walks finite even inside a cycle.
	//
	//   - Cycle: the downward walk comes back to the flag itself.
	//   - Longest chain through the flag: deepest step down plus deepest
	//     step up. Chains that do not pass through this flag did not change.
	const query = `
		WITH RECURSIVE down (name, depth) AS (
			SELECT $1::text, 0
			UNION
			SELECT p.requires_flag, d.depth + 1
			FROM down d
			JOIN flag_prerequisites p ON p.flag_name = d.name
			WHERE d.depth < $2
		), up (name, depth) AS (
			SELECT $1::text, 0
			UNION
			SELECT p.flag_name, u.depth + 1
			FROM up u
			JOIN flag_prerequisites p ON p.requires_flag = u.name
			WHERE u.depth < $2
		)
		SELECT
			EXISTS (SELECT 1 FROM down WHERE name = $1 AND depth > 0),
			(SELECT max(depth) FROM down) + (SELECT max(depth) FROM up)`
	var cycle bool
	var longestChain int
	if err := tx.QueryRow(ctx, query, name, flag.MaxPrerequisiteDepth+1).Scan(&cycle, &longestChain); err != nil {
		return fmt.Errorf("check dependency graph: %w", err)
	}
	if cycle {
		return flag.ErrDependencyCycle
	}
	if longestChain > flag.MaxPrerequisiteDepth {
		return flag.ErrDependencyTooDeep
	}
	return nil
}
