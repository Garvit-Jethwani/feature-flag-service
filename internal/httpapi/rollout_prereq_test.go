package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// mustPatch updates a flag as an admin and fails the test unless it works.
func (s *testServer) mustPatch(name string, body map[string]any) {
	s.t.Helper()
	if res := s.do(http.MethodPatch, "/v1/flags/"+name, testAdminToken, body); res.status != http.StatusOK {
		s.t.Fatalf("patch %q with %v: status %d, body %v", name, body, res.status, res.body)
	}
}

// wantPatchError updates a flag and checks that it is rejected with the
// given status and error code.
func (s *testServer) wantPatchError(step, name string, body map[string]any, wantStatus int, wantCode string) {
	s.t.Helper()
	res := s.do(http.MethodPatch, "/v1/flags/"+name, testAdminToken, body)
	if res.status != wantStatus || res.errorCode() != wantCode {
		s.t.Errorf("%s: got %d %q, want %d %q (body %v)", step, res.status, res.errorCode(), wantStatus, wantCode, res.body)
	}
}

// TestRolloutPercentage checks that the percentage can be set through the
// API and decides who gets an enabled flag.
func TestRolloutPercentage(t *testing.T) {
	s := newDatabaseServer(t)

	// Created at 0%: enabled globally, but on for nobody.
	res := s.do(http.MethodPost, "/v1/flags", testAdminToken, map[string]any{"name": "f", "enabled": true, "rollout_percentage": 0})
	if res.status != http.StatusCreated || res.body["rollout_percentage"] != float64(0) {
		t.Fatalf("create at 0%%: status %d, body %v", res.status, res.body)
	}
	s.wantEvaluation("f", "u1", false, "rollout")

	// At 50%, some users are on and some are off, and a user's answer does
	// not change between calls.
	s.mustPatch("f", map[string]any{"rollout_percentage": 50})
	on := 0
	for i := 0; i < 40; i++ {
		user := fmt.Sprintf("user-%d", i)
		first, _ := s.evaluate("f", user, true)
		second, _ := s.evaluate("f", user, true)
		if first != second {
			t.Errorf("%s: answer changed between calls", user)
		}
		if first {
			on++
		}
	}
	if on == 0 || on == 40 {
		t.Errorf("at 50%%, %d of 40 users are on; want some but not all", on)
	}

	// Back to 100%: on for everyone. The earlier cached "off" must be gone.
	s.mustPatch("f", map[string]any{"rollout_percentage": 100})
	s.wantEvaluation("f", "u1", true, "global_enabled")

	// An override still wins over the rollout.
	s.mustPatch("f", map[string]any{"rollout_percentage": 0})
	s.do(http.MethodPut, "/v1/flags/f/overrides/u1", testAdminToken, map[string]any{"enabled": true})
	s.wantEvaluation("f", "u1", true, "user_override")

	s.wantPatchError("above 100", "f", map[string]any{"rollout_percentage": 101}, http.StatusBadRequest, "invalid_argument")
	s.wantPatchError("negative", "f", map[string]any{"rollout_percentage": -1}, http.StatusBadRequest, "invalid_argument")
}

// TestPrerequisites checks that a flag can be made to depend on another
// through the API, that the dependency is a hard gate, and that changes
// which would break the dependency graph are rejected.
func TestPrerequisites(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("base", false)
	s.mustCreate("feature", true)
	s.mustPatch("feature", map[string]any{"prerequisites": []string{"base"}})

	// The prerequisite is off, so the feature is off, even for a user with
	// an override of true: a prerequisite is a hard gate.
	s.wantEvaluation("feature", "u1", false, "prerequisite_failed")
	s.do(http.MethodPut, "/v1/flags/feature/overrides/u1", testAdminToken, map[string]any{"enabled": true})
	s.wantEvaluation("feature", "u1", false, "prerequisite_failed")

	// Turning the prerequisite on turns the feature on. This passes only if
	// the dependent flag's cached result was invalidated too.
	s.mustPatch("base", map[string]any{"enabled": true})
	s.wantEvaluation("feature", "u1", true, "user_override")
	s.wantEvaluation("feature", "u2", true, "global_enabled")

	// The change is in the audit trail with the list before and after.
	audit := s.do(http.MethodGet, "/v1/flags/feature/audit", testAdminToken, nil)
	entries, _ := audit.body["entries"].([]any)
	found := false
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		previous, _ := entry["previous"].(map[string]any)
		next, _ := entry["new"].(map[string]any)
		if entry["action"] == "flag.updated" && fmt.Sprint(previous["prerequisites"]) == "[]" && fmt.Sprint(next["prerequisites"]) == "[base]" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry shows prerequisites going from [] to [base]: %v", entries)
	}

	// Rejected changes. None of them may alter the flag.
	s.wantPatchError("direct cycle", "base", map[string]any{"prerequisites": []string{"feature"}}, http.StatusConflict, "dependency_cycle")
	s.mustCreate("top", true)
	s.mustPatch("top", map[string]any{"prerequisites": []string{"feature"}})
	s.wantPatchError("cycle through two flags", "base", map[string]any{"prerequisites": []string{"top"}}, http.StatusConflict, "dependency_cycle")
	s.wantPatchError("unknown flag", "feature", map[string]any{"prerequisites": []string{"missing"}}, http.StatusBadRequest, "invalid_argument")
	s.wantPatchError("itself", "feature", map[string]any{"prerequisites": []string{"feature"}}, http.StatusBadRequest, "invalid_argument")
	s.wantPatchError("repeated", "feature", map[string]any{"prerequisites": []string{"base", "base"}}, http.StatusBadRequest, "invalid_argument")

	got := s.do(http.MethodGet, "/v1/flags/feature", testReaderToken, nil)
	if fmt.Sprint(got.body["prerequisites"]) != "[base]" {
		t.Errorf("after rejected changes, prerequisites = %v, want [base]", got.body["prerequisites"])
	}

	// A required flag cannot be deleted; once the dependency is removed it
	// can.
	if res := s.do(http.MethodDelete, "/v1/flags/feature", testAdminToken, nil); res.status != http.StatusConflict {
		t.Errorf("deleting a required flag: status %d, want 409", res.status)
	}
	s.mustPatch("top", map[string]any{"prerequisites": []string{}})
	if res := s.do(http.MethodDelete, "/v1/flags/feature", testAdminToken, nil); res.status != http.StatusNoContent {
		t.Errorf("deleting after the dependency was removed: status %d, want 204", res.status)
	}
}

// TestPrerequisiteChainDepthLimit checks that a chain longer than the
// evaluator follows is rejected when it is written, so evaluation can never
// fail on depth.
func TestPrerequisiteChainDepthLimit(t *testing.T) {
	s := newDatabaseServer(t)

	// f0 <- f1 <- ... <- f10: ten levels of prerequisites, the most allowed.
	s.mustCreate("f0", true)
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("f%d", i)
		s.mustCreate(name, true)
		s.mustPatch(name, map[string]any{"prerequisites": []string{fmt.Sprintf("f%d", i-1)}})
	}
	s.wantEvaluation("f10", "u1", true, "global_enabled")

	// An eleventh level is refused, whether it is added at the top of the
	// chain or at the bottom.
	s.mustCreate("above", true)
	s.wantPatchError("extend at the top", "above", map[string]any{"prerequisites": []string{"f10"}}, http.StatusConflict, "dependency_too_deep")
	s.mustCreate("below", true)
	s.wantPatchError("extend at the bottom", "f0", map[string]any{"prerequisites": []string{"below"}}, http.StatusConflict, "dependency_too_deep")
}
