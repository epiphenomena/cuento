package web

import (
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cuento/internal/i18n"
	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/reports"
	"cuento/internal/store"
)

// p15.1 reports framework -- the web side of internal/reports. The reports package
// owns the report SHAPE (Report/Table/Toolkit) and the CSV renderer but no HTTP and
// no html/template (so it never imports web); THIS file is the generic glue:
//
//   - reportPage renders ANY report's Table into the app shell via ONE generic
//     template (report.tmpl), formatting typed cells per the user's settings
//     (rule 10) and localizing every label (rule 9). p15.3–p15.11 add reports
//     WITHOUT touching this handler or that template.
//   - reportCSV streams the same Table as text/csv via reports.WriteCSV.
//   - the shared params form (rendered on EVERY report page) carries the subsidiary
//     SCOPE selector (always, D18), plus as-of / period / granularity / target
//     currency controls gated by the report's ParamsSpec.
//
// Routes are auto-mounted per report in routes.go (GET /reports/{id} and
// /reports/{id}.csv), each gated by ReportGroup(report.Group); this handler derives
// WHICH report from the request path (the routes are concrete literals, so there is
// no {id} PathValue). An unknown id never matches a route (mux 404s); a defensive
// lookup here 404s too.

// reportFromPath resolves the Report the concrete route serves from the request
// path. The mounted patterns are literal "/reports/<id>" and "/reports/<id>.csv",
// so the id is the last path segment (minus a ".csv" suffix). Returns ok=false for
// an id not in the registry (defensive: the route only exists for a registered
// report, so this is unreachable in normal serving, but keeps the handler total).
func (s *server) reportFromPath(r *http.Request) (reports.Report, bool) {
	id := strings.TrimPrefix(r.URL.Path, "/reports/")
	id = strings.TrimSuffix(id, ".csv")
	return s.reports.Get(id)
}

