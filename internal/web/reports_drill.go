package web

import (
	"context"
	"net/http"
	"strings"

	"cuento/internal/i18n"
	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/reports"
	"cuento/internal/store"
)

// p15.3d report DRILL-DOWN (the web side). Every report balance/activity figure can
// "click through" to the transactions that produce it: a report attaches a
// reports.Drill to a drillable cell, the HTML renderer turns it into a link to
// /reports/{id}/drill?{Drill.Encode()} (reports.go), and THIS handler decodes the
// filter, re-fetches exactly the contributing splits via store.DrillSplits, and
// lists them (reusing the register row rendering) with a header echoing the drilled
// figure -- its signed NATIVE sum, which reconciles to the report cell by
// construction (store.DrillSplits mirrors the toolkit's scope/currency/date filter).
//
// PERMISSION: the route is auto-mounted per report in routes.go gated by the SAME
// ReportGroup(report.Group) as the report page, so drill visibility EQUALS report
// visibility and the permission matrix covers it with zero test edits (a concrete
// registry route). CONVERTED/CONSOLIDATED cells drill to their NATIVE splits: the
// figure a report converted (D12) still lists the native underlying transactions,
// and the header annotates that the report figure was shown converted -- the
// reconciliation is against the PRE-conversion native figure.

// reportDrillFromPath resolves the report the drill route serves from the request
// path "/reports/{id}/drill". reportFromPath (the page/csv resolver) can't be reused:
// it strips a ".csv" suffix and treats the remainder as the id, which would mis-parse
// "{id}/drill". Here the id is the segment between "/reports/" and "/drill".
func (s *server) reportDrillFromPath(r *http.Request) (reports.Report, bool) {
	p := strings.TrimPrefix(r.URL.Path, "/reports/")
	p = strings.TrimSuffix(p, "/drill")
	return s.reports.Get(p)
}

// drillRow is one rendered drill line: the same shape the register uses (date, sub,
// description, memo, counter-account, fund chip, amount) plus the txn id each row links to
// (the p12.4 editor/history). It carries the raw signed amount so the handler sums
// the rows to echo the reconciled figure and a test can assert the sum without HTML
// scraping.
type drillRow struct {
	SplitID int64
	TxnID   int64

	Amount   int64 // raw signed minor units (net-debit, D2) -- summed to reconcile
	Currency string

	Date           string // formatted per the user's date setting
	SubName        string
	Description    string // per-split free-text (p26.20; description -> memo fallback)
	Memo           string
	CounterAccount string
	IsSplit        bool
	FundName       string
	AmountFmt      string
}

// drillPageModel is the drill page's template model: the drilled account name +
// currency, the figure (the signed sum of the listed splits, formatted), a
// "converted" annotation when the report cell was shown in a different currency, the
// rows, and the back-link to the report.
type drillPageModel struct {
	ReportTitle string
	ReportID    string
	AccountName string
	Currency    string

	FigureFmt string // the reconciled signed native sum, formatted
	Converted bool   // the report cell was shown converted (native drill annotation)

	Rows []drillRow
}

