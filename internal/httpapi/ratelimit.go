package httpapi

import (
	"math"
	"sync"
	"time"
)

// RateLimit configures the per-key request limit. A PerSecond of zero
// disables rate limiting.
type RateLimit struct {
	// PerSecond is the sustained number of requests per second each API key
	// may make.
	PerSecond float64
	// Burst is how many requests a key may make at once before the
	// sustained rate applies.
	Burst int
}

// tokenBucket is a classic token bucket rate limiter for one API key.
//
// The bucket holds up to burst tokens and gains perSecond tokens every
// second. Each request takes one token; a request that finds the bucket
// empty is refused. This allows short bursts while holding the long-run
// rate to perSecond.
type tokenBucket struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	tokens    float64
	last      time.Time        // when tokens was last brought up to date
	now       func() time.Time // replaced in tests to control time
}

// newTokenBucket returns a full bucket, or nil when the limit is disabled.
// A nil bucket allows everything.
func newTokenBucket(limit RateLimit) *tokenBucket {
	if limit.PerSecond <= 0 {
		return nil
	}
	burst := math.Max(1, float64(limit.Burst))
	return &tokenBucket{
		perSecond: limit.PerSecond,
		burst:     burst,
		tokens:    burst,
		last:      time.Now(),
		now:       time.Now,
	}
}

// allow takes one token if one is available. When none is, it reports how
// long the caller should wait before the next token arrives.
func (b *tokenBucket) allow() (ok bool, retryAfter time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Add the tokens earned since the last call, up to the bucket size.
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.perSecond)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := 1 - b.tokens
	return false, time.Duration(missing / b.perSecond * float64(time.Second))
}
