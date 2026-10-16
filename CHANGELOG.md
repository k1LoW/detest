# Changelog

## [v0.6.1](https://github.com/k1LoW/detest/compare/v0.6.0...v0.6.1) - 2026-10-15

### Fix bug 🐛
- fix: order goroutines adopted at Current by where Current was called by @k1LoW in https://github.com/k1LoW/detest/pull/91
- fix: read input typed by an assignment, a cast or a branch before any row by @k1LoW in https://github.com/k1LoW/detest/pull/83
- fix: read a parameter beside a number in arithmetic as that number's type before any row by @k1LoW in https://github.com/k1LoW/detest/pull/94
- fix: divide by the sum of a bigint as a numeric by @k1LoW in https://github.com/k1LoW/detest/pull/99
- fix: reduce a domain to its base type where an operator or a function resolves it by @k1LoW in https://github.com/k1LoW/detest/pull/101
- fix: stop COALESCE at the first argument that is not NULL by @k1LoW in https://github.com/k1LoW/detest/pull/95
- fix: refuse an aggregate of only outer columns in a subquery by @k1LoW in https://github.com/k1LoW/detest/pull/108
- fix: refuse a scalar subquery of several columns and a locking aggregate query before any row by @k1LoW in https://github.com/k1LoW/detest/pull/110
- fix: read a numeric back as its text by @k1LoW in https://github.com/k1LoW/detest/pull/103
- fix: name the columns of a derived table over a * that a column alias list renames by @k1LoW in https://github.com/k1LoW/detest/pull/115
- fix: run an uncorrelated subquery once for the statement by @k1LoW in https://github.com/k1LoW/detest/pull/111
- fix: refuse arithmetic over a boolean, a uuid or text before the statement runs by @k1LoW in https://github.com/k1LoW/detest/pull/109
- fix: lock the snapshot's version of a row a non-key update changed under FOR KEY SHARE by @k1LoW in https://github.com/k1LoW/detest/pull/119
- fix: test every field of a row under IS NULL and IS NOT NULL by @k1LoW in https://github.com/k1LoW/detest/pull/123
- fix: read a string literal or parameter in a boolean position as boolean input by @k1LoW in https://github.com/k1LoW/detest/pull/128
- fix: type a column default by the column check before evaluating it by @k1LoW in https://github.com/k1LoW/detest/pull/127
- fix: run a query's CTE when a row first reads it by @k1LoW in https://github.com/k1LoW/detest/pull/126
- fix: type a scalar subquery's target in its own scope in every check before a statement by @k1LoW in https://github.com/k1LoW/detest/pull/124
- fix: raise the errors Postgres raises folding expressions of constants by @k1LoW in https://github.com/k1LoW/detest/pull/107
### Other Changes
- feat: type the columns of a *, a set operation and generate_series in FROM by @k1LoW in https://github.com/k1LoW/detest/pull/89
- fix: refuse a statement reading a view whose query the column check refuses by @k1LoW in https://github.com/k1LoW/detest/pull/131
- fix: raise the server's error for a power with no real result by @k1LoW in https://github.com/k1LoW/detest/pull/136
- fix: type a query output of string literals or NULLs as text to the query around it by @k1LoW in https://github.com/k1LoW/detest/pull/132
- fix: type a CHECK constraint and a generated column by the column check before evaluating them by @k1LoW in https://github.com/k1LoW/detest/pull/135

## [v0.6.0](https://github.com/k1LoW/detest/compare/v0.5.1...v0.6.0) - 2026-10-14

### Fix bug 🐛
- fix: wait only for writers in the Postgres duplicate check by @k1LoW in https://github.com/k1LoW/detest/pull/79
- fix: name an unaliased Postgres select item as Postgres does by @k1LoW in https://github.com/k1LoW/detest/pull/86
- fix: read a process's goroutine id under its lock in the DETEST_DEBUG trace by @k1LoW in https://github.com/k1LoW/detest/pull/87
### Other Changes
- feat: order Postgres enums by their labels by @k1LoW in https://github.com/k1LoW/detest/pull/81
- test: tell apart the sibling goroutines of the canceled-statement test by @k1LoW in https://github.com/k1LoW/detest/pull/84
- test: wait for database/sql to roll back a canceled transaction on the real server by @k1LoW in https://github.com/k1LoW/detest/pull/85

