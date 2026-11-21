# Critical user journeys

What people and systems do with this service, and what the service does internally in each case. Part 1 is seen from the outside (the caller's goal). Part 2 is seen from the inside (how specific situations are handled, and where the code is).

Runnable versions of these journeys: [`local-walkthrough.md`](local-walkthrough.md) and [`live-walkthrough.md`](live-walkthrough.md). Diagrams: [`architecture.md`](architecture.md).

## Who uses it

| Actor | Key role | What they do |
|---|---|---|
| Release engineer or product owner | `admin` | Creates flags, toggles them, sets overrides, rollout, and dependencies; reads the audit trail |
| Application backend | `reader` | Asks "is this flag on for this user?" on its request path, many times a second |
| Operator | `reader` | Watches health, metrics, and logs |

## Part 1: User journeys

### J1. Ship a feature dark, then test it with one user

| Step | Request | Result |
|---|---|---|
| Create the flag, off | `POST /v1/flags` with `"enabled": false` | `201`. Everyone gets off, reason `global_disabled`. |
| Turn it on for the tester | `PUT /v1/flags/{name}/overrides/{userID}` with `"enabled": true` | That user gets on, reason `user_override`. Everyone else is unchanged. |

Why it works: a per-user override is checked before the global state, so it wins in both directions.

### J2. Roll out gradually

| Step | Request | Result |
|---|---|---|
| Enable for 10% | `PATCH /v1/flags/{name}` with `"enabled": true, "rollout_percentage": 10` | About one user in ten gets on, reason `rollout`. |
| Widen | `PATCH` with `"rollout_percentage": 50`, then `100` | Users who were on stay on; more are added. At 100 the reason becomes `global_enabled`. |

Why it works: each user's bucket (0 to 99) is `sha256(flag name + ":" + user ID)`. It never changes, so raising the percentage only adds users. Including the flag name means different flags pick different users.

### J3. Kill switch

| Step | Request | Result |
|---|---|---|
| Something is wrong; turn it off | `PATCH /v1/flags/{name}` with `"enabled": false` | The very next evaluation is off for everyone without an override. No waiting for the cache to expire. |

Why it works: the write invalidates every cached result for the flag as soon as it commits (case C2 below).

### J4. Make one feature depend on another

| Step | Request | Result |
|---|---|---|
| `new-checkout` requires `payments-v2` | `PATCH /v1/flags/new-checkout` with `"prerequisites": ["payments-v2"]` | `new-checkout` is off for any user for whom `payments-v2` is off, reason `prerequisite_failed`, even with an override of `true`. |
| Turn `payments-v2` off later | `PATCH /v1/flags/payments-v2` | `new-checkout` goes off too, at the next evaluation. |
| Try to delete `payments-v2` | `DELETE /v1/flags/payments-v2` | `409 flag_has_dependents`, until the dependency is removed. |

### J5. The application asks whether a feature is on

`GET /v1/flags/{name}/evaluate?user_id=U` returns `{"enabled": true|false, "reason": "..."}`. This is the hot path. Most calls are answered from memory (`X-Cache: HIT`). A flag that does not exist is `404`, so a typo in a flag name is visible instead of silently "off".

### J6. Find out who changed a flag

`GET /v1/flags/{name}/audit` (admin only) lists every change, newest first, with the actor, the time, and the values before and after. It still works after the flag is deleted. `GET /v1/audit` lists changes across all flags.

### J7. Retire a flag

`DELETE /v1/flags/{name}` removes the flag and its overrides. Evaluations return `404` from then on. The audit history stays.

### J8. Check that the service is healthy

| Question | Where to look |
|---|---|
| Is it up and can it reach its database? | `GET /healthz`, `GET /readyz` |
| How much traffic, how many errors, how fast? | `GET /metrics`: `http_requests_total`, `http_request_duration_seconds` |
| Is the cache working? | `evaluation_cache_results_total` (hit ratio), `evaluation_cache_entries` |
| What happened to one request? | The JSON log line with the `request_id` the client received in `X-Request-Id` |

## Part 2: Internal cases

Each case says what happens and where the code is.

### C1. An evaluation request, step by step

1. **Middleware** assigns a request ID, sets a 5-second deadline, and will write the log line and metrics at the end. `internal/httpapi/middleware.go`, `observe`
2. **Authentication, then rate limit, then role check.** `internal/httpapi/auth.go`, `require`
3. **Validation** of the flag name and user ID. `internal/service/service.go`, `Evaluate`
4. **Cache lookup** by (flag, user), unless the request sent `Cache-Control: no-cache`. A hit returns here. `internal/cache/cache.go`, `Get`
5. **On a miss:** note the cache generation, then load the flag, this user's overrides, and all prerequisite flags in **one** SQL query. `internal/store/store.go`, `EvaluationSnapshot`
6. **Evaluate** with a pure function: no database, no cache, no clock. `internal/flag/evaluate.go`, `Evaluate`
7. **Store** the result with a TTL, but only if the generation has not moved. `internal/cache/cache.go`, `Set`

### C2. A change to a flag

Every change to an existing flag (update, delete, override set or removed) follows the same pattern in `internal/store/store.go`. Creating a flag is the same without the lock and the invalidation, since nothing is cached for a flag that does not exist yet.

1. Begin a transaction and lock the flag's row (`lockFlag`, `SELECT ... FOR UPDATE`).
2. Compare the request with the current state. If nothing would change, stop: no write, no audit entry, no invalidation.
3. Apply the change.
4. Insert the audit entry with the previous and new values (`internal/store/audit.go`, `insertAudit`).
5. Work out which flags are affected: this one and every flag that depends on it (`affectedFlags`).
6. Commit.
7. Only then, the service invalidates the cache for the affected flags (`internal/service/service.go`, `invalidate`).

Because the change and the audit entry are in one transaction, either both exist or neither does.

### C3. A read and a write race (stale write-back)

The dangerous order: a read loads the old value from the database; a write commits and invalidates; the read then stores its old value in the cache, where it would live for a full TTL.

How it is prevented: every invalidation increases a generation counter. The read noted the counter before its query and passes it to `Set`; the numbers differ, so the old value is not stored. `internal/cache/cache.go`, `Set` and `Invalidate`

What remains: between a write's commit and its invalidation (microseconds, in the same process) a concurrent request can still get the old cached value once.

### C4. Two admins change the same flag at the same moment

The row lock makes the two transactions run one after the other. The second sees the result of the first, so its audit entry records the true previous value, and neither change is lost.

### C5. Dependency changes that would break the graph

Checked in the database, inside the transaction, after the new list is written (`internal/store/prerequisites.go`):

| Situation | Answer |
|---|---|
| A prerequisite does not exist | `400`, naming the flag |
| The flag would require itself through any path | `409 dependency_cycle` |
| A chain would be deeper than 10 levels | `409 dependency_too_deep` |

On any of these the transaction rolls back and nothing is saved. Dependency edits also take one advisory lock (`lockDependencyEdits`), so that "A requires B" and "B requires A" sent at the same moment cannot each pass the check and form a cycle together. The evaluator has its own depth limit as a second guard.

### C6. Bad input

| Layer | What it rejects | Where |
|---|---|---|
| HTTP | Wrong content type (`415`), body over 1 MB (`413`), malformed JSON, unknown fields, wrong types (`400`) | `internal/httpapi/handlers.go`, `decodeJSON` |
| Domain | Flag name pattern, description length, user ID, rollout range, prerequisite list (`400`) | `internal/flag/flag.go` |
| Database | The same name pattern and ranges as `CHECK` constraints, as a last line of defence | `internal/store/migrations/0001_init.sql` |

All SQL is parameterized. Every error becomes a status code in exactly one function, and anything unrecognised becomes a `500` with a generic message, so internal details never reach the client. `internal/httpapi/errors.go`, `classify`

### C7. Who is calling

| Situation | Answer |
|---|---|
| No key, or a wrong key | `401` |
| A valid key that is over its rate limit | `429` with `Retry-After` |
| A valid `reader` key on an admin route | `403` |

Keys are compared as SHA-256 hashes in constant time, against every configured key, so the time taken does not reveal how close a guess was. The actor name bound to the key is what the audit trail records; a body that tries to supply an `actor` is rejected as an unknown field. `internal/httpapi/auth.go`

### C8. One client sends too much

Each API key has its own token bucket: it refills at `RATE_LIMIT_RPS` and holds at most `RATE_LIMIT_BURST`. An empty bucket means `429`. One noisy key cannot use up another key's allowance. `internal/httpapi/ratelimit.go`

### C9. A flag that does not exist

`404 flag_not_found` on every route, never `500`. Nothing is cached for it, so a flag created a moment later is visible at once.

### C10. The database is slow or down

| What | Behaviour |
|---|---|
| Evaluations already in the cache | Still answered, until their TTL expires |
| Anything that needs the database | Fails within the 5-second request deadline. A timeout is `503`; any other database error is `500` with a generic message, and the real error is logged with the request ID. |
| `GET /readyz` | `503`, so a load balancer can stop sending traffic |
| `GET /healthz` | Still `200`: the process itself is fine, so it is not restarted for a database problem |

### C11. A bug panics inside a handler

The panic is caught, logged with the request ID and stack, and answered with `500`. The process keeps serving other requests. `internal/httpapi/middleware.go`, `recoverPanic`

### C12. The cache fills up

At `CACHE_MAX_ENTRIES` (100,000 by default) one arbitrary entry is removed to make room. This can cost an extra database query but never returns a wrong answer, because no entry outlives its TTL and every change invalidates. `internal/cache/cache.go`, `evictOne`

### C13. Start and stop

| Moment | Behaviour | Where |
|---|---|---|
| Start with missing or invalid configuration | Refuses to start and says which variable is wrong, without printing its value | `internal/config/config.go` |
| Start | Applies database migrations under an advisory lock, so two instances starting together do not collide | `internal/store/migrate.go` |
| Stop (`SIGTERM`) | Stops accepting connections, lets requests in flight finish, then exits | `cmd/server/main.go` |

### C14. More than one instance

Not supported correctly today, and deployed as one instance for that reason. Each instance would have its own cache (a change on one reaches the others only when their entries expire, at most 60 seconds) and its own rate-limit buckets. The fix is a shared cache or broadcast invalidation; see [`next-steps.md`](next-steps.md).
