package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// evaluate calls the evaluation endpoint and returns whether the flag is on
// and the X-Cache header. With noCache set it sends Cache-Control: no-cache.
func (s *testServer) evaluate(name, userID string, noCache bool) (enabled bool, cacheStatus string) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/v1/flags/"+name+"/evaluate?user_id="+userID, nil)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	if noCache {
		req.Header.Set("Cache-Control", "no-cache")
	}
	res := s.send(req, testReaderToken)
	if res.status != http.StatusOK {
		s.t.Fatalf("evaluate %s for %s: status %d, body %v", name, userID, res.status, res.body)
	}
	enabled, _ = res.body["enabled"].(bool)
	return enabled, res.header.Get("X-Cache")
}

// wantCached evaluates and checks both the answer and how it was served.
func (s *testServer) wantCached(step, name, userID string, wantEnabled bool, wantCache string) {
	s.t.Helper()
	enabled, status := s.evaluate(name, userID, false)
	if enabled != wantEnabled || status != wantCache {
		s.t.Errorf("%s: got enabled=%v X-Cache=%s, want enabled=%v X-Cache=%s", step, enabled, status, wantEnabled, wantCache)
	}
}

// TestEvaluationCacheAndInvalidation checks the caching contract end to end:
// repeated evaluations are served from the cache, and every kind of change
// to a flag makes the next evaluation recompute with the new state.
func TestEvaluationCacheAndInvalidation(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("f", true)

	s.wantCached("first evaluation", "f", "u1", true, "MISS")
	s.wantCached("second evaluation", "f", "u1", true, "HIT")
	s.wantCached("another user has its own entry", "f", "u2", true, "MISS")

	// A global toggle drops the flag's cached results.
	s.do(http.MethodPatch, "/v1/flags/f", testAdminToken, map[string]any{"enabled": false})
	s.wantCached("after disabling", "f", "u1", false, "MISS")
	s.wantCached("after disabling, cached again", "f", "u1", false, "HIT")

	// A change that changes nothing leaves the cache alone.
	s.do(http.MethodPatch, "/v1/flags/f", testAdminToken, map[string]any{"enabled": false})
	s.wantCached("after a no-op update", "f", "u1", false, "HIT")

	// Setting an override drops the flag's cached results.
	s.do(http.MethodPut, "/v1/flags/f/overrides/u1", testAdminToken, map[string]any{"enabled": true})
	s.wantCached("after an override", "f", "u1", true, "MISS")
	s.wantCached("after an override, cached again", "f", "u1", true, "HIT")

	// Removing the override does too.
	s.do(http.MethodDelete, "/v1/flags/f/overrides/u1", testAdminToken, nil)
	s.wantCached("after removing the override", "f", "u1", false, "MISS")

	// Cache-Control: no-cache skips the lookup and refreshes the entry.
	if enabled, status := s.evaluate("f", "u1", true); enabled || status != "BYPASS" {
		t.Errorf("no-cache: got enabled=%v X-Cache=%s, want false BYPASS", enabled, status)
	}
	s.wantCached("after a bypass", "f", "u1", false, "HIT")

	// A missing flag is a 404 every time and is never cached.
	for i := 0; i < 2; i++ {
		res := s.do(http.MethodGet, "/v1/flags/missing/evaluate?user_id=u1", testReaderToken, nil)
		if res.status != http.StatusNotFound || res.header.Get("X-Cache") != "" {
			t.Errorf("missing flag: status %d X-Cache=%q, want 404 with no X-Cache", res.status, res.header.Get("X-Cache"))
		}
	}

	// Deleting the flag drops its cached results: 404, not a stale HIT.
	s.do(http.MethodDelete, "/v1/flags/f", testAdminToken, nil)
	if res := s.do(http.MethodGet, "/v1/flags/f/evaluate?user_id=u1", testReaderToken, nil); res.status != http.StatusNotFound {
		t.Errorf("evaluating a deleted flag: status %d, want 404", res.status)
	}
}

// A change to a flag must also drop the cached results of flags that depend
// on it, or they would keep answering from the old state.
func TestDependentFlagIsInvalidated(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("base", true)
	s.mustCreate("feature", true)
	// The API does not expose prerequisites yet, so write the row directly.
	const query = `INSERT INTO flag_prerequisites (flag_name, requires_flag) VALUES ('feature', 'base')`
	if _, err := s.pool.Exec(context.Background(), query); err != nil {
		t.Fatalf("insert prerequisite: %v", err)
	}

	s.wantCached("before the change", "feature", "u1", true, "MISS")
	s.wantCached("before the change, cached", "feature", "u1", true, "HIT")

	s.do(http.MethodPatch, "/v1/flags/base", testAdminToken, map[string]any{"enabled": false})

	s.wantCached("after disabling the prerequisite", "feature", "u1", false, "MISS")
	s.wantEvaluation("feature", "u1", false, "prerequisite_failed")
}

