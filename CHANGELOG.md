# Changelog

## [v0.9.0](https://github.com/k1LoW/detest/compare/v0.8.0...v0.9.0) - 2026-10-27

### New Features 🎉
- feat: add IterativeBounding to explore schedules in order of preemptions by @k1LoW in https://github.com/k1LoW/detest/pull/231
- feat: let IterativeBounding run with PartialOrder, one reduced pass per bound by @k1LoW in https://github.com/k1LoW/detest/pull/235
- feat!: let an OnMessage consumer handle every waiting message at once by @k1LoW in https://github.com/k1LoW/detest/pull/236
- feat: explore only from the state ExploreAfter waits for by @k1LoW in https://github.com/k1LoW/detest/pull/239
### Fix bug 🐛
- fix!: count changes of a working tick's spawned process as progress by @k1LoW in https://github.com/k1LoW/detest/pull/237
- fix: drop PartialOrder's records of states no run goes through again by @k1LoW in https://github.com/k1LoW/detest/pull/238

## [v0.8.0](https://github.com/k1LoW/detest/compare/v0.7.0...v0.8.0) - 2026-10-21

### New Features 🎉
- feat: estimate the runs left when an exploration is cut short by @k1LoW in https://github.com/k1LoW/detest/pull/223
### Other Changes
- feat: raise the default MaxRuns from 200000 to 1000000 by @k1LoW in https://github.com/k1LoW/detest/pull/224

## [v0.7.0](https://github.com/k1LoW/detest/compare/v0.6.5...v0.7.0) - 2026-10-20

### Breaking Changes 🛠
- feat!: remove the Pods option by @k1LoW in https://github.com/k1LoW/detest/pull/219
### New Features 🎉
- feat: wake an idle loop under PartialOrder only on a change to what it read by @k1LoW in https://github.com/k1LoW/detest/pull/221
### Other Changes
- docs: list Replay, ObserveSQL and Verbose in the README's options table by @k1LoW in https://github.com/k1LoW/detest/pull/218

## [v0.6.5](https://github.com/k1LoW/detest/compare/v0.6.4...v0.6.5) - 2026-10-19

### Fix bug 🐛
- fix: match a MySQL table qualifier or alias in its own case by @k1LoW in https://github.com/k1LoW/detest/pull/203
- fix: resolve a qualified MySQL INSERT column against the target table by @k1LoW in https://github.com/k1LoW/detest/pull/208
- fix: resolve a qualified MySQL VALUES() argument against the INSERT target by @k1LoW in https://github.com/k1LoW/detest/pull/209
- fix: refuse a comparison on a case-insensitive column a derived table renames by @k1LoW in https://github.com/k1LoW/detest/pull/210
- fix: refuse a MySQL VALUES() in a subquery of ON DUPLICATE KEY UPDATE by @k1LoW in https://github.com/k1LoW/detest/pull/214
- fix: refuse a comparison on a case-insensitive column read through a view by @k1LoW in https://github.com/k1LoW/detest/pull/215
- fix: refuse a MySQL derived table with a column list by @k1LoW in https://github.com/k1LoW/detest/pull/216
### Other Changes
- fix: resolve a MySQL column qualified by its database by @k1LoW in https://github.com/k1LoW/detest/pull/202
- style: gofmt mysqlcolumns.go and viewbind.go by @k1LoW in https://github.com/k1LoW/detest/pull/217

## [v0.6.4](https://github.com/k1LoW/detest/compare/v0.6.3...v0.6.4) - 2026-10-17

### Fix bug 🐛
- fix: read a numeric back as text under a repeated parameter, a view or a * over a join by @k1LoW in https://github.com/k1LoW/detest/pull/196
- fix: resolve MySQL column names before a statement runs by @k1LoW in https://github.com/k1LoW/detest/pull/197

## [v0.6.3](https://github.com/k1LoW/detest/compare/v0.6.2...v0.6.3) - 2026-10-16

