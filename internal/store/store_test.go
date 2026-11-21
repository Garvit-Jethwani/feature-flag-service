package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store/storetest"
)

func mustCreate(t *testing.T, s *store.Store, name string, enabled bool) {
	t.Helper()
	if _, err := s.CreateFlag(context.Background(), name, "", enabled); err != nil {
		t.Fatalf("CreateFlag(%q): %v", name, err)
	}
}

// require makes flagName depend on requires. The API cannot do this until
// prerequisites are exposed, so the test writes the row directly.
func require(t *testing.T, pool *pgxpool.Pool, flagName, requires string) {
	t.Helper()
	const query = `INSERT INTO flag_prerequisites (flag_name, requires_flag) VALUES ($1, $2)`
	if _, err := pool.Exec(context.Background(), query, flagName, requires); err != nil {
		t.Fatalf("insert prerequisite %s -> %s: %v", flagName, requires, err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := storetest.NewPool(t) // already migrated once
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations has %d rows, want 1", count)
	}
}

func TestCreateFlagDefaultsAndDuplicate(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))

	f, err := s.CreateFlag(ctx, "new-checkout", "New checkout flow", false)
	if err != nil {
		t.Fatalf("CreateFlag: %v", err)
	}
	if f.Enabled || f.RolloutPercentage != 100 || len(f.Prerequisites) != 0 {
		t.Errorf("unexpected defaults: %+v", f)
	}
	if f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", f)
	}

	if _, err := s.CreateFlag(ctx, "new-checkout", "", true); !errors.Is(err, flag.ErrAlreadyExists) {
		t.Errorf("duplicate CreateFlag error = %v, want ErrAlreadyExists", err)
	}
}

func TestUpdateFlagChangesOnlyGivenFields(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))
	if _, err := s.CreateFlag(ctx, "f", "original", false); err != nil {
		t.Fatalf("CreateFlag: %v", err)
	}

	enabled := true
	f, err := s.UpdateFlag(ctx, "f", flag.Patch{Enabled: &enabled})
	if err != nil {
		t.Fatalf("UpdateFlag: %v", err)
	}
	if !f.Enabled || f.Description != "original" {
		t.Errorf("after enabling: %+v, want enabled with the original description", f)
	}

	description := "changed"
	f, err = s.UpdateFlag(ctx, "f", flag.Patch{Description: &description})
	if err != nil {
		t.Fatalf("UpdateFlag: %v", err)
	}
	if !f.Enabled || f.Description != "changed" {
		t.Errorf("after describing: %+v, want still enabled with the new description", f)
	}

	if _, err := s.UpdateFlag(ctx, "missing", flag.Patch{Enabled: &enabled}); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("UpdateFlag on a missing flag: error = %v, want ErrNotFound", err)
	}
}

func TestOverrides(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))
	mustCreate(t, s, "f", false)

	if _, err := s.SetOverride(ctx, "missing", "u1", true); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("SetOverride on a missing flag: error = %v, want ErrNotFound", err)
	}

	if _, err := s.SetOverride(ctx, "f", "u1", true); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if _, err := s.SetOverride(ctx, "f", "u1", false); err != nil { // replace
		t.Fatalf("SetOverride (replace): %v", err)
	}
	if _, err := s.SetOverride(ctx, "f", "u2", true); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	f, err := s.GetFlag(ctx, "f")
	if err != nil {
		t.Fatalf("GetFlag: %v", err)
	}
	if len(f.Overrides) != 2 ||
		f.Overrides[0].UserID != "u1" || f.Overrides[0].Enabled ||
		f.Overrides[1].UserID != "u2" || !f.Overrides[1].Enabled {
		t.Errorf("overrides = %+v, want u1=false then u2=true", f.Overrides)
	}

	if err := s.DeleteOverride(ctx, "f", "u1"); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if err := s.DeleteOverride(ctx, "f", "u1"); !errors.Is(err, flag.ErrOverrideNotFound) {
		t.Errorf("second DeleteOverride: error = %v, want ErrOverrideNotFound", err)
	}
	if err := s.DeleteOverride(ctx, "missing", "u1"); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("DeleteOverride on a missing flag: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteFlag(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewPool(t)
	s := store.New(pool)
	mustCreate(t, s, "base", true)
	mustCreate(t, s, "feature", true)
	require(t, pool, "feature", "base")
	if _, err := s.SetOverride(ctx, "feature", "u1", true); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	if err := s.DeleteFlag(ctx, "base"); !errors.Is(err, flag.ErrHasDependents) {
		t.Errorf("deleting a required flag: error = %v, want ErrHasDependents", err)
	}
	if err := s.DeleteFlag(ctx, "feature"); err != nil { // cascades to its override and prerequisite rows
		t.Fatalf("DeleteFlag(feature): %v", err)
	}
	if err := s.DeleteFlag(ctx, "base"); err != nil {
		t.Fatalf("DeleteFlag(base) once nothing requires it: %v", err)
	}
	if err := s.DeleteFlag(ctx, "base"); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("deleting twice: error = %v, want ErrNotFound", err)
	}
}

