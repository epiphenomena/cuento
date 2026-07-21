# Report performance

Reports are pure reads over the ledger, but a report with many period / as-of
COLUMNS or a data-heavy db can go from milliseconds to tens of seconds if built
naively. These are the battle-tested tricks; apply them from the start. Each has
a real commit so you can see the pattern.

## 1. Single ordered scan, not N as-of passes

A report that shows many period / as-of columns must NOT recompute a full
inception-to-date pass per column -- that is O(columns x ledger). Instead do ONE
date-ordered scan of postings, accumulate running balances, and SNAPSHOT the
accumulator at each column's cutoff date.

- `internal/reports/balance_sheet.go` (commit 6d9a399) -- Statement of Position
  multi-period columns.
- income statement FX remeasurement columns (commit 2f1265e) -- went
  43.9s -> 2.8s.

## 2. Hoist date-independent work out of the per-column / per-row loop

The account tree, currency / rate lookups, and id sets (R/E account ids,
intercompany set, restricted-fund set), plus consolidation flags -- compute ONCE
before the loop and reuse across every column.

- income statement (commit c181ca9) -- hoisted a native-activity read that fed
  only the drill-currency union out of the column loop.

## 3. Composite indexes for range seeks

Per-period queries that filter `subsidiary_id = ? AND date BETWEEN ? AND ?` were
doing a full index scan on `subsidiary_id` with the date applied as a residual
filter. A composite `transactions(subsidiary_id, date)` index (migration 00037,
commit 63b6970) turns them into range seeks: program_statement 9.6x,
functional_expenses 6.2x, 990 3.6x.

HOW TO FIND: run `EXPLAIN QUERY PLAN` on the hot query and look for
`SEARCH ... USING INDEX <single-col>` where a second predicate is applied as a
filter. The composite is ADDITIVE -- keep the old single-column index if a test
or another query pins it.

## 4. Float non-associativity when telescoping

When accumulating float (converted / txn-date-rate) sums across period
boundaries in a single scan, preserve the EXACT per-date accumulation order so
the result stays byte-identical to the old per-column computation -- float
addition is not associative. Skip a key that has no row on/before a cutoff so the
per-date key set matches the old pass. This is what lets the single-scan refactor
keep goldens byte-identical.

## 5. Verification discipline

A performance change MUST NOT change output: `make golden` shows ZERO diff after
the refactor (figures byte-identical). Add a multi-COLUMN test (month / quarter
granularity) to pin the telescoping -- single-column GranNone tests don't
exercise the boundary math.

- `TestIncomeStatementFXMultiColumnFoots` (commit 239b814).

## 6. Measure first

Don't guess the hot path -- profile it. A Go harness that drives each report's
`Run` against a data-heavy db (e.g. `bin/dev.db`) with `runtime/pprof`
cpuprofile, then optimize the profiled hot line. This session's ranking found the
income statement at 43.9s dominated 95% by one FX line -- invisible without
profiling.