// resolveParams builds the validated Params for a report run from the query string,
// applying the shared defaults (D18): scope defaults to the user's default
// subsidiary, else the root (full consolidation); as-of/period default to a sensible
// window; target currency defaults to the scope subsidiary's base currency. Only the
// params the report's spec declares are parsed from the query; the rest keep their
// zero value. It also returns the params-form model so the page can re-render the
// controls with the resolved (defaulted) values.
func (s *server) resolveParams(
	ctx context.Context, u *store.CurrentUser, rep reports.Report, q map[string][]string,
) (reports.Params, paramsForm, error) {
	rows, err := s.store.SubTree(ctx)
	if err != nil {
		return reports.Params{}, paramsForm{}, err
	}
	// Reduce the sqlc rows to a local, web-owned shape so the rest of this file
	// (and the form model) never names the store's generated type. SubTree is
	// pre-order, so subs[0] is the single root (D18).
	subs := make([]subInfo, len(rows))
	for i, r := range rows {
		subs[i] = subInfo{ID: int64(r.ID), Name: r.Name, Base: r.BaseCurrency}
	}

	// Scope: query override -> user default -> root.
	scope := int64(0)
	if len(subs) > 0 {
		scope = subs[0].ID
	}
	if u != nil && u.DefaultSubsidiaryID != nil {
		scope = int64(*u.DefaultSubsidiaryID)
	}
	if v := first(q, "scope"); v != "" {
		if id := parseID(v); id != 0 && subExists(subs, id) {
			scope = id
		}
	}

	// Base currency of the resolved scope subsidiary (the target-currency default).
	base := ""
	for _, sub := range subs {
		if sub.ID == scope {
			base = sub.Base
			break
		}
	}

	p := reports.Params{Scope: reports.SubsidiaryID(scope), Lang: langOf(ctx)}
	df := dateFormatFor(u)
	today := s.now()

	if rep.ParamsSpec.AsOf {
		p.AsOf = resolveDate(first(q, "asof"), df, today)
	}
	if rep.ParamsSpec.Period {
		// p30.8 (refines p29.12): the period default distinguishes an ABSENT bound from a
		// PRESENT-BUT-EMPTY one.
		//   - ABSENT (no from=/to= key at all — first page load): default to YTD, i.e.
		//     From = Jan 1 of the current year, To = today. This is the sensible everyday
		//     window ("everywhere probably").
		//   - PRESENT-BUT-EMPTY (from= in the query, value ""): the user CLEARED the input
		//     and the htmx GET auto-submitted the empty field, so BRACKET that bound —
		//     From = day BEFORE the oldest txn, To = day AFTER the newest (via
		//     LedgerDateRange) — capturing everything on that side.
		// The discriminator is KEY EXISTENCE in the parsed form (a present-empty text
		// input still submits its key), not the trimmed value. Only query the range when a
		// bound is actually present-empty (bracket needed); an absent bound stays YTD.
		yearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		fromDefault, toDefault := yearStart, today
		_, fromPresent := q["from"]
		_, toPresent := q["to"]
		fromRaw, toRaw := first(q, "from"), first(q, "to")
		fromBracket := fromPresent && fromRaw == ""
		toBracket := toPresent && toRaw == ""
		if fromBracket || toBracket {
			if lo, hi, ok, err := s.store.LedgerDateRange(ctx); err != nil {
				return reports.Params{}, paramsForm{}, err
			} else if ok {
				// Bracket everything: day BEFORE the oldest, day AFTER the newest. ISO in,
				// ISO out (mirrors resolveDate's time arithmetic; the store keeps ISO).
				if fromBracket {
					if t, perr := time.Parse("2006-01-02", lo); perr == nil {
						fromDefault = t.AddDate(0, 0, -1)
					}
				}
				if toBracket {
					if t, perr := time.Parse("2006-01-02", hi); perr == nil {
						toDefault = t.AddDate(0, 0, 1)
					}
				}
			}
		}
		p.From = resolveDate(fromRaw, df, fromDefault)
		p.To = resolveDate(toRaw, df, toDefault)
	}
	if rep.ParamsSpec.Granularity {
		p.Granularity = reports.ParseGranularity(first(q, "granularity"))
	}
	if rep.ParamsSpec.Currency {
		p.TargetCurrency = base
		if v := first(q, "currency"); v != "" {
			p.TargetCurrency = v
		}
	}
	if rep.ParamsSpec.CurrencyOptional {
		// p26.54: NATIVE by default (empty target); convert only when a currency is
		// explicitly picked. A bare report URL therefore renders native per-currency
		// rows (the program statement's documented default), unlike Currency (which
		// always defaults the target to the scope base).
		if v := first(q, "currency"); v != "" {
			p.TargetCurrency = v
		}
	}
	if rep.ParamsSpec.Detail {
		// Only "currency" turns detail on; any other value (incl. empty) is the
		// default converted-only view.
		if first(q, "detail") == "currency" {
			p.Detail = "currency"
		}
	}
	if rep.ParamsSpec.Account {
		// The report-specific ACCOUNT param (p15.6): parse it and validate against the
		// real leaf-account set (an arbitrary/non-leaf id is dropped -> no account,
		// empty table). Only fetched for a report whose spec declares it.
		accts, err := s.accountLedgerOptions(ctx, langOf(ctx))
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		if v := first(q, "account"); v != "" {
			if id := parseID(v); id != 0 && acctExists(accts, id) {
				p.Account = reports.AccountID(id)
			}
		}
	}
	if rep.ParamsSpec.Fund {
		// The report-specific FUND param (p15.8): parse it and validate against the real
		// fund set (an arbitrary id is dropped -> no fund, the LIST view). Fund 0 is never
		// a valid selection (the synthetic unrestricted group, list-only). Only fetched for
		// a report whose spec declares it.
		funds, err := s.fundActivityOptions(ctx)
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		if v := first(q, "fund"); v != "" {
			if id := parseID(v); id != 0 && fundExists(funds, id) {
				p.Fund = reports.FundID(id)
			}
		}
	}
	if rep.ParamsSpec.Program {
		// The report-specific PROGRAM param (p15.10): parse it and validate against the real
		// program set (an arbitrary id is dropped -> no program, the comparative view). Only
		// fetched for a report whose spec declares it.
		progs, err := s.programStatementOptions(ctx)
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		if v := first(q, "program"); v != "" {
			if id := parseID(v); id != 0 && programExists(progs, id) {
				p.Program = reports.ProgramID(id)
			}
		}
	}
	if rep.ParamsSpec.Reconciliation {
		// The report-specific RECONCILIATION param (p16.4): parse it from ?reconciliation=
		// (or the ?recon= alias) and validate against the real FINALIZED-recon set (an
		// arbitrary/open/unknown id is dropped -> no recon, empty table). Only fetched for a
		// report whose spec declares it. NOT scoped -- a recon spans all funds AND
		// subsidiaries (D13/D20), so the report's included set never narrows by scope.
		recons, err := s.reconStatementOptions(ctx, langOf(ctx))
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		v := first(q, "reconciliation")
		if v == "" {
			v = first(q, "recon")
		}
		if v != "" {
			if id := parseID(v); id != 0 && reconExists(recons, id) {
				p.Reconciliation = reports.ReconciliationID(id)
			}
		}
	}
	if rep.ParamsSpec.Budget {
		// The report-specific BUDGET param (p27.3): parse ?budget= as a budget-PLAN id
		// and validate against the real plan set (an arbitrary id is dropped -> no plan,
		// empty table). When a plan IS chosen and the user has NOT overridden the period,
		// default From/To to the SPAN of the plan's split dates (a plan has no stored
		// period -- p27.3 DECISIONS -- so the data drives the natural window). Only
		// fetched for a report whose spec declares it.
		plans, err := s.budgetReportOptions(ctx)
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		if v := first(q, "budget"); v != "" {
			if id := parseID(v); id != 0 && budgetExists(plans, id) {
				p.Budget = reports.BudgetPlanID(id)
			}
		}
		if p.Budget != 0 && rep.ParamsSpec.Period {
			from, to, err := s.planDateSpan(ctx, ids.BudgetPlanID(p.Budget))
			if err != nil {
				return reports.Params{}, paramsForm{}, err
			}
			// Only when the user did not supply from/to (else respect the override).
			if first(q, "from") == "" && from != "" {
				p.From = from
			}
			if first(q, "to") == "" && to != "" {
				p.To = to
			}
		}
	}

	// p27.4: a program-SCOPED report grant restricts a program-dimensioned report's
	// rows to the granted program's subtree. Resolve the grant scope for THIS report's
	// group and, when program-scoped, set Params.ProgramScope to the subtree ids; a
	// user Program SELECTION is then clamped to that scope (an out-of-scope pick is
	// dropped -> the subtree-comparative view, never a sibling subtree). Admins and
	// unscoped grants leave ProgramScope empty (org-wide, unchanged). Non-program
	// reports are never reached by a purely program-scoped grant (decide()), so this
	// only bites where the report is program-dimensioned.
	if rep.ProgramDimensioned && u != nil && !u.IsAdmin {
		scopeIDs, err := s.grantProgramScope(ctx, u, rep.Group)
		if err != nil {
			return reports.Params{}, paramsForm{}, err
		}
		if len(scopeIDs) > 0 {
			p.ProgramScope = scopeIDs
			if p.Program != 0 && !containsID(scopeIDs, p.Program) {
				p.Program = 0 // an out-of-scope selection falls back to the scoped comparative view
			}
		}
	}

	form, err := s.buildParamsForm(ctx, u, rep, p, subs)
	if err != nil {
		return reports.Params{}, paramsForm{}, err
	}
	return p, form, nil
}

// grantProgramScope returns the program-subtree ids the current user's report grant
// for group restricts rows to (p27.4), or nil when the grant is UNSCOPED (org-wide)
// or absent. It loads the user's grants, finds the one for this group, and -- if it
// carries a program id -- resolves that program's subtree (self + descendants) via
// the store. Admin callers never reach here (resolveParams gates on !IsAdmin). A
// resolved-but-empty subtree cannot happen (ProgramSubtree always includes self).
func (s *server) grantProgramScope(ctx context.Context, u *store.CurrentUser, group string) ([]ids.ProgramID, error) {
	grants, err := s.store.ReportGrants(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if g.Group != group {
			continue
		}
		if g.ProgramID == nil {
			return nil, nil // unscoped grant -> no row filter
		}
		return s.store.ProgramSubtree(ctx, *g.ProgramID)
	}
	return nil, nil // no grant on this group (unreachable for an authorized run)
}

