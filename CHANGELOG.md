# Changelog

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
