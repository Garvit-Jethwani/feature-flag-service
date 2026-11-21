package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// Role is what an API key is allowed to do.
type Role string

// Roles. An admin may call every endpoint; a reader may only read and
// evaluate.
const (
	RoleAdmin  Role = "admin"
	RoleReader Role = "reader"
)

// MinTokenLength is the shortest API token the service accepts.
const MinTokenLength = 32

// Principal is the authenticated caller. Actor is recorded in the audit
// trail; it comes from the key's configuration, never from the request.
type Principal struct {
	Actor string
	Role  Role
}

var actorPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

type apiKey struct {
	tokenHash [sha256.Size]byte
	principal Principal
}

// Authenticator checks Bearer tokens against the configured API keys. Only
// SHA-256 hashes of the tokens are kept in memory.
type Authenticator struct {
	keys []apiKey
}

// NewAuthenticator parses a comma-separated list of actor:role:token entries.
// It rejects malformed entries, unknown roles, short or duplicate tokens, and
// a list without an admin key. Errors identify entries by position and never
// include a token.
func NewAuthenticator(spec string) (*Authenticator, error) {
	a := &Authenticator{}
	seen := map[[sha256.Size]byte]bool{}
	hasAdmin := false
	position := 0

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		position++

		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("API_KEYS entry %d: want actor:role:token", position)
		}
		actor, role, token := parts[0], Role(parts[1]), parts[2]

		if !actorPattern.MatchString(actor) {
			return nil, fmt.Errorf("API_KEYS entry %d: actor must be 1 to 64 letters, digits, '.', '_', '@' or '-'", position)
		}
		if role != RoleAdmin && role != RoleReader {
			return nil, fmt.Errorf("API_KEYS entry %d: role must be admin or reader", position)
		}
		if len(token) < MinTokenLength {
			return nil, fmt.Errorf("API_KEYS entry %d: token must be at least %d characters", position, MinTokenLength)
		}
		if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("API_KEYS entry %d: token must not contain whitespace", position)
		}

		hash := sha256.Sum256([]byte(token))
		if seen[hash] {
			return nil, fmt.Errorf("API_KEYS entry %d: token is already used by another entry", position)
		}
		seen[hash] = true
		hasAdmin = hasAdmin || role == RoleAdmin
		a.keys = append(a.keys, apiKey{tokenHash: hash, principal: Principal{Actor: actor, Role: role}})
	}

	if len(a.keys) == 0 {
		return nil, errors.New("API_KEYS: at least one key is required")
	}
	if !hasAdmin {
		return nil, errors.New("API_KEYS: at least one admin key is required")
	}
	return a, nil
}

// Authenticate returns the principal for a token. Hashes are compared in
// constant time and every key is checked, so the time taken does not reveal
// which key, if any, came close to matching.
func (a *Authenticator) Authenticate(token string) (Principal, bool) {
	hash := sha256.Sum256([]byte(token))
	var principal Principal
	found := false
	for _, key := range a.keys {
		if subtle.ConstantTimeCompare(hash[:], key.tokenHash[:]) == 1 {
			principal = key.principal
			found = true
		}
	}
	return principal, found
}

type principalKey struct{}

// principalFrom returns the authenticated caller stored by require.
func principalFrom(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalKey{}).(Principal)
	return principal
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// require wraps next so that it runs only for an authenticated caller holding
// at least the given role. RoleReader admits any valid key; RoleAdmin admits
// only admin keys. It answers 401 without valid credentials and 403 when the
// key is valid but lacks the role.
func (a *Authenticator) require(role Role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="feature-flags"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a Bearer API key is required")
			return
		}
		principal, ok := a.Authenticate(token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="feature-flags", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "the API key is not valid")
			return
		}
		if info := requestInfoFrom(r.Context()); info != nil {
			info.actor = principal.Actor
		}
		if role == RoleAdmin && principal.Role != RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "this API key may not change flags")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}