## [v0.5.1](https://github.com/k1LoW/detest/compare/v0.5.0...v0.5.1) - 2026-10-11

### Fix bug 🐛
- fix: keep no lock on the row ON CONFLICT DO NOTHING skips on Postgres by @k1LoW in https://github.com/k1LoW/detest/pull/75
### Other Changes
- feat: save DETEST_CHECKPOINT when SIGINT or SIGTERM stops go test by @k1LoW in https://github.com/k1LoW/detest/pull/78

## [v0.5.0](https://github.com/k1LoW/detest/compare/v0.4.0...v0.5.0) - 2026-10-09

### New Features 🎉
- feat: skip runs that only reorder independent steps with PartialOrder by @k1LoW in https://github.com/k1LoW/detest/pull/72

## [v0.4.0](https://github.com/k1LoW/detest/compare/v0.3.0...v0.4.0) - 2026-10-08

### Breaking Changes 🛠
- feat!: start Manual processes eagerly by default by @k1LoW in https://github.com/k1LoW/detest/pull/70
### Fix bug 🐛
- fix: pass a multi-row INSERT in CheckSQL instead of reporting 23505 by @k1LoW in https://github.com/k1LoW/detest/pull/64
### Other Changes
- fix: run ALTER COLUMN TYPE ... USING on a table without rows by @k1LoW in https://github.com/k1LoW/detest/pull/66
- feat: run AND, OR and row comparisons over aggregates in HAVING by @k1LoW in https://github.com/k1LoW/detest/pull/67
- feat: compare a row with a subquery of as many columns on Postgres by @k1LoW in https://github.com/k1LoW/detest/pull/68
- feat: order Postgres text by a collation the server is given by @k1LoW in https://github.com/k1LoW/detest/pull/69
- docs: point the skill to glibctext for a libc collation by @k1LoW in https://github.com/k1LoW/detest/pull/71

## [v0.3.0](https://github.com/k1LoW/detest/compare/v0.2.0...v0.3.0) - 2026-10-07

- refactor: move the server packages and ddl under db/ by @k1LoW in https://github.com/k1LoW/detest/pull/58
- feat: schedule goroutines sharing a GORM transaction through gormpool by @k1LoW in https://github.com/k1LoW/detest/pull/60
- feat: stall processes past a TTL, and fake a key-value store on ext.Do by @k1LoW in https://github.com/k1LoW/detest/pull/61
- feat: add sqlcdbtx and sqlxext, seam packages for sqlc and sqlx by @k1LoW in https://github.com/k1LoW/detest/pull/62
- feat: list the messages dropped after MaxRedeliveries in State by @k1LoW in https://github.com/k1LoW/detest/pull/63

## [v0.2.0](https://github.com/k1LoW/detest/compare/v0.1.1...v0.2.0) - 2026-10-06

- refactor: move expression evaluation and casts out of sqlexec.go by @k1LoW in https://github.com/k1LoW/detest/pull/35
- feat: run = ANY and <> ALL over an array parameter on Postgres by @k1LoW in https://github.com/k1LoW/detest/pull/38
- feat: pick schedules at random from a seed with Random by @k1LoW in https://github.com/k1LoW/detest/pull/36
- feat: override the run budget and the random seed from the environment by @k1LoW in https://github.com/k1LoW/detest/pull/42
- ci: run the race detector in its own job, without coverage by @k1LoW in https://github.com/k1LoW/detest/pull/43
- fix: read untyped text compared with a uuid column by uuid input by @k1LoW in https://github.com/k1LoW/detest/pull/40
- feat: pick schedules the way PCT does with Prioritized by @k1LoW in https://github.com/k1LoW/detest/pull/44
- fix: compile the PostgreSQL parser before the stall watchdog starts by @k1LoW in https://github.com/k1LoW/detest/pull/46
- feat: run WITH ... UPDATE and WITH ... DELETE on Postgres by @k1LoW in https://github.com/k1LoW/detest/pull/41
- chore: credit libpg_query and PostgreSQL in CREDITS by @k1LoW in https://github.com/k1LoW/detest/pull/48
- feat: run interval text, CURRENT_DATE, date_trunc and extract on Postgres by @k1LoW in https://github.com/k1LoW/detest/pull/49
- fix: let a process that busy-waits give way, and report one that spins alone by @k1LoW in https://github.com/k1LoW/detest/pull/47
- feat: hold Postgres intervals as months, days and microseconds by @k1LoW in https://github.com/k1LoW/detest/pull/51
- feat: run Postgres in a TimeZone other than UTC by @k1LoW in https://github.com/k1LoW/detest/pull/52
- feat: schedule the goroutines the code under test starts by @k1LoW in https://github.com/k1LoW/detest/pull/50
- fix: stop runs stalling inside database/sql's locks by @k1LoW in https://github.com/k1LoW/detest/pull/54
- test: write a row after the process's own idle transaction was canceled by @k1LoW in https://github.com/k1LoW/detest/pull/55
- docs: say that any shared context canceled among siblings goes unchecked by @k1LoW in https://github.com/k1LoW/detest/pull/56

