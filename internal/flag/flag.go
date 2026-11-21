// Package flag holds the feature flag domain: types, validation rules,
// domain errors, and the evaluator. It performs no I/O.
package flag

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

// Input limits.
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 500
	MaxUserIDLength      = 128
)

// namePattern allows lowercase letters, digits, '.', '_' and '-', starting
// with a letter or digit, 1 to 64 characters. The same rule is enforced by a
// CHECK constraint in the database.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Flag is a feature flag with its global state and targeting data.
type Flag struct {
	Name        string
	Description string
	// Enabled is the global state. False is a kill switch for every user
	// without an override.
	Enabled bool
	// RolloutPercentage is the share of users, 0 to 100, for whom an enabled
	// flag is on. 100 means everyone.
	RolloutPercentage int
	// Prerequisites are the names of flags that must be on for a user before
	// this flag can be on for that user.
	Prerequisites []string
	// Overrides is populated only when a single flag is read in full.
	Overrides []Override
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Override forces a flag on or off for one user.
type Override struct {
	UserID    string
	Enabled   bool
	UpdatedAt time.Time
}

// Patch is a partial update. A nil field is left unchanged.
type Patch struct {
	Description *string
	Enabled     *bool
}

// IsEmpty reports whether the patch changes nothing.
func (p Patch) IsEmpty() bool {
	return p.Description == nil && p.Enabled == nil
}

// Domain errors. The HTTP layer maps these to status codes in one place.
var (
	ErrNotFound         = errors.New("flag not found")
	ErrAlreadyExists    = errors.New("flag already exists")
	ErrOverrideNotFound = errors.New("override not found")
	ErrHasDependents    = errors.New("flag is required by other flags")
)

// ValidationError reports a rejected input field.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ValidateName checks a flag name.
func ValidateName(name string) error {
	return validateName("name", name)
}

// ValidateNameField checks a flag name supplied in a differently named
// field, such as a pagination cursor.
func ValidateNameField(field, name string) error {
	return validateName(field, name)
}

func validateName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	if !namePattern.MatchString(name) {
		return invalid(field, "must be 1 to %d characters of lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", MaxNameLength)
	}
	return nil
}

// ValidateDescription checks a flag description.
func ValidateDescription(description string) error {
	if !utf8.ValidString(description) {
		return invalid("description", "must be valid UTF-8")
	}
	if utf8.RuneCountInString(description) > MaxDescriptionLength {
		return invalid("description", "must be at most %d characters", MaxDescriptionLength)
	}
	for _, r := range description {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return invalid("description", "must not contain control characters")
		}
	}
	return nil
}

// ValidateUserID checks a user identifier.
func ValidateUserID(userID string) error {
	if userID == "" {
		return invalid("user_id", "is required")
	}
	if !utf8.ValidString(userID) {
		return invalid("user_id", "must be valid UTF-8")
	}
	if utf8.RuneCountInString(userID) > MaxUserIDLength {
		return invalid("user_id", "must be at most %d characters", MaxUserIDLength)
	}
	for _, r := range userID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return invalid("user_id", "must not contain whitespace or control characters")
		}
	}
	return nil
}