// containsID reports whether id is in ids (a small linear scan for the grant-scope
// clamp; the subtree sets are small).
func containsID[T ~int64](ids []T, id T) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// subInfo is the web-owned reduction of a subsidiary row the report params form
// needs: its id, display name, and base currency. Reducing the store's generated
// SubTree row to this keeps the generated type out of this file's signatures.
type subInfo struct {
	ID   int64
	Name string
	Base string
}

// acctOption is one selectable account in the account-ledger's ACCOUNT selector
// (p15.6): a LEAF account (splits post only to leaves) with its resolved name. It is
// the report-specific analogue of scopeOption.
type acctOption struct {
	ID   int64
	Name string
	// Path (p28.2) is the account's dotted ancestor chain (e.g. "Cash.BOA"); the
	// shared account combobox fuzzy-ranks on it so "c.boa" lines up with Cash.BOA.
	Path string
	// Type is the account's type ("asset"/"expense"/…), used to render an <optgroup>
	// per type in the native fallback select (presentation parity with the grid).
	Type string
}

// accountLedgerOptions returns the LEAF accounts (name-resolved for lang, tree order)
// the account-ledger's account selector offers — splits post only to leaf accounts, so
// a placeholder parent is not a valid ledger target. Leaf = an account that is not the
// parent of any other account (derived from the tree, matching the reports Rollup /
// indexTree convention).
func (s *server) accountLedgerOptions(ctx context.Context, lang string) ([]acctOption, error) {
	tree, err := s.store.Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	isParent := make(map[ids.AccountID]bool, len(tree))
	for _, r := range tree {
		if r.ParentID.Valid {
			isParent[ids.AccountID(r.ParentID.Int64)] = true
		}
	}
	paths, err := s.store.AccountPaths(ctx, lang)
	if err != nil {
		return nil, err
	}
	var out []acctOption
	for _, r := range tree {
		if isParent[r.ID] {
			continue // placeholder parent: not a split target
		}
		out = append(out, acctOption{ID: int64(r.ID), Name: r.Name, Path: paths[r.ID], Type: r.Type})
	}
	return out, nil
}

// acctExists reports whether id is one of the offered leaf accounts (a query account
// override must name a real leaf account, else no account is selected).
func acctExists(accts []acctOption, id int64) bool {
	for _, a := range accts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// fundOption is one selectable fund in the fund-activity report's FUND selector
// (p15.8): a fund with its resolved name (a stored proper noun). The report-specific
// analogue of acctOption; picking one switches the report from its LIST view (the fund
// roster) to that fund's period statement.
type fundOption struct {
	ID   int64
	Name string
}

// fundActivityOptions returns every fund (active and closed — a closed fund may still
// carry a residual balance worth a statement) with its name, id-ordered, for the fund
// report's fund selector.
func (s *server) fundActivityOptions(ctx context.Context) ([]fundOption, error) {
	fs, err := s.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]fundOption, 0, len(fs))
	for _, f := range fs {
		out = append(out, fundOption{ID: int64(f.ID), Name: f.Name})
	}
	return out, nil
}

// fundExists reports whether id is one of the offered funds (a query fund override must
// name a real fund, else the LIST view stands).
func fundExists(funds []fundOption, id int64) bool {
	for _, f := range funds {
		if f.ID == id {
			return true
		}
	}
	return false
}

// programStatementOptions returns every program (tree pre-order) with its name, for the
// program report's program selector. Program names are stored proper nouns (a single
// Name, no per-language variant), rendered verbatim.
func (s *server) programStatementOptions(ctx context.Context) ([]programOption, error) {
	tree, err := s.store.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	// p29.13: the dotted hierarchy path per program, stamped on each option's data-path
	// so the report's program combobox fuzzy-ranks by hierarchy like the account pickers.
	paths, err := s.store.ProgramPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]programOption, 0, len(tree))
	for _, p := range tree {
		out = append(out, programOption{ID: int64(p.ID), Name: p.Name, Path: paths[p.ID]})
	}
	return out, nil
}

// programExists reports whether id is one of the offered programs (a query program
// override must name a real program, else the comparative view stands).
func programExists(progs []programOption, id int64) bool {
	for _, p := range progs {
		if p.ID == id {
			return true
		}
	}
	return false
}

// reconOption is one selectable finalized reconciliation in the statement report's
// RECONCILIATION selector (p16.4): a finalized recon with a human label built from its
// account name + statement date + currency (proper-noun account name resolved for lang;
// the rest already ISO/code). Picking one prints that recon's statement detail.
type reconOption struct {
	ID    int64
	Label string
}

// reconStatementOptions returns every FINALIZED reconciliation across all reconcilable
// accounts (newest statement first per account), each with a display label, for the
// statement report's recon selector. The account name is a stored proper noun resolved
// for lang (D5); the statement date + currency are appended as plain context. It is the
// report-specific analogue of programStatementOptions.
func (s *server) reconStatementOptions(ctx context.Context, lang string) ([]reconOption, error) {
	accts, err := s.store.ReconcilableAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []reconOption
	for _, a := range accts {
		recs, err := s.store.FinalizedReconciliationsForAccount(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		name := s.accountName(ctx, a.ID, lang)
		for _, rc := range recs {
			out = append(out, reconOption{
				ID:    int64(rc.ID),
				Label: name + " " + rc.StatementDate + " " + rc.Currency,
			})
		}
	}
	return out, nil
}

// reconExists reports whether id is one of the offered finalized recons (a query recon
// override must name a real finalized recon, else no recon is selected -> empty table).
func reconExists(recons []reconOption, id int64) bool {
	for _, rc := range recons {
		if rc.ID == id {
			return true
		}
	}
	return false
}

// budgetOption is one selectable budget PLAN in the budget reports' BUDGET selector
// (p27.3): a plan with a display label (its name). Picking one drives the cashflow-
// projection / budget-variance report. The report-specific analogue of reconOption.
type budgetOption struct {
	ID    int64
	Label string
}

// budgetReportOptions returns every budget PLAN (id-ordered) with a display label
// (name), for the budget reports' plan selector.
func (s *server) budgetReportOptions(ctx context.Context) ([]budgetOption, error) {
	ps, err := s.store.ListBudgetPlans(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]budgetOption, 0, len(ps))
	for _, p := range ps {
		out = append(out, budgetOption{ID: int64(p.ID), Label: p.Name})
	}
	return out, nil
}

