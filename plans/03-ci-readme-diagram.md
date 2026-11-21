# Plan 03: CI, README, and architecture diagram

Status: Implemented. Approved in advance by the owner on 21 Nov 2026 ("create the plan 3, consider it approved"), with the instruction to keep it quick: correctness matters, extra tests do not.

Covers E1 (architecture diagram), E4 (CI), and E5 (README) from `SPECS.md`. E3 (tests) is already met by plans 01 and 02. Budget: 10 minutes.

## What gets built

| # | File | Content |
|---|---|---|
| 1 | `.github/workflows/ci.yml` | On every push and pull request: `gofmt` check, `go vet`, `go test -race ./...` against a Postgres service container, and a Docker image build. |
| 2 | `README.md` | Setup, API with examples, evaluation rules, how caching and invalidation work, audit, auth and rate limiting, configuration, testing, known limits. |
| 3 | `docs/architecture.md` | Final diagram of the evaluation request path (where the cache sits, when it is consulted, bypassed, and invalidated) and of the write path. `SPECS.md` links to it instead of keeping a second copy. |
| 4 | `internal/cache/cache.go` | Comment-only fix: the eviction comment claimed the entry limit is rarely reached. It is not; expired entries are only removed when read again. |

## CI details

- **Database:** a `postgres:17-alpine` service container, the same major version as local. It uses trust authentication, so the workflow contains no password at all. The container lives only for the job and is reachable only from the runner.
- **`CI=true`** is set by GitHub, which makes database-backed tests fail instead of skip when `TEST_DATABASE_URL` is missing. A misconfigured pipeline therefore cannot pass by silently skipping tests.
- **Go version** is read from `go.mod`, so local and CI cannot drift.
- **Supply chain:** actions are pinned to commit SHAs; the workflow token is read-only (`contents: read`).

## Not in this plan

- **More tests.** The spec asks for evaluator unit tests and at least one HTTP end-to-end test; both exist (`internal/flag/evaluate_test.go`, `internal/httpapi/api_test.go`, `internal/httpapi/cache_audit_test.go`).
- **Dockerfile changes.** It is already multi-stage, static, distroless, and non-root. CI only proves it still builds.
- **Linters beyond `go vet`,** image publishing, and deployment (plan 05).

## Verification

1. `make lint` and `make test` pass locally.
2. The GitHub Actions run for the pushed commit is green, and its log shows the database-backed tests ran (not skipped).
3. The Mermaid diagrams render on GitHub.

Result: 1 and 2 passed (CI ran 173 test cases, none skipped). 3 is **not checked**: the agent's browser is not allowed to open github.com, so the owner needs to look at `docs/architecture.md` on GitHub once.

## Commits

1. `docs: add plan 03`
2. `ci: run vet, race tests against Postgres, and image build on push`
3. `docs: add README and architecture diagrams`
