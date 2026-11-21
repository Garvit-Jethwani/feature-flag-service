# AGENTS.md

Guidance for AI agents working in this repository. Read `SPECS.md` first; it is the source of truth for what we are building.

## Project

A production-ready Feature Flag REST API in Go: flag CRUD, per-user overrides, cached evaluation, and an audit trail. This is a time-boxed exercise (about 3 hours), so prefer the simplest design that is correct and explainable.

## Environment

- All work stays inside `/Users/s-coding-interview/workspaces`. Never create or modify files outside it.
- Load the toolchain before running anything: `source /Users/s-coding-interview/workspaces/.tools/env.sh`.
- Homebrew is not installed and sudo needs a password. Install tools as binaries under `workspaces/.tools`, or run them in Docker.

## Workflow: plan first

1. Every phase starts with a plan file at `plans/NN-<slug>.md` with a `Status:` line (`Draft`, `Approved`, `Implemented`).
2. Do not write implementation code, create cloud resources, or deploy until the user has approved that plan.
3. If the work needs to deviate from an approved plan, stop and update the plan before continuing.
4. When a decision in `SPECS.md` changes, update `SPECS.md` in the same commit.

## Security (non-negotiable)

This repository is **public**. Treat everything committed as published.

- Never commit secrets: API keys, tokens, database URLs with credentials, `.env` files, cloud credentials, SSH keys. Configuration comes from environment variables; commit only `.env.example` with placeholder values.
- Never print secrets to the terminal, logs, test output, or chat. Redact tokens when showing command output.
- Before every commit, review `git diff --staged` for secrets and personal data.
- Do not log API keys or request bodies. Log user IDs only where needed for debugging.
- All SQL uses parameterized queries. No string-built SQL.
- Validate and bound every input: body size, string lengths, allowed characters. Reject unknown JSON fields.
- Mutating endpoints require authentication. The audit `actor` comes from the authenticated identity, never from a client-supplied field.
- Compare secrets in constant time. Never disable TLS verification.
- Keep dependencies minimal and pinned through `go.sum`. Prefer the standard library. Do not pipe remote scripts into a shell.
- Containers run as a non-root user on a minimal base image.
- Cloud resources (DigitalOcean): create only with an approved plan, use the smallest size, tag them `feature-flag-service`, and record each one in the deploy plan so it can be torn down.
- No force-pushes or history rewrites without the user asking.

## Engineering conventions

- Go: `gofmt`, `go vet`, and `go test -race ./...` must pass before every commit.
- Pass `context.Context` through every request path. Wrap errors with `%w`. No panics in request handling.
- Domain errors map to HTTP status codes in one place. A missing flag is `404`, never `500`.
- Keep evaluation logic a pure function so it can be unit-tested without a database or cache.
- Tests accompany the code they cover, in the same commit.

## Git

- Small, focused commits with conventional prefixes (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`).
- Push to `origin main` after each completed step so specs, plans, diagrams, and code are always on GitHub.
- `gh`, `doctl`, and `git push` need network access outside the sandbox.

## End of session

Remind the user to sign out of personal accounts and remove credentials from this machine:

- `gh auth logout` (token is in the macOS keychain)
- `doctl auth remove --context default`
- the `gh` credential-helper entries in `~/.gitconfig`
- any `s3cmd` configuration, plus browser sessions for GitHub and DigitalOcean
