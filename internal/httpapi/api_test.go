package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/cache"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/service"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store/storetest"
)

// lockedBuffer is a bytes.Buffer safe for concurrent use, for capturing logs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testServer is a running API plus helpers to call it.
type testServer struct {
	t    *testing.T
	srv  *httptest.Server
	logs *lockedBuffer
	pool *pgxpool.Pool // nil for servers without a database
}

// newTestCache returns a cache like the production one.
func newTestCache() *cache.Memory {
	return cache.NewMemory(time.Minute, 1000)
}

// response is a decoded API response.
type response struct {
	status int
	header http.Header
	body   map[string]any
}

// errorCode returns error.code from an error response, or "".
func (r response) errorCode() string {
	detail, _ := r.body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}

// errorMessage returns error.message from an error response, or "".
func (r response) errorMessage() string {
	detail, _ := r.body["error"].(map[string]any)
	message, _ := detail["message"].(string)
	return message
}

func startServer(t *testing.T, handler func(logger *slog.Logger) http.Handler) *testServer {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(handler(logger))
	t.Cleanup(srv.Close)
	return &testServer{t: t, srv: srv, logs: logs}
}

// newDatabaseServer runs the full stack against real Postgres. The test is
// skipped when no test database is configured.
func newDatabaseServer(t *testing.T) *testServer {
	t.Helper()
	pool := storetest.NewPool(t)
	flagStore := store.New(pool)
	auth := testAuthenticator(t)
	s := startServer(t, func(logger *slog.Logger) http.Handler {
		return New(service.New(flagStore, newTestCache(), logger), auth, logger, flagStore.Ping)
	})
	s.pool = pool
	return s
}

// newStorelessServer runs the HTTP layer with no store, for requests that
// must be answered before any data is touched.
func newStorelessServer(t *testing.T, ready func(context.Context) error) *testServer {
	t.Helper()
	auth := testAuthenticator(t)
	return startServer(t, func(logger *slog.Logger) http.Handler {
		return New(service.New(nil, newTestCache(), logger), auth, logger, ready)
	})
}

// do sends a request. body may be nil, a string sent as is, or any value
// encoded as JSON. A Content-Type of application/json is set whenever there
// is a body.
func (s *testServer) do(method, path, token string, body any) response {
	s.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			s.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.srv.URL+path, reader)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.send(req, token)
}

func (s *testServer) send(req *http.Request, token string) response {
	s.t.Helper()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.srv.Client().Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		s.t.Fatalf("read response: %v", err)
	}
	out := response{status: res.StatusCode, header: res.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			s.t.Fatalf("%s %s: response is not a JSON object: %q", req.Method, req.URL.Path, raw)
		}
	}
	return out
}

func (s *testServer) mustCreate(name string, enabled bool) {
	s.t.Helper()
	res := s.do(http.MethodPost, "/v1/flags", testAdminToken, map[string]any{"name": name, "enabled": enabled})
	if res.status != http.StatusCreated {
		s.t.Fatalf("create %q: status %d, body %v", name, res.status, res.body)
	}
}

// wantEvaluation evaluates a flag for a user and checks the outcome.
func (s *testServer) wantEvaluation(name, userID string, enabled bool, reason string) {
	s.t.Helper()
	res := s.do(http.MethodGet, "/v1/flags/"+name+"/evaluate?user_id="+userID, testReaderToken, nil)
	if res.status != http.StatusOK {
		s.t.Fatalf("evaluate %s for %s: status %d, body %v", name, userID, res.status, res.body)
	}
	if res.body["enabled"] != enabled || res.body["reason"] != reason {
		s.t.Errorf("evaluate %s for %s: got enabled=%v reason=%v, want enabled=%v reason=%s",
			name, userID, res.body["enabled"], res.body["reason"], enabled, reason)
	}
	if res.body["flag"] != name || res.body["user_id"] != userID {
		s.t.Errorf("evaluate echoed flag=%v user_id=%v, want %s and %s", res.body["flag"], res.body["user_id"], name, userID)
	}
}

