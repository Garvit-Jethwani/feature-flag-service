# Feature Flag Service: Specification

Status: **Design decisions accepted by the owner on 21 Nov 2026** (section 5). Details are refined in the phase plans under `plans/`.

## 1. Objective

Build a production-ready REST API service that:

- stores feature flags,
- manages their state globally and per user,
- evaluates whether a feature is available for a given user,
- uses caching to keep evaluation fast.

Time box: about 3 hours in total, including review. Language: Go.

## 2. Requirements (from the problem statement)

### 2.1 Functional

| # | Requirement |
|---|---|
| F1 | **Flag management.** Create, read, update, and delete flags. A flag has a name, a description, an enabled/disabled state, and an optional per-user override list. |
| F2 | **Global and per-user control.** A flag can be toggled globally (all users) or overridden for one user. A per-user override takes precedence over the global state. |
| F3 | **Evaluation.** An endpoint takes a flag name and a user ID and returns whether the feature is enabled for that user. Results are cached, and the invalidation strategy is explained. |
| F4 | **Audit trail.** Every flag state change records who changed it, when, and the previous and new values. |

### 2.2 Engineering

| # | Requirement |
|---|---|
| E1 | **Architecture diagram** of the evaluation request path: where the cache sits, when it is consulted, and when it is bypassed or invalidated. |
| E2 | **Validation and error handling.** Flag names have no spaces and a reasonable length limit. Correct status codes everywhere; a missing flag is `404`, not `500`. |
| E3 | **Testing.** Unit tests for evaluation (global on, global off, per-user override). At least one integration test through the HTTP layer end to end. |
| E4 | **CI/CD.** A GitHub Actions pipeline that runs the tests on push. |
| E5 | **Documentation.** A README covering setup, evaluation rules, and how caching works. |

### 2.3 Extensions

| # | Requirement |
|---|---|
| X1 | **Deployment** to DigitalOcean. |
| X2 | **Percentage rollout.** Enable a flag for a percentage of users using a deterministic hash of the user ID. |
| X3 | **Flag dependencies.** A flag can require another flag to be enabled before it evaluates to true. |
| X4 | **Observability and monitoring.** Metrics, logs, and alerting for the running service. Added by the owner as a consideration, not part of the original problem statement; whether to implement it is decided at the end. Options are in `docs/discovery.md` section 8.4. |
| X5 | **Rate limiting.** Limit requests per API key and answer `429` when exceeded. Raised by the owner; not part of the original problem statement. Accepted by the owner as an in-process token bucket per API key, built in plan 02; options are in `docs/discovery.md` section 8.5. |

## 3. Owner's take

- This is a mini LaunchDarkly: a management API for flags plus a fast evaluation path.
- Work phase by phase. Each phase gets a plan under `plans/` that is reviewed and approved before any implementation or deployment.
- Security matters from the first commit; the rules live in `AGENTS.md`.
- Specs, plans, diagrams, and code are pushed to the personal GitHub repository continuously.

## 4. Agent's take

1. **Two workloads with different shapes.** Management is low-volume and must be strictly correct and audited. Evaluation is high-volume and read-only. The design keeps them separate: writes go through a transaction that also writes the audit row; reads go through the cache.
2. **Evaluation is a pure function.** Given a flag, its overrides, and its prerequisites, the answer is deterministic. Keeping it free of I/O makes the required unit tests trivial and makes the rules easy to explain.
3. **Invalidation is the hard part, not caching.** Results are cached per (flag, user), grouped by flag, so one write can drop every cached result for that flag at once. A short TTL is the safety net. Section 7 has the details.
4. **Design for the extensions now.** Rollout percentage and prerequisites change the data model and the evaluation order. Including them in the schema and the evaluator from the start avoids rework, even though the endpoints for them land later.
5. **"Who changed it" needs real identity.** A client-supplied name can be forged, so the audit actor should come from the API key used.
6. **Keep the dependency count low.** Standard-library HTTP routing and one Postgres driver are enough. Less to review, less to go wrong in three hours.
7. **Deliberately out of scope:** a UI, multiple environments or projects, multivariate (non-boolean) flags, targeting rules and segments, streaming SDKs, and user accounts or permissions beyond the two API key roles.

## 5. Decisions

The options, trade-offs, and reasoning behind each decision are in [`docs/discovery.md`](docs/discovery.md), which also raises D8 (what the cache holds) and D9 (integration test database).

