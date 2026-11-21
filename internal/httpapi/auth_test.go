package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fake tokens for tests only.
const (
	testAdminToken  = "test-admin-token-0000000000000000000000"
	testReaderToken = "test-reader-token-000000000000000000000"
)

func testAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	auth, err := NewAuthenticator("alice:admin:" + testAdminToken + ", checkout:reader:" + testReaderToken)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return auth
}

func TestNewAuthenticatorRejectsBadSpecs(t *testing.T) {
	long := strings.Repeat("x", MinTokenLength)
	tests := []struct {
		name string
		spec string
		want string
	}{
		{"empty", "", "at least one key"},
		{"only separators", " , ,", "at least one key"},
		{"missing token", "alice:admin", "want actor:role:token"},
		{"unknown role", "alice:owner:" + long, "role must be admin or reader"},
		{"bad actor", "al ice:admin:" + long, "actor must be"},
		{"short token", "alice:admin:" + strings.Repeat("x", MinTokenLength-1), "at least 32 characters"},
		{"token with whitespace", "alice:admin:" + long + " tail", "must not contain whitespace"},
		{"duplicate token", "alice:admin:" + long + ",bob:reader:" + long, "already used"},
		{"no admin", "app:reader:" + long, "at least one admin key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAuthenticator(tt.spec)
			if err == nil {
				t.Fatal("NewAuthenticator succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), long) {
				t.Errorf("error leaks a token: %q", err)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	auth := testAuthenticator(t)

	if p, ok := auth.Authenticate(testAdminToken); !ok || p.Actor != "alice" || p.Role != RoleAdmin {
		t.Errorf("admin token: got %+v, %v", p, ok)
	}
	if p, ok := auth.Authenticate(testReaderToken); !ok || p.Actor != "checkout" || p.Role != RoleReader {
		t.Errorf("reader token: got %+v, %v", p, ok)
	}
	for _, token := range []string{"", "wrong", testAdminToken + "x", testAdminToken[:len(testAdminToken)-1]} {
		if _, ok := auth.Authenticate(token); ok {
			t.Errorf("token %q was accepted", token)
		}
	}
}

func TestRequire(t *testing.T) {
	auth := testAuthenticator(t)

	tests := []struct {
		name       string
		role       Role
		header     string
		wantStatus int
		wantActor  string
	}{
		{"no header", RoleReader, "", http.StatusUnauthorized, ""},
		{"wrong scheme", RoleReader, "Basic " + testAdminToken, http.StatusUnauthorized, ""},
		{"scheme without token", RoleReader, "Bearer ", http.StatusUnauthorized, ""},
		{"unknown token", RoleReader, "Bearer " + strings.Repeat("z", 40), http.StatusUnauthorized, ""},
		{"reader on a read", RoleReader, "Bearer " + testReaderToken, http.StatusOK, "checkout"},
		{"admin on a read", RoleReader, "Bearer " + testAdminToken, http.StatusOK, "alice"},
		{"reader on a write", RoleAdmin, "Bearer " + testReaderToken, http.StatusForbidden, ""},
		{"admin on a write", RoleAdmin, "Bearer " + testAdminToken, http.StatusOK, "alice"},
		{"scheme is case-insensitive", RoleAdmin, "bearer " + testAdminToken, http.StatusOK, "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotActor := ""
			handler := auth.require(tt.role, func(w http.ResponseWriter, r *http.Request) {
				gotActor = principalFrom(r.Context()).Actor
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/flags", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if gotActor != tt.wantActor {
				t.Errorf("actor seen by the handler = %q, want %q", gotActor, tt.wantActor)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 response is missing WWW-Authenticate")
			}
			if strings.Contains(rec.Body.String(), testAdminToken) || strings.Contains(rec.Body.String(), testReaderToken) {
				t.Error("response body echoes a token")
			}
		})
	}
}
