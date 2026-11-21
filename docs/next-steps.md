# Next steps and improvements

What exists today, what was deliberately left out of a three-hour build, and how each gap would be closed. Ordered by how much it matters in production.

## 1. Observability

### What exists

| Signal | Where | What it answers |
|---|---|---|
| Access logs | stdout, JSON, one line per request: request ID, method, route pattern, status, duration, actor | What happened to a request; who made a change |
| Error logs | stdout, with the same request ID that the client received in `X-Request-ID` | Why a request failed |
| Metrics | `GET /metrics` (needs an API key), Prometheus text format | Traffic, error rate, average latency, cache hit ratio, cache size |
| Health | `GET /healthz` (process is up), `GET /readyz` (database reachable) | Should this instance receive traffic |
| Platform | DigitalOcean App Platform Insights and alerts (see `deploy/app.yaml`) | CPU, memory, restarts, failed deployments |
| Per response | `X-Cache: HIT, MISS, BYPASS` | Was this evaluation served from the cache |

The metrics:

| Metric | Type | Use |
|---|---|---|
| `http_requests_total{route,status}` | counter | Request rate and error rate per route. `status="429"` is rate limiting; `5xx` is the service failing. |
| `http_request_duration_seconds_sum` and `_count` by `route` | summary | Average latency: rate of `_sum` divided by rate of `_count`. |
| `evaluation_cache_results_total{result}` | counter | Hit ratio: `hit / (hit + miss)`. A falling ratio means more database load. |
| `evaluation_cache_entries` | gauge | If it sits at `CACHE_MAX_ENTRIES`, the cache is full and evicting. |

Labels are route patterns and fixed words, never flag names or user IDs. That keeps the number of series small and keeps personal data out of the monitoring system.

### How to extend it

| Step | What to do | Why |
|---|---|---|
| Latency percentiles | Replace the hand-written collector with `prometheus/client_golang` and record a histogram. | Averages hide slow requests. The p99 of the evaluation route is the number callers feel. |
| Scrape and dashboards | Run Prometheus (or a hosted equivalent) scraping `/metrics` with a reader key; Grafana on top. | Today the numbers are only visible by calling the endpoint. |
| Service level objectives | For example: 99.9% of evaluations succeed, and 99% finish within 50 ms. Alert on the rate at which the error budget is spent. | Alerts tied to what users experience, not to CPU graphs. |
| More metrics | Database pool usage and wait time (`pgxpool.Stat`), cache evictions, invalidations, audit rows written. | The pool is the first thing to saturate; evictions show an undersized cache. |
| Tracing | OpenTelemetry: one span per request, child spans for cache lookup and database query, trace ID in the logs. | Shows where the time of one slow request went. |
| Log shipping | Forward App Platform logs to a log store (OpenSearch, Datadog, Loki) with retention. | Platform logs are kept only briefly and cannot be searched across time. |
| Audit alerts | Alert on unusual audit activity, for example many flags changed by one actor in a minute. | The audit trail is also a security signal. |

## 2. Caching

| Gap today | Improvement |
|---|---|
| The cache is per process, so with several instances a change reaches the others only when their entries expire (60 seconds at most). | Put a shared cache (Redis or Valkey) behind the existing `Cache` interface, or keep the in-process cache and broadcast invalidations to every instance (Postgres `LISTEN/NOTIFY` or Redis pub/sub). |
| Eviction removes an arbitrary entry, and expired entries are only removed when read again. | On eviction, sample a few entries and drop an expired one first (the approach Redis uses), or add a periodic sweep. Full LRU only if the access pattern justifies it. |
| Results are cached per (flag, user), so memory grows with flags times active users. | Cache the flag definition once per flag and evaluate in memory per request. Evaluation is a pure function and cheap, so this trades a little CPU for far less memory and far fewer misses. |
| A burst of misses for the same key each runs its own database query. | Collapse concurrent identical lookups into one (`singleflight`). |

## 3. Security

| Gap today | Improvement |
|---|---|
| API keys live in an environment variable; changing one means a redeploy. | Store hashed keys in the database with create, rotate, and revoke endpoints, expiry dates, and last-used times. |
| Two roles for the whole service. | Scope keys to projects or environments; separate "evaluate only" from "read flag definitions". |
| The deployed app connects as the database's default admin user. | A dedicated user that owns only this schema; a separate migration user so the running service cannot alter tables. |
| Rate limiting is per key and per process, and unauthenticated requests are not limited. | A shared limiter (Redis) and an IP-based limit or a web application firewall in front for unauthenticated traffic. |
| The audit trail can be changed by anyone with database access. | Revoke `UPDATE` and `DELETE` on `audit_log` for the service user; export entries to append-only storage. |

## 4. Reliability and operations

| Gap today | Improvement |
|---|---|
| One instance. A deployment or crash is a short outage. | Two or more instances once the cache is shared (section 2). |
| Deployment is started by hand. | Deploy from CI after tests pass on `main`, to a staging app first, then production. |
| The single-node database has daily backups only. | A standby node for failover; point-in-time recovery is already available on DigitalOcean managed databases and should be tested by restoring. |
| The audit table grows without bound. | Partition by month and archive old partitions. |
| Migrations run at startup. | Keep that for a single instance; with several, run them as a separate pre-deploy job so that old and new versions overlap safely. |

## 5. Product

| Gap today | Improvement |
|---|---|
| Callers ask the service on every evaluation. | Client libraries that download flag definitions, evaluate locally, and receive updates by streaming (server-sent events). This is how LaunchDarkly keeps evaluation off the network path. |
| Targeting is by user ID only. | Rules on user attributes (plan, country) and reusable segments. |
| Flags are on or off. | Multivariate flags that return a string or JSON value, for experiments and configuration. |
| One flat list of flags. | Projects and environments (staging, production) with separate states. |
| No bulk evaluation. | An endpoint that returns every flag for one user in a single call. |
| Stale flags accumulate. | Track when each flag was last evaluated and report flags that are safe to remove. |