// TestFlagLifecycle drives one flag through the HTTP API end to end: create,
// read, toggle globally, override per user, evaluate at each step, delete.
func TestFlagLifecycle(t *testing.T) {
	s := newDatabaseServer(t)

	created := s.do(http.MethodPost, "/v1/flags", testAdminToken, map[string]any{
		"name":        "new-checkout",
		"description": "New checkout flow",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: status %d, body %v", created.status, created.body)
	}
	if got := created.header.Get("Location"); got != "/v1/flags/new-checkout" {
		t.Errorf("Location = %q, want /v1/flags/new-checkout", got)
	}
	if created.body["enabled"] != false {
		t.Errorf("a new flag should be disabled by default, got enabled=%v", created.body["enabled"])
	}
	if created.body["rollout_percentage"] != float64(100) {
		t.Errorf("rollout_percentage = %v, want 100", created.body["rollout_percentage"])
	}
	for _, field := range []string{"overrides", "prerequisites"} {
		if list, ok := created.body[field].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty list", field, created.body[field])
		}
	}

	// Globally off: off for everyone.
	s.wantEvaluation("new-checkout", "u1", false, "global_disabled")

	// Toggle globally on: on for everyone.
	patched := s.do(http.MethodPatch, "/v1/flags/new-checkout", testAdminToken, map[string]any{"enabled": true})
	if patched.status != http.StatusOK || patched.body["enabled"] != true {
		t.Fatalf("patch: status %d, body %v", patched.status, patched.body)
	}
	if patched.body["description"] != "New checkout flow" {
		t.Errorf("patching enabled changed the description to %v", patched.body["description"])
	}
	s.wantEvaluation("new-checkout", "u1", true, "global_enabled")

	// Override u1 to off: the override wins for u1 only.
	override := s.do(http.MethodPut, "/v1/flags/new-checkout/overrides/u1", testAdminToken, map[string]any{"enabled": false})
	if override.status != http.StatusOK || override.body["user_id"] != "u1" || override.body["enabled"] != false {
		t.Fatalf("set override: status %d, body %v", override.status, override.body)
	}
	s.wantEvaluation("new-checkout", "u1", false, "user_override")
	s.wantEvaluation("new-checkout", "u2", true, "global_enabled")

	// Toggle globally off and override u2 to on: again the override wins.
	s.do(http.MethodPatch, "/v1/flags/new-checkout", testAdminToken, map[string]any{"enabled": false})
	s.do(http.MethodPut, "/v1/flags/new-checkout/overrides/u2", testAdminToken, map[string]any{"enabled": true})
	s.wantEvaluation("new-checkout", "u2", true, "user_override")
	s.wantEvaluation("new-checkout", "u3", false, "global_disabled")

	// The flag read lists both overrides.
	read := s.do(http.MethodGet, "/v1/flags/new-checkout", testReaderToken, nil)
	if read.status != http.StatusOK {
		t.Fatalf("read: status %d, body %v", read.status, read.body)
	}
	if overrides, _ := read.body["overrides"].([]any); len(overrides) != 2 {
		t.Errorf("read shows %d overrides, want 2: %v", len(overrides), read.body["overrides"])
	}

	// Removing an override returns the user to the global state.
	if res := s.do(http.MethodDelete, "/v1/flags/new-checkout/overrides/u2", testAdminToken, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete override: status %d, body %v", res.status, res.body)
	}
	s.wantEvaluation("new-checkout", "u2", false, "global_disabled")

	// Delete: the flag and its evaluation are gone, with 404 rather than 500.
	if res := s.do(http.MethodDelete, "/v1/flags/new-checkout", testAdminToken, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %v", res.status, res.body)
	}
	for _, path := range []string{"/v1/flags/new-checkout", "/v1/flags/new-checkout/evaluate?user_id=u1"} {
		res := s.do(http.MethodGet, path, testReaderToken, nil)
		if res.status != http.StatusNotFound || res.errorCode() != "flag_not_found" {
			t.Errorf("GET %s after delete: status %d code %q, want 404 flag_not_found", path, res.status, res.errorCode())
		}
	}
}