// TestAuditTrail checks that every state change is recorded with who made
// it and the values before and after, and that only admins can read it.
func TestAuditTrail(t *testing.T) {
	s := newDatabaseServer(t)

	s.do(http.MethodPost, "/v1/flags", testAdminToken, map[string]any{"name": "f", "description": "first"})
	s.do(http.MethodPatch, "/v1/flags/f", testAdminToken, map[string]any{"enabled": true})
	s.do(http.MethodPatch, "/v1/flags/f", testAdminToken, map[string]any{"enabled": true}) // no-op: not recorded
	s.do(http.MethodPut, "/v1/flags/f/overrides/u1", testAdminToken, map[string]any{"enabled": false})
	s.do(http.MethodDelete, "/v1/flags/f/overrides/u1", testAdminToken, nil)
	s.do(http.MethodDelete, "/v1/flags/f", testAdminToken, nil)

	// The history is still readable after the flag is deleted.
	res := s.do(http.MethodGet, "/v1/flags/f/audit", testAdminToken, nil)
	if res.status != http.StatusOK {
		t.Fatalf("read audit: status %d, body %v", res.status, res.body)
	}
	entries, _ := res.body["entries"].([]any)
	wantActions := []string{"flag.deleted", "override.removed", "override.set", "flag.updated", "flag.created"} // newest first
	if len(entries) != len(wantActions) {
		t.Fatalf("got %d audit entries, want %d: %v", len(entries), len(wantActions), entries)
	}
	entry := func(i int) map[string]any { return entries[i].(map[string]any) }
	for i, want := range wantActions {
		e := entry(i)
		if e["action"] != want {
			t.Errorf("entry %d: action = %v, want %s", i, e["action"], want)
		}
		// The actor is the name bound to the API key, not anything the
		// client sent.
		if e["actor"] != "alice" {
			t.Errorf("entry %d: actor = %v, want alice", i, e["actor"])
		}
		if e["flag"] != "f" || e["occurred_at"] == nil {
			t.Errorf("entry %d is missing its flag or time: %v", i, e)
		}
	}

	// Previous and new values.
	deleted, removed, set, updated, created := entry(0), entry(1), entry(2), entry(3), entry(4)
	if created["previous"] != nil || created["new"].(map[string]any)["enabled"] != false {
		t.Errorf("flag.created: previous=%v new=%v, want null and enabled=false", created["previous"], created["new"])
	}
	if updated["previous"].(map[string]any)["enabled"] != false || updated["new"].(map[string]any)["enabled"] != true {
		t.Errorf("flag.updated: previous=%v new=%v, want enabled false then true", updated["previous"], updated["new"])
	}
	if set["previous"] != nil || set["new"].(map[string]any)["user_id"] != "u1" || set["new"].(map[string]any)["enabled"] != false {
		t.Errorf("override.set: previous=%v new=%v, want null and u1=false", set["previous"], set["new"])
	}
	if removed["previous"].(map[string]any)["user_id"] != "u1" || removed["new"] != nil {
		t.Errorf("override.removed: previous=%v new=%v, want u1 and null", removed["previous"], removed["new"])
	}
	if deleted["previous"].(map[string]any)["enabled"] != true || deleted["new"] != nil {
		t.Errorf("flag.deleted: previous=%v new=%v, want enabled=true and null", deleted["previous"], deleted["new"])
	}

	// Paging: two entries, then continue from next_before.
	page := s.do(http.MethodGet, "/v1/audit?limit=2", testAdminToken, nil)
	pageEntries, _ := page.body["entries"].([]any)
	nextBefore, _ := page.body["next_before"].(float64)
	if len(pageEntries) != 2 || nextBefore == 0 {
		t.Fatalf("first page: %d entries, next_before=%v; want 2 and a cursor", len(pageEntries), page.body["next_before"])
	}
	if next := s.do(http.MethodGet, "/v1/flags/f/audit?before=2", testAdminToken, nil); len(next.body["entries"].([]any)) != 1 {
		t.Errorf("before=2 returned %v, want only the first entry", next.body["entries"])
	}

	// Only admins may read the audit trail; a name that never existed is 404.
	for _, path := range []string{"/v1/audit", "/v1/flags/f/audit"} {
		if res := s.do(http.MethodGet, path, testReaderToken, nil); res.status != http.StatusForbidden {
			t.Errorf("reader GET %s: status %d, want 403", path, res.status)
		}
	}
	if res := s.do(http.MethodGet, "/v1/flags/never-existed/audit", testAdminToken, nil); res.status != http.StatusNotFound {
		t.Errorf("audit of a name that never existed: status %d, want 404", res.status)
	}
}