// reportDrill handles GET /reports/{id}/drill (ReportGroup(group), same as the
// report). It decodes the reports.Drill from the query, fetches the contributing
// splits (store.DrillSplits, which mirrors the toolkit's scope/currency/date filter
// so the sum reconciles), and renders them as a transaction list. A bare hit (no
// query -- the permission matrix) decodes to an empty drill and renders an empty
// list (200), so the route is matrix-reachable.
func (s *server) reportDrill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	rep, ok := s.reportDrillFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	d := reports.DecodeDrill(r.Form)
	filter := store.DrillFilter{
		Scope:     d.Scope,
		Currency:  d.Currency,
		AsOf:      d.AsOf,
		From:      d.From,
		To:        d.To,
		FundID:    d.FundID,
		ProgramID: d.ProgramID,
		Class:     d.Class,
	}
	// The trial-balance retrofit drills exactly one account per cell; the Drill
	// framework carries an account SET for a rollup cell. A p15.9 "released" cell also
	// carries a fund SET (Drill.FundIDs) — it aggregates applications across several
	// RESTRICTED funds, so the drill unions the per-(account, fund) split sets (the
	// store query still filters ONE fund per call, no SQL change). With no fund set the
	// single FundID applies (the established shape). Fetch each combination and merge.
	fundFilters := []*ids.FundID{d.FundID}
	if len(d.FundIDs) > 0 {
		fundFilters = fundFilters[:0]
		for i := range d.FundIDs {
			id := d.FundIDs[i]
			fundFilters = append(fundFilters, &id)
		}
	}
	// A p15.10 program-statement ROLLUP cell carries a program SET (Drill.ProgramIDs) —
	// a parent program's figure folds in its descendant programs, so its drill unions
	// the per-program split sets (account SET × program SET). With no program set the
	// single ProgramID applies (the established shape). Mutually exclusive with ProgramID.
	progFilters := []*ids.ProgramID{d.ProgramID}
	if len(d.ProgramIDs) > 0 {
		progFilters = progFilters[:0]
		for i := range d.ProgramIDs {
			id := d.ProgramIDs[i]
			progFilters = append(progFilters, &id)
		}
	}
	// p27.4: a program-SCOPED report grant clamps the drill to the granted subtree, the
	// SAME data-scoping the report body applies (resolveParams -> Params.ProgramScope).
	// The drill's program filter is URL-supplied (DecodeDrill), so without this a scoped
	// user could hand-craft a sibling-subtree program id and read splits the report body
	// hides. Intersect the requested program filters with the grant subtree: an
	// out-of-scope id (or the unfiltered nil, which would match every program) is dropped;
	// if NOTHING survives, the drill returns an empty list (no leak). Only a
	// program-dimensioned report is reachable by a scoped grant (decide()), so this only
	// bites there; admins/unscoped grants leave progFilters untouched.
	if rep.ProgramDimensioned && u != nil && !u.IsAdmin {
		scopeIDs, err := s.grantProgramScope(ctx, u, rep.Group)
		if err != nil {
			s.serverError(w)
			return
		}
		if len(scopeIDs) > 0 {
			progFilters = clampProgramFilters(progFilters, scopeIDs)
		}
	}
	var rows []store.DrillRow
	for _, fund := range fundFilters {
		filter.FundID = fund
		for _, prog := range progFilters {
			filter.ProgramID = prog
			for _, acct := range d.AccountIDs {
				filter.AccountID = acct
				part, err := s.store.DrillSplits(ctx, filter)
				if err != nil {
					s.serverError(w)
					return
				}
				rows = append(rows, part...)
			}
		}
	}

	opts := formatOptsFor(u)
	rendered, sum, err := s.renderDrillRows(ctx, rows, lang, opts)
	if err != nil {
		s.serverError(w)
		return
	}

	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	// The account name: with exactly one drilled account (the common case) show it;
	// otherwise leave blank (a multi-account rollup drill).
	acctName := ""
	if len(d.AccountIDs) == 1 {
		acctName = s.accountName(ctx, d.AccountIDs[0], lang)
	}

	figureFmt := ""
	if d.Currency != "" {
		figureFmt = money.FormatMoney(sum, d.Currency, exps[d.Currency], opts)
	}

	model := drillPageModel{
		ReportTitle: i18n.T(lang, rep.TitleKey),
		ReportID:    rep.ID,
		AccountName: acctName,
		Currency:    d.Currency,
		FigureFmt:   figureFmt,
		// The report cell was CONVERTED (D12) when the drill was reached from a
		// converted column: the trial-balance retrofit only drills the NATIVE cell, so
		// Converted is false here; the annotation exists so a later report drilling a
		// converted column can set it (native-splits-for-converted rule).
		Converted: false,
		Rows:      rendered,
	}
	s.render(w, r, http.StatusOK, "report-drill.tmpl", s.newShellPage(r, model))
}

