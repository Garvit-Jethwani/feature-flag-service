# Plan 05: Deploy to DigitalOcean

Status: Approved by the owner on 21 Nov 2026 ("go with phase 5, draft it and implement and deploy now, run the doctl commands yourself"). Implementation in progress.

Covers X1 from `SPECS.md` and decision D7 (App Platform with Managed PostgreSQL, one instance). Budget: 15 minutes, most of it waiting for the database to provision.

## What gets created

Both resources are billed until deleted. Region: Bangalore (`blr` / `blr1`).

| # | Resource | Name | Size | Cost | ID |
|---|---|---|---|---|---|
| 1 | Managed PostgreSQL 17, one node | `feature-flag-service-db` | `db-s-1vcpu-1gb` (smallest) | about $15 a month | filled in after creation |
| 2 | App Platform app, one service `api`, one instance | `feature-flag-service` | `apps-s-1vcpu-0.5gb` (smallest) | about $5 a month | filled in after creation |

The database is tagged `feature-flag-service`. App Platform apps cannot be tagged, so the app carries the name instead.

## How it is deployed

- **App spec:** `deploy/app.yaml`, committed with a placeholder where the API keys go.
- **Source:** App Platform clones this public repository (`main`) and builds the existing `Dockerfile`. No registry and no GitHub integration are needed. A new deployment is started by hand with `doctl apps create-deployment`; it does not redeploy on every push.
- **Database connection:** App Platform injects `DATABASE_URL` from the attached cluster (`${db.DATABASE_URL}`), with TLS required. The credentials never appear in the repository or in a terminal.
- **API keys:** two fresh keys (`prod-admin`, `prod-reader`) are generated on this machine, stored in `.env.production` (ignored by git, mode 600), and sent to App Platform as an encrypted `SECRET` variable. They are never printed.
- **Network:** after the app exists, the database firewall ("trusted sources") is restricted to the app, so the database accepts no other connections.
- **Health:** App Platform probes `GET /healthz`. Migrations run at startup under an advisory lock, as they do locally.
- **Alerts:** failed deployment, restarts, and CPU or memory above 80% (emailed by DigitalOcean to the account owner).

## Known shortcuts (listed in `docs/next-steps.md`)

- The app connects as the cluster's default admin user to the default database. A dedicated least-privilege user is the next step.
- One instance, because the cache and the rate limiter are per process.
- No custom domain; the app gets a DigitalOcean HTTPS URL.

## Verification

1. The deployment reaches `ACTIVE` and `GET /readyz` answers `200` over HTTPS.
2. With the production keys: create a flag, evaluate it twice (`MISS` then `HIT`), read its audit entry, delete it.
3. A request without a key is `401`.
4. The database firewall lists only the app.

## Teardown

```sh
doctl apps delete <app-id> --force
doctl databases delete <database-id> --force
rm .env.production
```

## Commits

1. `docs: add plans 05 and 06`
2. `feat: app spec for DigitalOcean App Platform`
3. `docs: record deployed resources and live URL`
