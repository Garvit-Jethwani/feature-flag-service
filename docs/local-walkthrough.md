# Local walkthrough

Runs the service on one machine and exercises every feature with `curl`. Every command and output below was run against this repository on 21 Nov 2026; IDs, timestamps, and request IDs will differ on your run.

For the same steps against the deployed service, see [`live-walkthrough.md`](live-walkthrough.md). For what the service does internally at each step, see [`cujs.md`](cujs.md).

## What runs locally

| Piece | Where | Notes |
|---|---|---|
| API | `http://127.0.0.1:8080` | The Go binary in a distroless container, non-root, read-only filesystem |
| PostgreSQL 17 | `127.0.0.1:5432` | Container with a named volume; schema is created by the API at startup |
| Cache and rate limiter | Inside the API process | Restarting the API empties both |

## 0. Start

Needs Docker with Compose, `make`, `curl`, and `jq`. Go 1.27 is needed only for step 14.

```sh
make up
```

`make up` writes `.env` with random credentials on the first run, builds the image, and waits until both containers are healthy. Then load the two API keys into the shell without printing them:

```sh
B=http://127.0.0.1:8080
ADMIN=$(grep '^API_KEYS=' .env | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="admin"{print $3}')
READER=$(grep '^API_KEYS=' .env | cut -d= -f2 | tr ',' '\n' | awk -F: '$2=="reader"{print $3}')
```

## 1. Health

```sh
curl -s $B/healthz
curl -s $B/readyz
```

```
{"status":"ok"}
{"status":"ready"}
```

`/healthz` says the process is up. `/readyz` also checks the database.

## 2. Create a flag

```sh
curl -s -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"new-checkout","description":"New checkout flow","enabled":false}'
```

```json
{"name":"new-checkout","description":"New checkout flow","enabled":false,"rollout_percentage":100,"prerequisites":[],"created_at":"2026-11-21T09:23:21.447179Z","updated_at":"2026-11-21T09:23:21.447179Z","overrides":[]}
```

## 3. Evaluate it, twice

```sh
curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"
```

```
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json; charset=utf-8
X-Cache: MISS
X-Content-Type-Options: nosniff
X-Request-Id: 23b210a8b2bd9313bcec911a

{"flag":"new-checkout","user_id":"user-42","enabled":false,"reason":"global_disabled"}
```

Run the same command again: the answer is identical and the header is now `X-Cache: HIT`. The first call read the database; the second was answered from memory.

## 4. Per-user override

Turn the flag on for one user while it stays off for everyone else.

```sh
curl -s -X PUT $B/v1/flags/new-checkout/overrides/user-42 -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{"enabled":true}'

curl -s "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"
curl -s "$B/v1/flags/new-checkout/evaluate?user_id=user-7"  -H "Authorization: Bearer $READER"
```

```
{"user_id":"user-42","enabled":true,"updated_at":"2026-11-21T09:23:21.479451Z"}
{"flag":"new-checkout","user_id":"user-42","enabled":true,"reason":"user_override"}
{"flag":"new-checkout","user_id":"user-7","enabled":false,"reason":"global_disabled"}
```

`user-42` was cached as off in step 3. Setting the override dropped that entry, so the next evaluation shows the new answer at once.

## 5. Global toggle

```sh
curl -s -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{"enabled":true}'

curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-7" -H "Authorization: Bearer $READER" | grep -i -E 'x-cache|^\{'
```

```
X-Cache: MISS
{"flag":"new-checkout","user_id":"user-7","enabled":true,"reason":"global_enabled"}
```

`user-7` was cached as off a moment ago; the toggle invalidated it (`MISS`, new answer).

## 6. Validation and status codes

```sh
code() { curl -s -o /dev/null -w '%{http_code}\n' "$@"; }
J='Content-Type: application/json'

code -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"name":"new checkout"}'    # name with a space
code "$B/v1/flags/does-not-exist/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"       # missing flag
code -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"name":"new-checkout"}'    # duplicate name
code $B/v1/flags                                                                                    # no key
code $B/v1/flags -H "Authorization: Bearer not-a-real-key"                                          # wrong key
code -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $READER" -H "$J" -d '{"enabled":false}'               # reader key on a write
code -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"enabled":true,"actor":"mallory"}' # unknown field
code "$B/v1/flags/new-checkout/evaluate" -H "Authorization: Bearer $READER"                         # missing user_id
```

```
400
404
409
401
401
403
400
400
```

The error body always has the same shape:

```sh
curl -s -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"name":"new checkout"}'
curl -s "$B/v1/flags/does-not-exist/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"
```

```json
{"error":{"code":"invalid_argument","message":"name: must be 1 to 64 characters of lowercase letters, digits, '.', '_' or '-', starting with a letter or digit"}}
{"error":{"code":"flag_not_found","message":"flag not found"}}
```

The "unknown field" case is also the proof that a caller cannot name themselves as the actor: the body is rejected, and the audit actor always comes from the API key.

