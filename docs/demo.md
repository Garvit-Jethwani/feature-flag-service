# Setup and walkthrough

A short path through the service: start it, then nine steps that show flag CRUD, cached evaluation, invalidation, per-user override, status codes, the audit trail, and metrics. About five minutes.

Each step ends with **In the code**: the functions that handled it, in call order, and the test that covers it. Section 4 is a map of the packages.

The same steps run against the local stack or the deployed service; only the setup differs. Longer versions with more cases (rollout, dependencies, rate limiting): [`local-walkthrough.md`](local-walkthrough.md), [`live-walkthrough.md`](live-walkthrough.md).

## 1. Setup

You need Docker with Compose, `make`, `curl`, and `jq`. Go 1.27 is needed only for `make test`.

### Local

```sh
make up        # writes .env with random local credentials, builds, starts API + Postgres
```

```sh
U=http://127.0.0.1:8080
KEYS=.env
```

### Deployed service

Nothing to start. The API keys of the deployed service are not in this repository; put them in a git-ignored `.env.production` in the same `API_KEYS=actor:role:token,...` form.

```sh
U=https://feature-flag-service-ey3u3.ondigitalocean.app
KEYS=.env.production
```

### Both: load the keys and two helpers

This reads the keys into shell variables without printing them.

```sh
ADMIN=$(grep '^API_KEYS=' $KEYS | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="admin"{print $3}')
READER=$(grep '^API_KEYS=' $KEYS | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="reader"{print $3}')
J='Content-Type: application/json'

# ev USER: evaluate demo-flag for a user; print the cache header and the answer.
ev() { curl -si "$U/v1/flags/demo-flag/evaluate?user_id=$1" -H "Authorization: Bearer $READER" | grep -i -E 'x-cache|^\{'; }
```

## 2. Walkthrough

### Step 1: the service is up and can reach its database

```sh
curl -s $U/readyz
```

```
{"status":"ready"}
```