func TestErrorResponses(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("existing", true)

	object := func(pairs ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(pairs); i += 2 {
			m[pairs[i].(string)] = pairs[i+1]
		}
		return m
	}

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		// A missing flag is always 404.
		{"read missing flag", "GET", "/v1/flags/missing", nil, 404, "flag_not_found"},
		{"evaluate missing flag", "GET", "/v1/flags/missing/evaluate?user_id=u1", nil, 404, "flag_not_found"},
		{"update missing flag", "PATCH", "/v1/flags/missing", object("enabled", true), 404, "flag_not_found"},
		{"delete missing flag", "DELETE", "/v1/flags/missing", nil, 404, "flag_not_found"},
		{"override on missing flag", "PUT", "/v1/flags/missing/overrides/u1", object("enabled", true), 404, "flag_not_found"},
		{"remove override on missing flag", "DELETE", "/v1/flags/missing/overrides/u1", nil, 404, "flag_not_found"},
		{"remove missing override", "DELETE", "/v1/flags/existing/overrides/nobody", nil, 404, "override_not_found"},

		{"duplicate name", "POST", "/v1/flags", object("name", "existing"), 409, "flag_exists"},

		// Flag name validation.
		{"name with a space", "POST", "/v1/flags", object("name", "dark mode"), 400, "invalid_argument"},
		{"name in upper case", "POST", "/v1/flags", object("name", "Dark-Mode"), 400, "invalid_argument"},
		{"name too long", "POST", "/v1/flags", object("name", strings.Repeat("a", 65)), 400, "invalid_argument"},
		{"name missing", "POST", "/v1/flags", object("description", "no name"), 400, "invalid_argument"},
		{"description too long", "POST", "/v1/flags", object("name", "ok", "description", strings.Repeat("d", 501)), 400, "invalid_argument"},
		{"invalid name in the path", "GET", "/v1/flags/Dark%20Mode", nil, 400, "invalid_argument"},
		{"invalid name when evaluating", "GET", "/v1/flags/Dark%20Mode/evaluate?user_id=u1", nil, 400, "invalid_argument"},

		// Body handling.
		{"unknown field", "POST", "/v1/flags", object("name", "ok", "owner", "me"), 400, "invalid_request"},
		{"rollout not accepted yet", "POST", "/v1/flags", object("name", "ok", "rollout_percentage", 50), 400, "invalid_request"},
		{"client-supplied actor rejected", "PATCH", "/v1/flags/existing", object("enabled", false, "actor", "mallory"), 400, "invalid_request"},
		{"malformed JSON", "POST", "/v1/flags", `{"name": `, 400, "invalid_request"},
		{"empty body", "POST", "/v1/flags", ``, 400, "invalid_request"},
		{"two JSON objects", "POST", "/v1/flags", `{"name":"a"}{"name":"b"}`, 400, "invalid_request"},
		{"array body", "POST", "/v1/flags", `[]`, 400, "invalid_request"},
		{"wrong field type", "POST", "/v1/flags", object("name", "ok", "enabled", "yes"), 400, "invalid_request"},
		{"body over 1 MB", "POST", "/v1/flags", object("name", "ok", "description", strings.Repeat("d", 1<<20)), 413, "body_too_large"},
		{"empty patch", "PATCH", "/v1/flags/existing", object(), 400, "invalid_argument"},
		{"override without enabled", "PUT", "/v1/flags/existing/overrides/u1", object(), 400, "invalid_argument"},

		// User ID validation.
		{"evaluate without user_id", "GET", "/v1/flags/existing/evaluate", nil, 400, "invalid_argument"},
		{"evaluate with a space in user_id", "GET", "/v1/flags/existing/evaluate?user_id=a%20b", nil, 400, "invalid_argument"},
		{"evaluate with an overlong user_id", "GET", "/v1/flags/existing/evaluate?user_id=" + strings.Repeat("u", 129), nil, 400, "invalid_argument"},
		{"override with a space in user ID", "PUT", "/v1/flags/existing/overrides/a%20b", object("enabled", true), 400, "invalid_argument"},

		// List parameters.
		{"limit of zero", "GET", "/v1/flags?limit=0", nil, 400, "invalid_argument"},
		{"limit too large", "GET", "/v1/flags?limit=501", nil, 400, "invalid_argument"},
		{"limit not a number", "GET", "/v1/flags?limit=many", nil, 400, "invalid_argument"},
		{"invalid cursor", "GET", "/v1/flags?after=Not%20Valid", nil, 400, "invalid_argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := s.do(tt.method, tt.path, testAdminToken, tt.body)
			if res.status != tt.wantStatus || res.errorCode() != tt.wantCode {
				t.Errorf("got status %d code %q, want %d %q (body %v)", res.status, res.errorCode(), tt.wantStatus, tt.wantCode, res.body)
			}
			if res.errorMessage() == "" {
				t.Error("error response has no message")
			}
		})
	}

	// None of the rejected writes may have changed the existing flag.
	res := s.do(http.MethodGet, "/v1/flags/existing", testReaderToken, nil)
	if res.body["enabled"] != true {
		t.Errorf("existing flag was modified by a rejected request: %v", res.body)
	}
}

