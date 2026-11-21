# Plan 06: Minimal observability

Status: Approved by the owner on 21 Nov 2026 ("a minute version of observability, and a separate doc on how it can be extended"). Implementation in progress.

Covers X4 from `SPECS.md` in its smallest useful form. Budget: 10 minutes.

## What already exists

- Structured JSON access logs: request ID, method, route pattern, status, duration, actor. No keys, bodies, or raw paths.
- `GET /healthz` and `GET /readyz`.
- `X-Cache` on every evaluation response.

## What gets built

| # | Item | Detail |
|---|---|---|
| 1 | `GET /metrics` | Prometheus text format, written with the standard library (no new dependency). Requires an API key (either role), because the service is on the public internet. |
| 2 | Request metrics | `http_requests_total{route,status}` and `http_request_duration_seconds_sum` / `_count` by route. The route label is the pattern including the method (`GET /v1/flags/{name}`), never the raw path, so the number of series stays small and no flag name or user ID leaks. |
| 3 | Cache metrics | `evaluation_cache_results_total{result="hit|miss|bypass"}` and `evaluation_cache_entries`. Together they give the hit ratio and show whether the cache sits at its entry limit. |
| 4 | Rate limiting | No separate metric: rate-limited requests are `http_requests_total{status="429"}`. |
| 5 | Platform alerts | In the App Platform spec (plan 05): failed deployment, restarts, CPU, memory. |
| 6 | `docs/next-steps.md` | How to extend this (histograms, dashboards, tracing, SLOs, log shipping) and the other improvements deliberately left out. |

## Tests

One HTTP test: after a few requests, `/metrics` reports the expected counters, and it refuses a request without a key.

## Not in this plan

Latency histograms and percentiles, a Prometheus or Grafana deployment, tracing, log forwarding. All are described in `docs/next-steps.md`.

## Commits

1. `feat: metrics endpoint in Prometheus format`
2. `docs: next steps and improvements`
