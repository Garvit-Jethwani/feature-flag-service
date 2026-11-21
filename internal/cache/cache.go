// Package cache keeps evaluation results in memory so that repeated
// evaluations of the same flag for the same user do not reach the database.
package cache

import (
	"sync"
	"time"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// entry is one cached evaluation result and the time it stops being valid.
type entry struct {
	result  flag.Result
	expires time.Time
}

// Memory is an in-process cache of evaluation results.
//
// Results are grouped by flag (flag name -> user ID -> result), so that when
// a flag changes, every cached result for it is dropped in one step.
//
// It is safe for concurrent use. There is no background goroutine: expired
// entries are removed when they are next read, and the entry limit bounds
// whatever is left.
type Memory struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time // replaced in tests to control time

	byFlag map[string]map[string]entry
	size   int // total entries across all flags

	// generation increases on every invalidation. A reader notes it before
	// querying the database and passes it to Set; if a flag changed in
	// between, the numbers differ and the possibly stale result is not
	// stored.
	generation uint64
}

// NewMemory returns a cache that keeps each result for ttl and holds at most
// maxEntries results. A ttl or maxEntries of zero disables caching: nothing
// is stored and every lookup misses.
func NewMemory(ttl time.Duration, maxEntries int) *Memory {
	return &Memory{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		byFlag:     map[string]map[string]entry{},
	}
}

// Get returns the cached result for a flag and user, if there is one that
// has not expired. An expired entry is removed.
func (c *Memory) Get(flagName, userID string) (flag.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.byFlag[flagName][userID]
	if !ok {
		return flag.Result{}, false
	}
	if !c.now().Before(cached.expires) {
		c.remove(flagName, userID)
		return flag.Result{}, false
	}
	return cached.result, true
}

// Generation returns the current invalidation counter. Call it before
// loading data from the database and pass the value to Set.
func (c *Memory) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// Set stores a result, unless an invalidation has happened since generation
// was read. That check stops a slow reader from putting back a result that
// was computed from data read before a flag changed.
func (c *Memory) Set(flagName, userID string, result flag.Result, generation uint64) {
	if c.ttl <= 0 || c.maxEntries <= 0 {
		return // caching is disabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if generation != c.generation {
		return // a flag changed while this result was being computed
	}

	users := c.byFlag[flagName]
	if _, exists := users[userID]; !exists {
		if c.size >= c.maxEntries {
			c.evictOne()
			users = c.byFlag[flagName] // eviction may have removed this flag's map
		}
		if users == nil {
			users = map[string]entry{}
			c.byFlag[flagName] = users
		}
		c.size++
	}
	users[userID] = entry{result: result, expires: c.now().Add(c.ttl)}
}

// Invalidate drops every cached result for the named flags and advances the
// generation. The generation advances even if nothing was cached, because a
// reader may be computing a result for one of these flags right now.
func (c *Memory) Invalidate(flagNames ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range flagNames {
		c.size -= len(c.byFlag[name])
		delete(c.byFlag, name)
	}
	c.generation++
}

// Len returns the number of cached results.
func (c *Memory) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// remove deletes one entry. The caller holds the lock.
func (c *Memory) remove(flagName, userID string) {
	users := c.byFlag[flagName]
	if _, ok := users[userID]; !ok {
		return
	}
	delete(users, userID)
	c.size--
	if len(users) == 0 {
		delete(c.byFlag, flagName)
	}
}

// evictOne makes room by removing one arbitrary entry. The caller holds the
// lock.
//
// Go starts map iteration at a random position, so this is in effect random
// eviction. It is deliberately simple: the entry limit is a safety cap that
// is rarely reached (entries normally leave by TTL or invalidation), and
// evicting a useful entry only costs one extra database query, never a wrong
// answer. A least-recently-used policy would need bookkeeping on every read.
func (c *Memory) evictOne() {
	for flagName, users := range c.byFlag {
		for userID := range users {
			c.remove(flagName, userID)
			return
		}
	}
}