### Fix bug 🐛
- fix: give a view whose query has a * the columns its query has by @k1LoW in https://github.com/k1LoW/detest/pull/185
- fix: refuse an aggregate only in DISTINCT ON or a window by @k1LoW in https://github.com/k1LoW/detest/pull/179
- fix: write a boolean argument of concat as the server's output function does by @k1LoW in https://github.com/k1LoW/detest/pull/180
- fix: write extract's numeric with its scale and refuse a time of day by @k1LoW in https://github.com/k1LoW/detest/pull/182
- fix: read a string literal in a column of VALUES as the type of its rows by @k1LoW in https://github.com/k1LoW/detest/pull/181
- fix: refuse a write whose target is a view by @k1LoW in https://github.com/k1LoW/detest/pull/190
- fix: type a parameter by the function argument it stands in by @k1LoW in https://github.com/k1LoW/detest/pull/184
- fix: fail a MySQL write whose subquery reads its own table by @k1LoW in https://github.com/k1LoW/detest/pull/192
- fix: bind a view's query when the view is created by @k1LoW in https://github.com/k1LoW/detest/pull/191
- fix: read a numeric back with the scale Postgres gives it by @k1LoW in https://github.com/k1LoW/detest/pull/193

## [v0.6.2](https://github.com/k1LoW/detest/compare/v0.6.1...v0.6.2) - 2026-10-16

### Fix bug 🐛
- fix: refuse an aggregate or a window function in another's arguments before any row by @k1LoW in https://github.com/k1LoW/detest/pull/145
- fix: refuse a text expression assigned to a non-text column in UPDATE SET and INSERT VALUES by @k1LoW in https://github.com/k1LoW/detest/pull/143
- fix: refuse a value of a non-boolean type where Postgres wants a boolean by @k1LoW in https://github.com/k1LoW/detest/pull/142
- fix: refuse sum and avg of an argument type Postgres has no form of by @k1LoW in https://github.com/k1LoW/detest/pull/144
- fix: type the fields of a row comparison and of IN (SELECT ...) before any row by @k1LoW in https://github.com/k1LoW/detest/pull/147
- fix: refuse a number compared with a CASE, COALESCE or aggregate of string literals by @k1LoW in https://github.com/k1LoW/detest/pull/153
- fix: fail a relation that does not exist where no row reaches it by @k1LoW in https://github.com/k1LoW/detest/pull/152
- fix: refuse an aggregate or a window function in a clause that allows none before any row by @k1LoW in https://github.com/k1LoW/detest/pull/160
- fix: type a subquery whose select list is a * over a derived table, a CTE or a view by @k1LoW in https://github.com/k1LoW/detest/pull/158
- fix: type a query output that is a row as a record to the query around it by @k1LoW in https://github.com/k1LoW/detest/pull/159
- fix: compute only the rows a query pulls, and refuse FROM items with effects read in part by @k1LoW in https://github.com/k1LoW/detest/pull/137
- fix: refuse a non-string parameter in a column of VALUES Postgres resolves to text by @k1LoW in https://github.com/k1LoW/detest/pull/167
- fix: refuse LIKE and ILIKE over an operand Postgres has no operator for by @k1LoW in https://github.com/k1LoW/detest/pull/168
- fix: refuse a WINDOW definition that no OVER uses, and resolve one that copies another by @k1LoW in https://github.com/k1LoW/detest/pull/169
- fix: refuse an ORDER BY key of a set operation other than an output column by @k1LoW in https://github.com/k1LoW/detest/pull/170
- fix: fail a MySQL table that does not exist where no row reaches it by @k1LoW in https://github.com/k1LoW/detest/pull/172
- fix: type a set operation whose first query is a * over a derived table, a CTE or a view by @k1LoW in https://github.com/k1LoW/detest/pull/171
- fix: write a row read back or cast to text as Postgres's record output by @k1LoW in https://github.com/k1LoW/detest/pull/175
### Other Changes
- fix: refuse a row of columns in arithmetic, unary minus and the typed-argument functions by @k1LoW in https://github.com/k1LoW/detest/pull/165

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
