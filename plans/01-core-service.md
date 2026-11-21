# Plan 01: Core service

Status: Implemented (approved by the owner on 21 Nov 2026; all verification steps passed)

Covers F1, F2, F3 (uncached), and E2 from `SPECS.md`, plus the unit-test half of E3. Design reasoning is in `docs/discovery.md`. Budget: 35 minutes.

## Outcome

`docker compose up` starts the API and Postgres. With an API key, a caller can create, read, update, and delete flags, set and remove per-user overrides, and evaluate a flag for a user. Evaluation reads the database on every call; the cache arrives in plan 02.

## In scope

1. Go module `github.com/Garvit-Jethwani/feature-flag-service`, with `pgx/v5` as the only third-party dependency.
2. Domain package: flag type, validation, domain errors, and the evaluator.
3. Postgres store with embedded SQL migrations applied at startup.
4. HTTP API: flag CRUD, overrides, evaluation, health checks.
5. Authentication with `admin` and `reader` key roles.
6. Dockerfile, Docker Compose, Makefile, `.env.example`.
7. Tests for everything above.

## Out of scope (later plans)

| Item | Plan |
|---|---|
| Cache, invalidation, `X-Cache` header | 02 |
| Audit rows and audit endpoints | 02 |
| GitHub Actions, README, wider integration tests | 03 |
| Rollout percentage and prerequisites in the API | 04 |
| Deployment | 05 |

The schema and the evaluator already include rollout percentage and prerequisites, so plan 04 adds API fields rather than reshaping the core. Until then the API rejects those fields as unknown, and stored flags keep the defaults (100%, no prerequisites).

## Files

| Path | Contents |
|---|---|
| `cmd/server/main.go` | Config, pool, migrations, server, graceful shutdown on SIGINT and SIGTERM |
| `internal/config/config.go` | Environment parsing; refuses to start on missing or weak settings |
| `internal/flag/flag.go` | `Flag`, `Override`, validation functions, domain errors |
| `internal/flag/evaluate.go` | `Evaluate`: pure function returning `enabled` and a reason code |
| `internal/store/store.go` | Queries and transactions |
| `internal/store/migrations/0001_init.sql` | `flags`, `flag_overrides`, `flag_prerequisites` |
| `internal/service/service.go` | Use cases; every write takes the actor, ready for audit in plan 02 |
| `internal/httpapi/` | Routes, handlers, middleware, error mapping |
| `Dockerfile`, `docker-compose.yml`, `Makefile`, `.env.example` | Local run |

## Schema

```sql
CREATE TABLE flags (
    name               TEXT PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    description        TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    enabled            BOOLEAN NOT NULL DEFAULT FALSE,
    rollout_percentage SMALLINT NOT NULL DEFAULT 100 CHECK (rollout_percentage BETWEEN 0 AND 100),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE flag_overrides (
    flag_name  TEXT NOT NULL REFERENCES flags(name) ON DELETE CASCADE,
    user_id    TEXT NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 128),
    enabled    BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (flag_name, user_id)
);

CREATE TABLE flag_prerequisites (
    flag_name     TEXT NOT NULL REFERENCES flags(name) ON DELETE CASCADE,
    requires_flag TEXT NOT NULL REFERENCES flags(name) ON DELETE RESTRICT,
    PRIMARY KEY (flag_name, requires_flag),
    CHECK (flag_name <> requires_flag)
);
```

Migrations are numbered SQL files embedded in the binary. At startup the service takes a Postgres advisory lock, applies any file not yet listed in a `schema_migrations` table, and releases the lock.

## Endpoints

| Method and path | Role | Success | Errors |
|---|---|---|---|
| `POST /v1/flags` | admin | `201` | `400`, `409` name exists |
| `GET /v1/flags?limit=&after=` | any | `200`, ordered by name | `400` |
| `GET /v1/flags/{name}` | any | `200`, with overrides | `404` |
| `PATCH /v1/flags/{name}` | admin | `200` | `400`, `404` |
| `DELETE /v1/flags/{name}` | admin | `204` | `404`, `409` other flags depend on it |
| `PUT /v1/flags/{name}/overrides/{userID}` | admin | `200` | `400`, `404` |
| `DELETE /v1/flags/{name}/overrides/{userID}` | admin | `204` | `404` |
| `GET /v1/flags/{name}/evaluate?user_id=U` | any | `200` | `400`, `404` |
| `GET /healthz`, `GET /readyz` | none | `200` | `503` database unreachable |

Details:

- A new flag is disabled unless the request says `"enabled": true`. Create accepts `name`, `description`, and `enabled`.
- `PATCH` accepts `description` and `enabled`; an empty patch is `400`.
- The list returns at most `limit` flags (default 100, maximum 500) after the name given in `after`.
- Missing or invalid key: `401`. Valid `reader` key on a write: `403`. Wrong content type: `415`. Body over 1 MB: `413`.
- All errors use `{"error": {"code": "...", "message": "..."}}`. One function maps domain errors to status codes; anything unrecognised is logged with the request ID and returned as a generic `500`.

## Evaluator

`Evaluate(name, userID, snapshot)` takes the flag, its transitive prerequisites, and this user's overrides for those flags, and applies the order in `SPECS.md` section 6. It performs no I/O and returns `enabled` plus one of `prerequisite_failed`, `user_override`, `global_disabled`, `rollout`, `global_enabled`. The rollout bucket is the first 8 bytes of `sha256(flag + ":" + user)` modulo 100. Recursion stops with an error beyond depth 10.

## Configuration

| Variable | Meaning | Default |
|---|---|---|
| `DATABASE_URL` | Postgres connection string | required |
| `API_KEYS` | Comma-separated `actor:role:token` entries | required; at least one `admin`; tokens of 32 characters or more |
| `PORT` | Listen port | `8080` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |

No credential is committed. `make env` writes a local `.env` (ignored by Git) with a random Postgres password and random API tokens; Compose reads it. `.env.example` holds placeholders only. Postgres is published on `127.0.0.1` only.

## Tests

| Test | Needs a database | Checks |
|---|---|---|
| Evaluator, table-driven | no | global on; global off; override on over global off; override off over global on; rollout at 0, 100, and in between, and stable across calls; prerequisite off blocks an override of `true`; depth limit |
| Validation | no | flag names (spaces, upper case, length 0, 64, 65, leading punctuation); user IDs; description length |
| Auth middleware | no | no key `401`; wrong key `401`; `reader` on a write `403`; `admin` allowed |
| HTTP end to end | yes | create, read, patch, override, evaluate, delete; missing flag `404`; duplicate `409`; unknown JSON field `400` |

Database-backed tests read `TEST_DATABASE_URL` and skip when it is unset; `make test` starts Postgres through Compose and sets it. Plan 03 makes CI fail rather than skip.

## Verification

1. `gofmt -l .` prints nothing; `go vet ./...` and `go test -race ./...` pass.
2. `make env && docker compose up --build -d`, then `/readyz` returns `200`.
3. With an admin key: create a flag, evaluate it for a user (off), enable it (on), override one user to off (off for that user, on for another).
4. Evaluating an unknown flag returns `404`; a name with a space returns `400`; a `reader` key on `POST` returns `403`.
5. The container runs as a non-root user.

## Commits

1. `chore:` module, config, Makefile, `.env.example`
2. `feat:` domain, validation, evaluator, with unit tests
3. `feat:` Postgres store and migrations
4. `feat:` HTTP API, auth, error mapping, with tests
5. `chore:` Dockerfile and Compose

Each commit is pushed to `origin main` after `gofmt`, `go vet`, and `go test -race` pass.