func TestListFlagsPagination(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))
	for _, name := range []string{"c", "a", "b"} {
		mustCreate(t, s, name, false)
	}

	first, err := s.ListFlags(ctx, "", 2)
	if err != nil {
		t.Fatalf("ListFlags: %v", err)
	}
	if len(first) != 2 || first[0].Name != "a" || first[1].Name != "b" {
		t.Fatalf("first page = %+v, want a then b", first)
	}
	second, err := s.ListFlags(ctx, "b", 2)
	if err != nil {
		t.Fatalf("ListFlags: %v", err)
	}
	if len(second) != 1 || second[0].Name != "c" {
		t.Errorf("second page = %+v, want c", second)
	}
}

func TestEvaluationSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewPool(t)
	s := store.New(pool)

	// feature -> mid -> base, plus an unrelated flag that must not be loaded.
	mustCreate(t, s, "base", false)
	mustCreate(t, s, "mid", true)
	mustCreate(t, s, "feature", true)
	mustCreate(t, s, "unrelated", true)
	require(t, pool, "feature", "mid")
	require(t, pool, "mid", "base")
	for _, o := range []struct {
		flag, user string
		enabled    bool
	}{
		{"base", "u1", true},
		{"unrelated", "u1", true},
		{"feature", "someone-else", false},
	} {
		if _, err := s.SetOverride(ctx, o.flag, o.user, o.enabled); err != nil {
			t.Fatalf("SetOverride: %v", err)
		}
	}

	snap, err := s.EvaluationSnapshot(ctx, "feature", "u1")
	if err != nil {
		t.Fatalf("EvaluationSnapshot: %v", err)
	}
	if len(snap.Flags) != 3 {
		t.Errorf("snapshot has %d flags, want feature, mid, and base", len(snap.Flags))
	}
	if got := snap.Flags["feature"].Prerequisites; len(got) != 1 || got[0] != "mid" {
		t.Errorf("feature prerequisites = %v, want [mid]", got)
	}
	if len(snap.Overrides) != 1 || !snap.Overrides["base"] {
		t.Errorf("overrides = %v, want only base=true for u1", snap.Overrides)
	}

	// The loaded snapshot must evaluate correctly: u1 has base forced on, so
	// the whole chain is on; another user is blocked by base being off.
	if got, err := flag.Evaluate("feature", "u1", snap); err != nil || !got.Enabled {
		t.Errorf("u1: got %+v, %v; want enabled", got, err)
	}
	other, err := s.EvaluationSnapshot(ctx, "feature", "u2")
	if err != nil {
		t.Fatalf("EvaluationSnapshot: %v", err)
	}
	if got, err := flag.Evaluate("feature", "u2", other); err != nil || got.Enabled || got.Reason != flag.ReasonPrerequisiteFailed {
		t.Errorf("u2: got %+v, %v; want off with prerequisite_failed", got, err)
	}

	if _, err := s.EvaluationSnapshot(ctx, "missing", "u1"); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("snapshot of a missing flag: error = %v, want ErrNotFound", err)
	}
}

func TestEvaluationSnapshotTerminatesOnCycle(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewPool(t)
	s := store.New(pool)
	mustCreate(t, s, "a", true)
	mustCreate(t, s, "b", true)
	require(t, pool, "a", "b")
	require(t, pool, "b", "a") // the API will reject this; the store must still cope

	snap, err := s.EvaluationSnapshot(ctx, "a", "u1")
	if err != nil {
		t.Fatalf("EvaluationSnapshot: %v", err)
	}
	if _, err := flag.Evaluate("a", "u1", snap); !errors.Is(err, flag.ErrPrerequisiteDepth) {
		t.Errorf("evaluating a cycle: error = %v, want ErrPrerequisiteDepth", err)
	}
}