// budgetExists reports whether id is one of the offered plans (a query budget override
// must name a real plan, else no plan is selected -> empty table).
func budgetExists(plans []budgetOption, id int64) bool {
	for _, p := range plans {
		if p.ID == id {
			return true
		}
	}
	return false
}

// planDateSpan returns the min and max split dates of a budget plan (its natural
// report window, since a plan carries no stored period -- p27.3). Empty strings when
// the plan has no splits. BudgetSplits is date-ordered, so the first and last rows
// bound the span.
func (s *server) planDateSpan(ctx context.Context, planID ids.BudgetPlanID) (from, to string, err error) {
	splits, err := s.store.BudgetSplits(ctx, planID)
	if err != nil {
		return "", "", err
	}
	if len(splits) == 0 {
		return "", "", nil
	}
	from, to = splits[0].Date, splits[0].Date
	for _, sp := range splits {
		if sp.Date < from {
			from = sp.Date
		}
		if sp.Date > to {
			to = sp.Date
		}
	}
	return from, to, nil
}

// resolveDate parses a query date value per the user's date format (ISO always
// accepted), falling back to fallback (a time.Time) rendered ISO when the value is
// empty or unparseable. The stored/param form is always ISO (YYYY-MM-DD), the
// canonical internal date form the store queries expect.
func resolveDate(v string, df money.DateFormat, fallback time.Time) string {
	if v != "" {
		if t, err := money.ParseDate(v, df, fallback); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return fallback.Format("2006-01-02")
}

// ---- params form model ----------------------------------------------------

// paramsForm is the shared report params form's render model. Scope is ALWAYS
// present (every report is scoped, D18); the other controls render only when the
// report's ParamsSpec declares them. Values are the RESOLVED (defaulted) params so
// the form shows what will run, and dates are formatted per the user's setting.
type paramsForm struct {
	// ReportID is the report slug, for the form's action and the CSV link.
	ReportID string

	// Scopes are the subsidiary options (id, name, selected) for the scope selector.
	Scopes []scopeOption

	// The gate flags mirror ParamsSpec so the template shows only relevant controls.
	ShowAsOf        bool
	ShowPeriod      bool
	ShowGranularity bool
	ShowCurrency    bool
	// CurrencyNative marks a CurrencyOptional currency select: it renders a leading
	// "— native —" option (empty value) and native is the default (p26.54).
	CurrencyNative bool
	ShowDetail     bool
	ShowAccount    bool
	ShowFund       bool
	ShowProgram    bool
	ShowRecon      bool
	ShowBudget     bool

	// Resolved control values (formatted for display where dated).
	AsOf        string // user-formatted date
	From        string // user-formatted date
	To          string // user-formatted date
	Granularity string // token: none|month|quarter
	Currency    string // selected target currency code
	Detail      string // token: ""|currency (per-currency detail toggle)
	Account     int64  // selected leaf account id (0 = none chosen)
	Fund        int64  // selected fund id (0 = all funds / list view)
	Program     int64  // selected program id (0 = all programs / comparative view)
	Recon       int64  // selected finalized recon id (0 = none chosen / empty)
	Budget      int64  // selected budget id (0 = none chosen / empty)

	// Options for the selects.
	Currencies []ccyChoice
	Accounts   []acctOption    // the leaf-account options (account-ledger only)
	Funds      []fundOption    // the fund options (fund-activity report only)
	Programs   []programOption // the program options (program-statement report only)
	Recons     []reconOption   // the finalized-recon options (statement report only)
	Budgets    []budgetOption  // the budget options (budget reports only)
}

// scopeOption is one subsidiary choice in the scope selector.
type scopeOption struct {
	ID       int64
	Name     string
	Selected bool
}

// ccyChoice is one target-currency choice in the report currency selector (the
// existing currencyOption in accounts.go carries a name; the report select shows
// codes only, and marks the selected one).
type ccyChoice struct {
	Code     string
	Selected bool
}

// buildParamsForm assembles the params-form model for rep with the resolved params
// p. It lists every subsidiary as a scope option (marking the resolved scope), and
// -- when the report converts -- every ACTIVE currency as a target-currency option
// (marking the resolved target, defaulted to the scope base). Dates are formatted
// per the user's setting for the inputs (which are plain text, never
// input[type=date] -- rule 10 / rule 12).
func (s *server) buildParamsForm(
	ctx context.Context, u *store.CurrentUser, rep reports.Report,
	p reports.Params, subs []subInfo,
) (paramsForm, error) {
	df := dateFormatFor(u)

	f := paramsForm{
		ReportID:        rep.ID,
		ShowAsOf:        rep.ParamsSpec.AsOf,
		ShowPeriod:      rep.ParamsSpec.Period,
		ShowGranularity: rep.ParamsSpec.Granularity,
		ShowCurrency:    rep.ParamsSpec.Currency || rep.ParamsSpec.CurrencyOptional,
		CurrencyNative:  rep.ParamsSpec.CurrencyOptional,
		ShowDetail:      rep.ParamsSpec.Detail,
		ShowAccount:     rep.ParamsSpec.Account,
		ShowFund:        rep.ParamsSpec.Fund,
		ShowProgram:     rep.ParamsSpec.Program,
		ShowRecon:       rep.ParamsSpec.Reconciliation,
		ShowBudget:      rep.ParamsSpec.Budget,
		Granularity:     p.Granularity.String(),
		Currency:        p.TargetCurrency,
		Detail:          p.Detail,
		Account:         int64(p.Account),
		Fund:            int64(p.Fund),
		Program:         int64(p.Program),
		Recon:           int64(p.Reconciliation),
		Budget:          int64(p.Budget),
	}
	for _, sub := range subs {
		f.Scopes = append(f.Scopes, scopeOption{
			ID: sub.ID, Name: sub.Name, Selected: sub.ID == int64(p.Scope),
		})
	}
	if p.AsOf != "" {
		f.AsOf = money.FormatDate(parseISOForDisplay(p.AsOf), df)
	}
	if p.From != "" {
		f.From = money.FormatDate(parseISOForDisplay(p.From), df)
	}
	if p.To != "" {
		f.To = money.FormatDate(parseISOForDisplay(p.To), df)
	}
	if rep.ParamsSpec.Currency || rep.ParamsSpec.CurrencyOptional {
		curs, err := s.store.Currencies(ctx)
		if err != nil {
			return paramsForm{}, err
		}
		for _, c := range curs {
			if c.Active == 0 {
				continue
			}
			f.Currencies = append(f.Currencies, ccyChoice{
				Code: c.Code, Selected: c.Code == p.TargetCurrency,
			})
		}
	}
	if rep.ParamsSpec.Account {
		accts, err := s.accountLedgerOptions(ctx, langOf(ctx))
		if err != nil {
			return paramsForm{}, err
		}
		f.Accounts = accts
	}
	if rep.ParamsSpec.Fund {
		funds, err := s.fundActivityOptions(ctx)
		if err != nil {
			return paramsForm{}, err
		}
		f.Funds = funds
	}
	if rep.ParamsSpec.Program {
		progs, err := s.programStatementOptions(ctx)
		if err != nil {
			return paramsForm{}, err
		}
		f.Programs = progs
	}
	if rep.ParamsSpec.Reconciliation {
		recons, err := s.reconStatementOptions(ctx, langOf(ctx))
		if err != nil {
			return paramsForm{}, err
		}
		f.Recons = recons
	}
	if rep.ParamsSpec.Budget {
		budgets, err := s.budgetReportOptions(ctx)
		if err != nil {
			return paramsForm{}, err
		}
		f.Budgets = budgets
	}
	return f, nil
}

// ---- rendered table model -------------------------------------------------

// reportPageModel is the report page's template model: the localized title, the
// params form, and the rendered table (localized headers + per-user-formatted
// cells). It also carries the CSV link so the page can offer the export.
type reportPageModel struct {
	Title   string
	Params  paramsForm
	Table   renderedTable
	CSVHref string
	// Error is a localized report-LEVEL error message (currently: a required
	// exchange rate is missing for the chosen target currency). When set, the
	// results region renders the message inline INSTEAD of the table + CSV link,
	// with an HTTP 200 -- so under apply-on-change (p26.90) htmx swaps the fragment
	// and the user sees the reason, rather than a silent no-op from an un-swapped 5xx.
	Error string
	// Tree is true for a report presenting a NESTED account hierarchy (p26.26): the
	// template then emits `data-depth` on every table row, renders the shared
	// collapse/expand tree-controls above the table, and loads treetable.js to enhance
	// it. False leaves the report table byte-identical (no controls, no data-depth).
	Tree bool
	// FullWidth (p29.11) mirrors reports.Report.WideMatrix: a comparative statement
	// (monthly Statement of Activities, per-program statement) that fans into many
	// columns renders in the FULL-viewport-width shell (app-main-full) so no column
	// truncates/scrolls. False keeps the ordinary wide shell (100rem reading cap).
	FullWidth bool
	// MeasureToggle (p30.9) mirrors reports.Report.MeasureToggle: the budget-variance
	// grid folds three measures per cell (budgeted/actual/variance) and offers a
	// client-side button group to switch which shows. The template then renders the
	// toggle above the table (with a default-measure table attribute) and loads
	// budgetvariance.js. False renders no toggle (byte-identical to before).
	MeasureToggle bool
}

// renderedTable is a Table prepared for the HTML template: localized column headers
// with alignment, and rows whose cells are already display strings (money/date per
// the user's settings, text/labels localized). Keeping the render here (not in
// reports) means the reports package stays i18n/format-free and the same generic
// template serves every report.
type renderedTable struct {
	Columns []renderedColumn
	Rows    []renderedRow
	// HeaderGroups, when non-empty, is the TOP row of a STACKED (two-row) header (p31
	// program statement): one spanning cell per contiguous run of columns sharing a group,
	// left to right. Empty for a flat single-row header (every other report), so those
	// tables render byte-identically.
	HeaderGroups []renderedHeaderGroup
}

// renderedHeaderGroup is one spanning cell of a stacked header's TOP row: a localized (or
// blank) label over Colspan leaf columns. The leftmost row-label column(s) with no group are
// covered by a blank leading cell so the group row aligns with the leaf row.
type renderedHeaderGroup struct {
	Label   string
	Colspan int
	Right   bool // right-align hint (a money-column group)
}

type renderedColumn struct {
	Header string
	Right  bool // right-align hint (money columns)
	// ProgramID / ProgramParent / ProgramGroup are the p31 10b program-column-tree data
	// attributes the template stamps on this column's leaf <th> as FIXED-name attributes
	// (data-program / data-program-parent / data-program-group). Static attribute names +
	// a dynamic value is the html/template-safe pattern; a computed attribute NAME trips
	// the escaper (it neutralizes hyphenated dynamic names to "zgotmplz"), so these are
	// typed fields, not a generic name/value map. Empty = the attribute is not emitted (a
	// ROOT program has no ProgramParent; a leaf has no ProgramGroup marker).
	ProgramID     string
	ProgramParent string
	ProgramGroup  bool
}

type renderedRow struct {
	Cells        []renderedCell
	Indent       int
	Subtotal     bool
	SectionTotal bool // p30.10: the middle total tier ("Total revenue"/"Total assets").
	Total        bool
	Warning      bool
}

type renderedCell struct {
	Text  string
	Right bool // right-align (money)
	// Href, when non-empty, makes the cell a DRILL link (p15.3d): the HTML template
	// renders <a href="{Href}">{Text}</a> (a plain link, strict CSP -- no inline
	// handler). It is set for a drillable cell (Cell.Drill != nil), pointing at the
	// report's /reports/{id}/drill route with the encoded filter.
	Href string
	// Measures (p30.9), when non-nil, marks a budget-variance grid cell that carries
	// THREE pre-formatted measures (budgeted/actual/variance) the template renders as
	// three toggle-able spans instead of a single value. Mutually exclusive with a plain
	// Text render.
	Measures *renderedMeasures
}

// renderedMeasures holds the three pre-formatted measure strings of a CellMeasures cell
// (p30.9), each formatted server-side per the user's settings (rule 10 -- the JS never
// does money math, it only shows/hides). ActualHref carries the actual measure's drill
// link (only the posted actuals drill); Bucket is the over/under CSS-class suffix ("" =
// no color) the template stamps on the variance span so a total reads its magnitude.
type renderedMeasures struct {
	Budgeted   string
	Actual     string
	Variance   string
	ActualHref string
	Bucket     string // "" | "over-slight" | "under-large" | ... (signed magnitude class)
}

// buildHeaderGroups builds the TOP row of a STACKED header (p31 program statement): one
// spanning cell per contiguous run of columns sharing a group (Column.Group), left to right.
// Leading columns with NO group (the row-label column) collapse into ONE blank leading cell
// so the group row aligns with the leaf row. Returns nil when NO column declares a group, so
// every flat-header report renders a single-row <thead> unchanged. Adjacent columns form one
// span iff they share the same non-empty GroupID (a nil group breaks the run).
func buildHeaderGroups(cols []reports.Column, lang string) []renderedHeaderGroup {
	anyGroup := false
	for _, c := range cols {
		if c.Group != nil {
			anyGroup = true
			break
		}
	}
	if !anyGroup {
		return nil
	}
	var groups []renderedHeaderGroup
	i := 0
	for i < len(cols) {
		g := cols[i].Group
		if g == nil {
			// A run of ungrouped columns → one blank spanning cell.
			span := 0
			for i < len(cols) && cols[i].Group == nil {
				span++
				i++
			}
			groups = append(groups, renderedHeaderGroup{Colspan: span})
			continue
		}
		// A run of columns sharing this group's id.
		id := g.GroupID
		label := ""
		if g.Key != "" {
			label = i18n.T(lang, g.Key)
		}
		span := 0
		right := true
		for i < len(cols) && cols[i].Group != nil && cols[i].Group.GroupID == id {
			if cols[i].Align != reports.AlignRight {
				right = false
			}
			span++
			i++
		}
		groups = append(groups, renderedHeaderGroup{Label: label, Colspan: span, Right: right})
	}
	return groups
}

// renderTable turns a reports.Table into the display-ready renderedTable for lang,
// formatting money/date cells per the user's settings (rule 10) and localizing
// column headers and LABEL cells (CellLabel carries an i18n key), while TEXT cells
// (proper nouns) render verbatim. The label/text distinction is EXPLICIT in the
// cell kind (not guessed from the string), so a report can never render a raw key
// or wrongly translate a stored name. Money cells use the per-user opts with the
// currency code prefixed (e.g. "USD 1,234.56") so mixed-currency tables stay
// unambiguous.
func renderTable(t reports.Table, reportID, lang string, opts money.FormatOpts, df money.DateFormat, exps map[string]int) renderedTable {
	var rt renderedTable
	for _, c := range t.Columns {
		header := c.HeaderText // a verbatim proper-noun header (a program name, rule 9)
		if header == "" {
			header = i18n.T(lang, c.HeaderKey)
		}
		rc := renderedColumn{
			Header: header,
			Right:  c.Align == reports.AlignRight,
		}
		if c.Group != nil {
			// p31 10b: the program-column-tree data attributes. Fixed names, dynamic values
			// (html/template-safe); a root omits program-parent, a leaf omits program-group.
			rc.ProgramID = c.Group.Data["program"]
			rc.ProgramParent = c.Group.Data["program-parent"]
			rc.ProgramGroup = c.Group.Data["program-group"] == "1"
		}
		rt.Columns = append(rt.Columns, rc)
	}
	rt.HeaderGroups = buildHeaderGroups(t.Columns, lang)
	for _, row := range t.Rows {
		rr := renderedRow{
			Indent:       row.Indent,
			Subtotal:     row.Kind == reports.RowSubtotal,
			SectionTotal: row.Kind == reports.RowSectionTotal,
			Total:        row.Kind == reports.RowTotal,
			Warning:      row.Kind == reports.RowWarning,
		}
		for _, cell := range row.Cells {
			rr.Cells = append(rr.Cells, renderCell(cell, reportID, lang, opts, df, exps))
		}
		rt.Rows = append(rt.Rows, rr)
	}
	return rt
}

// renderCell formats one typed cell to a display string. A cell carrying a Drill
// (p15.3d) also gets an Href to the report's /reports/{id}/drill route with the
// encoded filter, so the HTML renders the value as a plain link (strict CSP).
func renderCell(c reports.Cell, reportID, lang string, opts money.FormatOpts, df money.DateFormat, exps map[string]int) renderedCell {
	href := ""
	if c.Drill != nil {
		href = "/reports/" + reportID + "/drill?" + c.Drill.Encode()
	} else if c.TxnID != 0 {
		// p15.6: a ledger LINE cell links to the transaction editor/history (p12.4).
		// The reports package carries only the txn id; the web layer builds the URL
		// (parallel to how it builds the drill URL from Drill), keeping URL
		// construction out of reports.
		href = "/transactions/" + strconv.FormatInt(int64(c.TxnID), 10) + "/edit"
	}
	switch c.Kind {
	case reports.CellMoney:
		if c.Blank {
			return renderedCell{Text: "", Right: true}
		}
		return renderedCell{
			Text:  money.FormatMoney(c.Minor, c.Currency, exps[c.Currency], opts),
			Right: true,
			Href:  href,
		}
	case reports.CellMeasures:
		// p30.9: format all three measures server-side (rule 10) into three spans the
		// client toggles; the ACTUAL span carries the drill (only posted actuals drill).
		exp := exps[c.Currency]
		return renderedCell{
			Right: true,
			Measures: &renderedMeasures{
				Budgeted:   money.FormatMoney(c.Budgeted, c.Currency, exp, opts),
				Actual:     money.FormatMoney(c.Actual, c.Currency, exp, opts),
				Variance:   money.FormatMoney(c.Variance, c.Currency, exp, opts),
				ActualHref: href,
				Bucket:     measureBucketClass(c.Variance, c.Bucket),
			},
		}
	case reports.CellDate:
		if c.Text == "" {
			return renderedCell{}
		}
		return renderedCell{Text: money.FormatDate(parseISOForDisplay(c.Text), df), Href: href}
	case reports.CellLabel:
		return renderedCell{Text: i18n.T(lang, c.Text)}
	default: // CellText -- a stored proper noun, verbatim
		return renderedCell{Text: c.Text}
	}
}

// measureBucketClass maps a variance total's SIGN (over vs under budget) and its
// magnitude bucket (reports.VarianceBucket name) to a CSS-class suffix the template
// stamps on the variance span (p30.9). Positive variance = OVER budget (a red ramp);
// negative = UNDER (a green ramp); the magnitude deepens the shade. "" (neutral bucket
// or zero variance) => no class (no color). The number's sign already carries over/under
// for accessibility; the color only reinforces it (never the sole cue).
func measureBucketClass(variance int64, bucket string) string {
	if bucket == reports.VarianceNeutral {
		return ""
	}
	dir := "over"
	if variance < 0 {
		dir = "under"
	}
	return dir + "-" + bucket // e.g. "over-slight", "under-large"
}

// ---- index (p15.12) -------------------------------------------------------

// reportsIndexModel is the /reports index template model: the report groups the
// current user may access, each a section of report CARDS (p28.12). It carries the
// SAME allSection/hubCardItem shape the "All" landing uses, so both pages render
// through the shared "card-section" partial (identical card grid). Empty (no permitted
// group) renders the empty-state message, not an error.
type reportsIndexModel struct {
	Sections []allSection
}

// reportsIndex handles GET /reports (AnyUser): it lists the reports the CURRENT user
// may access, grouped by report group, each a link to /reports/{id}. Only PERMITTED
// groups/reports appear -- a user sees a group's reports only if they hold that
// group's grant (or are admin). It reuses the SAME enforcement path the report routes
// use (decide + grantChecker), so the listing can never drift from actual access: a
// report is listed iff decide(ReportGroup(rep.Group), u, ...) == outcomeAllow, exactly
// the check enforce() runs on GET /reports/{id}. An ungranted (non-admin) user gets a
// 200 index with an empty list + the empty-state message, never a 403 -- the page
// itself is AnyUser; it filters its own contents.
func (s *server) reportsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	// One grant load, reused across every group check. Passing a ReportGroup perm is
	// REQUIRED: grantChecker returns the always-false stub for any other perm kind, so
	// AnyUser here would list nothing. decide() then short-circuits admin -> allow all
	// and consults this closure per group for a non-admin user -- the identical path
	// enforce() takes for a concrete report route.
	checker := s.grantChecker(ctx, u, ReportGroup(""))

	// Bucket the permitted reports by group, preserving reports.Groups() order for the
	// sections and All() order within each group (both stable), so the index is
	// deterministic and matches the grant UI's group order (reports.go / D10).
	byGroup := make(map[string][]hubCardItem)
	for _, rep := range s.reports.All() {
		// ReportGroupFor carries the report's program-dimensioned bit (p27.4), so a
		// purely program-scoped user's index lists the program-dimensioned reports
		// (which they may reach, filtered) and HIDES the non-program ones (which they
		// may not) -- exactly the enforce() outcome, no 403 traps in the listing.
		if decide(ReportGroupFor(rep), u, checker) != outcomeAllow {
			continue
		}
		byGroup[rep.Group] = append(byGroup[rep.Group], hubCardItem{
			Label: i18n.T(lang, rep.TitleKey),
			Href:  "/reports/" + rep.ID,
			// p28.12: each report card carries its one-line description (the same blurb
			// the "All" landing shows), from reports.<id>.desc via reportDescKey.
			Desc: i18n.T(lang, reportDescKey(rep.TitleKey)),
			Icon: "report", // p28.14: document/report outline glyph, matching the All landing.
		})
	}

	var model reportsIndexModel
	for _, g := range reports.Groups() {
		cards := byGroup[g]
		if len(cards) == 0 {
			continue // a section renders only when it has >=1 permitted report
		}
		model.Sections = append(model.Sections, allSection{
			Label: i18n.T(lang, "reports.group."+g),
			Cards: cards,
		})
	}

	s.render(w, r, http.StatusOK, "reports_index.tmpl", s.newShellPage(r, model))
}