## 7. Percentage rollout

```sh
curl -s -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"rollout_percentage":25}' | jq -c '{name, enabled, rollout_percentage}'

for i in 1 2 3 4 5 6 7 8; do
  curl -s "$B/v1/flags/new-checkout/evaluate?user_id=user-$i" -H "Authorization: Bearer $READER" | jq -r '"\(.user_id) \(.enabled) \(.reason)"'
done

curl -s "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"
```

```
{"name":"new-checkout","enabled":true,"rollout_percentage":25}
user-1 false rollout
user-2 false rollout
user-3 true rollout
user-4 false rollout
user-5 false rollout
user-6 false rollout
user-7 true rollout
user-8 false rollout
{"flag":"new-checkout","user_id":"user-42","enabled":true,"reason":"user_override"}
```

Two of eight users are on at 25%. The answers are the same on every run and on every machine, because the bucket is a hash of the flag name and the user ID. `user-42` is on regardless: an override beats the rollout.

Set it back to everyone before the next step:

```sh
curl -s -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"rollout_percentage":100}' | jq -c '{name, rollout_percentage}'
```

## 8. Flag dependencies

Make `new-checkout` require a second flag that is off.

```sh
curl -s -X POST $B/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"name":"payments-v2","enabled":false}' | jq -c '{name, enabled}'
curl -s -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"prerequisites":["payments-v2"]}' | jq -c '{name, prerequisites}'

curl -s "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER"
```

```
{"name":"payments-v2","enabled":false}
{"name":"new-checkout","prerequisites":["payments-v2"]}
{"flag":"new-checkout","user_id":"user-42","enabled":false,"reason":"prerequisite_failed"}
```

`user-42` has an override of `true` and is still off: a prerequisite is a hard gate. Now turn the prerequisite on. Note that the change is made to `payments-v2`, and the cached result of `new-checkout` is dropped with it:

```sh
curl -s -X PATCH $B/v1/flags/payments-v2 -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"enabled":true}' | jq -c '{name, enabled}'
curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER" | grep -i -E 'x-cache|^\{'
```

```
{"name":"payments-v2","enabled":true}
X-Cache: MISS
{"flag":"new-checkout","user_id":"user-42","enabled":true,"reason":"user_override"}
```

Changes that would break the dependency graph are refused:

```sh
curl -s -X PATCH $B/v1/flags/payments-v2 -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"prerequisites":["new-checkout"]}'   # a cycle
curl -s -X DELETE $B/v1/flags/payments-v2 -H "Authorization: Bearer $ADMIN"                                                  # still required
```

```json
{"error":{"code":"dependency_cycle","message":"this change would make a flag require itself, directly or through other flags"}}
{"error":{"code":"flag_has_dependents","message":"other flags require this flag; remove those dependencies first"}}
```

Both are `409`.

## 9. Audit trail

```sh
curl -s $B/v1/flags/new-checkout/audit -H "Authorization: Bearer $ADMIN" | jq -r '.entries[] | "\(.id)  \(.action)  by \(.actor)"'
```

```
19  flag.updated  by local-admin
17  flag.updated  by local-admin
16  flag.updated  by local-admin
15  flag.updated  by local-admin
14  override.set  by local-admin
13  flag.created  by local-admin
```

Newest first: prerequisites set, rollout back to 100, rollout to 25, enabled, override, created. The rejected requests from step 6 left no entries; only changes that were saved are recorded. (Entry 18 belongs to the other flag, `payments-v2`.) One entry in full:

```sh
curl -s "$B/v1/flags/new-checkout/audit?limit=1" -H "Authorization: Bearer $ADMIN" | jq '.entries[0]'
```

```json
{
  "id": 19,
  "flag": "new-checkout",
  "action": "flag.updated",
  "actor": "local-admin",
  "previous": {
    "enabled": true,
    "description": "New checkout flow",
    "prerequisites": [],
    "rollout_percentage": 100
  },
  "new": {
    "enabled": true,
    "description": "New checkout flow",
    "prerequisites": [
      "payments-v2"
    ],
    "rollout_percentage": 100
  },
  "occurred_at": "2026-11-21T09:23:21.738632Z"
}
```

The audit trail is admin-only, and the rows are ordinary rows in Postgres:

```sh
curl -s -o /dev/null -w '%{http_code}\n' $B/v1/audit -H "Authorization: Bearer $READER"
docker compose exec -T db psql -U flags -d flags -c 'SELECT id, flag_name, action, actor FROM audit_log ORDER BY id DESC LIMIT 3'
```

```
403
 id |  flag_name   |    action    |    actor
----+--------------+--------------+-------------
 20 | payments-v2  | flag.updated | local-admin
 19 | new-checkout | flag.updated | local-admin
 18 | payments-v2  | flag.created | local-admin
```

## 10. Bypass the cache

```sh
curl -si "$B/v1/flags/new-checkout/evaluate?user_id=user-42" -H "Authorization: Bearer $READER" -H 'Cache-Control: no-cache' | grep -i x-cache
```

