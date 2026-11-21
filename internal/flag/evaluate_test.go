package flag

import (
	"errors"
	"fmt"
	"testing"
)

// snapshot builds a Snapshot from flags and one user's overrides.
func snapshot(overrides map[string]bool, flags ...Flag) Snapshot {
	snap := Snapshot{Flags: map[string]Flag{}, Overrides: overrides}
	for _, f := range flags {
		snap.Flags[f.Name] = f
	}
	return snap
}

func on(name string, prerequisites ...string) Flag {
	return Flag{Name: name, Enabled: true, RolloutPercentage: 100, Prerequisites: prerequisites}
}

func off(name string, prerequisites ...string) Flag {
	return Flag{Name: name, Enabled: false, RolloutPercentage: 100, Prerequisites: prerequisites}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want Result
	}{
		{
			name: "global on",
			snap: snapshot(nil, on("f")),
			want: Result{true, ReasonGlobalEnabled},
		},
		{
			name: "global off",
			snap: snapshot(nil, off("f")),
			want: Result{false, ReasonGlobalDisabled},
		},
		{
			name: "override on beats global off",
			snap: snapshot(map[string]bool{"f": true}, off("f")),
			want: Result{true, ReasonUserOverride},
		},
		{
			name: "override off beats global on",
			snap: snapshot(map[string]bool{"f": false}, on("f")),
			want: Result{false, ReasonUserOverride},
		},
		{
			name: "override on another flag is ignored",
			snap: snapshot(map[string]bool{"other": true}, off("f")),
			want: Result{false, ReasonGlobalDisabled},
		},
		{
			name: "override on beats rollout of zero",
			snap: snapshot(map[string]bool{"f": true}, Flag{Name: "f", Enabled: true, RolloutPercentage: 0}),
			want: Result{true, ReasonUserOverride},
		},
		{
			name: "rollout of zero is off for everyone",
			snap: snapshot(nil, Flag{Name: "f", Enabled: true, RolloutPercentage: 0}),
			want: Result{false, ReasonRollout},
		},
		{
			name: "global off beats rollout",
			snap: snapshot(nil, Flag{Name: "f", Enabled: false, RolloutPercentage: 50}),
			want: Result{false, ReasonGlobalDisabled},
		},
		{
			name: "prerequisite on allows the flag",
			snap: snapshot(nil, on("f", "base"), on("base")),
			want: Result{true, ReasonGlobalEnabled},
		},
		{
			name: "prerequisite off blocks the flag",
			snap: snapshot(nil, on("f", "base"), off("base")),
			want: Result{false, ReasonPrerequisiteFailed},
		},
		{
			name: "prerequisite off blocks an override of true",
			snap: snapshot(map[string]bool{"f": true}, on("f", "base"), off("base")),
			want: Result{false, ReasonPrerequisiteFailed},
		},
		{
			name: "prerequisite satisfied by the user's override on it",
			snap: snapshot(map[string]bool{"base": true}, on("f", "base"), off("base")),
			want: Result{true, ReasonGlobalEnabled},
		},
		{
			name: "prerequisite switched off for the user by an override",
			snap: snapshot(map[string]bool{"base": false}, on("f", "base"), on("base")),
			want: Result{false, ReasonPrerequisiteFailed},
		},
		{
			name: "transitive prerequisite off blocks the flag",
			snap: snapshot(nil, on("f", "mid"), on("mid", "base"), off("base")),
			want: Result{false, ReasonPrerequisiteFailed},
		},
		{
			name: "one of several prerequisites off blocks the flag",
			snap: snapshot(nil, on("f", "a", "b"), on("a"), off("b")),
			want: Result{false, ReasonPrerequisiteFailed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate("f", "user-1", tt.snap)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tt.want {
				t.Errorf("Evaluate = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEvaluateUnknownFlag(t *testing.T) {
	_, err := Evaluate("missing", "user-1", snapshot(nil, on("f")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestEvaluateMissingPrerequisiteIsNotReportedAsNotFound(t *testing.T) {
	_, err := Evaluate("f", "user-1", snapshot(nil, on("f", "gone")))
	if !errors.Is(err, ErrIncompleteSnapshot) {
		t.Errorf("error = %v, want ErrIncompleteSnapshot", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a missing prerequisite must not look like a missing flag")
	}
}

func TestEvaluateStopsOnCycle(t *testing.T) {
	_, err := Evaluate("a", "user-1", snapshot(nil, on("a", "b"), on("b", "a")))
	if !errors.Is(err, ErrPrerequisiteDepth) {
		t.Errorf("error = %v, want ErrPrerequisiteDepth", err)
	}
}

func TestEvaluateDepthLimit(t *testing.T) {
	// chain builds f0 -> f1 -> ... -> f<length>, all enabled.
	chain := func(length int) Snapshot {
		flags := make([]Flag, 0, length+1)
		for i := 0; i < length; i++ {
			flags = append(flags, on(fmt.Sprintf("f%d", i), fmt.Sprintf("f%d", i+1)))
		}
		flags = append(flags, on(fmt.Sprintf("f%d", length)))
		return snapshot(nil, flags...)
	}

	if got, err := Evaluate("f0", "user-1", chain(MaxPrerequisiteDepth)); err != nil || !got.Enabled {
		t.Errorf("chain at the limit: got %+v, %v; want enabled", got, err)
	}
	if _, err := Evaluate("f0", "user-1", chain(MaxPrerequisiteDepth+1)); !errors.Is(err, ErrPrerequisiteDepth) {
		t.Errorf("chain past the limit: error = %v, want ErrPrerequisiteDepth", err)
	}
}

func TestBucketIsStableAndInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		user := fmt.Sprintf("user-%d", i)
		first := Bucket("checkout", user)
		if first < 0 || first > 99 {
			t.Fatalf("Bucket(%q) = %d, want 0 to 99", user, first)
		}
		if again := Bucket("checkout", user); again != first {
			t.Fatalf("Bucket(%q) changed from %d to %d", user, first, again)
		}
	}
}

func TestBucketDiffersAcrossFlags(t *testing.T) {
	same := 0
	const users = 1000
	for i := 0; i < users; i++ {
		user := fmt.Sprintf("user-%d", i)
		if Bucket("flag-a", user) == Bucket("flag-b", user) {
			same++
		}
	}
	// Independent buckets agree about 1% of the time.
	if same > users/10 {
		t.Errorf("%d of %d users share a bucket across two flags; buckets look correlated", same, users)
	}
}

func TestRolloutSharesAndMonotonicity(t *testing.T) {
	const users = 10000
	enabledAt := func(percentage int, user string) bool {
		f := Flag{Name: "checkout", Enabled: true, RolloutPercentage: percentage}
		got, err := Evaluate("checkout", user, snapshot(nil, f))
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return got.Enabled
	}

	for _, percentage := range []int{0, 1, 25, 50, 99, 100} {
		count := 0
		for i := 0; i < users; i++ {
			if enabledAt(percentage, fmt.Sprintf("user-%d", i)) {
				count++
			}
		}
		share := float64(count) / users * 100
		if diff := share - float64(percentage); diff < -2 || diff > 2 {
			t.Errorf("rollout %d%%: %.1f%% of users enabled, want within 2 points", percentage, share)
		}
	}

	// Raising the percentage must never switch a user off.
	for i := 0; i < users; i++ {
		user := fmt.Sprintf("user-%d", i)
		if enabledAt(30, user) && !enabledAt(60, user) {
			t.Fatalf("%s is on at 30%% but off at 60%%", user)
		}
	}
}
