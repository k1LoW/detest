# Concurrency hazards and how to model them

Use this to survey a codebase for candidates (step 2) and to design the scenario and invariant for each. Every hazard lists what to look for in the code, the scenario to declare, the invariant, and the usual fixes to propose.

The invariant is always a business rule, checked from committed rows (`st.Rows`, `st.Row`) and from what the callers observed (Go variables reset in a `Seed`). Pair it with a `Sometimes` for the situation the bug needs, so a pass means something.

## Contents

1. Lost update (read, compute, write back)
2. Check then insert (duplicates)
3. Capacity and quota checks (overbooking)
4. Racing state transitions
5. Optimistic locking that ignores the affected-row count
6. Deadlocks from lock order
7. Work claimed, then the worker dies
8. Duplicate or lost messages
9. Retries around calls with side effects
10. Writes to the database and a second system
11. Mutexes mixed with row locks, and calls inside transactions
12. Server-specific traps

## 1. Lost update

**Look for** a `SELECT` (or ORM `First`/`Find`/`Get`) of a value, a computation in Go, and an `UPDATE` writing the computed value, without `FOR UPDATE` or a condition on the old value. ORM `Save` of a whole struct loaded earlier is the same pattern for every column. On MySQL at Repeatable Read, even inside one transaction the plain read comes from a snapshot, so the pattern loses updates there too.

**Scenario** two actors updating the same row (`Manual` each, one run).

**Invariant** the final value equals the initial value plus every successful change (balance, stock, counter), or the number of successes is bounded by what the row allowed.

**Fixes** `SELECT ... FOR UPDATE`, an atomic `UPDATE ... SET n = n - 1 WHERE id = ? AND n > 0` with an affected-row check, a version column.

## 2. Check then insert

**Look for** "does it exist?" followed by an insert, such as sign-up by email, one active subscription per user, idempotency keys checked by `SELECT`, "get or create".

**Scenario** two actors creating the same logical thing.

**Invariant** at most one row per logical key. If a unique constraint exists, the duplicate is prevented, and the question becomes whether the losing request gets a handled outcome. Record each caller's result and require it to be success or the app's own "already exists" error, not a raw unique-violation or 500.

**Fixes** a unique constraint plus handling of its violation, `INSERT ... ON CONFLICT DO NOTHING` / `INSERT IGNORE` with a re-read, locking a parent row.

## 3. Capacity and quota checks

**Look for** `SELECT count(*) ... ` or a sum compared with a limit, then an insert, as for seats, rate limits, coupon redemptions, team member limits. A row lock on existing rows cannot stop a concurrent insert of a new row (a phantom).

**Scenario** one more actor than the remaining capacity (capacity 1, two actors).

**Invariant** the count never exceeds the limit (`Always`, so it is caught the moment it happens).

**Fixes** lock a parent row (`SELECT ... FROM events WHERE id = ? FOR UPDATE`) before counting, a counter column updated atomically, a constraint.

## 4. Racing state transitions

**Look for** status machines, such as an order cancelled while being shipped, a payment refunded while being captured, a job cancelled while running. `UPDATE ... SET status = ? WHERE id = ?` without `AND status = <expected>`, or a status checked in Go after a plain read.

**Scenario** one actor per conflicting transition (`cancel` and `ship`), plus the normal flow.

**Invariant** only allowed end states, and side effects consistent with the end state (a cancelled order has no shipment row; a refunded payment was captured). Use `Always` with `st.Prev()` to forbid illegal transitions step by step.

**Fixes** conditional updates with an affected-row check, `FOR UPDATE` on the row before deciding.

## 5. Optimistic locking that ignores the affected-row count

**Look for** `UPDATE ... WHERE id = ? AND version = ?` whose `RowsAffected` is not checked, or GORM `Updates` with a version condition where `RowsAffected == 0` is treated as success.

**Scenario** two actors editing the same record.

**Invariant** a caller told "saved" has its change in the final row; or the version increments once per reported success.

**Fixes** check the affected-row count and return a conflict.

## 6. Deadlocks from lock order