// ---- handlers -------------------------------------------------------------

// reportPage handles GET /reports/{id} (ReportGroup(group)): it resolves the params
// from the query (with shared defaults), runs the report over a fresh Toolkit, and
// renders the Table into the app shell via the generic report.tmpl. The params form
// (incl. the always-present scope selector) is re-rendered with the resolved values.
func (s *server) reportPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	rep, ok := s.reportFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	params, form, err := s.resolveParams(ctx, u, rep, r.Form)
	if err != nil {
		s.serverError(w)
		return
	}

	model := reportPageModel{
		Title:         i18n.T(lang, rep.TitleKey),
		Params:        form,
		Tree:          rep.Tree,          // p26.26: nested-account reports emit data-depth + tree controls.
		FullWidth:     rep.WideMatrix,    // p29.11: comparative statements use the full-viewport shell.
		MeasureToggle: rep.MeasureToggle, // p30.9: budget variance offers the measure toggle.
	}

	table, err := rep.Run(ctx, reports.NewToolkit(s.store, params), params)
	if err != nil {
		// A missing exchange rate for the chosen target currency is a USER-level
		// condition (no rate on file), not a server fault: render a clean inline
		// message in the results region with a 200 so the apply-on-change fragment
		// swaps (a 5xx would leave htmx showing nothing). Any other error is a real
		// 500.
		if errors.Is(err, store.ErrRateMissing) {
			model.Error = i18n.T(lang, "reports.error.no_rate", params.TargetCurrency)
			s.renderReportResults(w, r, model)
			return
		}
		s.serverError(w)
		return
	}

	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	model.Table = renderTable(table, rep.ID, lang, formatOptsFor(u), dateFormatFor(u), exps)
	model.CSVHref = "/reports/" + rep.ID + ".csv?" + r.Form.Encode()
	s.renderReportResults(w, r, model)
}

