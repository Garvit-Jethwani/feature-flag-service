# Feature Flag Service

[![ci](https://github.com/Garvit-Jethwani/feature-flag-service/actions/workflows/ci.yml/badge.svg)](https://github.com/Garvit-Jethwani/feature-flag-service/actions/workflows/ci.yml)

A REST API that stores feature flags, lets you switch them on or off for everyone or for one user, and answers "is this feature on for this user?" quickly from a cache. Every change is recorded in an audit trail.

Written in Go with the standard library HTTP server and PostgreSQL. The only third-party dependency is the Postgres driver (`pgx`).

| Document | What it covers |
|---|---|
| [`SPECS.md`](SPECS.md) | Requirements, decisions, rules. The source of truth. |
| [`docs/architecture.md`](docs/architecture.md) | Diagrams of the evaluation and write paths; where the cache sits. |
| [`docs/discovery.md`](docs/discovery.md) | Options considered and trade-offs. |
| [`plans/`](plans) | One plan per phase, approved before it was built. |

## Setup

You need Docker (with Compose) and `make`. Go 1.27 is needed only to run the tests.

```sh
make up      # writes .env with random local credentials, builds, starts API + Postgres
```

The API listens on `http://127.0.0.1:8080`. `make up` creates two API keys in `.env`: an admin key and a read-only key. Load them into your shell without printing them:

```sh
ADMIN=$(grep '^API_KEYS=' .env | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="admin"{print $3}')
READER=$(grep '^API_KEYS=' .env | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="reader"{print $3}')
```

Other commands: `make down` (stop), `make logs` (API logs), `make test` (all tests), `make lint` (`gofmt` and `go vet`).

## Try it

```sh
B=http://127.0.0.1:8080

# Create a flag that is off for everyone.
curl -s -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"new-checkout","description":"New checkout flow","enabled":false}'

# Evaluate it for a user: off, reason "global_disabled".
curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"

# Turn it on for that one user only.
curl -s -X PUT $B/v1/flags/new-checkout/overrides/user-42 -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{"enabled":true}'

# Evaluate again: on, reason "user_override". Run it twice to see X-Cache go MISS, then HIT.
curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"

# Turn it on for everyone.
curl -s -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{"enabled":true}'

# See who changed what.
curl -s $B/v1/flags/new-checkout/audit -H "Authorization: Bearer $ADMIN"
```

An evaluation response:

```json
{"flag":"new-checkout","user_id":"user-42","enabled":true,"reason":"user_override"}
```

## API

All `/v1` routes need `Authorization: Bearer <key>`. Bodies are JSON.

| Method and path | Role | Purpose | Success | Errors |
|---|---|---|---|---|
| `POST /v1/flags` | admin | Create a flag (`name`, `description`, `enabled`) | `201` | `400`, `409` name exists |
| `GET /v1/flags` | reader | List flags by name; page with `limit` and `after` | `200` | `400` |
| `GET /v1/flags/{name}` | reader | One flag with its per-user overrides | `200` | `404` |
| `PATCH /v1/flags/{name}` | admin | Change `description` and/or `enabled` | `200` | `400`, `404` |
| `DELETE /v1/flags/{name}` | admin | Delete a flag and its overrides | `204` | `404`, `409` other flags depend on it |
| `PUT /v1/flags/{name}/overrides/{userID}` | admin | Set a per-user override (`enabled`) | `200` | `400`, `404` |
| `DELETE /v1/flags/{name}/overrides/{userID}` | admin | Remove a per-user override | `204` | `404` |
| `GET /v1/flags/{name}/evaluate?user_id=U` | reader | Is the flag on for this user? | `200` | `400`, `404` |
| `GET /v1/flags/{name}/audit` | admin | Audit entries for one flag, newest first; page with `limit` and `before` | `200` | `404` name never existed |
| `GET /v1/audit` | admin | Audit entries for all flags, newest first | `200` | |
| `GET /healthz`, `GET /readyz` | none | Liveness; readiness (checks the database) | `200` | `503` |

"reader" means either role works. Any `/v1` route can also answer `401` (no key or a wrong key), `403` (a reader key on an admin route), and `429` (rate limit).

Errors always have the same shape, and a missing flag is `404`, never `500`:

```json
{"error":{"code":"flag_not_found","message":"flag not found"}}
```

### Validation

| Input | Rule |
|---|---|
| Flag name | `^[a-z0-9][a-z0-9._-]{0,63}$`: lowercase letters, digits, `.`, `_`, `-`; no spaces; 1 to 64 characters. The name is the identifier and cannot be changed. |
| Description | At most 500 characters |
| User ID | 1 to 128 characters, no whitespace or control characters |
| Request body | `Content-Type: application/json`, at most 1 MB, unknown fields rejected |

## Evaluation rules

Checked in this order; the first rule that matches decides. The response's `reason` says which one it was.

| Order | Condition | Result | `reason` |
|---|---|---|---|
| 1 | A flag this one requires is off for the user | off | `prerequisite_failed` |
| 2 | The user has an override on this flag | the override's value | `user_override` |
| 3 | The flag is disabled globally | off | `global_disabled` |
| 4 | The flag is rolled out to less than 100% of users | on if the user falls inside the percentage | `rollout` |
| 5 | Otherwise | on | `global_enabled` |

- **A per-user override beats the global state** in both directions: it can turn a disabled flag on for one user, or an enabled flag off.
- **A prerequisite is a hard gate.** If a required flag is off for the user, this flag is off even with an override of `true`.
- **Rollout is deterministic.** The user's bucket is `sha256(flagName + ":" + userID)` reduced to 0 to 99. The same user always gets the same answer, different flags pick different users, and raising the percentage only adds users.

Rules 1 and 4 are implemented and unit-tested in the evaluator (`internal/flag/evaluate.go`), but the API cannot set prerequisites or a rollout percentage yet; that is plan 04. Until then every flag is at 100% with no prerequisites.

The evaluator is a pure function: flag data in, answer out, no database or cache. That is why its rules can be tested without any infrastructure.

## How caching works

Diagrams: [`docs/architecture.md`](docs/architecture.md).

**What is cached.** The evaluation result for one (flag, user) pair, held in the service's memory and grouped by flag.

**Reading.** An evaluation looks in the cache first. On a hit it answers without touching the database. On a miss it runs one database query, evaluates, and stores the result for the TTL (60 seconds by default). The `X-Cache` response header says which happened: `HIT`, `MISS`, or `BYPASS`.

**Invalidation.** Every committed change to a flag (update, delete, override set or removed) immediately drops all cached results for that flag and for every flag that depends on it. The next evaluation sees the change. Three details make this correct:

1. **Invalidation runs after the database commit.** Doing it earlier would let a concurrent read fetch the old value and cache it again.
2. **A generation counter stops stale write-backs.** Each invalidation increases a counter. A read notes the counter before its database query and stores its result only if the counter has not moved. Without this, a read that started just before a write could store the old answer just after the invalidation.
3. **The TTL is the safety net.** If an invalidation were ever missed, an entry would be wrong for at most 60 seconds.

**Bypass.** Send `Cache-Control: no-cache` to skip the lookup and get a result straight from the database. A flag that does not exist returns `404` and is never cached.

**Limits to know about.**

- The cache is per instance. With several instances, a change made on one reaches the others only when their entries expire. A shared cache (Redis) behind the same `Cache` interface would remove this; with one instance it does not arise.
- Memory is bounded by an entry limit (100,000 by default). When it is reached, one arbitrary entry is evicted, which can cost an extra database query but never gives a wrong answer.
- Expired entries are removed when read again, not by a background job.

## Audit trail

Every change writes an entry with the action, the actor, the time, and the previous and new values:

```json
{"id":2,"flag":"new-checkout","action":"flag.updated","actor":"local-admin",
 "previous":{"enabled":false,"description":"New checkout flow","prerequisites":[],"rollout_percentage":100},
 "new":{"enabled":true,"description":"New checkout flow","prerequisites":[],"rollout_percentage":100},
 "occurred_at":"2026-11-21T08:40:12Z"}
```

- **Actions:** `flag.created`, `flag.updated`, `flag.deleted`, `override.set`, `override.removed`.
- **The actor comes from the API key**, never from the request body, so it cannot be forged by a caller.
- **The entry is written in the same database transaction as the change.** Either both are saved or neither.
- **A request that changes nothing** (enabling a flag that is already on) succeeds but writes no entry.
- **Entries outlive the flag.** The audit of a deleted flag is still readable.
- **Only admin keys can read it**, because entries name actors and overridden users.

## Authentication and rate limiting

- **API keys** are configured in the `API_KEYS` environment variable as `actor:role:token`, comma-separated. Tokens must be at least 32 characters.
- **Two roles:** `admin` can do everything; `reader` can read flags and evaluate.
- **Keys are compared in constant time** against stored hashes, and are never logged.
- **Each key has its own rate limit** (a token bucket: 100 requests per second sustained, bursts of 200, by default). Over the limit the answer is `429` with a `Retry-After` header. One noisy client cannot use up another's allowance.

## Configuration

Everything comes from environment variables; see [`.env.example`](.env.example).

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | required | PostgreSQL connection string |
| `API_KEYS` | required | `actor:role:token` list; at least one admin |
| `PORT` | `8080` | Listen port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `CACHE_TTL` | `60s` | How long a result is cached; `0` disables the cache |
| `CACHE_MAX_ENTRIES` | `100000` | Most results kept in memory |
| `RATE_LIMIT_RPS` | `100` | Sustained requests per second per key; `0` disables the limit |
| `RATE_LIMIT_BURST` | `200` | Requests a key may send at once |

With Docker Compose, `DATABASE_URL` is assembled from `POSTGRES_PASSWORD` in `.env`. Database migrations are embedded in the binary and applied at startup.

## Testing and CI

```sh
make test       # everything, against the Postgres container
make test-unit  # no database needed; database-backed tests are skipped
```

| Tests | Where | What they cover |
|---|---|---|
| Evaluator unit tests | `internal/flag/evaluate_test.go` | Global on, global off, per-user override, prerequisites, rollout |
| Cache unit tests | `internal/cache/cache_test.go` | Expiry, invalidation, stale write-back guard, entry limit |
| HTTP end-to-end tests | `internal/httpapi/*_test.go` | Real HTTP server and real Postgres: flag lifecycle, status codes, auth, cache `HIT`/`MISS` and invalidation, audit trail |
| Store tests | `internal/store/store_test.go` | Queries, transactions, audit rows, migrations |

GitHub Actions ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs on every push: a `gofmt` check, `go vet`, `go test -race` against a Postgres service container, and a Docker image build. In CI, database-backed tests fail rather than skip if the database is missing.

## Production notes

- Structured JSON logs with a request ID. API keys, request bodies, and raw paths are not logged.
- Server timeouts, a 5-second request deadline, a 1 MB body limit, panic recovery, graceful shutdown.
- All SQL is parameterized.
- The container is a static binary on a distroless base image, runs as a non-root user with a read-only filesystem.

## Project layout

```
cmd/server          entry point: config, wiring, graceful shutdown
internal/config     environment variables
internal/flag       domain types, validation, the evaluator (pure)
internal/cache      in-process result cache
internal/store      PostgreSQL queries and migrations
internal/service    cache + store + evaluator orchestration
internal/httpapi    routes, auth, rate limiting, handlers, error mapping
```
