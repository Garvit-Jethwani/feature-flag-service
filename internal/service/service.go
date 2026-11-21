// Package service implements the flag use cases: it validates input, calls
// the store, and evaluates flags. Writes take the acting identity so that the
// audit trail can record it.
package service

import (
	"context"
	"log/slog"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// List page sizes.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// Store is the persistence the service needs.
type Store interface {
	CreateFlag(ctx context.Context, name, description string, enabled bool) (flag.Flag, error)
	GetFlag(ctx context.Context, name string) (flag.Flag, error)
	ListFlags(ctx context.Context, after string, limit int) ([]flag.Flag, error)
	UpdateFlag(ctx context.Context, name string, patch flag.Patch) (flag.Flag, error)
	DeleteFlag(ctx context.Context, name string) error
	SetOverride(ctx context.Context, name, userID string, enabled bool) (flag.Override, error)
	DeleteOverride(ctx context.Context, name, userID string) error
	EvaluationSnapshot(ctx context.Context, name, userID string) (flag.Snapshot, error)
}

// Service implements the flag use cases.
type Service struct {
	store Store
	log   *slog.Logger
}

// New returns a Service.
func New(store Store, logger *slog.Logger) *Service {
	return &Service{store: store, log: logger}
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
}

// CreateFlag creates a flag on behalf of actor.
func (s *Service) CreateFlag(ctx context.Context, actor string, in CreateInput) (flag.Flag, error) {
	if err := flag.ValidateName(in.Name); err != nil {
		return flag.Flag{}, err
	}
	if err := flag.ValidateDescription(in.Description); err != nil {
		return flag.Flag{}, err
	}
	f, err := s.store.CreateFlag(ctx, in.Name, in.Description, in.Enabled)
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

// UpdateFlag applies a partial update on behalf of actor.
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
	f, err := s.store.UpdateFlag(ctx, name, patch)
	if err != nil {
		return flag.Flag{}, err
	}
	s.log.InfoContext(ctx, "flag updated", "flag", f.Name, "enabled", f.Enabled, "actor", actor)
	return f, nil
}

// DeleteFlag deletes a flag on behalf of actor.
func (s *Service) DeleteFlag(ctx context.Context, actor, name string) error {
	if err := flag.ValidateName(name); err != nil {
		return err
	}
	if err := s.store.DeleteFlag(ctx, name); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "flag deleted", "flag", name, "actor", actor)
	return nil
}

// SetOverride forces a flag on or off for one user, on behalf of actor.
func (s *Service) SetOverride(ctx context.Context, actor, name, userID string, enabled bool) (flag.Override, error) {
	if err := flag.ValidateName(name); err != nil {
		return flag.Override{}, err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return flag.Override{}, err
	}
	o, err := s.store.SetOverride(ctx, name, userID, enabled)
	if err != nil {
		return flag.Override{}, err
	}
	s.log.InfoContext(ctx, "override set", "flag", name, "enabled", enabled, "actor", actor)
	return o, nil
}

// DeleteOverride removes a user's override on behalf of actor.
func (s *Service) DeleteOverride(ctx context.Context, actor, name, userID string) error {
	if err := flag.ValidateName(name); err != nil {
		return err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return err
	}
	if err := s.store.DeleteOverride(ctx, name, userID); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "override removed", "flag", name, "actor", actor)
	return nil
}

// Evaluate reports whether the flag is on for the user.
func (s *Service) Evaluate(ctx context.Context, name, userID string) (Evaluation, error) {
	if err := flag.ValidateName(name); err != nil {
		return Evaluation{}, err
	}
	if err := flag.ValidateUserID(userID); err != nil {
		return Evaluation{}, err
	}
	snap, err := s.store.EvaluationSnapshot(ctx, name, userID)
	if err != nil {
		return Evaluation{}, err
	}
	result, err := flag.Evaluate(name, userID, snap)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Flag: name, UserID: userID, Enabled: result.Enabled, Reason: result.Reason}, nil
}