**In the code:** [`readyz`](../internal/httpapi/api.go#L116) pings the database through [`Store.Ping`](../internal/store/store.go#L55). Test: [`TestHealthAndReadiness`](../internal/httpapi/api_test.go#L472).

### Step 2: create a flag (admin key)

```sh
curl -s -X POST $U/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" \
  -d '{"name":"demo-flag","description":"Demo","enabled":true}'
```

```
{"name":"demo-flag","description":"Demo","enabled":true,"rollout_percentage":100,"prerequisites":[],"overrides":[],"created_at":"...","updated_at":"..."}
```

**In the code:** [route table](../internal/httpapi/api.go#L30) → [`require`](../internal/httpapi/auth.go#L160) (key, rate limit, role) → handler [`createFlag`](../internal/httpapi/handlers.go#L151) with [`decodeJSON`](../internal/httpapi/handlers.go#L90) → service [`CreateFlag`](../internal/service/service.go#L116) with [`ValidateName`](../internal/flag/flag.go#L131) → store [`CreateFlag`](../internal/store/store.go#L122), which inserts the flag and its audit entry in one transaction. Test: [`TestFlagLifecycle`](../internal/httpapi/api_test.go#L186).

### Step 3: evaluate twice (reader key)

```sh
ev user-42
ev user-42
```

```
X-Cache: MISS
{"flag":"demo-flag","user_id":"user-42","enabled":true,"reason":"global_enabled"}
X-Cache: HIT
{"flag":"demo-flag","user_id":"user-42","enabled":true,"reason":"global_enabled"}
```

The first call read Postgres and cached the result. The second was answered from memory.

**In the code:** handler [`evaluate`](../internal/httpapi/handlers.go#L296) → service [`Evaluate`](../internal/service/service.go#L254), which is the whole read path: cache [`Get`](../internal/cache/cache.go#L56) → on a miss, store [`EvaluationSnapshot`](../internal/store/store.go#L405) (one query) → the pure evaluator [`flag.Evaluate`](../internal/flag/evaluate.go#L58) → cache [`Set`](../internal/cache/cache.go#L82). Tests: [`TestEvaluate`](../internal/flag/evaluate_test.go#L26) (rules), [`TestEvaluationCacheAndInvalidation`](../internal/httpapi/cache_audit_test.go#L39) (cache).

### Step 4: turn the flag off, evaluate again

```sh
curl -s -X PATCH $U/v1/flags/demo-flag -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"enabled":false}' | jq -c '{name, enabled}'
ev user-42
```

```
{"name":"demo-flag","enabled":false}
X-Cache: MISS
{"flag":"demo-flag","user_id":"user-42","enabled":false,"reason":"global_disabled"}
```

The write dropped the cached "on" result. The change is visible at once, not after the 60-second TTL.

**In the code:** handler [`updateFlag`](../internal/httpapi/handlers.go#L221) → service [`UpdateFlag`](../internal/service/service.go#L165) → store [`UpdateFlag`](../internal/store/store.go#L217), the write transaction: [`lockFlag`](../internal/store/store.go#L81) (row lock), update, [`insertAudit`](../internal/store/audit.go#L20), [`affectedFlags`](../internal/store/store.go#L97) (the flag and its dependents), commit. Then the service calls [`invalidate`](../internal/service/service.go#L109) → cache [`Invalidate`](../internal/cache/cache.go#L111). Tests: [`TestEvaluationCacheAndInvalidation`](../internal/httpapi/cache_audit_test.go#L39), [`TestSetWithOldGenerationIsIgnored`](../internal/cache/cache_test.go#L76) (a slow reader cannot put a stale value back).

### Step 5: override for one user

```sh
curl -s -X PUT $U/v1/flags/demo-flag/overrides/user-42 -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"enabled":true}'
ev user-42
ev user-7
```

```
{"user_id":"user-42","enabled":true,"updated_at":"..."}
X-Cache: MISS
{"flag":"demo-flag","user_id":"user-42","enabled":true,"reason":"user_override"}
X-Cache: MISS
{"flag":"demo-flag","user_id":"user-7","enabled":false,"reason":"global_disabled"}
```

The flag is off globally, but on for `user-42`: the override wins. `user-7` still gets the global value.

**In the code:** handler [`setOverride`](../internal/httpapi/handlers.go#L257) → service [`SetOverride`](../internal/service/service.go#L213) → store [`SetOverride`](../internal/store/store.go#L314) (an upsert, audited, then invalidated like step 4). The rule "override before global toggle" is two lines of the evaluator: [`evaluate.go`](../internal/flag/evaluate.go#L84). Tests: [`TestEvaluate`](../internal/flag/evaluate_test.go#L26), [`TestOverrides`](../internal/store/store_test.go#L108).

### Step 6: status codes

```sh
curl -s "$U/v1/flags/nope/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"; echo                    # missing flag
curl -s -o /dev/null -w '%{http_code}\n' $U/v1/flags                                                             # no key
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $U/v1/flags/demo-flag -H "Authorization: Bearer $READER"     # reader key on a write
curl -s -X POST $U/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"name":"Has Space"}'; echo            # invalid name
```

```
{"error":{"code":"flag_not_found","message":"flag not found"}}
401
403
{"error":{"code":"invalid_argument","message":"name: must be 1 to 64 characters of lowercase letters, digits, '.', '_' or '-', starting with a letter or digit"}}
```

A missing flag is `404`, not `500`. The invalid name is `400`.

**In the code:** every error becomes a status code in one function, [`classify`](../internal/httpapi/errors.go#L38). `401` and `403` come from [`require`](../internal/httpapi/auth.go#L160); the name rule is [`ValidateName`](../internal/flag/flag.go#L131); unknown JSON fields and oversized bodies are rejected in [`decodeJSON`](../internal/httpapi/handlers.go#L90). Tests: [`TestErrorResponses`](../internal/httpapi/api_test.go#L265), [`TestAuthorizationOverHTTP`](../internal/httpapi/api_test.go#L406).

### Step 7: audit trail (admin key)

```sh
curl -s $U/v1/flags/demo-flag/audit -H "Authorization: Bearer $ADMIN" \
  | jq -c '.entries[] | {action, actor, previous: .previous.enabled, new: .new.enabled}'
```

```
{"action":"override.set","actor":"local-admin","previous":null,"new":true}
{"action":"flag.updated","actor":"local-admin","previous":true,"new":false}
{"action":"flag.created","actor":"local-admin","previous":null,"new":true}
```

Newest first. The actor is the name bound to the API key (`prod-admin` on the deployed service); the rejected requests of step 6 left no entries. Drop the `jq` filter to see the full entry with timestamps and the whole flag before and after.

**In the code:** written by [`insertAudit`](../internal/store/audit.go#L20) inside each write transaction; read by handler [`flagAudit`](../internal/httpapi/handlers.go#L406) → service [`FlagAudit`](../internal/service/service.go#L316) → store [`ListAudit`](../internal/store/audit.go#L55). Table: [`0002_audit.sql`](../internal/store/migrations/0002_audit.sql). Test: [`TestAuditTrail`](../internal/httpapi/cache_audit_test.go#L105).

### Step 8: metrics

```sh
curl -s $U/metrics -H "Authorization: Bearer $READER" | grep '^evaluation_cache'
```

```
evaluation_cache_results_total{result="hit"} 1
evaluation_cache_results_total{result="miss"} 4
evaluation_cache_results_total{result="bypass"} 0
evaluation_cache_entries 2
```

One hit and four misses, matching steps 3 to 5. The counters start at zero when the process starts, so a long-running service shows larger numbers.

**In the code:** the [`observe`](../internal/httpapi/middleware.go#L78) middleware counts every request; the evaluate handler reports the cache result through [`observeCacheResult`](../internal/httpapi/metrics.go#L65); [`write`](../internal/httpapi/metrics.go#L76) prints the Prometheus text. Test: [`TestMetrics`](../internal/httpapi/metrics_test.go#L36).

### Step 9: clean up

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $U/v1/flags/demo-flag -H "Authorization: Bearer $ADMIN"
```

```
204
```

**In the code:** handler [`deleteFlag`](../internal/httpapi/handlers.go#L247) → service [`DeleteFlag`](../internal/service/service.go#L198) → store [`DeleteFlag`](../internal/store/store.go#L287). The overrides go with the flag; the audit entries stay.

## 3. Local only

```sh
make test      # every test with the race detector, against the local Postgres
```

```sh
# Rate limiting: restart the API with a tiny limit, send five requests, restore the default.
RATE_LIMIT_RPS=1 RATE_LIMIT_BURST=3 docker compose up -d --wait api
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "request $i: %{http_code}\n" $U/v1/flags -H "Authorization: Bearer $READER"; done
docker compose up -d --wait api
```

```
request 1: 200
request 2: 200
request 3: 200
request 4: 429
request 5: 429
```

```sh
# The audit entries are ordinary rows, written in the same transaction as the change.
docker compose exec -T db psql -U flags -d flags -c 'SELECT id, flag_name, action, actor FROM audit_log ORDER BY id DESC LIMIT 5'

# One JSON log line per request: no API key, no body, no raw path.
docker compose logs --no-log-prefix --tail 3 api
```

Stop everything with `make down`.

**In the code:** rate limiting is a token bucket per key, [`allow`](../internal/httpapi/ratelimit.go#L53), called from [`require`](../internal/httpapi/auth.go#L160); test [`TestRateLimitIsPerKey`](../internal/httpapi/ratelimit_test.go#L67). The log line is written by [`observe`](../internal/httpapi/middleware.go#L78); test [`TestLogsDoNotContainUserIDsOrTokens`](../internal/httpapi/api_test.go#L447).

## 4. Code map

A request goes down through the layers; each layer knows only the one below it.

```
cmd/server  →  internal/httpapi  →  internal/service  →  internal/store  →  PostgreSQL
  wiring        HTTP, auth           use cases,           SQL, transactions,
                                     cache + invalidation  audit rows
                                          │
                                          ├→ internal/cache   results in memory
                                          └→ internal/flag    types, validation, pure evaluator
```

| Package | What it does | Start reading at |
|---|---|---|
| [`cmd/server`](../cmd/server/main.go) | Loads config, connects, migrates, builds the layers, serves, shuts down gracefully | [`run`](../cmd/server/main.go#L38) |
| [`internal/config`](../internal/config/config.go) | Environment variables to a typed struct; errors never contain values | [`Load`](../internal/config/config.go#L40) |
| [`internal/httpapi`](../internal/httpapi/api.go) | Routes, auth, rate limit, JSON decoding, error mapping, logs, metrics | [route table](../internal/httpapi/api.go#L30), [`require`](../internal/httpapi/auth.go#L160), [`classify`](../internal/httpapi/errors.go#L38), [handlers](../internal/httpapi/handlers.go#L151) |
| [`internal/service`](../internal/service/service.go) | Use cases: validate, call the store, invalidate after commit, cached evaluation | [`Evaluate`](../internal/service/service.go#L254), [`UpdateFlag`](../internal/service/service.go#L165), [`invalidate`](../internal/service/service.go#L109) |
| [`internal/flag`](../internal/flag/flag.go) | Domain with no I/O: types, validation rules, errors, the evaluator | [`Evaluate`](../internal/flag/evaluate.go#L58), [`Bucket`](../internal/flag/evaluate.go#L101), [`ValidateName`](../internal/flag/flag.go#L131) |
| [`internal/cache`](../internal/cache/cache.go) | Results per flag and user in memory: TTL, entry limit, generation counter | [`Get`](../internal/cache/cache.go#L56), [`Set`](../internal/cache/cache.go#L82), [`Invalidate`](../internal/cache/cache.go#L111) |
| [`internal/store`](../internal/store/store.go) | PostgreSQL: queries, transactions, audit rows, dependency graph check, migrations | [`UpdateFlag`](../internal/store/store.go#L217), [`EvaluationSnapshot`](../internal/store/store.go#L405), [`insertAudit`](../internal/store/audit.go#L20), [`checkDependencyGraph`](../internal/store/prerequisites.go#L84), [`Migrate`](../internal/store/migrate.go#L25) |

| Also | Where |
|---|---|
| Schema | [`0001_init.sql`](../internal/store/migrations/0001_init.sql), [`0002_audit.sql`](../internal/store/migrations/0002_audit.sql) |
| Tests | Next to the code, in `*_test.go`. End to end over HTTP with a real Postgres: [`api_test.go`](../internal/httpapi/api_test.go), [`cache_audit_test.go`](../internal/httpapi/cache_audit_test.go), [`rollout_prereq_test.go`](../internal/httpapi/rollout_prereq_test.go) |
| CI | [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) |
| Container and local stack | [`Dockerfile`](../Dockerfile), [`docker-compose.yml`](../docker-compose.yml), [`Makefile`](../Makefile) |
| Deployment spec | [`deploy/app.yaml`](../deploy/app.yaml) |
| Diagrams | [`architecture.md`](architecture.md) |