// clampProgramFilters intersects a drill's requested program filters with a grant's
// program-subtree scope (p27.4). A nil filter (an unfiltered drill, which would match
// EVERY program) is dropped -- a scoped user may never drill unfiltered; a concrete id
// survives only if it is in the granted subtree. An empty result means NO program is in
// scope for this drill, so the caller lists nothing (a sibling-subtree drill request
// yields an empty page, never sibling splits).
func clampProgramFilters(requested []*ids.ProgramID, scope []ids.ProgramID) []*ids.ProgramID {
	inScope := make(map[ids.ProgramID]bool, len(scope))
	for _, id := range scope {
		inScope[id] = true
	}
	out := make([]*ids.ProgramID, 0, len(requested))
	for _, p := range requested {
		if p != nil && inScope[*p] {
			out = append(out, p)
		}
	}
	return out
}

// renderDrillRows turns store.DrillRow splits into display-ready drillRow lines
// (formatting money/date per the user's settings, resolving the subsidiary/
// fund/counter-account names) and returns the signed sum of the raw amounts (the
// reconciled figure). It mirrors registerRows' name-map + counter-account resolution
// so the drill list matches the register's row shape. The sum is over the SAME rows
// the list shows, so the header figure and the list are provably consistent (and the
// sum equals the report cell it drills, since store.DrillSplits filtered to that
// cell).
func (s *server) renderDrillRows(
	ctx context.Context, rows []store.DrillRow, lang string, opts money.FormatOpts,
) ([]drillRow, int64, error) {
	names, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		return nil, 0, err
	}
	funds, err := fundNameMap(ctx, s.store)
	if err != nil {
		return nil, 0, err
	}
	subs, err := subNameMap(ctx, s.store)
	if err != nil {
		return nil, 0, err
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		return nil, 0, err
	}
	df := dateFormatForLang(ctx)

	// Counter-accounts resolved per distinct txn (bounded by the row count). "self"
	// is the drilled account -- take it from the split's account when known; but a
	// DrillRow doesn't carry its own account id (the query filtered to it), so resolve
	// the counter as "the OTHER split's account" via the split's transaction.
	counters := make(map[int64]counterAccount)

	out := make([]drillRow, 0, len(rows))
	var sum int64
	for _, rw := range rows {
		sum += rw.Amount

		ca, ok := counters[rw.TxnID]
		if !ok {
			ca, err = resolveDrillCounter(ctx, s.store, rw.TxnID, rw.SplitID, names)
			if err != nil {
				return nil, 0, err
			}
			counters[rw.TxnID] = ca
		}

		memo := rw.SplitMemo
		if memo == "" {
			memo = rw.TxnMemo
		}
		fund := ""
		if rw.FundID != nil {
			fund = funds[*rw.FundID]
		}
		exp := exps[rw.Currency]

		out = append(out, drillRow{
			SplitID:        rw.SplitID,
			TxnID:          rw.TxnID,
			Amount:         rw.Amount,
			Currency:       rw.Currency,
			Date:           money.FormatDate(parseISOForDisplay(rw.Date), df),
			SubName:        subs[int64(rw.SubsidiaryID)],
			Description:    rw.Description,
			Memo:           memo,
			CounterAccount: ca.name,
			IsSplit:        ca.isSplit,
			FundName:       fund,
			AmountFmt:      money.FormatMoney(rw.Amount, rw.Currency, exp, opts),
		})
	}
	return out, sum, nil
}

// resolveDrillCounter returns the counter-account for a drill row: the OTHER split's
// account for a 2-split txn, or the "Split" marker for a >2-split txn. Unlike the
// register's resolveCounterAccount (which knows the self account id), the drill query
// filtered to one account so the self split is identified by its split id -- the
// counter is the other split when the txn has exactly two.
func resolveDrillCounter(ctx context.Context, st *store.Store, txnID, selfSplitID int64, names map[int64]string) (counterAccount, error) {
	splits, err := st.TransactionSplits(ctx, txnID)
	if err != nil {
		return counterAccount{}, err
	}
	if len(splits) == 2 {
		for _, sp := range splits {
			if sp.ID != selfSplitID {
				return counterAccount{name: names[sp.AccountID]}, nil
			}
		}
	}
	return counterAccount{isSplit: true}, nil
}
