package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// insertAudit writes one audit entry. It is always called inside the same
// transaction as the change it describes, so the change and its audit entry
// are committed or rolled back together.
//
// previous and next are the values before and after the change, encoded as
// JSON. Pass nil when there is no value (before a creation, after a
// deletion); nil is stored as SQL NULL.
func insertAudit(ctx context.Context, tx pgx.Tx, actor, flagName string, action flag.AuditAction, previous, next any) error {
	previousJSON, err := auditValue(previous)
	if err != nil {
		return err
	}
	nextJSON, err := auditValue(next)
	if err != nil {
		return err
	}
	const query = `
		INSERT INTO audit_log (flag_name, action, actor, previous_value, new_value)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := tx.Exec(ctx, query, flagName, string(action), actor, previousJSON, nextJSON); err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}

// auditValue encodes a value as JSON text for a JSONB column, or returns nil
// (SQL NULL) when there is no value.
func auditValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode audit value: %w", err)
	}
	return string(encoded), nil
}

// ListAudit returns up to limit audit entries, newest first. An empty
// flagName means entries for every flag. A before of zero starts from the
// newest entry; otherwise only entries with a smaller id are returned, which
// is how a client asks for the next page.
func (s *Store) ListAudit(ctx context.Context, flagName string, before int64, limit int) ([]flag.AuditEntry, error) {
	const query = `
		SELECT id, flag_name, action, actor, previous_value, new_value, occurred_at
		FROM audit_log
		WHERE ($1::text = '' OR flag_name = $1::text)
		  AND ($2::bigint = 0 OR id < $2::bigint)
		ORDER BY id DESC
		LIMIT $3`
	rows, err := s.pool.Query(ctx, query, flagName, before, limit)
	if err != nil {
		return nil, fmt.Errorf("select audit entries: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (flag.AuditEntry, error) {
		var e flag.AuditEntry
		var action string
		var previous, next []byte // raw JSON; nil when the column is NULL
		err := row.Scan(&e.ID, &e.FlagName, &action, &e.Actor, &previous, &next, &e.OccurredAt)
		e.Action = flag.AuditAction(action)
		e.Previous, e.New = previous, next
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read audit entries: %w", err)
	}
	return entries, nil
}

// FlagExists reports whether a flag with this name exists now.
func (s *Store) FlagExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	const query = `SELECT EXISTS (SELECT 1 FROM flags WHERE name = $1)`
	if err := s.pool.QueryRow(ctx, query, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("check flag exists: %w", err)
	}
	return exists, nil
}
