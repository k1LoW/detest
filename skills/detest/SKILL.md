---
name: detest
description: Run In-Process DST with detest on a Go application, from choosing the target with the user to a report of what the run guarantees. detest runs the application's real code inside `go test`, with its PostgreSQL or MySQL database, queues, calls to other services and mutexes simulated at the client boundary, and explores the interleavings and failures within bounds under a deterministic scheduler. Use only when the user has decided to use detest or In-Process DST, by asking or by accepting your suggestion. It adds a dependency and runs long explorations, so do not start it on your own. Suggest it in one line, and wait, when the user has a problem it could catch in Go code on PostgreSQL or MySQL, such as data that is sometimes wrong (oversold stock, double charges), a bug seen only under load, a deadlock, a flaky test around transactions, or a change to code with transactions or workers. Say it runs their real code and replays the failing order exactly; never call it a database simulation or an exhaustive search.
---

# detest, In-Process DST for Go applications

detest runs the application's real code once per interleaving of its operations on simulated resources (a PostgreSQL or MySQL database behind `database/sql`, queues, external calls, mutexes), checks invariants on every run, and prints a replayable schedule when one breaks. It finds the bugs that need one specific interleaving, which reading the code and ordinary tests both miss.

Call this In-Process DST when talking to the user, not plain DST. When you explain what it is, say this, translated faithfully, rather than a summary of your own.

> In-Process DST runs the application's real code inside the `go test` process. The resources it shares (databases through `database/sql`, queues, calls to other services, mutexes) are replaced at their client boundary by simulations that behave as the real ones do under concurrency. A deterministic scheduler makes every decision, which operation runs next and which failure happens (a crash, a lost response, a duplicate or lost message), so any run can be replayed exactly. It explores these decisions systematically within the bounds set, and checks the invariants on every run.

Do not shorten it to "simulates the database", since the real code runs and the queues, calls and mutexes are simulated too. Do not call it exhaustive without "within the bounds", and do not leave out that runs replay exactly.

**Start only on the user's decision.** If the user has not asked for detest or In-Process DST, and has not accepted a suggestion to use it, do not begin the workflow. Suggest it in one or two lines, saying what it would check in their code and that it adds a test dependency, and wait. A yes to that suggestion is the decision; the checkpoints below still apply.

**The user is assumed to know nothing about detest.** They decide to use it; you do everything else. Talk to them about their code and the rules it must keep, never about detest's API, unless they ask. The user owns the code and will fix what you find, but may not hold it in their head. Judge how well they know it from the session. A user who wrote or edited this code in the session, or talked about it at the level of functions and statements, knows it; a user who only named a feature, or asked for detest without pointing at any code, may know what it does only roughly. Every proposal and report says, however familiar the user is, which domain the code belongs to, which operation is targeted, and what the test does to it. For a user who does not know the code well, also walk through what the operation does before any scenario, namely which function is called, what it reads and writes and in which transactions, with the `file:line` of each. Then explain the scenario in those terms, naming the functions, the SQL statements and the transactions involved. Use the business story to say why it matters, not in place of the code. How much of the domain you can name depends on how much you learned from the session and the code, so say what you inferred when it is a guess. Ask only the questions whose answers you cannot get from the code, and offer a proposed answer with each so they can just confirm. Speak the user's language.

## Workflow

