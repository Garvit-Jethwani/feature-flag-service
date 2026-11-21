# Setup and walkthrough

A short path through the service: start it, then nine steps that show flag CRUD, cached evaluation, invalidation, per-user override, status codes, the audit trail, and metrics. About five minutes.

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

### Step 2: create a flag (admin key)

```sh
curl -s -X POST $U/v1/flags -H "Authorization: Bearer $ADMIN" -H "$J" \
  -d '{"name":"demo-flag","description":"Demo","enabled":true}'
```

```
{"name":"demo-flag","description":"Demo","enabled":true,"rollout_percentage":100,"prerequisites":[],"overrides":[],"created_at":"...","updated_at":"..."}
```

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

### Step 9: clean up

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $U/v1/flags/demo-flag -H "Authorization: Bearer $ADMIN"
```

```
204
```

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
