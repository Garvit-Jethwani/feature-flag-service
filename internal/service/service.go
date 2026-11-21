// Package service implements the flag use cases. It validates input, calls
// the store, keeps the evaluation cache correct, and evaluates flags.
//
// Two rules live here:
//   - Reads of an evaluation go through the cache first.
//   - Every write, once committed, invalidates the cached results it could
//     have changed.
package service

import (
	"context"
	"log/slog"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// Page sizes for list endpoints.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500

	DefaultAuditLimit = 50
	MaxAuditLimit     = 200
)

// Store is the persistence the service needs. Write methods take the actor
// for the audit trail and return the names of the flags whose cached
// evaluation results are out of date after the write.
type Store interface {
	CreateFlag(ctx context.Context, actor, name, description string, enabled bool) (flag.Flag, error)
	GetFlag(ctx context.Context, name string) (flag.Flag, error)
	ListFlags(ctx context.Context, after string, limit int) ([]flag.Flag, error)
	UpdateFlag(ctx context.Context, actor, name string, patch flag.Patch) (flag.Flag, []string, error)
	DeleteFlag(ctx context.Context, actor, name string) ([]string, error)
	SetOverride(ctx context.Context, actor, name, userID string, enabled bool) (flag.Override, []string, error)
	DeleteOverride(ctx context.Context, actor, name, userID string) ([]string, error)
	EvaluationSnapshot(ctx context.Context, name, userID string) (flag.Snapshot, error)
	ListAudit(ctx context.Context, flagName string, before int64, limit int) ([]flag.AuditEntry, error)
	FlagExists(ctx context.Context, name string) (bool, error)
}

// Cache holds evaluation results. The in-process implementation is
// cache.Memory; a shared cache such as Redis could implement the same
// methods, treating its own failures as misses.
type Cache interface {
	// Get returns a cached result that has not expired.
	Get(flagName, userID string) (flag.Result, bool)
	// Generation returns a counter that advances on every invalidation.
	Generation() uint64
	// Set stores a result unless an invalidation happened after generation
	// was read.
	Set(flagName, userID string, result flag.Result, generation uint64)
	// Invalidate drops every cached result for the named flags.
	Invalidate(flagNames ...string)
}

// CacheStatus says how an evaluation was answered. It is returned to the
// client in the X-Cache header.
type CacheStatus string

// Cache statuses.
const (
	CacheHit    CacheStatus = "HIT"    // answered from the cache
	CacheMiss   CacheStatus = "MISS"   // not cached; computed from the database
	CacheBypass CacheStatus = "BYPASS" // the caller asked for a fresh result
)

// Service implements the flag use cases.
type Service struct {
	store Store
	cache Cache
	log   *slog.Logger
}

// New returns a Service.
func New(store Store, cache Cache, logger *slog.Logger) *Service {
	return &Service{store: store, cache: cache, log: logger}
}

// CreateInput describes a new flag.
type CreateInput struct {
	Name        string
	Description string
	Enabled     bool
}

// Evaluation is the result of evaluating a flag for a user.
type Evaluation struct {
	Flag    string
	UserID  string
	Enabled bool
	Reason  flag.Reason
	Cache   CacheStatus
}

// invalidate drops the cached results of the flags a committed write
// affected. It must be called after the transaction commits, never before:
// invalidating first would let a concurrent reader re-cache the old value.
func (s *Service) invalidate(affected []string) {
	if len(affected) > 0 {
		s.cache.Invalidate(affected...)
	}
}

// CreateFlag creates a flag on behalf of actor.
func (s *Service) CreateFlag(ctx context.Context, actor string, in CreateInput) (flag.Flag, error) {
	if err := flag.ValidateName(in.Name); err != nil {
		return flag.Flag{}, err
	}
	if err := flag.ValidateDescription(in.Description); err != nil {
		return flag.Flag{}, err
	}
	f, err := s.store.CreateFlag(ctx, actor, in.Name, in.Description, in.Enabled)
	if err != nil {
		return flag.Flag{}, err
	}
	s.log.InfoContext(ctx, "flag created", "flag", f.Name, "enabled", f.Enabled, "actor", actor)
	return f, nil
}

// GetFlag returns a flag with its overrides and prerequisites.
func (s *Service) GetFlag(ctx context.Context, name string) (flag.Flag, error) {
	if err := flag.ValidateName(name); err != nil {
		return flag.Flag{}, err
	}
	return s.store.GetFlag(ctx, name)
}

// ListFlags returns one page of flags in name order. A limit of zero means
// the default page size; after is the last name of the previous page.
func (s *Service) ListFlags(ctx context.Context, after string, limit int) ([]flag.Flag, error) {
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, &flag.ValidationError{Field: "limit", Message: "must be from 1 to 500"}
	}
	if after != "" {
		if err := flag.ValidateNameField("after", after); err != nil {
			return nil, err
		}
	}
	return s.store.ListFlags(ctx, after, limit)
}