1. Orient. Confirm the project can run detest, and load the current detest docs
2. Propose targets and agree on scenarios and invariants with the user
3. Set up the dependency (with the user's consent)
4. Write the simulation test
5. Run it, fix harness problems, widen the bounds
6. Triage any violation
7. Report what In-Process DST was run, what it guarantees and what it found

Keep the user posted briefly between steps. The rest of the work is yours, except for four checkpoints where you stop and ask.

1. The target (step 2)
2. The scenario and the invariant (step 2)
3. Adding the dependency (step 3)
4. The conditions of the run (step 5)

Stop at every checkpoint even when the user's request already answers it, such as naming the target, stating the rule, or saying "leave it all to you". Fill in what they said as your proposal and ask them to confirm it. What they wrote before seeing the code is not yet a decision about what detest will check, and confirming costs them one word.

## 1. Orient

Check these without asking the user.

- **Go module and version.** detest needs Go 1.26 or later (`testing/synctest`). Read `go.mod` and `go version`. If the module's `go` line is older, adding detest raises it, which is a decision for the user (step 3).
- **How the code talks to the database.** detest intercepts `database/sql` only. `*sql.DB` used directly, GORM, sqlx, sqlc on `database/sql`, ent and bun all work, because each can be built on a `*sql.DB`. Code that holds a `*pgxpool.Pool` or `*pgx.Conn` (pgx native interface) cannot be intercepted; see `references/wiring.md` for what to tell the user.
- **Which server.** PostgreSQL (`postgres.New()`, Read Committed) or MySQL/InnoDB (`mysql.New()`, Repeatable Read by default). Use the production server's kind and isolation level. If production uses a level detest does not implement for that server, detest refuses it rather than approximating, and so should you.
- **Where the schema comes from.** Migrations directory, schema dump, ORM models. Constraints decide outcomes, so the simulated schema must have the production constraints (primary keys, unique, foreign keys, checks).
- **The driver's error type** the code branches on (`*pgconn.PgError`, `*pq.Error`, `*mysql.MySQLError`, GORM's translated errors), so the simulation converts errors the same way.
- **The SQL the flow sends.** detest refuses what it does not run exactly, and a flow whose statements are refused cannot be simulated, so check them before proposing it. Collect the statements of the candidate flows (sqlc query files, repository code, the ORM's generated SQL from its logger) and read them against the README's **Not supported** lists, which are grouped by what a statement does (functions and operators, arrays and JSON, `WITH` on writes, isolation settings, ...). The README lists every function detest runs; any other, such as `date_trunc`, `->>` or `@>`, is refused. Once detest is a dependency, run the collected statements through `detest.CheckSQL(postgres.New(), q)` in a throwaway test, which refuses a function or an operator detest does not know wherever it stands. What depends on the schema or on the values, such as a string compared with a timestamp column, or an expression in a `CHECK` or a generated column, passes `CheckSQL` and shows only in the run, through `failOnUnsupported` (step 4). A default detest cannot evaluate shows there only when the table's own schema reads the column; otherwise a write that leaves the column out stores the `Unknown` marker, which a process reading the column gets back, so read the defaults of the columns the flow reads yourself.

### When detest does not apply

Stop before changing anything when the codebase, or the part the user cares about, cannot be simulated, and tell the user why. A pass from a harness that bypassed the real concurrency would look like evidence and be none, so do not force it. These are the typical blockers.

- not Go, or a Go module that cannot move to Go 1.26 (the user declined the bump, or a dependency pins an older toolchain);
- the shared state lives where detest has no simulation, such as a database other than PostgreSQL or MySQL (SQLite, MongoDB, DynamoDB, Spanner), Redis or another cache used for locks or counters, object storage, or a message broker the code talks to through a client that cannot be replaced at a seam;
- the database is reached without `database/sql`, such as through `pgxpool`/`pgx.Conn` or a vendor SDK;
- the race is on Go memory between goroutines rather than through shared resources, which `go test -race` covers and detest does not;
- the flow depends on SQL or settings detest refuses (see the README's unsupported list), such as an isolation level detest does not implement for that server, and it cannot be worked around without changing semantics;
- there is no concurrency to explore, such as a CLI or batch job that runs alone and touches nothing shared.

Report it in this shape, in the user's language.

```markdown
## detest cannot be applied to <the codebase | the flow>

- Blocker: <which of the above>
- Evidence: <file:line, go.mod line, import, or statement that shows it>
- What was checked: <the parts you looked at, and any part that can still be simulated>
- What would make it applicable: <the smallest change, such as injecting a `*sql.DB` at <file:line>, with its cost>, or "nothing reasonable"
- Alternatives: <what fits instead, such as `go test -race`, an integration test against the real database with concurrent clients, or a review of the specific race>
```

When only part of the codebase is blocked, say which flows can still be simulated and offer them as candidates in step 2. Ask before making any change the "What would make it applicable" line proposes.

Once detest is a dependency (step 3), read its docs from the module rather than relying on memory, because the API evolves:

```sh
dir=$(go list -m -f '{{.Dir}}' github.com/k1LoW/detest)
cat "$dir/README.md"            # supported and unsupported SQL, options, env vars
go doc -all github.com/k1LoW/detest
ls "$dir/examples"              # small complete examples
```

`references/api.md` is a cheat sheet of the parts used most.

## 2. Propose targets and agree on the scenario

Survey the code for flows where concurrency can break a business rule. Read `references/bug-patterns.md` for the hazards to look for and how each is modelled. Typical signs are a read followed by a write computed from it, a check followed by an insert, a status transition without a condition on the current status, a claim of work committed before the work is done, a retry around a call with side effects, a message handler that is not idempotent, and locks taken in different orders on different paths.

Decide where to look in this order, and say in one line which source the candidates came from.

1. **What the user named.** Make it the recommended candidate, and list anything nearby that looks riskier after it.
2. **The session so far.** When the user names nothing, the conversation usually shows what they care about, such as the feature being built, the files read or edited, the bug being chased, or the review under way. Deciding to use detest at that point most often means "check what we have been working on". Survey that code and its callers first.
3. **The work in progress.** Without such context, look at the uncommitted changes and the branch's diff from the default branch (`git status`, `git diff`, `git diff <default>...HEAD`). Fresh code is where an unexamined race is most likely.
4. **The whole repository.** Only when none of the above points anywhere, survey the flows that write shared state.

Present 2 to 5 candidates, ranked by how likely and how costly the bug would be, with the ones from the session or the diff first, and have the user pick **one**, recommending the top one. One target per round keeps the scenario, the invariant and the report focused, and the user's attention on one decision at a time. The others are offered again after the report. When the user named the target, still ask, with their target as the recommendation. For each candidate, give the code and the consequence together:

- the function and where it is (`path/file.go:line`), and the statements or calls whose order matters ("`SELECT count(*)` at `booking.go:43`, then `INSERT` at `booking.go:51`, with no lock between")
- what could go wrong, as a story ("two customers buy the last item at the same moment, both succeed")
- the rule that would be broken (this becomes the invariant)
- what detest would vary (interleavings only, or also crashes, duplicate deliveries, lost responses)

Then agree on the details with the user. Propose an answer to each question from what the code says, and let them correct it. Present the proposal in the shape below, which is the scenario the test will run, told from the code up.

- **The rule that must never break**, stated so it can be checked from the database or from what the callers saw. "Stock never goes below zero and orders never exceed the initial stock." This is the most important thing to get right, since detest proves exactly the invariant you write and nothing else.
- **Who runs concurrently, and how many.** Two concurrent actors are usually enough to show a race; three rarely add anything but cost.
- **Which failures happen in production**: workers dying mid-flow, a queue delivering twice or losing a message, an external call whose response is lost after it took effect.
- **Which outcomes are acceptable.** A request that fails cleanly with "sold out" is fine; one that fails with a deadlock error may or may not be, depending on whether callers retry.

```markdown
## Test scenario: <flow name>

### What this code does
<always: the domain, the operation targeted and what the test does to it, in two to four sentences, such as "In the shop's checkout, `Buy` sells one unit of a product. The test has two buyers call it at once for the last unit."
For a user who does not know the code well, also the path a request takes, one line per step, such as
1. `Buy` (shop/service.go:30) begins a transaction
2. reads the stock with `SELECT stock FROM products WHERE id = $1` (shop/repo.go:43)
3. returns `ErrSoldOut` when it is 0, otherwise `UPDATE products SET stock = stock - 1` (shop/repo.go:51) and `INSERT INTO orders` (shop/repo.go:58)
4. commits>

### What the test does
- Data before each run: <the concrete rows, such as one product `p1` with stock 1>
- What happens at the same time: <who calls what with which arguments, such as two buyers, alice and bob, each call `Buy(p1)` once>
- Faults injected: <none, or what fails, where, and what it stands for in production>
- What is checked: <the rule in plain words, and how it is read from the database or the callers' results>
- Acceptable outcomes: <such as one buyer gets `ErrSoldOut`>; not acceptable: <such as a deadlock error, if callers do not retry>
- Situation that must be reached: <what the bug needs, such as both buyers past the stock check before either commits>

### The order that would break it
<the suspected interleaving, one numbered step per operation, with the actor, the `file:line` and what it reads or writes, ending in the broken rule>

### Guesses
<what you assumed because the code did not say, or "none">
```

Write the whole proposal in the message before asking for the decision. A question tool's options carry only the choice (go ahead, change the rule, change who runs or what fails), not the scenario, and the user should not have to open them to learn what is being tested. Never stand in for the proposal with a line saying the scenario and rules were worked out. Every name in it must be one the user can find in their code. Names you make up for the test, such as actor names or seeded ids, are introduced with what they stand for the first time they appear.

Do not proceed on an invariant the user has not agreed with. A wrong invariant produces either a false alarm or a false sense of safety.

## 3. Set up the dependency

Adding detest changes `go.mod` and `go.sum`, and may raise the `go` line to 1.26. Tell the user what will change and get a yes before running:

```sh
go get github.com/k1LoW/detest/postgres@latest            # or .../mysql
go get github.com/k1LoW/detest/postgres/pgxerr@latest     # the error converter the test uses, if any
```

Get the packages the test imports, not only the module root. `go get` of the root records the checksums of the root's dependencies only, and the build then fails on the server package's ones (the SQL parsers). Avoid `go mod tidy` for this, since it also upgrades unrelated dependencies.

The detest tests are kept out of the everyday `go test ./...` and run one package at a time, because an exploration takes seconds to minutes and uses every core.

- Every detest test file, the shared helpers included, starts with `//go:build detest`. A normal `go test ./...` then neither runs nor compiles them. A build tag is used rather than a `t.Skip` on an environment variable, since a skipped test still compiles and reads as passed.
- They run with `go test -tags detest -p 1`. Within a package Go already runs them one after another, as `Explore` does not call `t.Parallel` (so do not add it either), and `-p 1` keeps packages from running at the same time. Explorations running side by side would only slow each other down and could trip the real-time stall watchdog (`DETEST_STALL`) or eat `MaxDuration` budgets.
- `DETEST_WORKERS` uses the cores inside each exploration instead.

Tell the user this in a line or two when asking about the dependency. If the project has a task runner (Makefile, Taskfile, justfile, package scripts), propose adding a `detest` target that runs the command in step 5, and add it only if they agree.

## 4. Write the simulation test

Always write the test into a new file next to the code under test. Never add it to an existing test file, not even an earlier detest one. The shared helper file below is the only exception. A file of its own can be reviewed, kept or deleted as a unit without touching the project's tests, and the user can tell at a glance what this round added. Name the file and the test after the scenario, so that the name alone says which code runs, who does what at the same time, and which fault is injected. Start with the function or handler under test and follow with the scenario, such as `reserve_two_buyers_last_seat_detest_test.go` with `TestReserveTwoBuyersRaceForLastSeat`, `pay_retry_after_lost_response_detest_test.go` with `TestPayRetriesAfterLostResponse`, or `transfer_opposite_directions_detest_test.go` with `TestTransferOppositeDirectionsAtOnce`. Someone reading the file list or a CI failure should know what was simulated without opening the file. Avoid names that give only the target or nothing at all, such as `reserve_detest_test.go`, `dst_test.go` or `concurrency_detest_test.go`. Several scenarios of one target get one file each, named by their scenarios. Drive the highest layer that still runs in-process (the service or use-case function, or the HTTP handler through `ServeHTTP`) so the test covers the code that matters, and construct it inside the declaration function on the handle detest returns. `references/wiring.md` covers ORMs, error conversion, schema loading, HTTP handlers, outgoing calls, queues, mutexes and background workers.

The shape of every simulation:

```go
//go:build detest

package shop

func TestBuyTwoBuyersRaceForLastItem(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		// Runs once per worker. The exploration starts after it returns.
		db, store := s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert)))
		loadMigrations(t, db)            // schema: once, before any run
		svc := shop.NewService(shop.NewRepo(db)) // build the app here, on db

		s.Seed(func() { // runs at the start of every run
			mustExec(t, db, `INSERT INTO products (id, stock) VALUES ('p1', 1)`)
		})
		var unexpected []error            // Go state the processes change...
		s.Seed(func() { unexpected = nil }) // ...is reset in a seed

		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *detest.Proc) error {
				err := svc.Buy(p.Context(), buyer, "p1") // always p.Context()
				if err != nil && !errors.Is(err, shop.ErrSoldOut) {
					unexpected = append(unexpected, fmt.Errorf("%s: %w", buyer, err))
				}
				return nil
			})
		}

		s.AtQuiescence(func(st *detest.State) error { // once all processes are done
			if len(unexpected) > 0 {
				return fmt.Errorf("unexpected error: %w", unexpected[0])
			}
			if n := len(st.Rows(store, "orders")); n > 1 {
				return fmt.Errorf("%d orders for 1 item in stock", n)
			}
			return nil
		})
		s.Sometimes("someone bought it", func(st *detest.State) bool {
			return len(st.Rows(store, "orders")) == 1
		})
	}, failOnUnsupported(t))
}
```

`loadMigrations`, `mustExec` and `failOnUnsupported` are small helpers (see `references/wiring.md`). They go in `detest_helpers_test.go`, which is the one exception to the new-file rule. It is shared by every detest test of the package, so create it when it is missing, and when an earlier round left one, reuse its helpers and add the missing ones to it.

Rules that, when broken, silently make the test meaningless:

- **A process returning an error is not a violation.** detest only notes it in the trace. Record the errors the caller should never see and check them in an invariant, as above. Decide with the user which errors are acceptable.
- **Surface unsupported SQL.** A statement detest cannot run fails with `*detest.ErrUnsupportedSQL`, and if the app swallows or maps that error the exploration passes for nothing. Always pass `failOnUnsupported(t)`.
- **Separate concurrent actors.** `s.Manual(name, n, fn)` runs its `n` instances one at a time. For actors that run at the same time, declare one `Manual` per actor, or add `detest.Instances(n)`.
- **Everything is built inside the declaration function** (pools, clients, channels, services), and the schema is created there. Every run starts from empty tables, since each run resets the databases before the seeds run, so insert all initial rows in `s.Seed`. Reset Go variables there too, and clear or rebuild anything in the app that remembers state between calls (caches, `sync.Once`, in-memory counters), since the declaration function runs once per worker, not once per run.
- **Pass `p.Context()`** to the code under test, so transactions of a run cut short roll back.
- **Add a `Sometimes`** for the situation the bug needs (both actors past the check, a job claimed, a message delivered twice). An exploration that never gets there passes and proves nothing; a complete exploration fails when a `Sometimes` never held.
- **Do not change production code** to make it testable without asking. If a seam is missing (a `sync.Mutex` held across a query, a hard-coded HTTP client), propose the smallest injection point and let the user decide.

## 5. Run and iterate

Before the first run, show the user the conditions the test sets, in plain words, and ask once whether they are fine. Time has passed since step 2 and the user may not remember the scenario, so restate it in a few lines first (what the code does, who does what at the same time, what is checked), then give what is new, with your proposal filled in so the answer can be a yes.

- anything about the scenario that changed while writing the test, and why
- the bounds on the search and what they leave out, why you chose them, and what widening them later would add and cost
- any way the test's schema differs from production's, such as a collation override (see `references/troubleshooting.md`), with what it leaves unchecked

Describe each bound by what it limits in the user's code, never by its option name, such as "at most two points per run where a buyer that could still go on is paused in the middle of `Buy` so the other runs, while a buyer waiting for a lock gives way without counting" rather than `MaxPreemptions(2)`. Keep progress ("the test compiles") on a line of its own, apart from the questions.

These decide what the result can claim, so the user should own them. Ask again only when you change one of them later, such as when widening the scenario or adding a bound to make the search finish.

```sh
go test -tags detest -run 'TestBuyTwoBuyersRaceForLastItem$' -v ./path/to/pkg/
```

To run every detest test of the project, as the report's next steps tell the user:

```sh
DETEST_WORKERS=$(getconf _NPROCESSORS_ONLN) GOGC=400 go test -tags detest -p 1 ./...
```

Use `-v` so the summary line `detest: explored N runs (max depth D, complete=true|false)` is printed even when the test passes. The report needs it. An exploration stops at the first violation and prints the run that found it (`run 5, schedule ...`) instead, so a run count and `complete=true` come only from an exploration without violations, such as the one after a fix or one of a flow that held.

Start with the smallest scenario that can show the bug, which is two actors, one run each, one seeded row and the default bounds. When it passes and its `Sometimes` hold, widen it in the direction the user cares about. Add crashes (`detest.MaxCrashes(1)`), duplicate deliveries (`detest.Duplicates(1)`), a third actor, a background sweeper. Each step multiplies the runs, so widen one thing at a time and watch the run count and `complete=`.

When the harness itself fails (unsupported SQL, "blocked outside the scheduler", "nondeterministic simulation", runs capped before completion, self-waits), read `references/troubleshooting.md`. Fix the harness, not the result. Never rewrite the production SQL inside the test or loosen an invariant to make a run pass.

## 6. Triage a violation

A violation prints a trace and `DETEST_REPLAY=...`. Before calling it a bug:

1. **Is the harness right?** The invariant says what the user agreed, the counters are reset in seeds, the scenario is one production can see (for example, nothing upstream already serializes these requests). If not, fix the test and rerun.
2. **Is it in the code under test?** Read the trace step by step against the source lines it cites. Each line is recorded when its operation starts running, so the trace reads top to bottom in the order the operations started. What a statement saw still depends on the database: it sees only writes committed before its snapshot (each statement's at PostgreSQL Read Committed, the transaction's first read at MySQL Repeatable Read for plain reads, while MySQL's locking reads, `UPDATE` and `DELETE` read the latest committed rows), never another transaction's uncommitted writes, and a statement that waited for a lock finished after the commit or rollback that released it, which may be printed below it. Retell the story in that order, with each read's value explained by the commits it could see. It should tell a story, such as "alice reads stock 1, bob reads stock 1, alice writes 0 and commits, bob writes 0 and commits, two orders". Replay it with `DETEST_REPLAY=<choices> go test -tags detest -run ...` to confirm it is deterministic.
3. **Is the simulated database right?** detest matches the server exactly where an outcome depends on it, and refuses (`ErrUnsupportedSQL`) what it does not implement, rather than approximating. A violation is therefore normally real. If the trace depends on a database behavior you doubt, say so in the report and, if the user has a real database at hand, offer to reproduce the interleaving there with two connections.

Then pin it. Keep the test with `s.ExpectViolation("<substring of the message>")`, which passes while the bug is there and fails once it is fixed, at which point the line is removed. Propose a fix as a concrete change to their code, a diff of the lines to change, not only its name (row lock, conditional update with an affected-row check, unique constraint, idempotency key, lock ordering, doing the work in the claiming transaction). Say why it closes the interleaving that was found, and what it costs (more lock waits, a new error the caller must handle). When there are several fixes, recommend one. Apply it to production code only with the user's go-ahead. Then rerun without `ExpectViolation`, and confirm that a complete exploration passes, which is how the fix is shown to work.

## 7. Report

Always end with a report the user can act on without knowing detest. It answers three questions, namely what In-Process DST was run, what it guarantees, and what it found. Use this structure, in the user's language.

```markdown
## In-Process DST report: <flow name>

### What the code does
<the same few lines as in the step 2 proposal, so the report reads on its own>

### What was run
- Code under test: <functions/handlers, with file:line>, real code, unchanged
- Scenario: <who runs concurrently, how many times, with which seeded data>
- Faults injected: <none | worker crash at any step (up to N) | duplicate delivery | lost response after effect | ...>
- Database: <PostgreSQL Read Committed | MySQL InnoDB Repeatable Read>, schema from <migrations/dump>
- Exploration: <N> runs, <complete: every interleaving within the bounds | stopped at MaxRuns/MaxDuration | stopped at run K, where the violation was found>, bounds: <preemptions, crashes, failures, ...>
- Invariants checked: <each rule in plain words, and whether after every step or at the end>
- Reached situations (Sometimes): <each, reached or not>

### Result
<For a violation:
- the broken rule and the observed values ("2 reservations for capacity 1")
- the interleaving in execution order, one numbered step per operation, each with the process, the `file:line`, the statement or call, and what it read or wrote ("bob `SELECT count(*)` at booking.go:43 reads 0, since alice's INSERT is not committed")
- the root cause, pointing at the lines that allow that order
- the replay command
- the impact in production>
<For a pass: "No interleaving within the bounds breaks <rules>.">
<When the exploration did not cover every interleaving, open the result with that, before anything else (see below).>

### What this guarantees, and what it does not
- Guaranteed: <for every interleaving of the listed operations, with the listed faults, within the bounds, the listed invariants hold (or: this schedule breaks them, reproducibly)>
- Not covered: <bounds not explored (more actors, more crashes, preemptions beyond N), faults not injected, invariants not written, code outside the scenario, data races on Go memory between DB calls (use `go test -race`), statements detest refused, behavior that depends on the real server's planner or settings beyond the defaults>

### Next steps
<the recommended fix as a diff with why it works and what it costs, the offer to apply it and verify it with a complete exploration, the pinned regression test and how to run it, the remaining candidates from step 2, offered as the next round>
```

Be exact about the bounds. A pass is a statement about the schedules explored, not a proof that the code is correct, and the user should be able to tell how far it reaches. When the exploration did not cover every interleaving of the scenario, say so at the top of the result, in plain words, and do not let a pass read as "no bug". This applies when:

- it stopped at `MaxRuns` or `MaxDuration` (`complete=false`). Give the runs made and say the rest is unexplored;
- runs were cut at `MaxIdleTicks` (`N runs cut at MaxIdleTicks` in the summary line). Those runs went no further than a loop's last allowed idle tick and their invariants at quiescence were not checked;
- `MaxPreemptions` or another bound cut the search (`complete=true` then means complete within that bound only). Name the bound and what it leaves out;
- it explored one shard (`DETEST_SHARD`) of the space;
- a `Sometimes` condition never held, so the situation the bug needs was never reached.

Then say what it would take to cover the rest (a higher bound, more time, a checkpoint to resume from), and offer to run it. Explain terms like "interleaving" or "invariant" in a few words the first time if the user seems unfamiliar with them.