// renderReportResults writes the report page: p26.90 a filter change is the subnav
// form's hx-get targeting #report-results (HX-Target header), so swap ONLY the
// results fragment (CSV link + tree controls + table, OR the inline error); a full
// load or a boosted nav (HX-Target absent / "body") renders the whole page. The
// CSVHref is recomputed by the caller from r.Form, so the swapped fragment always
// carries a fresh export link, and hx-push-url keeps the URL in sync for persistence.
// Both the success and the missing-rate error path route through here so the error
// renders in the SAME results region with a 200 (htmx swaps it), never a 5xx.
func (s *server) renderReportResults(w http.ResponseWriter, r *http.Request, model reportPageModel) {
	if r.Header.Get("HX-Target") == "report-results" {
		s.render(w, r, http.StatusOK, "report-results", model)
		return
	}
	// p26.86: EVERY report renders its filter controls in the SECOND-LEVEL nav bar
	// (SubNavControls="report" renders the shared "report-filters" partial off the
	// paramsForm). The wider filter sets wrap within the subnav band (flex-wrap); no
	// report keeps its filters inline any more.
	// p28.23: render in the WIDE shell so a statement with many period columns
	// (e.g. the monthly statement of activities) uses the available horizontal
	// width instead of horizontally scrolling at the 60rem reading cap. The filter
	// controls still render in the second-level nav (SubNavControls="report").
	page := s.newWideShellPage(r, model)
	page.Shell.SubNavControls = "report"
	// p29.11: comparative statements (income_statement, program_statement) opt into the
	// FULL-viewport-width shell so their many period/program columns show without a
	// horizontal scroll; other reports keep the ordinary wide (100rem-capped) shell.
	page.Shell.FullWidth = model.FullWidth
	s.render(w, r, http.StatusOK, "report.tmpl", page)
}