| # | Decision | Proposal | Alternative considered | Status |
|---|---|---|---|---|
| D1 | Storage | **PostgreSQL**, with SQL migrations embedded in the binary. Run locally through Docker Compose. | SQLite (simpler, but awkward on ephemeral cloud filesystems); in-memory only (not production-ready). | Accepted |
| D2 | Cache | **In-process cache behind a `Cache` interface**, with per-flag invalidation and a TTL. Redis implementation of the same interface only if time allows. | Redis from the start: correct across multiple instances, but one more service to run, test, and deploy. | Accepted |
| D3 | Auth and audit actor | **Bearer API keys from environment config**, each mapped to an actor name and a role: `admin` (everything) or `reader` (read and evaluate). All `/v1` endpoints require a key; the actor in the audit log is derived from it. | One key type for everything; trusting an `X-Actor` header (forgeable); full user accounts (too large). | Accepted |
| D4 | Prerequisites versus overrides | **Prerequisites are a hard gate.** If any prerequisite is off for the user, the flag is off, even with a per-user override of `true`. | Override wins over everything, including prerequisites. | Accepted |
| D5 | HTTP stack | **Standard library `net/http`** (method and path patterns), `log/slog` for logs, `pgx` for Postgres. | A router framework such as chi or gin. | Assumed (default, no objection raised) |
| D6 | Flag identity | **The name is the identifier and is immutable.** Lowercase letters, digits, `.`, `_`, `-`; starts with a letter or digit; 1 to 64 characters. | Mixed case (invites `Dark-Mode` versus `dark-mode` confusion); separate numeric IDs. | Assumed (default, no objection raised) |
| D7 | Deployment target | **DigitalOcean App Platform** with Managed PostgreSQL, one instance, for HTTPS and health checks without server upkeep. | A droplet running Docker Compose (no TLS without a domain). | Accepted |
| D8 | What the cache holds | **Evaluation results per (flag, user)**, grouped by flag, with a size cap. | Flag snapshots per flag, evaluated on every request. | Accepted |
| D9 | Integration test database | **Real PostgreSQL**: Docker Compose locally, a service container in CI. | In-memory fake store; Testcontainers. | Accepted |

Risk for D7: `doctl account get` reports the account status as `warning`. This may block resource creation and should be checked in the DigitalOcean console before the deploy phase.

## 6. Evaluation rules

Evaluated in this order; the first rule that matches decides.

| Order | Condition | Result | Reason code |
|---|---|---|---|
| 0 | Flag does not exist | `404` | n/a |
| 1 | Any prerequisite flag evaluates to off for this user | off | `prerequisite_failed` |
| 2 | A per-user override exists | the override value | `user_override` |
| 3 | Flag is globally disabled | off | `global_disabled` |
| 4 | Rollout percentage is below 100 | on if the user's bucket is below the percentage, otherwise off | `rollout` |
| 5 | Otherwise | on | `global_enabled` |

Notes:

- **Rollout bucket:** `sha256(flagName + ":" + userID)` reduced to a number from 0 to 99. Including the flag name means different flags pick different users. Raising the percentage only adds users; nobody who was on is turned off.
- **Prerequisites** are evaluated for the same user, recursively. Cycles are rejected when the dependency is written (`409`), and evaluation has a depth limit as a second guard.
- A flag that other flags depend on cannot be deleted (`409`) until those dependencies are removed.
- Rule 1 before rule 2 is decision D4.

## 7. Caching and invalidation

**What is cached:** the evaluation result for a (flag, user) pair, grouped by flag.

**Read path:**

1. Look up (flag, user) in the cache. On a hit, return it.
2. On a miss, load the flag, the user's override, and prerequisites from Postgres, run the evaluator, store the result with a TTL, and return it.

**Invalidation:**

- Any committed write to a flag (update, delete, override set or removed, prerequisite change) drops every cached result for that flag **and for every flag that depends on it**, directly or transitively.
- Invalidation runs after the database transaction commits.
- A TTL (default 60 seconds, configurable) bounds staleness if an invalidation is ever missed.

**When the cache is bypassed:**

- The flag does not exist: return `404` and cache nothing.
- The cache backend fails: log it, evaluate from Postgres, and still answer.
- The caller asks for a fresh result with `Cache-Control: no-cache`.

**Known limits, to be stated in the README:**

- With the in-process cache, each instance has its own cache, so a write on one instance is seen by others only after the TTL. A shared Redis cache removes this (D2).
- Results grow with flags times active users, so the cache has a configurable cap on entries. When the cap is reached, one arbitrary entry is removed to make room; this costs at most one extra database query and never a wrong answer.
- Expired entries are removed when they are read again; there is no background sweep. A long-running instance with many one-time users therefore sits at the entry cap, and an eviction may drop a live entry while expired ones remain.
- Between a write's commit and its invalidation (microseconds, in the same process), a concurrent evaluation can still be answered with the old cached value.

**Stale write-back guard:** a read that started before a write could store its old result just after the invalidation. Each invalidation bumps a generation counter, and a read stores its result only if the generation is unchanged since before it queried the database.

Responses carry an `X-Cache: HIT | MISS | BYPASS` header so the behaviour is observable and testable.

### Evaluation request path (E1)

