# Changelog

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