// reportCSV handles GET /reports/{id}.csv (same ReportGroup Perm as the HTML page):
// it runs the report with the same param resolution and streams the Table as
// text/csv via reports.WriteCSV, with headers localized to the request language and
// a download filename of the report id. The CSV values are machine-plain (no
// grouping, ISO dates) and correctly escaped by encoding/csv.
func (s *server) reportCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	rep, ok := s.reportFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	params, _, err := s.resolveParams(ctx, u, rep, r.Form)
	if err != nil {
		s.serverError(w)
		return
	}

	table, err := rep.Run(ctx, reports.NewToolkit(s.store, params), params)
	if err != nil {
		s.serverError(w)
		return
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	// The reports CSV writer is i18n-free (it emits a text cell's raw string), so
	// resolve LABEL cells to localized TEXT cells here before handing it off; proper-
	// noun TEXT cells and money/date cells are untouched. Headers are localized by
	// the localizer passed to WriteCSV.
	localized := localizeLabelCells(table, lang)

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", "attachment; filename=\""+rep.ID+".csv\"")
	h.Set("Cache-Control", "no-store")

	// Owner request: a downloaded CSV opens in a spreadsheet with the report's title
	// and its date context (as-of, or from/to) at the top, so the file is
	// self-describing away from the app. Emit a small metadata preamble on its OWN
	// csv.Writer BEFORE the table -- its rows are 1-2 fields wide (ragged vs. the
	// table's N columns), which is fine because each writer tracks its own field
	// count, and we Flush() it before WriteCSV writes to the same ResponseWriter. The
	// preamble is handler-side (localized here) so the i18n-free reports.WriteCSV and
	// its goldens stay unchanged.
	writeReportCSVPreamble(w, rep, params, lang, dateFormatFor(u))

	// Best-effort: a write error mid-stream has already sent a 200 header, so there
	// is no clean way to signal it; the export is a read with no side effects.
	_ = reports.WriteCSV(w, localized, func(key string) string { return i18n.T(lang, key) }, exps)
}

