# Plan 05: Deploy to DigitalOcean

Status: Implemented. Approved by the owner on 21 Nov 2026 ("go with phase 5, draft it and implement and deploy now, run the doctl commands yourself").

Live at https://feature-flag-service-ey3u3.ondigitalocean.app. One deviation from the plan as approved (how the database URL reaches the app) is described under "How it is deployed".

Covers X1 from `SPECS.md` and decision D7 (App Platform with Managed PostgreSQL, one instance). Budget: 15 minutes, most of it waiting for the database to provision.

## What gets created

Both resources are billed until deleted. Region: Bangalore (`blr` / `blr1`).

| # | Resource | Name | Size | Cost | ID |
|---|---|---|---|---|---|
| 1 | Managed PostgreSQL 17, one node | `feature-flag-service-db` | `db-s-1vcpu-1gb` (smallest) | about $15 a month | `6d559eb0-343e-46e8-8cd9-34820051aac1` |
| 2 | App Platform app, one service `api`, one instance | `feature-flag-service` | `apps-s-1vcpu-0.5gb` (smallest) | about $5 a month | `3e841b51-914a-4734-ad87-5f76fc8b0f84` |

The database is tagged `feature-flag-service`. App Platform apps cannot be tagged, so the app carries the name instead.

## How it is deployed

- **App spec:** `deploy/app.yaml`, committed with a placeholder where the API keys go.
- **Source:** App Platform clones this public repository (`main`) and builds the existing `Dockerfile`. No registry and no GitHub integration are needed. A new deployment is started by hand with `doctl apps create-deployment`; it does not redeploy on every push.
- **Database connection (changed during deployment):** the plan was to attach the cluster in the app spec so that App Platform injects `DATABASE_URL`. App Platform rejected that twice with `databases.limit_exceeded` ("the account's Managed Database limit only allows for up to (0) new databases"), even though the cluster already existed; this is an account quota, probably related to the account's `warning` status. Instead, the connection string (TLS required) is read from the DigitalOcean API at deploy time and given to the app as an encrypted `SECRET` variable. The credentials still never appear in the repository or in a terminal. Cost of the change: if the database password is rotated, the secret must be updated by hand.
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

Results (21 Nov 2026):

| # | Result |
|---|---|
| 1 | Passed. `GET /readyz` is `200` over HTTPS. |
| 2 | Passed. Create `201`; evaluations `MISS` then `HIT`; audit entry with actor `prod-admin`; delete `204`. `/metrics` showed one hit and one miss. |
| 3 | Passed. `401` without a key. |
| 4 | Passed. The only firewall rule is the app. A connection from the deploying machine, which worked before the rule, timed out after it. The app was then restarted so that it had to open new connections through the firewall; it came back ready and served a write. |

What went wrong on the way:

1. Attaching the database in the app spec was rejected by an account quota (see "Database connection" above).
2. The first deployment failed with `password authentication failed`. Cause: `doctl databases connection` masks the password unless `--show` is passed, so the masked connection string was stored as the secret. The format check that was in place could not tell a masked password from a real one. Fix: fetch with `--show`, and test the connection string against the database before deploying. No secret was printed or committed at any point.

Left on the live service: one example flag, `welcome-banner`.

## Teardown

```sh
doctl apps delete 3e841b51-914a-4734-ad87-5f76fc8b0f84 --force
doctl databases delete 6d559eb0-343e-46e8-8cd9-34820051aac1 --force
rm .env.production
```

## Commits

1. `docs: add plans 05 and 06`
2. `feat: app spec for DigitalOcean App Platform`
3. `docs: record deployed resources and live URL`