**Look for** a transaction updating several rows in an order taken from the input (from/to accounts, a cart's items in request order), or two code paths touching the same tables in opposite orders (parent then child on one path, child then parent on another).

**Scenario** two actors locking the same rows in opposite orders (a to b, and b to a).

**Invariant** every caller succeeds, or fails only with errors the app retries. Deadlock errors come back to the app as `ErrDeadlock` (40P01 / 1213), so record unexpected errors and fail on them. Whether a deadlock is a bug depends on whether callers retry; ask the user.

**Fixes** a global order (sort the ids), a single statement, retry on deadlock at the transaction boundary.

## 7. Work claimed, then the worker dies

**Look for** a job, lease or outbox row marked `running`/`claimed` and committed, then the work, then `done` in a later transaction, with no expiry or reaper. `FOR UPDATE SKIP LOCKED` queues often have this shape.

**Scenario** a worker as `Manual` or `Loop` with two runs, `detest.MaxCrashes(1)`. Add the reaper/sweeper as a `Loop` if one exists.

**Invariant** at quiescence every job is `done` (or retried to `done`), and done exactly once if doing it twice is harmful.

**Fixes** do the work inside the claiming transaction when it is short, a lease with expiry plus a reaper, idempotent work.

## 8. Duplicate or lost messages

**Look for** consumers of SQS/Kafka/PubSub/NATS/River jobs that insert rows, send mail, charge, or increment counters without a dedup key. Producers that publish after commit (a crash between them loses the event) or before commit (the event describes a rolled-back change).

**Scenario** the consumer via `OnMessage` on a `Queue` with `detest.Duplicates(1)`; for loss, `detest.Losses(1)` plus the backstop sweeper as a `Loop`; for the producer, `MaxCrashes(1)`.

**Invariant** the effect happened exactly once per logical message; nothing is left unprocessed at quiescence.

**Fixes** a processed-messages table with a unique key written in the same transaction as the effect, a transactional outbox.

## 9. Retries around calls with side effects

**Look for** a call to a payment provider, another internal service or an email API, retried on error or timeout, without an idempotency key. And the reverse, a DB transaction that commits only after the remote call succeeded, which loses the record when the response is lost after the remote effect.

**Scenario** inject `s.External(...).Transport(fake)` (or `Do`) into the client; the fake counts effects. `MaxFailures(1)` is the default.

**Invariant** the remote effect happened at most once per user action, and the local record agrees with the remote one at quiescence (charged implies recorded as paid, and vice versa or reconciled by a sweeper).

**Fixes** idempotency keys stored before the call, a pending record committed before the call and completed after, a reconciler.

## 10. Writes to the database and a second system

**Look for** a DB commit followed by a cache write, search-index update, publish or file upload (or the other order). A crash or failure between the two leaves them inconsistent.

**Scenario** the second system as an `External` fake or a `Queue`; `MaxCrashes(1)`.

**Invariant** at quiescence the two agree, possibly after a sweeper `Loop` runs.

## 11. Mutexes mixed with row locks, and calls inside transactions

**Look for** a `sync.Mutex` taken around database work (per-key locks, singleflight-like guards); an HTTP/gRPC call made while a transaction is open, to a service that writes the same database; a function that opens a transaction and then calls a helper using the pool (`db`) instead of the transaction on the same rows.

**Scenario** inject `s.Mutex`; serve the other service through `Transport` on `store.Open()`.

**Invariant** none needed. detest reports cycles of waits through mutexes and row locks, and a process waiting for a lock its own open transaction holds, as progress violations. In production these hang until a timeout.

## 12. Server-specific traps

- **PostgreSQL Read Committed**: each statement sees a new snapshot, so two statements in one transaction can see different data; an `UPDATE ... WHERE` re-checks its condition after waiting for a lock and may then skip the row (zero rows affected).
- **MySQL Repeatable Read**: plain reads see the snapshot taken at the transaction's first read, while `UPDATE` and locking reads see the latest data, so code that reads then writes back loses updates even inside a transaction; locking reads take gap locks, so two transactions that check for a missing row with `FOR UPDATE` and then insert it deadlock.
- **Unique index waits**: a concurrent insert of the same key waits for the first transaction, then fails or succeeds depending on whether it committed.