// writeReportCSVPreamble streams the two-line metadata header (title, then the date
// context) plus a trailing blank row ahead of the report table. It keys on the
// report's ParamsSpec, not on whether a date happens to be set, so:
//   - AsOf report   -> ["As of", <asof>]
//   - Period report -> ["From", <from>, "To", <to>]
//   - both (rare)   -> both date rows
//   - neither (scope-only) -> title + blank only, no date row
//
// Dates are formatted in the user's date format (rule 10). Labels reuse the existing
// params catalog keys (reports.params.asof/from/to) so no new i18n keys are needed.
// Errors are ignored: the caller is a best-effort export whose 200 header is already
// committed.
func writeReportCSVPreamble(w http.ResponseWriter, rep reports.Report, params reports.Params, lang string, df money.DateFormat) {
	cw := csv.NewWriter(w)

	_ = cw.Write([]string{i18n.T(lang, rep.TitleKey)})

	if rep.ParamsSpec.AsOf && params.AsOf != "" {
		_ = cw.Write([]string{
			i18n.T(lang, "reports.params.asof"),
			money.FormatDate(parseISOForDisplay(params.AsOf), df),
		})
	}
	if rep.ParamsSpec.Period && (params.From != "" || params.To != "") {
		_ = cw.Write([]string{
			i18n.T(lang, "reports.params.from"),
			money.FormatDate(parseISOForDisplay(params.From), df),
			i18n.T(lang, "reports.params.to"),
			money.FormatDate(parseISOForDisplay(params.To), df),
		})
	}

	// Blank separator row between the metadata and the table.
	_ = cw.Write([]string{})

	cw.Flush()
}

// localizeLabelCells returns a copy of t with each LABEL cell resolved to a
// localized TEXT cell (so the i18n-free CSV writer emits the localized string);
// proper-noun TEXT cells and money/date cells are unchanged. This is the CSV
// counterpart of renderCell's CellLabel branch -- one place each renderer localizes.
func localizeLabelCells(t reports.Table, lang string) reports.Table {
	out := reports.Table{Columns: t.Columns}
	out.Rows = make([]reports.Row, len(t.Rows))
	for i, row := range t.Rows {
		nr := reports.Row{Indent: row.Indent, Kind: row.Kind}
		nr.Cells = make([]reports.Cell, len(row.Cells))
		for j, c := range row.Cells {
			if c.Kind == reports.CellLabel {
				c = reports.TextCell(i18n.T(lang, c.Text))
			}
			nr.Cells[j] = c
		}
		out.Rows[i] = nr
	}
	return out
}

// ---- small helpers --------------------------------------------------------

// first returns the first value for key in a parsed form, or "".
func first(q map[string][]string, key string) string {
	if vs := q[key]; len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// subExists reports whether id is one of the subsidiaries (a query scope override
// must name a real subsidiary, else the default stands -- no arbitrary scope).
func subExists(subs []subInfo, id int64) bool {
	for _, s := range subs {
		if s.ID == id {
			return true
		}
	}
	return false
}
