package flag

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Reason explains why an evaluation produced its result.
type Reason string

// Reason codes, in the order the rules are applied.
const (
	ReasonPrerequisiteFailed Reason = "prerequisite_failed"
	ReasonUserOverride       Reason = "user_override"
	ReasonGlobalDisabled     Reason = "global_disabled"
	ReasonRollout            Reason = "rollout"
	ReasonGlobalEnabled      Reason = "global_enabled"
)

// MaxPrerequisiteDepth bounds how many levels of prerequisites are followed.
// Cycles are rejected when a prerequisite is written; this is the second
// guard, so evaluation always terminates.
const MaxPrerequisiteDepth = 10

// Evaluation errors that indicate bad stored data rather than a bad request.
var (
	ErrPrerequisiteDepth  = errors.New("prerequisite chain is too deep")
	ErrIncompleteSnapshot = errors.New("snapshot is missing a prerequisite flag")
)

// Result is the outcome of evaluating one flag for one user.
type Result struct {
	Enabled bool
	Reason  Reason
}

// Snapshot is everything needed to evaluate one flag for one user.
type Snapshot struct {
	// Flags holds the flag being evaluated and all of its transitive
	// prerequisites, keyed by name.
	Flags map[string]Flag
	// Overrides holds this user's overrides, keyed by flag name.
	Overrides map[string]bool
}

// Evaluate decides whether the named flag is on for the user. It is a pure
// function of its arguments. The first matching rule decides:
//
//  1. any prerequisite is off for the user: off
//  2. the user has an override: the override value
//  3. the flag is globally disabled: off
//  4. the rollout is below 100%: on if the user's bucket is below it
//  5. otherwise: on
//
// It returns ErrNotFound if the snapshot does not contain the flag.
func Evaluate(name, userID string, snap Snapshot) (Result, error) {
	if _, ok := snap.Flags[name]; !ok {
		return Result{}, ErrNotFound
	}
	return evaluate(name, userID, snap, 0)
}

func evaluate(name, userID string, snap Snapshot, depth int) (Result, error) {
	if depth > MaxPrerequisiteDepth {
		return Result{}, fmt.Errorf("evaluating %q: %w", name, ErrPrerequisiteDepth)
	}
	f, ok := snap.Flags[name]
	if !ok {
		return Result{}, fmt.Errorf("evaluating %q: %w", name, ErrIncompleteSnapshot)
	}

	for _, prerequisite := range f.Prerequisites {
		result, err := evaluate(prerequisite, userID, snap, depth+1)
		if err != nil {
			return Result{}, err
		}
		if !result.Enabled {
			return Result{Enabled: false, Reason: ReasonPrerequisiteFailed}, nil
		}
	}

	if enabled, ok := snap.Overrides[name]; ok {
		return Result{Enabled: enabled, Reason: ReasonUserOverride}, nil
	}
	if !f.Enabled {
		return Result{Enabled: false, Reason: ReasonGlobalDisabled}, nil
	}
	if f.RolloutPercentage < 100 {
		return Result{Enabled: Bucket(name, userID) < f.RolloutPercentage, Reason: ReasonRollout}, nil
	}
	return Result{Enabled: true, Reason: ReasonGlobalEnabled}, nil
}

// Bucket maps a user to a stable number from 0 to 99 for one flag. A user is
// inside a rollout of p percent when their bucket is below p, so raising the
// percentage only ever adds users. The flag name is part of the hash so that
// different flags select different users. Flag names cannot contain ':', so
// the hashed string is unambiguous.
func Bucket(flagName, userID string) int {
	sum := sha256.Sum256([]byte(flagName + ":" + userID))
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}
