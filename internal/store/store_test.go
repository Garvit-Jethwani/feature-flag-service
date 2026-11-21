package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store/storetest"
)

const actor = "tester"

func mustCreate(t *testing.T, s *store.Store, name string, enabled bool) {
	t.Helper()
	if _, err := s.CreateFlag(context.Background(), actor, name, "", enabled); err != nil {
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

// auditCount returns how many audit entries exist for a flag.
func auditCount(t *testing.T, s *store.Store, name string) int {
	t.Helper()
	entries, err := s.ListAudit(context.Background(), name, 0, 200)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return len(entries)
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := storetest.NewPool(t) // already migrated once
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))

	f, err := s.CreateFlag(ctx, actor, "f", "original", false)
	if err != nil {
		t.Fatalf("CreateFlag: %v", err)
	}
	if f.Enabled || f.RolloutPercentage != 100 || len(f.Prerequisites) != 0 {
		t.Errorf("unexpected defaults: %+v", f)
	}
	if _, err := s.CreateFlag(ctx, actor, "f", "", true); !errors.Is(err, flag.ErrAlreadyExists) {
		t.Errorf("duplicate CreateFlag error = %v, want ErrAlreadyExists", err)
	}

	// A patch changes only the fields it names.
	enabled := true
	f, affected, err := s.UpdateFlag(ctx, actor, "f", flag.Patch{Enabled: &enabled})
	if err != nil {
		t.Fatalf("UpdateFlag: %v", err)
	}
	if !f.Enabled || f.Description != "original" {
		t.Errorf("after enabling: %+v, want enabled with the original description", f)
	}
	if !slices.Equal(affected, []string{"f"}) {
		t.Errorf("affected = %v, want [f]", affected)
	}

	// A patch that changes nothing reports nothing to invalidate and writes
	// no audit entry. So far: one entry for the create, one for the update.
	_, affected, err = s.UpdateFlag(ctx, actor, "f", flag.Patch{Enabled: &enabled})
	if err != nil {
		t.Fatalf("no-op UpdateFlag: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("no-op update reported %v for invalidation", affected)
	}
	if got := auditCount(t, s, "f"); got != 2 {
		t.Errorf("audit entries = %d, want 2 (the failed duplicate and the no-op must not be recorded)", got)
	}

	if _, _, err := s.UpdateFlag(ctx, actor, "missing", flag.Patch{Enabled: &enabled}); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("UpdateFlag on a missing flag: error = %v, want ErrNotFound", err)
	}
	if _, err := s.DeleteFlag(ctx, actor, "f"); err != nil {
		t.Fatalf("DeleteFlag: %v", err)
	}
	if _, err := s.DeleteFlag(ctx, actor, "f"); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("deleting twice: error = %v, want ErrNotFound", err)
	}
	// The history outlives the flag.
	if got := auditCount(t, s, "f"); got != 3 {
		t.Errorf("audit entries after delete = %d, want 3", got)
	}
}

func TestOverrides(t *testing.T) {
	ctx := context.Background()
	s := store.New(storetest.NewPool(t))
	mustCreate(t, s, "f", false)

	if _, _, err := s.SetOverride(ctx, actor, "missing", "u1", true); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("SetOverride on a missing flag: error = %v, want ErrNotFound", err)
	}
	if _, _, err := s.SetOverride(ctx, actor, "f", "u1", true); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if _, _, err := s.SetOverride(ctx, actor, "f", "u1", false); err != nil { // replace
		t.Fatalf("SetOverride (replace): %v", err)
	}
	// Setting the same value again changes nothing.
	if _, affected, err := s.SetOverride(ctx, actor, "f", "u1", false); err != nil || len(affected) != 0 {
		t.Errorf("no-op SetOverride: affected %v, error %v; want nothing", affected, err)
	}

	f, err := s.GetFlag(ctx, "f")
	if err != nil {
		t.Fatalf("GetFlag: %v", err)
	}
	if len(f.Overrides) != 1 || f.Overrides[0].UserID != "u1" || f.Overrides[0].Enabled {
		t.Errorf("overrides = %+v, want u1=false", f.Overrides)
	}

	if _, err := s.DeleteOverride(ctx, actor, "f", "u1"); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if _, err := s.DeleteOverride(ctx, actor, "f", "u1"); !errors.Is(err, flag.ErrOverrideNotFound) {
		t.Errorf("second DeleteOverride: error = %v, want ErrOverrideNotFound", err)
	}
	if _, err := s.DeleteOverride(ctx, actor, "missing", "u1"); !errors.Is(err, flag.ErrNotFound) {
		t.Errorf("DeleteOverride on a missing flag: error = %v, want ErrNotFound", err)
	}
}

// A change to a flag must report every flag that depends on it, so their
// cached results are invalidated too. A required flag cannot be deleted.
func TestDependentsAreReportedAndProtectDeletion(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewPool(t)
	s := store.New(pool)
	for _, name := range []string{"base", "mid", "feature", "unrelated"} {
		mustCreate(t, s, name, true)
	}
	require(t, pool, "feature", "mid") // feature -> mid -> base
	require(t, pool, "mid", "base")

	disabled := false
	_, affected, err := s.UpdateFlag(ctx, actor, "base", flag.Patch{Enabled: &disabled})
	if err != nil {
		t.Fatalf("UpdateFlag: %v", err)
	}
	slices.Sort(affected)
	if !slices.Equal(affected, []string{"base", "feature", "mid"}) {
		t.Errorf("affected = %v, want base, feature, and mid", affected)
	}

	if _, err := s.DeleteFlag(ctx, actor, "base"); !errors.Is(err, flag.ErrHasDependents) {
		t.Errorf("deleting a required flag: error = %v, want ErrHasDependents", err)
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
		if _, _, err := s.SetOverride(ctx, actor, o.flag, o.user, o.enabled); err != nil {
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
	if len(snap.Overrides) != 1 || !snap.Overrides["base"] {
		t.Errorf("overrides = %v, want only base=true for u1", snap.Overrides)
	}

	// u1 has base forced on, so the whole chain is on; another user is
	// blocked by base being off.
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

// The API will reject cycles; if one ever reached the database, loading and
// evaluating must still terminate with an error rather than loop.
func TestEvaluationSnapshotTerminatesOnCycle(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewPool(t)
	s := store.New(pool)
	mustCreate(t, s, "a", true)
	mustCreate(t, s, "b", true)
	require(t, pool, "a", "b")
	require(t, pool, "b", "a")

	snap, err := s.EvaluationSnapshot(ctx, "a", "u1")
	if err != nil {
		t.Fatalf("EvaluationSnapshot: %v", err)
	}
	if _, err := flag.Evaluate("a", "u1", snap); !errors.Is(err, flag.ErrPrerequisiteDepth) {
		t.Errorf("evaluating a cycle: error = %v, want ErrPrerequisiteDepth", err)
	}
}
