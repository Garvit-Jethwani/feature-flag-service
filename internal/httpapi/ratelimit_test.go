package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenBucketAllowsBurstThenRefills(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newTokenBucket(RateLimit{PerSecond: 2, Burst: 3})
	b.now = func() time.Time { return now }
	b.last = now

	// The full burst is available at once.
	for i := 0; i < 3; i++ {
		if ok, _ := b.allow(); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	// The next request is refused and told when a token will be ready: at
	// two tokens per second, half a second.
	ok, retryAfter := b.allow()
	if ok {
		t.Fatal("request beyond the burst was allowed")
	}
	if retryAfter != 500*time.Millisecond {
		t.Errorf("retryAfter = %v, want 500ms", retryAfter)
	}

	// After one second two tokens have been earned, and no more.
	now = now.Add(time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow(); !ok {
			t.Fatalf("request %d after refill was refused", i+1)
		}
	}
	if ok, _ := b.allow(); ok {
		t.Error("a third request after a one-second refill was allowed")
	}

	// A long idle period never stores more than the burst.
	now = now.Add(time.Hour)
	allowed := 0
	for i := 0; i < 10; i++ {
		if ok, _ := b.allow(); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("%d requests allowed after a long idle period, want the burst of 3", allowed)
	}
}

func TestDisabledRateLimitAllowsEverything(t *testing.T) {
	b := newTokenBucket(RateLimit{PerSecond: 0, Burst: 1})
	for i := 0; i < 1000; i++ {
		if ok, _ := b.allow(); !ok {
			t.Fatal("a disabled limiter refused a request")
		}
	}
}

// Over HTTP: a key that exceeds its limit gets 429 with Retry-After, and
// another key is unaffected because each key has its own bucket.
func TestRateLimitIsPerKey(t *testing.T) {
	auth, err := NewAuthenticator(
		"alice:admin:"+testAdminToken+",checkout:reader:"+testReaderToken,
		RateLimit{PerSecond: 0.001, Burst: 2}, // effectively no refill during the test
	)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	handler := auth.require(RoleReader, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	call := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < 2; i++ {
		if rec := call(testAdminToken); rec.Code != http.StatusOK {
			t.Fatalf("request %d within the burst: status %d", i+1, rec.Code)
		}
	}
	limited := call(testAdminToken)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("request beyond the burst: status %d, want 429", limited.Code)
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Error("429 response is missing Retry-After")
	}

	if rec := call(testReaderToken); rec.Code != http.StatusOK {
		t.Errorf("a different key was limited too: status %d", rec.Code)
	}
}
