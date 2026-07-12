// Package reports is the report framework (p15.1): the shape every Phase-15
// report (p15.3–p15.11) is built from, plus the Registry that mounts them and
// the machine-readable (CSV) renderer. It owns NO HTTP and NO html/template —
// the web layer wraps a report's Table in the app shell (avoiding a
// reports→web→reports import cycle) and drives this package through the
// Registry. A report is a small value: an ID (its URL slug + registry key), a
// TitleKey (an i18n message id the web layer localizes), a Group (a report-group
// permission bucket the route is gated by), a ParamsSpec (which shared params it
// consumes), and a Run that turns resolved Params + a Toolkit into a Table of
// typed cells. The Toolkit (toolkit.go) carries the resolved scope/param context
// and the store; its Appendix-E computation methods land in p15.2 — here it is
// defined and wired far enough to prove the framework end to end.
package reports

import "context"

// Report is one report: pure DATA plus a Run function. It carries no HTTP and no
// rendering — the Registry mounts it, the web layer localizes TitleKey and renders
// the Table it returns. p15.3–p15.11 each add one Report to the Registry; nothing
// else in the framework changes.
type Report struct {
	// ID is the report's stable slug: its /reports/{ID} URL segment AND its
	// registry key. Lowercase ASCII (letters, digits, '-' or '_'); it must be
	// unique across the Registry (Register panics on a duplicate) and is never
	// localized.
	ID string

	// TitleKey is the i18n message id for the report's human title. The web layer
	// resolves it in the request language (rule 9); this package never renders text.
	TitleKey string

	// Group is the report-group permission bucket (D10) the route is gated by. It
	// MUST be one of Groups() — the code-declared set the web layer syncs to
	// report_groups at startup and references via ReportGroup(Group).
	Group string

	// ParamsSpec declares which shared params this report consumes, so the web
	// layer renders only the relevant controls on the params form. The subsidiary
	// scope selector is ALWAYS shown regardless of the spec (every report is scoped).
	ParamsSpec ParamsSpec

	// Run computes the report's Table from the resolved Params over the Toolkit
	// (the store + resolved scope/param context). It is a pure read: it opens no
	// transaction and writes nothing (rule 2). An error is surfaced by the web
	// layer as a 500; a report that legitimately has nothing to show returns an
	// empty Table, not an error.
	Run func(ctx context.Context, tk *Toolkit, p Params) (Table, error)
}

// Groups returns the code-declared report-group set (D10): the permission buckets
// every Report.Group must belong to and the web layer syncs to report_groups at
// startup (so p13.2's admin grant UI offers exactly these). The set is small and
// aligned to the Phase-15 report categories:
//
//   - "financial" — the core financial statements (trial balance, balance sheet,
//     income statement, account ledger, activities by restriction): p15.3–p15.6,
//     p15.9. The everyday bookkeeping/statement reports.
//   - "funds"     — the fund balances & activity report (p15.8): donor-restricted
//     fund tracking, the per-grant funder view.
//   - "programs"  — the program statement (p15.10): the decision-maker view of
//     revenue/expense per program.
//   - "tax"       — the IRS-990 / tax package (functional expenses = 990 Part IX,
//     and the full 990 package): p15.7, p15.11. Year-end preparer reports.
//
// Grouping is by AUDIENCE/permission need, not by data source, so an org can grant
// a bookkeeper the financial statements without exposing the 990 package, and a
// program manager the program view alone. The order is the declared sort order the
// grant UI shows. A group may exist before any report references it (the smoke
// report below lands under "financial").
func Groups() []string {
	return []string{"financial", "funds", "programs", "tax"}
}

// validGroup reports whether g is a declared report group.
func validGroup(g string) bool {
	for _, name := range Groups() {
		if name == g {
			return true
		}
	}
	return false
}
