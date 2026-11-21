# Plan 02: Evaluation cache, audit trail, and rate limiting

Status: Implemented. Approved by the owner on 21 Nov 2026: points 1 and 2 confirmed. On point 3 the owner asked for the reasoning, which is recorded below; the policy is isolated in one function so it can be changed. Part 3 (rate limiting) was added at the owner's request.

Covers F3 (cached evaluation with an explained invalidation strategy), F4 (audit trail), and X5 (rate limiting) from `SPECS.md`. Design reasoning is in `docs/discovery.md` sections 4.3 to 4.7 and 8.5. Budget: 20 minutes, plus about 10 for rate limiting.

## Outcome

- Evaluations are answered from an in-process cache when possible. Every response says what happened in an `X-Cache` header: `HIT`, `MISS`, or `BYPASS`.
- Any change to a flag removes that flag's cached results, so the next evaluation sees the change.
- Every change to a flag writes an audit entry (who, when, previous value, new value) in the same database transaction as the change.
- Admins can read the audit trail through the API.

## Points to confirm

| # | Question | Proposal |
|---|---|---|
| 1 | Who may read the audit trail? | **Admin keys only.** Entries contain actor names and the user IDs of overrides. |
| 2 | Is a change that changes nothing audited? | **No.** A `PATCH` that sets `enabled` to its current value returns `200`, writes no audit entry, and leaves the cache alone, because no state changed. |
| 3 | What happens when the cache is full? | **Remove one arbitrary entry** to make room. Simple and bounded; a least-recently-used policy is more code for little gain here. |

Why arbitrary eviction (point 3):

- The entry limit is a safety cap, not the normal way entries leave. Normally they expire by TTL or are dropped when their flag changes.
- A poor eviction costs one extra database query for that user. It can never produce a wrong answer.
- Least-recently-used needs a linked list beside the map and must update it on every read, so reads would take a write lock; dropping a whole flag would mean unlinking each entry. That is more code on the hottest path for a case that is rarely reached.
- Go starts map iteration at a random position, so "arbitrary" is in effect random eviction, a standard policy.
- Cost accepted: a frequently used entry can be evicted while a rarely used one stays.

## Part 1: cache

### What is stored

One entry per (flag, user): the evaluation result (`enabled` and reason) and its expiry time. Entries are held as a map of flag name to a map of user ID, so all results for a flag can be dropped in one step.

New package `internal/cache`:

| Operation | Behaviour |
|---|---|
| `Get(flag, user)` | Returns the result if present and not expired. An expired entry is removed. |
| `Generation()` | Returns a counter that increases on every invalidation. |
| `Set(flag, user, result, generation)` | Stores the result with the TTL, but only if the counter still equals `generation`. Makes room first if the cache is at its entry limit. |
| `Invalidate(flags...)` | Drops every entry for the named flags and increases the counter. |

The service depends on a small `Cache` interface with these four methods, so a Redis implementation can replace it later without touching the service. A lock protects the maps; there is no background goroutine.

### Read path

1. Request has `Cache-Control: no-cache`: skip the lookup (`BYPASS`).
2. Otherwise look up (flag, user). Found: return it (`HIT`).
3. Not found: note the generation, load the snapshot from Postgres, run the evaluator, and `Set` the result (`MISS`). A bypassed request also stores its fresh result.
4. Flag not found: return `404` and store nothing.

### Invalidation

- After a write transaction commits, the service calls `Invalidate` with the changed flag and every flag that depends on it, directly or transitively. The store finds the dependents inside the same transaction.
- This applies to: flag update, flag delete, override set, override removed. Creating a flag needs no invalidation, because nothing is cached for a flag that does not exist.
- **TTL** (default 60 seconds) is the safety net and, with several instances, the only way other instances notice a change.
- **Stale write-back guard:** a read that queried the database just before a write committed must not store its old result just after the invalidation. Because `Invalidate` increases the generation, that read's `Set` sees a different generation and stores nothing.

Known and accepted: between the commit and the `Invalidate` call (microseconds, in the same process) a concurrent request can still get a `HIT` with the old value. It is not stored again and is gone immediately afterwards.

### Configuration

| Variable | Meaning | Default |
|---|---|---|
| `CACHE_TTL` | How long a result is kept, as a Go duration such as `60s`. `0` disables caching. | `60s` |
| `CACHE_MAX_ENTRIES` | Upper limit on cached results across all flags. | `100000` |

## Part 2: audit trail

### Schema (migration `0002_audit.sql`)

```sql
CREATE TABLE audit_log (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    flag_name      TEXT NOT NULL,
    action         TEXT NOT NULL,
    actor          TEXT NOT NULL,
    previous_value JSONB,
    new_value      JSONB,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_flag_idx ON audit_log (flag_name, id DESC);
```

