# AGENTS.md

## Scope of the simulated databases

detest is a deterministic simulation testing tool for applications. Its databases do not aim to reproduce a database implementation in full. Decide what to implement, and how to answer review comments, by whether the behavior changes the outcome of the application's concurrent execution.

### Get these exactly right

Anything that changes the outcome of concurrent execution in the application.

- Isolation levels and MVCC
- Row locks, gap locks, deadlocks and lock timeouts
- Statement and transaction rollback
- Constraint violations, and the error codes the application branches on
- `AUTO_INCREMENT`, sequences and `LastInsertId`
- Comparison and conversion of values that change which rows match

### Leave these unsupported

DDL and administrative details that rarely appear while the application runs, for example `RENAME` across databases, `ENGINE=MyISAM`, `TRUNCATE` inside a transaction, a standalone `LOCK TABLES`, and `USE`. The same goes for changes that need a large rework of the engine (such as row version IDs) and exact reproduction of rare syntax (such as type coercion in array casts).

- Fail with `ErrUnsupportedSQL` instead of silently behaving differently.
- Prefer rejecting over implementing for anything out of scope, and do not grow the code for it.
- Still accept the forms that migrations and dumps routinely contain (`ADD COLUMN ... AFTER`, `AUTO_INCREMENT=n`, `LOCK TABLES` in a dump), because rejecting them makes the schema impossible to build. Check a proposed rejection against the statements the existing tests run.
- Ask the maintainer when it is unclear which side something falls on.

### Review comments

Do not accept every review comment (from Copilot or others) as a fix. Judge each one by the scope above. For a comment on something out of scope, reply that it is rejected explicitly as out of scope and resolve it.
