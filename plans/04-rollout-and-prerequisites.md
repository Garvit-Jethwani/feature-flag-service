# Plan 04: Percentage rollout and flag dependencies through the API

Status: Implemented. Approved by the owner on 21 Nov 2026 ("go with phase 4").

Deviation from the test list below: a third small HTTP test was added for the depth limit, because that rule has an off-by-one risk (ten levels allowed, the eleventh refused, at either end of the chain).

Covers X2 (percentage rollout) and X3 (flag dependencies) from `SPECS.md`. The evaluator, the database columns, the evaluation query, and cache invalidation of dependent flags already support both (plans 01 and 02). What is missing is a way to set them. Budget: 12 minutes.

## What changes for a caller

| Request | New field | Meaning |
|---|---|---|
| `POST /v1/flags` | `rollout_percentage` (optional, default 100) | Integer 0 to 100 |
| `PATCH /v1/flags/{name}` | `rollout_percentage` | Integer 0 to 100 |
| `PATCH /v1/flags/{name}` | `prerequisites` | List of flag names this flag requires. Replaces the whole list; `[]` removes all. |

Responses already include both fields. Evaluation rules are unchanged (`SPECS.md` section 6).

## Rules and status codes

| Case | Answer |
|---|---|
| `rollout_percentage` outside 0 to 100 | `400` |
| A prerequisite name is invalid, repeated, or the flag itself | `400` |
| More than 10 prerequisites on one flag | `400` |
| A prerequisite flag does not exist | `400`, naming the field `prerequisites` |
| The change would create a cycle (A requires B, B requires A, at any distance) | `409 dependency_cycle` |
| The change would make a chain longer than the evaluator's limit (10 levels) | `409 dependency_too_deep` |
| Deleting a flag that others require | `409 flag_has_dependents` (already built) |

## How it is built

- **Same write path as every other change:** one transaction that locks the flag, applies the change, checks it, writes the audit entry (`flag.updated`, previous and new values include both fields), then invalidates the flag and its dependents after commit.
- **Cycle and depth are checked in the database, inside the transaction,** with one recursive query after the new list is written. If the check fails the transaction rolls back and nothing is saved.
- **Dependency edits run one at a time.** A transaction-level Postgres advisory lock is taken before prerequisites change. Without it, "A requires B" and "B requires A" submitted at the same moment could each pass its own check and together form a cycle.
- **The evaluator keeps its own depth limit** as a second guard, and that error is mapped to `409` instead of falling through to `500`.

## Tests (kept small)

One HTTP test for each feature, against real Postgres:

1. **Rollout:** at 0% a user is off with reason `rollout`; at 100% on; an out-of-range value is `400`.
2. **Prerequisites:** B requires A. With A off, B is off with `prerequisite_failed` even with a per-user override of `true`; turning A on turns B on (this also proves the dependent's cache is invalidated). A cycle is `409`; an unknown prerequisite is `400`.

The evaluator's own rollout and prerequisite rules are already unit-tested.

## Also updated

`README.md` (API table, remove the "not yet settable" note), `SPECS.md` (phase 4 done).

## Not in this plan

- Adding or removing a single prerequisite (the list is replaced as a whole).
- Rollout by anything other than the user ID hash (no segments or attributes).

## Verification

1. `make lint` and `make test` pass; CI is green.
2. Against the live stack: set a flag to 50% and evaluate 200 user IDs; roughly half are on, and repeating the calls gives the same answers.

## Commits

1. `feat: set rollout percentage and prerequisites through the API`