`flag_name` has no foreign key on purpose, so entries survive deletion of the flag.

### What is recorded

| Action | Previous value | New value |
|---|---|---|
| `flag.created` | none | flag state |
| `flag.updated` | flag state before | flag state after |
| `flag.deleted` | flag state before | none |
| `override.set` | the user's earlier override, or none | `{"user_id", "enabled"}` |
| `override.removed` | `{"user_id", "enabled"}` | none |

Flag state is `description`, `enabled`, `rollout_percentage`, and `prerequisites`. The actor is the name bound to the API key; a client cannot supply it.

### Write path

Every write becomes one transaction:

1. Lock the flag row (`SELECT ... FOR UPDATE`). This also puts concurrent changes to one flag in a definite order.
2. Read the previous value.
3. Apply the change. If nothing would change, stop here with no audit entry (point 2).
4. Insert the audit entry.
5. Collect the flags that depend on this one.
6. Commit.

If any step fails, the change and the audit entry are both rolled back, so they can never disagree. After the commit the service invalidates the cache.

### Endpoints

| Method and path | Role | Result |
|---|---|---|
| `GET /v1/flags/{name}/audit?limit=&before=` | admin | Entries for one flag, newest first. Works for deleted flags. `404` if the name has never existed. |
| `GET /v1/audit?limit=&before=` | admin | All entries, newest first. |

`limit` defaults to 50 with a maximum of 200. `before` is the `id` to continue from; the response carries `next_before` when there may be more.

Entry shape: `{"id", "flag", "action", "actor", "previous", "new", "occurred_at"}`.

## Part 3: rate limiting

- One token bucket per API key, held in memory. Each request with a valid key takes one token; tokens refill at a steady rate up to a burst size.
- When a key has no tokens left the service answers `429` with code `rate_limited` and a `Retry-After` header in seconds.
- The check runs right after the key is authenticated, before any database work.
- Requests without a valid key are not counted: they are rejected with `401` after a hash comparison and never reach the database. Health checks are not limited.
- Buckets are created at startup, one per configured key, so the limiter's memory does not grow with traffic.
- Limits are per instance, the same caveat as the in-process cache.

| Variable | Meaning | Default |
|---|---|---|
| `RATE_LIMIT_RPS` | Sustained requests per second allowed per API key. `0` disables rate limiting. | `100` |
| `RATE_LIMIT_BURST` | Requests a key may make at once before the steady rate applies. | `200` |

## Files

| Path | Change |
|---|---|
| `internal/httpapi/ratelimit.go` | New: token bucket, used by the auth middleware |
| `internal/cache/cache.go` | New: in-process cache |
| `internal/flag/audit.go` | New: audit entry type and action names |
| `internal/store/migrations/0002_audit.sql` | New: `audit_log` table |
| `internal/store/store.go` | Writes become transactions that take the actor, insert the audit entry, and return the flags to invalidate |
| `internal/store/audit.go` | New: audit queries |
| `internal/service/service.go` | Cache lookup, generation check, invalidation after writes, audit reads |
| `internal/httpapi/` | `X-Cache` header, `Cache-Control: no-cache`, audit endpoints |
| `internal/config`, `cmd/server`, Compose, `.env.example` | Cache settings |

Every new type and function carries a short comment explaining what it does and why.

## Tests

Kept deliberately small, on the paths where a mistake would matter.

| Test | Needs a database | Checks |
|---|---|---|
| Cache unit test | no | hit after set; expiry using a fake clock; invalidating one flag leaves others; a `Set` with an old generation is ignored; the entry limit holds |
| Cache over HTTP | yes | `MISS` then `HIT`; a flag update, an override, and a delete each cause the next evaluation to be a `MISS` with the new answer; `no-cache` gives `BYPASS`; `404` is not cached |
| Dependent invalidation | yes | changing a prerequisite flag drops the cached result of the flag that requires it |
| Audit over HTTP | yes | create, update, override, and delete each add one entry with the right actor, previous, and new values; a no-op update adds none; entries survive deletion; a reader key gets `403` |
| Rate limit | no | a burst is allowed, the next request gets `429` with `Retry-After`, tokens refill with time, and each key has its own bucket |

## Verification

1. `gofmt`, `go vet`, and `go test -race ./...` pass.
2. Against the running stack: evaluate twice (`MISS`, then `HIT`); disable the flag; evaluate again (`MISS`, now off).
3. `GET /v1/flags/{name}/audit` shows the create and the update with actor `local-admin` and both values.

## Commits

1. `feat:` in-process cache with tests
2. `feat:` audit trail in the write transactions, with audit endpoints
3. `feat:` cached evaluation with invalidation and the `X-Cache` header
4. `feat:` per-key rate limiting