// UpdateFlag applies a partial update on behalf of actor, then invalidates
// the cached results for the flag and the flags that depend on it.
func (s *Service) UpdateFlag(ctx context.Context, actor, name string, patch flag.Patch) (flag.Flag, error) {
	if err := flag.ValidateName(name); err != nil {
		return flag.Flag{}, err
	}
	if patch.IsEmpty() {
		return flag.Flag{}, &flag.ValidationError{Field: "body", Message: "must set at least one of description or enabled"}
	}
	if patch.Description != nil {
		if err := flag.ValidateDescription(*patch.Description); err != nil {
			return flag.Flag{}, err
		}
	}
	f, affected, err := s.store.UpdateFlag(ctx, actor, name, patch)
	if err != nil {
		return flag.Flag{}, err
	}
	s.invalidate(affected)
	s.log.InfoContext(ctx, "flag updated", "flag", f.Name, "enabled", f.Enabled, "actor", actor, "changed", len(affected) > 0)
	return f, nil
}

// DeleteFlag deletes a flag on behalf of actor and invalidates its cached
// results.
func (s *Service) DeleteFlag(ctx context.Context, actor, name string) error {
	if err := flag.ValidateName(name); err != nil {
		return err
	}
	affected, err := s.store.DeleteFlag(ctx, actor, name)
	if err != nil {
		return err
	}
	s.invalidate(affected)
	s.log.InfoContext(ctx, "flag deleted", "flag", name, "actor", actor)
	return nil
}

// SetOverride forces a flag on or off for one user, on behalf of actor, and
// invalidates the cached results for the flag and its dependents.
func (s *Service) SetOverride(ctx context.Context, actor, name, userID string, enabled bool) (flag.Override, error) {
	if err := flag.ValidateName(name); err != nil {
		return flag.Override{}, err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return flag.Override{}, err
	}
	o, affected, err := s.store.SetOverride(ctx, actor, name, userID, enabled)
	if err != nil {
		return flag.Override{}, err
	}
	s.invalidate(affected)
	// The user ID is left out of the log on purpose.
	s.log.InfoContext(ctx, "override set", "flag", name, "enabled", enabled, "actor", actor, "changed", len(affected) > 0)
	return o, nil
}

// DeleteOverride removes a user's override on behalf of actor and
// invalidates the cached results for the flag and its dependents.
func (s *Service) DeleteOverride(ctx context.Context, actor, name, userID string) error {
	if err := flag.ValidateName(name); err != nil {
		return err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return err
	}
	affected, err := s.store.DeleteOverride(ctx, actor, name, userID)
	if err != nil {
		return err
	}
	s.invalidate(affected)
	s.log.InfoContext(ctx, "override removed", "flag", name, "actor", actor)
	return nil
}

// Evaluate reports whether the flag is on for the user.
//
// It answers from the cache when it can. On a miss it loads the flag data
// from the database, runs the evaluator, and caches the result. With
// bypassCache set, the lookup is skipped and the fresh result replaces
// whatever was cached. A flag that does not exist is never cached.
func (s *Service) Evaluate(ctx context.Context, name, userID string, bypassCache bool) (Evaluation, error) {
	if err := flag.ValidateName(name); err != nil {
		return Evaluation{}, err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return Evaluation{}, err
	}

	status := CacheBypass
	if !bypassCache {
		if result, ok := s.cache.Get(name, userID); ok {
			return Evaluation{Flag: name, UserID: userID, Enabled: result.Enabled, Reason: result.Reason, Cache: CacheHit}, nil
		}
		status = CacheMiss
	}

	// Note the generation before reading the database. If a write commits
	// and invalidates while we are computing, Set sees a newer generation
	// and discards our result instead of caching stale data.
	generation := s.cache.Generation()

	snap, err := s.store.EvaluationSnapshot(ctx, name, userID)
	if err != nil {
		return Evaluation{}, err
	}
	result, err := flag.Evaluate(name, userID, snap)
	if err != nil {
		return Evaluation{}, err
	}
	s.cache.Set(name, userID, result, generation)

	return Evaluation{Flag: name, UserID: userID, Enabled: result.Enabled, Reason: result.Reason, Cache: status}, nil
}

// auditLimit applies the default and the bounds for an audit page size.
func auditLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultAuditLimit, nil
	}
	if limit < 1 || limit > MaxAuditLimit {
		return 0, &flag.ValidationError{Field: "limit", Message: "must be from 1 to 200"}
	}
	return limit, nil
}

// Audit returns one page of audit entries for every flag, newest first.
// before is the id to continue from; zero starts at the newest entry.
func (s *Service) Audit(ctx context.Context, before int64, limit int) ([]flag.AuditEntry, error) {
	limit, err := auditLimit(limit)
	if err != nil {
		return nil, err
	}
	if before < 0 {
		return nil, &flag.ValidationError{Field: "before", Message: "must be a positive number"}
	}
	return s.store.ListAudit(ctx, "", before, limit)
}

// FlagAudit returns one page of audit entries for one flag, newest first.
// It works for deleted flags, whose history is kept. It returns
// flag.ErrNotFound only when the name has no history and no flag, meaning it
// never existed.
func (s *Service) FlagAudit(ctx context.Context, name string, before int64, limit int) ([]flag.AuditEntry, error) {
	if err := flag.ValidateName(name); err != nil {
		return nil, err
	}
	limit, err := auditLimit(limit)
	if err != nil {
		return nil, err
	}
	if before < 0 {
		return nil, &flag.ValidationError{Field: "before", Message: "must be a positive number"}
	}
	entries, err := s.store.ListAudit(ctx, name, before, limit)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 && before == 0 {
		exists, err := s.store.FlagExists(ctx, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, flag.ErrNotFound
		}
	}
	return entries, nil
}