func TestWrongContentType(t *testing.T) {
	s := newDatabaseServer(t)
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req, err := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/flags", strings.NewReader(`{"name":"ok"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res := s.send(req, testAdminToken)
		if res.status != http.StatusUnsupportedMediaType || res.errorCode() != "unsupported_media_type" {
			t.Errorf("Content-Type %q: status %d code %q, want 415", contentType, res.status, res.errorCode())
		}
	}
}

func TestListFlagsPagination(t *testing.T) {
	s := newDatabaseServer(t)
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		s.mustCreate(name, false)
	}

	names := func(res response) []string {
		var out []string
		flags, _ := res.body["flags"].([]any)
		for _, f := range flags {
			out = append(out, f.(map[string]any)["name"].(string))
		}
		return out
	}

	first := s.do(http.MethodGet, "/v1/flags?limit=2", testReaderToken, nil)
	if got := names(first); len(got) != 2 || got[0] != "alpha" || got[1] != "bravo" {
		t.Fatalf("first page = %v, want [alpha bravo]", got)
	}
	if first.body["next_after"] != "bravo" {
		t.Fatalf("next_after = %v, want bravo", first.body["next_after"])
	}

	second := s.do(http.MethodGet, "/v1/flags?limit=2&after=bravo", testReaderToken, nil)
	if got := names(second); len(got) != 1 || got[0] != "charlie" {
		t.Errorf("second page = %v, want [charlie]", got)
	}
	if _, more := second.body["next_after"]; more {
		t.Errorf("last page should not have next_after, got %v", second.body["next_after"])
	}

	// The list does not include per-user overrides.
	if flags, _ := first.body["flags"].([]any); len(flags) > 0 {
		if _, has := flags[0].(map[string]any)["overrides"]; has {
			t.Error("list entries should not include overrides")
		}
	}
}

func TestAuthorizationOverHTTP(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("existing", true)
	body := map[string]any{"name": "from-reader"}

	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		body       any
		wantStatus int
	}{
		{"read without a key", "GET", "/v1/flags/existing", "", nil, 401},
		{"evaluate without a key", "GET", "/v1/flags/existing/evaluate?user_id=u1", "", nil, 401},
		{"list with an unknown key", "GET", "/v1/flags", strings.Repeat("z", 40), nil, 401},
		{"create without a key", "POST", "/v1/flags", "", body, 401},
		{"reader may list", "GET", "/v1/flags", testReaderToken, nil, 200},
		{"reader may read", "GET", "/v1/flags/existing", testReaderToken, nil, 200},
		{"reader may evaluate", "GET", "/v1/flags/existing/evaluate?user_id=u1", testReaderToken, nil, 200},
		{"reader may not create", "POST", "/v1/flags", testReaderToken, body, 403},
		{"reader may not update", "PATCH", "/v1/flags/existing", testReaderToken, map[string]any{"enabled": false}, 403},
		{"reader may not delete", "DELETE", "/v1/flags/existing", testReaderToken, nil, 403},
		{"reader may not set an override", "PUT", "/v1/flags/existing/overrides/u1", testReaderToken, map[string]any{"enabled": false}, 403},
		{"reader may not remove an override", "DELETE", "/v1/flags/existing/overrides/u1", testReaderToken, nil, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if res := s.do(tt.method, tt.path, tt.token, tt.body); res.status != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %v)", res.status, tt.wantStatus, res.body)
			}
		})
	}

	// The refused writes changed nothing.
	if res := s.do(http.MethodGet, "/v1/flags/from-reader", testAdminToken, nil); res.status != http.StatusNotFound {
		t.Errorf("a flag was created by a refused request: status %d", res.status)
	}
	s.wantEvaluation("existing", "u1", true, "global_enabled")
}

