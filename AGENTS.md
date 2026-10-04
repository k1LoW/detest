# AGENTS.md

## What detest is for

detest finds concurrency bugs in application code. It runs the code under test once per interleaving of its operations within bounds, checks invariants on each run, and reports the sequence of choices that broke one so that the run can be replayed. The simulated databases, queues and mutexes exist for one reason, which is to give the application the same observable outcomes the real resources would give it under each interleaving. Everything else about them is incidental.

This sets the standard a simulated database is held to.

- It is not a database. It does not need a planner, an index, a storage format or a wire protocol, and it does not need to run every statement the server accepts.
- A difference from the real server matters only when it can change an outcome the application observes during its concurrent execution, such as which rows a statement reads or locks, whether it blocks, which error it gets, or how many rows it affects. Such a difference makes detest report a bug the application does not have, or pass a schedule that breaks production. Both defeat its purpose, and neither is noticed, because the result looks like any other.
- A difference that cannot change any such outcome is invisible to the purpose. Code written to remove it only grows the engine and the search.

## The three answers

Every behavior of the simulated databases gets exactly one of three answers. Decide which by one question.

> Can the difference from the real server change the outcome of the application's concurrent execution, for the statements an application runs while it serves requests?

**Exact.** The answer is yes, and detest implements the behavior. It must match the server, down to the error code and the affected-row count. This is where the work goes.

**Unsupported.** The answer is yes, and detest does not implement the behavior, or cannot implement it exactly, or not yet, or not without a large rework. The statement fails with `ErrUnsupportedSQL`. An approximation is never the answer here. "Roughly right" is the worst outcome detest can produce, because a result computed from the wrong semantics is indistinguishable from a real one.

**Approximate.** The answer is no. detest does the simplest thing that keeps the application's outcomes the same, and that may differ from the server as much as it likes, including ignoring the statement or the clause, or skipping a check the server makes. Rejecting it is also allowed when that is less code, but neither rejecting nor modelling it is required.

The question is asked about the application's concurrent execution, not about the SQL. A form that only shows up when one of the assumptions in the next section is broken cannot change the outcome, and is answered Approximate, however much the server does for it.

### How the databases are used

These assumptions are what the question is asked under. A behavior that only appears when one of them is broken is out of scope.

- The schema is built once, before the processes run, from migrations, dumps or `ddl.From` output. DDL is not part of the concurrent execution. The schema and the rows the processes then see must be the ones the server would have built, but how the server would have built them does not matter.
- The processes run DML (`SELECT`, `INSERT`, `UPDATE`, `DELETE`) inside and outside transactions, through `database/sql`.
- The server runs with its default settings, apart from the session settings detest lists as supported.
- Values are the ones applications hold, not the extremes of a type's range.

### Exact

Anything that changes the outcome of concurrent execution in the application. For example:

- Isolation levels and MVCC, including the re-check of the predicate after a lock wait that Read Committed does
- Row locks, their strengths and conflict table, gap and next-key locks, `NOWAIT`, `SKIP LOCKED`, lock timeouts and deadlock detection
- Statement and transaction rollback, savepoints
- Constraint violations and their timing, such as the wait-then-conflict of concurrent inserts into a unique index and deferred constraints checked at commit, with the error codes the application branches on
- Affected-row counts, which applications use for optimistic locking
- `AUTO_INCREMENT`, sequences and `LastInsertId`
- Comparison and conversion of values that change which rows match or which rows a statement locks

### Unsupported

Anything that would change the outcome if detest ran it with other semantics than the server's, and that detest does not implement exactly. Reject it with `ErrUnsupportedSQL`, or at declaration time when it is a property of the database rather than of a statement. For example:

