# AGENTS.md

## Scope of the simulated databases

detest is a deterministic simulation testing tool for applications. Its databases do not aim to reproduce a database implementation in full. Decide what to implement, and how to answer review comments, by one question.

> Can the difference change the outcome of the application's concurrent execution, for the statements an application runs while it serves requests?

If yes, it is in scope and must be exact. If no, it is out of scope, and the only requirement is that detest does not silently behave differently.

### How the databases are used

These assumptions draw the line. A behavior that only shows up when one of them is broken is out of scope.

- The schema is built once, before the processes run, from migrations, dumps or `ddl.From` output. DDL is not part of the concurrent execution.
- The processes run DML (`SELECT`, `INSERT`, `UPDATE`, `DELETE`) inside and outside transactions, through `database/sql`.
- The server runs with its default settings, apart from the session settings detest lists as supported.
- Values are the ones applications hold, not the extremes of a type's range.

### Get these exactly right

Anything that changes the outcome of concurrent execution in the application.

- Isolation levels and MVCC
- Row locks, gap locks, deadlocks and lock timeouts
- Statement and transaction rollback
- Constraint violations, and the error codes the application branches on
- Affected-row counts, which applications use for optimistic locking
- `AUTO_INCREMENT`, sequences and `LastInsertId`
- Comparison and conversion of values that change which rows match or which rows a statement locks

### Leave these unsupported

- DDL beyond building the schema. This covers DDL inside a transaction and its implicit commit, DDL running alongside other statements, metadata locks, and the state left by a DDL statement that fails halfway. A failed DDL statement fails the test's setup, so what it leaves behind does not matter.
- DDL and administrative details that do not affect the schema the processes see, for example `RENAME` across databases, `ENGINE=MyISAM`, `TRUNCATE` inside a transaction, a standalone `LOCK TABLES`, and `USE`.
- Server-wide settings, and session settings other than the ones detest lists as supported, such as `SET GLOBAL`, non-strict `sql_mode` and case-insensitive collations.
- Values at the edges of a type's range, such as integers near 2^63, precision lost in float conversion, and temporal strings in formats other than the ISO forms.
- Functions, clauses and syntax that applications rarely write in request handling, and exact reproduction of rare syntax (such as type coercion in array casts).
- Changes that need a large rework of the engine (such as row version IDs).

How to leave them unsupported.

- Fail with `ErrUnsupportedSQL` instead of silently behaving differently.
- Prefer rejecting over implementing for anything out of scope, and do not grow the code for it. When one area keeps drawing findings, reject the whole class of statements instead of fixing the cases one by one.
- Leave unchecked what cannot happen under the assumptions above, such as a failed DDL statement's partial state. Rejecting it is not needed either.
- Still accept the forms that migrations and dumps routinely contain (`ADD COLUMN ... AFTER`, `AUTO_INCREMENT=n`, `LOCK TABLES` in a dump), because rejecting them makes the schema impossible to build. Check a proposed rejection against the statements the existing tests run.
- Ask the maintainer when it is unclear which side something falls on.

### Review comments

Reviewers, including Copilot code review, read this file.

- When reviewing, do not report a difference from the real server that falls under "Leave these unsupported". If such a form is accepted and silently behaves differently, report that it should be rejected with `ErrUnsupportedSQL`, not that it should be modelled.
- When answering, do not accept every review comment as a fix. Judge each one by the scope above. For a comment on something out of scope, reply that it is rejected explicitly as out of scope and resolve it.