```
X-Cache: BYPASS
```

## 11. Metrics

```sh
curl -s $B/metrics -H "Authorization: Bearer $READER" | grep -E 'evaluate|cache' | grep -v '^#'
```

```
http_requests_total{route="GET /v1/flags/{name}/evaluate",status="200"} 17
http_requests_total{route="GET /v1/flags/{name}/evaluate",status="400"} 1
http_requests_total{route="GET /v1/flags/{name}/evaluate",status="404"} 2
http_request_duration_seconds_sum{route="GET /v1/flags/{name}/evaluate"} 0.011837541000000002
http_request_duration_seconds_count{route="GET /v1/flags/{name}/evaluate"} 20
evaluation_cache_results_total{result="hit"} 1
evaluation_cache_results_total{result="miss"} 15
evaluation_cache_results_total{result="bypass"} 1
evaluation_cache_entries 1
```

The label is the route pattern, so no flag name or user ID appears. The hit count is low here because this walkthrough changes the flag between almost every evaluation, and each change invalidates.

## 12. Logs

```sh
docker compose logs --no-log-prefix --tail 2 api
```

```json
{"time":"2026-11-21T09:23:21.8249701Z","level":"INFO","msg":"request","request_id":"17ba522ebf2ee6a0821dfe38","method":"GET","route":"GET /v1/audit","status":403,"duration_ms":0,"actor":"local-reader"}
{"time":"2026-11-21T09:23:21.915249551Z","level":"INFO","msg":"request","request_id":"9e28c4a39ec449c9d498d781","method":"GET","route":"GET /v1/flags/{name}/evaluate","status":200,"duration_ms":0,"actor":"local-reader"}
```

One JSON line per request. No API key, no body, no real path. The `request_id` is the one returned to the client in `X-Request-Id`.

## 13. Rate limiting

The default limit (100 requests a second per key, bursts of 200) is too high to hit by hand, so restart the API with a tiny one. This also empties the cache and the metrics, because both live in the process.

```sh
RATE_LIMIT_RPS=1 RATE_LIMIT_BURST=3 docker compose up -d --wait api

for i in 1 2 3 4 5; do curl -s -o /dev/null -w "request $i: %{http_code}\n" $B/v1/flags -H "Authorization: Bearer $READER"; done
curl -si $B/v1/flags -H "Authorization: Bearer $READER" | grep -i -E '^HTTP|retry-after|^\{'
curl -s -o /dev/null -w "admin key at the same moment: %{http_code}\n" $B/v1/flags -H "Authorization: Bearer $ADMIN"
```

```
request 1: 200
request 2: 200
request 3: 200
request 4: 429
request 5: 429
HTTP/1.1 429 Too Many Requests
Retry-After: 1
{"error":{"code":"rate_limited","message":"too many requests for this API key; retry later"}}
admin key at the same moment: 200
```

Each key has its own allowance: the reader key is limited while the admin key is not. Restore the defaults:

```sh
docker compose up -d --wait api
```

## 14. Tests

```sh
make lint    # gofmt check and go vet
make test    # every test, with the race detector, against the Postgres container
```

```
ok  	github.com/Garvit-Jethwani/feature-flag-service/internal/cache	1.558s
ok  	github.com/Garvit-Jethwani/feature-flag-service/internal/config	2.376s
ok  	github.com/Garvit-Jethwani/feature-flag-service/internal/flag	2.104s
ok  	github.com/Garvit-Jethwani/feature-flag-service/internal/httpapi	3.744s
ok  	github.com/Garvit-Jethwani/feature-flag-service/internal/store	3.650s
```

| Package | What its tests cover |
|---|---|
| `internal/flag` | The evaluator as a pure function: global on, global off, per-user override, prerequisites, rollout, depth limit |
| `internal/cache` | Expiry, invalidation, the stale write-back guard, the entry limit |
| `internal/httpapi` | End to end over real HTTP against real Postgres: lifecycle, status codes, auth, cache and invalidation, audit, rollout, prerequisites, metrics |
| `internal/store` | Queries, transactions, audit rows, migrations |
| `internal/config` | Environment parsing |

Each database-backed test creates its own schema and drops it afterwards, so tests do not touch the data from the steps above. The same suite runs in GitHub Actions on every push.

## 15. Clean up

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN" -H "$J" -d '{"prerequisites":[]}'
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $B/v1/flags/new-checkout -H "Authorization: Bearer $ADMIN"
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $B/v1/flags/payments-v2 -H "Authorization: Bearer $ADMIN"

curl -s "$B/v1/flags/new-checkout/audit?limit=1" -H "Authorization: Bearer $ADMIN" | jq -r '.entries[] | "\(.action) by \(.actor)"'
```

```
200
204
204
flag.deleted by local-admin
```

The flag is gone, and its history is still readable. To stop the containers (the data stays in the Docker volume):

```sh
make down
```
