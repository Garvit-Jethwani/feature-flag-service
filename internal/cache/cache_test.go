package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

var (
	onResult  = flag.Result{Enabled: true, Reason: flag.ReasonGlobalEnabled}
	offResult = flag.Result{Enabled: false, Reason: flag.ReasonGlobalDisabled}
)

// newTestCache returns a cache with a clock the test can move forward.
func newTestCache(ttl time.Duration, maxEntries int) (*Memory, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewMemory(ttl, maxEntries)
	c.now = func() time.Time { return now }
	return c, &now
}

func TestHitUntilTTLExpires(t *testing.T) {
	c, now := newTestCache(time.Minute, 10)

	if _, ok := c.Get("f", "u1"); ok {
		t.Fatal("empty cache returned a result")
	}
	c.Set("f", "u1", onResult, c.Generation())
	if got, ok := c.Get("f", "u1"); !ok || got != onResult {
		t.Fatalf("after Set: got %+v, %v; want a hit", got, ok)
	}
	if _, ok := c.Get("f", "u2"); ok {
		t.Error("a result for u1 was returned for u2")
	}

	*now = now.Add(59 * time.Second)
	if _, ok := c.Get("f", "u1"); !ok {
		t.Error("entry expired before its TTL")
	}
	*now = now.Add(time.Second) // exactly at the TTL
	if _, ok := c.Get("f", "u1"); ok {
		t.Error("entry was returned after its TTL")
	}
	if c.Len() != 0 {
		t.Errorf("expired entry was not removed: Len = %d", c.Len())
	}
}

func TestInvalidateDropsOnlyTheNamedFlags(t *testing.T) {
	c, _ := newTestCache(time.Minute, 10)
	gen := c.Generation()
	c.Set("a", "u1", onResult, gen)
	c.Set("a", "u2", onResult, gen)
	c.Set("b", "u1", offResult, gen)

	c.Invalidate("a")

	if _, ok := c.Get("a", "u1"); ok {
		t.Error("a/u1 survived invalidation of a")
	}
	if _, ok := c.Get("a", "u2"); ok {
		t.Error("a/u2 survived invalidation of a")
	}
	if got, ok := c.Get("b", "u1"); !ok || got != offResult {
		t.Error("b/u1 was dropped by invalidation of a")
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}

// A reader that loaded data before a flag changed must not be able to store
// its result after the invalidation.
func TestSetWithOldGenerationIsIgnored(t *testing.T) {
	c, _ := newTestCache(time.Minute, 10)

	gen := c.Generation() // reader starts, then queries the database
	c.Invalidate("f")     // meanwhile the flag changes
	c.Set("f", "u1", onResult, gen)

	if _, ok := c.Get("f", "u1"); ok {
		t.Fatal("a stale result was stored after an invalidation")
	}

	// A reader that starts after the invalidation stores normally.
	c.Set("f", "u1", offResult, c.Generation())
	if got, ok := c.Get("f", "u1"); !ok || got != offResult {
		t.Errorf("fresh result not stored: got %+v, %v", got, ok)
	}
}

func TestEntryLimitHolds(t *testing.T) {
	c, _ := newTestCache(time.Minute, 5)
	gen := c.Generation()
	for i := 0; i < 50; i++ {
		c.Set(fmt.Sprintf("flag-%d", i%3), fmt.Sprintf("user-%d", i), onResult, gen)
		if c.Len() > 5 {
			t.Fatalf("Len = %d after %d sets, limit is 5", c.Len(), i+1)
		}
	}
	// The newest entry is always present; eviction made room for it.
	if _, ok := c.Get("flag-1", "user-49"); !ok {
		t.Error("the most recently stored entry is missing")
	}
	// Overwriting an existing entry does not grow the cache.
	c.Set("flag-1", "user-49", offResult, gen)
	if c.Len() != 5 {
		t.Errorf("Len = %d after overwriting, want 5", c.Len())
	}
}

func TestZeroTTLDisablesCaching(t *testing.T) {
	c := NewMemory(0, 10)
	c.Set("f", "u1", onResult, c.Generation())
	if _, ok := c.Get("f", "u1"); ok {
		t.Error("a disabled cache returned a result")
	}
}