The diagrams of the evaluation path and the write path are in [`docs/architecture.md`](docs/architecture.md), with a table of when the cache is consulted, bypassed, and invalidated.

## 8. API surface (draft)

All routes are under `/v1` and require `Authorization: Bearer <key>`, except the health checks. `GET` routes accept either role; every other method requires the `admin` role. The audit endpoints are the exception: they are `GET` routes but require `admin`, because entries name actors and overridden users. Each API key has its own rate limit; a key that exceeds it gets `429` with `Retry-After`.

| Method and path | Purpose | Success | Notable errors |
|---|---|---|---|
| `POST /v1/flags` | Create a flag | `201` | `400` invalid, `409` name exists |
| `GET /v1/flags` | List flags | `200` | |
| `GET /v1/flags/{name}` | Read a flag with its overrides and prerequisites | `200` | `404` |
| `PATCH /v1/flags/{name}` | Update description, enabled state, rollout percentage, prerequisites | `200` | `400`, `404`, `409` dependency cycle |
| `DELETE /v1/flags/{name}` | Delete a flag | `204` | `404`, `409` other flags depend on it |
| `PUT /v1/flags/{name}/overrides/{userID}` | Set a per-user override | `200` | `400`, `404` |
| `DELETE /v1/flags/{name}/overrides/{userID}` | Remove a per-user override | `204` | `404` |
| `GET /v1/flags/{name}/evaluate?user_id=U` | Evaluate the flag for a user | `200` | `400`, `404` |
| `GET /v1/flags/{name}/audit` | Audit entries for one flag, newest first; works for deleted flags (admin only) | `200` | `403`, `404` name never existed |
| `GET /v1/audit` | All audit entries, newest first (admin only) | `200` | `403` |
| `GET /healthz`, `GET /readyz` | Liveness and readiness | `200` | `503` when the database is unreachable |

Errors use one JSON shape: `{"error": {"code": "flag_not_found", "message": "..."}}`. Missing or invalid credentials return `401`; a valid `reader` key on an admin-only route returns `403`; a key over its rate limit gets `429`.

Evaluation response: `{"flag": "...", "user_id": "...", "enabled": true, "reason": "user_override"}`.

## 9. Validation

| Field | Rule |
|---|---|
| Flag name | `^[a-z0-9][a-z0-9._-]{0,63}$` |
| Description | At most 500 characters |
| User ID | 1 to 128 characters, no whitespace or control characters |
| Rollout percentage | Integer from 0 to 100 |
| Request body | JSON only, at most 1 MB, unknown fields rejected |

## 10. Data model (draft)

| Table | Columns | Notes |
|---|---|---|
| `flags` | `name` (primary key), `description`, `enabled`, `rollout_percentage` (default 100), `created_at`, `updated_at` | |
| `flag_overrides` | `flag_name`, `user_id`, `enabled` | Primary key (`flag_name`, `user_id`); removed with the flag |
| `flag_prerequisites` | `flag_name`, `requires_flag` | Deleting a required flag is blocked |
| `audit_log` | `id`, `flag_name`, `action`, `actor`, `previous_value` (JSON), `new_value` (JSON), `occurred_at` | No foreign key, so entries survive flag deletion |

The audit row is written in the same transaction as the change it describes, so the two cannot disagree. A request that changes nothing (for example, enabling a flag that is already enabled) succeeds but writes no audit row and does not invalidate the cache, because no state changed.

## 11. Production-readiness checklist

- Configuration from environment variables; no secrets in the repository.
- Structured JSON logs with a request ID; panic recovery middleware.
- Server timeouts, request body limits, graceful shutdown.
- Liveness and readiness endpoints.
- Multi-stage Docker build, non-root user, minimal base image.
- CI runs `go vet` and `go test -race` on every push.

## 12. Phases

Each phase gets a plan in `plans/` that must be approved before work starts.

| Phase | Scope | Covers | Budget |
|---|---|---|---|
| 0 | Repository setup, `SPECS.md`, `AGENTS.md` | | done |
| 1 | Domain model, evaluator with unit tests, Postgres store, flag CRUD, overrides, evaluation endpoint, validation, auth | F1, F2, F3 (uncached), E2, E3 (unit) | done |
| 2 | Cache with invalidation, audit trail, per-key rate limiting | F3, F4, X5 | done |
| 3 | HTTP integration tests, GitHub Actions, Dockerfile, README, architecture diagram | E1, E3, E4, E5 | done |
| 4 | Percentage rollout and flag dependencies exposed through the API | X2, X3 | 20 min |
| 5 | Deployment to DigitalOcean | X1 | 15 min |
| 6 | Observability and monitoring (optional) | X4 | only if time remains |

Phases 1 to 3 are the required scope and take priority. Phases 4 to 6 are extensions. Phase 6 is attempted only if time remains after the others; if time runs short before that, phase 5 (deployment) is dropped first.
