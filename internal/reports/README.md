# Adding a report

This package (`internal/reports/`) is the report framework: a `Registry` of `Report`
values, each a small piece of DATA (`ID`, `TitleKey`, `Group`, `ParamsSpec`) plus a
`Run` that turns resolved `Params` + a `Toolkit` into a `Table` of typed cells. The web
layer (`internal/web/reports.go`) auto-mounts a route per report and renders the `Table`
through ONE generic template; the permission matrix, the `/reports` index, and the CSV
export all pick a report up with no edits.

So a NEW report is a **code-only addition**: no new route, no new handler, no template,
no index or nav change. Follow this checklist. The trial-balance report
(`trial_balance.go`) is the exemplar — copy it.

## Checklist

1. **`internal/reports/<id>.go`** — declare the `Report` and its `Run`.
   - A `const <X>ReportID = "<id>"`: the URL slug + registry key. Lowercase ASCII,
     letters/digits/`-`/`_`, unique across the registry.
   - A `register<X>(reg *Registry)` that calls `reg.Register(Report{...})` with the
     `ID`, a `TitleKey` (see step 4), a `Group` (one of `Groups()`:
     `financial` / `funds` / `programs` / `tax` — pick by AUDIENCE / permission need),
     the `ParamsSpec` (which shared params the report consumes — scope is always
     shown), and `Run`.
   - `Run(ctx, tk *Toolkit, p Params) (Table, error)` is a pure READ: it computes
     through the `Toolkit` (Appendix-E methods over the store — `BalancesAsOf`,
     `Activity`, `NetIncome`, `FundBalances`, `Rollup`, …), opens no transaction, and
     writes nothing (rule 2). Build the `Table` from typed cells (`MoneyCell`,
     `TextCell` for stored proper nouns, `LabelCell` for i18n keys, `DateCell`), with
     `Indent` for tree depth and `Kind` (`RowData` / `RowSubtotal` / `RowTotal` /
     `RowWarning`). Column headers are i18n KEYS (`HeaderKey`), never text. A report
     with nothing to show returns an empty `Table`, NOT an error.

2. **Store query (only if the toolkit lacks what you need)** — add a sqlc query.
   - Put the `.sql` in the store's query dir; keep it ASCII, use plain positional `?`
     placeholders, and no string-concatenated SQL (rule 6). Run `make gen` to
     regenerate. Reads outside the store go only through sqlc-generated queries; expose
     the new call as a `Toolkit` method so `Run` stays store-type-free.

3. **Register it** — add one line to `registry.go`'s `Default()`:
   `register<X>(reg)`. Order there is the report's stable position in `All()` (route
   mount order, index order within its group). This is the ONLY place the app assembles
   its report set.

4. **i18n keys in BOTH catalogs** — add every `TitleKey` / `HeaderKey` / `LabelCell`
   key to `internal/i18n/en.toml` AND `es.toml` (identical key sets, enforced by test;
   rule 9). Watch the dotted-key/leaf collision: a key that is also a table prefix
   (`reports.<id>` as both a leaf and a parent of `reports.<id>.col.*`) fails the i18n
   test — nest under a leaf like `reports.<id>.title`.

5. **Attach `Drill` to figures (p15.3d)** — for each balance/activity money cell, add
   `.WithDrill(&Drill{Scope, AccountIDs, Fund, Program, Class, Mode, AsOf|From/To,
   Currency})` mirroring the toolkit filter that produced the cell. The framework then
   renders it as a drill link (HTML) whose contributing splits reconcile to the figure;
   the per-report `/reports/{id}/drill` route is auto-mounted with the report's group
   perm. A converted/consolidated cell drills to its NATIVE underlying splits.

6. **Golden test** — add `testdata/<id>.txt` and `testdata/<id>.csv` and a golden test
   over `testutil.Fixture` (copy trial-balance's). Generate/refresh the goldens with
   `make golden` (or the `-update` flag) and REVIEW the diff — never blind-commit it.

## What you get for free (do NOT touch)

- **Route** — `internal/web/routes.go` auto-mounts `GET /reports/<id>`,
  `/reports/<id>.csv`, and `/reports/<id>/drill`, each gated by
  `ReportGroup(report.Group)`.
- **Permission matrix** — the matrix test iterates the same registry, so the new
  report's group enforcement (granted → 200, ungranted → 403, anon → login) is asserted
  with zero test edits.
- **`/reports` index (p15.12)** — the index lists the report under its group section for
  any user permitted that group; it reuses the route enforcement path, so it can't drift
  from actual access.
- **Rendering** — the generic `report.tmpl` renders the `Table` (localized headers,
  per-user money/date formatting, drill links); the CSV endpoint streams the same
  `Table`. No template or handler edit.