- An isolation level detest does not implement for the kind of server. The kind's default level fails when the database is declared, and a level asked for in `BeginTx` fails when the transaction begins. Neither runs with the semantics of another level.
- Query forms detest does not evaluate, such as recursive CTEs, `RIGHT` and `FULL` joins, `JOIN ... USING`, `NATURAL JOIN`, and `ANY (subquery)` with an operator other than `=`.
- Locking forms detest does not model, such as `FOR UPDATE` over a view or a subquery, where the server locks the tables behind it.
- Settings that change which rows match or lock, such as `SET GLOBAL`, non-strict `sql_mode` and case-insensitive collations. They are out of scope under the default-settings assumption, but accepting them silently would run the application against other semantics, so they are refused rather than ignored.
- A value detest cannot convert the way the server would, such as an `OFFSET` or `LIMIT` that is not a bigint.
- DDL whose result detest cannot reproduce in the schema or rows the processes see, such as adding a generated column to a table that already has rows.
- `BEGIN`, `COMMIT` and `ROLLBACK` sent as SQL, which bypass the transaction tracking that `database/sql` gives detest.
- Functions, clauses and syntax that applications rarely write in request handling, and the exact reproduction of rare syntax, such as type coercion in array casts. Rarity is a reason not to implement them, not a reason to approximate them. A rare clause in DML still decides which rows match or lock, so it is refused.
- Changes that need a large rework of the engine, such as row version IDs. Until the rework is done, the forms that need it are refused.

### Approximate

Anything that cannot change the outcome under the assumptions above. Prefer the simplest implementation, and prefer the one that keeps migrations and dumps loading. For example:

- Plain indexes are parsed and dropped. detest has no planner, so an index changes nothing it computes. Unique, partial and expression indexes are kept, because they constrain.
- DDL details that leave the schema the processes see unchanged, such as `ENGINE=`, `ADD COLUMN ... AFTER`, `AUTO_INCREMENT=n` on `CREATE TABLE`, `RENAME` across databases, `LOCK TABLES` in a dump, `TRUNCATE` inside a transaction, and `USE`. They are accepted and ignored, because rejecting them makes the schema impossible to build.
- DDL inside a transaction and its implicit commit, metadata locks, and the state left by a DDL statement that fails halfway. A failed DDL statement fails the test's setup, so what it leaves behind is not checked at all.
- Types detest does not check accept any value. The checks it does make, such as a uuid parsing or an integer fitting its width, are the ones that decide an application's error path.
- A function detest does not know in a generation expression is let through when the schema loads, and a write that needs it fails then.
- Values at the edges of a type's range, such as integers near 2^63, precision lost in float conversion, and temporal strings in formats other than the ISO forms. Leave them unchecked, or refuse them, whichever is less code.

An approximation still has two obligations. It must be deterministic, because a replay that takes a different path is reported as a nondeterministic simulation. And it must not add a yield point or a choice, because those enlarge the search for every test.

## How to apply the answers

### Implementing

- Fail with `ErrUnsupportedSQL` instead of silently behaving differently, for anything that can change an outcome.
- When one area keeps drawing findings, reject the whole class of statements instead of fixing the cases one by one. A class is judged by the question, not by listing its members.
- For an approximation, write the simplest form and say in a comment what the server does and why detest does not, so the next reader does not take the difference for a bug. Do not add an option to make it exact.
- Leave unchecked what cannot happen under the assumptions, such as a failed DDL statement's partial state. Rejecting it is not needed either.
- Check a proposed rejection against the statements the existing tests run, and against what migrations and dumps routinely contain. A rejection that stops a schema from loading is wrong even when the form is out of scope.
- Ask the maintainer when it is unclear which answer something gets.

### Review comments

Reviewers, including Copilot code review, read this file.

- A difference under Approximate is not a finding. Do not report it, and do not propose modelling it.
- A form under Unsupported that is accepted and silently behaves differently is a finding. Report that it should be rejected with `ErrUnsupportedSQL`, not that it should be modelled.
- A form under Exact that differs from the server is a finding, and the fix is to match the server.
- When answering a comment, give it one of the three answers first. For a comment on something under Approximate, reply that it is out of scope by this file and resolve it. Do not accept a review comment as a fix because it is correct about the server.