func TestLogsDoNotContainUserIDsOrTokens(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("existing", true)
	const userID = "private-user-4242"

	s.do(http.MethodPut, "/v1/flags/existing/overrides/"+userID, testAdminToken, map[string]any{"enabled": false})
	s.wantEvaluation("existing", userID, false, "user_override")
	s.do(http.MethodGet, "/v1/flags/existing/evaluate?user_id="+userID, strings.Repeat("z", 40), nil)
	s.do(http.MethodDelete, "/v1/flags/existing/overrides/"+userID, testAdminToken, nil)

	s.srv.Close() // waits for in-flight requests, so every log line is written
	logs := s.logs.String()
	if !strings.Contains(logs, `"route":"PUT /v1/flags/{name}/overrides/{userID}"`) {
		t.Fatalf("access log is missing the override request; logs:\n%s", logs)
	}
	for _, secret := range []string{userID, testAdminToken, testReaderToken, strings.Repeat("z", 40)} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
	if !strings.Contains(logs, `"actor":"alice"`) {
		t.Error("access log does not record the authenticated actor")
	}
}

func TestHealthAndReadiness(t *testing.T) {
	healthy := newStorelessServer(t, func(context.Context) error { return nil })
	for _, path := range []string{"/healthz", "/readyz"} {
		if res := healthy.do(http.MethodGet, path, "", nil); res.status != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200 without a key", path, res.status)
		}
	}

	down := newStorelessServer(t, func(context.Context) error { return errors.New("connection refused") })
	if res := down.do(http.MethodGet, "/healthz", "", nil); res.status != http.StatusOK {
		t.Errorf("/healthz with the database down: status %d, want 200", res.status)
	}
	res := down.do(http.MethodGet, "/readyz", "", nil)
	if res.status != http.StatusServiceUnavailable || res.errorCode() != "not_ready" {
		t.Errorf("/readyz with the database down: status %d code %q, want 503 not_ready", res.status, res.errorCode())
	}
	if strings.Contains(res.errorMessage(), "connection refused") {
		t.Error("/readyz leaks the underlying error to the client")
	}
}

func TestUnmatchedRequestsUseTheJSONErrorShape(t *testing.T) {
	s := newStorelessServer(t, func(context.Context) error { return nil })

	notFound := s.do(http.MethodGet, "/v2/anything", testAdminToken, nil)
	if notFound.status != http.StatusNotFound || notFound.errorCode() != "not_found" {
		t.Errorf("unknown path: status %d code %q, want 404 not_found", notFound.status, notFound.errorCode())
	}

	wrongMethod := s.do(http.MethodPut, "/v1/flags", testAdminToken, map[string]any{})
	if wrongMethod.status != http.StatusMethodNotAllowed || wrongMethod.errorCode() != "method_not_allowed" {
		t.Errorf("wrong method: status %d code %q, want 405 method_not_allowed", wrongMethod.status, wrongMethod.errorCode())
	}
	if allow := wrongMethod.header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q, want it to list GET and POST", allow)
	}
}

func TestCommonResponseHeaders(t *testing.T) {
	s := newStorelessServer(t, func(context.Context) error { return nil })
	res := s.do(http.MethodGet, "/healthz", "", nil)

	if res.header.Get("X-Request-ID") == "" {
		t.Error("response is missing X-Request-ID")
	}
	if got := res.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestPanicBecomesA500(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	handler := observe(logger, recoverPanic(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/flags", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value was sent to the client")
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Error("the panic was not logged")
	}
}
