package flag

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"simple", "dark-mode", true},
		{"single character", "a", true},
		{"starts with digit", "2fa", true},
		{"dots and underscores", "checkout.new_flow-v2", true},
		{"exactly 64 characters", strings.Repeat("a", 64), true},
		{"empty", "", false},
		{"65 characters", strings.Repeat("a", 65), false},
		{"contains a space", "dark mode", false},
		{"leading space", " dark-mode", false},
		{"trailing space", "dark-mode ", false},
		{"trailing newline", "dark-mode\n", false},
		{"tab", "dark\tmode", false},
		{"upper case", "Dark-Mode", false},
		{"leading hyphen", "-dark-mode", false},
		{"leading dot", ".hidden", false},
		{"leading underscore", "_private", false},
		{"slash", "team/flag", false},
		{"colon", "team:flag", false},
		{"non-ASCII letter", "café", false},
		{"percent encoding", "dark%20mode", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.input)
			if tt.valid && err != nil {
				t.Errorf("ValidateName(%q) = %v, want valid", tt.input, err)
			}
			if !tt.valid {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("ValidateName(%q) = %v, want a ValidationError", tt.input, err)
				}
				if ve.Field != "name" {
					t.Errorf("Field = %q, want name", ve.Field)
				}
			}
		})
	}
}

func TestValidateNameFieldUsesTheGivenField(t *testing.T) {
	var ve *ValidationError
	if err := ValidateNameField("after", "Not Valid"); !errors.As(err, &ve) || ve.Field != "after" {
		t.Errorf("error = %v, want a ValidationError on field after", err)
	}
}

func TestValidateDescription(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"empty", "", true},
		{"plain text", "Enables the new checkout flow.", true},
		{"newlines and tabs", "line one\n\tline two\r\n", true},
		{"exactly 500 characters", strings.Repeat("d", 500), true},
		{"500 multi-byte characters", strings.Repeat("é", 500), true},
		{"501 characters", strings.Repeat("d", 501), false},
		{"NUL byte", "bad\x00text", false},
		{"escape character", "bad\x1btext", false},
		{"invalid UTF-8", "bad\xfftext", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDescription(tt.input)
			if tt.valid != (err == nil) {
				t.Errorf("ValidateDescription = %v, want valid=%v", err, tt.valid)
			}
		})
	}
}

func TestValidateUserID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"simple", "user-123", true},
		{"email", "alice@example.com", true},
		{"uuid", "81b8f217-3ab0-4e18-a065-1ca3d5a418e3", true},
		{"non-ASCII", "ユーザー1", true},
		{"exactly 128 characters", strings.Repeat("u", 128), true},
		{"empty", "", false},
		{"129 characters", strings.Repeat("u", 129), false},
		{"space", "user 123", false},
		{"newline", "user\n123", false},
		{"NUL byte", "user\x00123", false},
		{"non-breaking space", "user\u00a0123", false},
		{"invalid UTF-8", "user\xff", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUserID(tt.input)
			if tt.valid != (err == nil) {
				t.Errorf("ValidateUserID(%q) = %v, want valid=%v", tt.input, err, tt.valid)
			}
		})
	}
}

func TestPatchIsEmpty(t *testing.T) {
	enabled := true
	if !(Patch{}).IsEmpty() {
		t.Error("zero Patch should be empty")
	}
	if (Patch{Enabled: &enabled}).IsEmpty() {
		t.Error("Patch with Enabled set should not be empty")
	}
}