## [v0.1.1](https://github.com/k1LoW/detest/compare/v0.1.0...v0.1.1) - 2026-10-05

- fix: show the scenario the skill's test runs, told from the code up by @k1LoW in https://github.com/k1LoW/detest/pull/33
- docs: make the unsupported list and CheckSQL usable as a pre-check by @k1LoW in https://github.com/k1LoW/detest/pull/32

## [v0.1.0](https://github.com/k1LoW/detest/commits/v0.1.0) - 2026-10-05

- build(deps): bump github.com/moby/go-archive from 0.2.0 to 0.3.0 by @dependabot[bot] in https://github.com/k1LoW/detest/pull/1
- feat: cap an exploration by wall-clock time with MaxDuration by @k1LoW in https://github.com/k1LoW/detest/pull/5
- docs: add AGENTS.md with the scope of the simulated databases by @k1LoW in https://github.com/k1LoW/detest/pull/6
- fix: stop PostgreSQL SQL from silently running with a different meaning by @k1LoW in https://github.com/k1LoW/detest/pull/4
- fix: report a panic in a process as a violation with its schedule by @k1LoW in https://github.com/k1LoW/detest/pull/8
- fix: refuse ANY (subquery) with an operator other than = by @k1LoW in https://github.com/k1LoW/detest/pull/10
- docs: draw the scope of the simulated databases by how they are used by @k1LoW in https://github.com/k1LoW/detest/pull/11
- fix: give an untyped literal compared with a number the number's type by @k1LoW in https://github.com/k1LoW/detest/pull/12
- docs: answer every database behavior as exact, unsupported or approximate by @k1LoW in https://github.com/k1LoW/detest/pull/14
- test: compare detest with a real Postgres, and fix what it found by @k1LoW in https://github.com/k1LoW/detest/pull/17
- fix: refuse a default detest cannot compute when the table's own schema reads it by @k1LoW in https://github.com/k1LoW/detest/pull/16
- feat: run MySQL statements with InnoDB's semantics by @k1LoW in https://github.com/k1LoW/detest/pull/3
- ci: collect coverage from the race-enabled test run by @k1LoW in https://github.com/k1LoW/detest/pull/19
- fix: store number column values as numbers and refuse text compared with one by @k1LoW in https://github.com/k1LoW/detest/pull/15
- fix: run the pause-less difftest case again when load made it repeat a paused run by @k1LoW in https://github.com/k1LoW/detest/pull/21
- fix: record a trace line when the operation runs, not when it is reached by @k1LoW in https://github.com/k1LoW/detest/pull/24
- fix: store boolean and timestamp column values as their types by @k1LoW in https://github.com/k1LoW/detest/pull/26
- feat: add an agent skill that runs In-Process DST with detest by @k1LoW in https://github.com/k1LoW/detest/pull/23
- test: compare detest with a real MySQL, and fix the deadlock victim by @k1LoW in https://github.com/k1LoW/detest/pull/20
- ci: give the race-enabled test run 30 minutes by @k1LoW in https://github.com/k1LoW/detest/pull/29
- fix: count only idle ticks with no progress in between toward MaxIdleTicks by @k1LoW in https://github.com/k1LoW/detest/pull/27
- fix: refuse DECIMAL and numeric results that a float64 gets wrong where they show by @k1LoW in https://github.com/k1LoW/detest/pull/30
- docs: split the README's long database paragraphs into lists by @k1LoW in https://github.com/k1LoW/detest/pull/31
- test: compare locks and writes with Postgres, and fix what it found by @k1LoW in https://github.com/k1LoW/detest/pull/25
