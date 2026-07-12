package web

// p15.3d drill-down RECONCILIATION + route tests. The KEY invariant: the drill for a
// report cell lists exactly the transactions whose signed NATIVE sum equals that
// cell's figure. These assert it two ways:
//
//   - store level (TestDrillReconcilesToToolkit): store.DrillSplits summed == the
//     toolkit's BalancesAsOf native figure for that (account, currency), over the
//     canonical fixture, at ROOT scope (descendant closure) -- including a MULTI-
//     currency account (FX Clearing) to prove the per-currency filter, and a
//     fund/program/class-narrowed drill.
//   - HTTP level (TestDrillRouteRendersRows, TestDrillReconciliationThroughRoute):
//     the mounted /reports/{id}/drill route lists rows linking to the txn editor/
//     history, echoes the reconciled figure, and 200s on a bare (empty) hit.
//
// Every oracle is the toolkit/store over the synthetic fixture (DATA RULE 11), never
// the drill's own output.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// drillFixtureApp builds an app over the synthetic fixture with an admin user
// (is_admin reaches every ReportGroup without a grant FK, so no report-group sync is
// needed) and the FX rate seam applied (the trial balance's converted column, though
// the drill itself is native-only). Returns the handler, store, session manager, the
// admin user id, and the fixture ids.
func drillFixtureApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager, int64, fixture.IDs) {
	t.Helper()
	fx := fixture.New(t)
	fx.ExtendRates(t)
	app := NewApp(Config{Version: "test"}, fx.DB, fx.Store)
	admin := mkUser(t, fx.Store, "admin", "none", true)
	return app.handler, fx.Store, app.sessions, admin, fx.IDs
}

// drillSum sums store.DrillSplits over f, the reconciliation quantity.
func drillSum(t *testing.T, st *store.Store, f store.DrillFilter) (int64, int) {
	t.Helper()
	rows, err := st.DrillSplits(context.Background(), f)
	if err != nil {
		t.Fatalf("DrillSplits(%+v): %v", f, err)
	}
	var sum int64
	for _, r := range rows {
		sum += r.Amount
	}
	return sum, len(rows)
}

// TestDrillReconcilesToToolkit is THE reconciliation test: for selected trial-balance
// cells over the fixture at ROOT scope, the signed native sum of store.DrillSplits
// equals the toolkit's BalancesAsOf native figure for that (account, currency). It
// covers a single-currency account, a MULTI-currency account (FX Clearing, whose MXN
// cell reconciles ONLY because the drill filters currency), and a fund/program/class-
// narrowed drill.
func TestDrillReconcilesToToolkit(t *testing.T) {
	fx := fixture.New(t)
	ctx := context.Background()
	st := fx.Store
	ids := fx.IDs
	asof := fx.Expected.AsOf

	// The toolkit oracle: per-account native balances at ROOT scope, as-of.
	tk := reports.NewToolkit(st, reports.Params{Scope: ids.Root, AsOf: asof})
	balances, err := tk.BalancesAsOf(ctx, reports.Scope{Sub: ids.Root}, asof, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("BalancesAsOf: %v", err)
	}
	native := func(acct int64, ccy string) int64 {
		for _, a := range balances[acct] {
			if a.Currency == ccy {
				return a.Minor
			}
		}
		t.Fatalf("no native balance for account %d %s", acct, ccy)
		return 0
	}

	// --- per-cell drill sums reconcile to the native figure.
	cells := []struct {
		name string
		acct int64
		ccy  string
	}{
		{"Checking MX / MXN (single-ccy)", ids.CheckingMX, "MXN"},
		{"FX Clearing / MXN (multi-ccy: currency filter matters)", ids.FXClearing, "MXN"},
		{"FX Clearing / USD (the OTHER currency of the same account)", ids.FXClearing, "USD"},
		{"Government Grants / MXN", ids.GovernmentGrants, "MXN"},
		{"Contributions / USD", ids.Contributions, "USD"},
	}
	for _, c := range cells {
		want := native(c.acct, c.ccy)
		got, n := drillSum(t, st, store.DrillFilter{
			Scope:     ids.Root,
			AccountID: c.acct,
			Currency:  c.ccy,
			AsOf:      asof,
		})
		if got != want {
			t.Errorf("%s: drill sum = %d, want %d (toolkit native figure)", c.name, got, want)
		}
		if n == 0 {
			t.Errorf("%s: drill returned no rows (figure %d)", c.name, want)
		}
	}

	// --- FX Clearing MXN must NOT accidentally pick up its USD splits: prove the
	// currency filter by checking the MXN drill sum differs from the (MXN+USD) naive
	// sum. Its MXN figure is 500,000; its USD figure is 974,000 -- distinct.
	mxn, _ := drillSum(t, st, store.DrillFilter{Scope: ids.Root, AccountID: ids.FXClearing, Currency: "MXN", AsOf: asof})
	usd, _ := drillSum(t, st, store.DrillFilter{Scope: ids.Root, AccountID: ids.FXClearing, Currency: "USD", AsOf: asof})
	if mxn == usd {
		t.Errorf("FX Clearing MXN (%d) == USD (%d): currency filter not applied", mxn, usd)
	}
	if mxn != native(ids.FXClearing, "MXN") {
		t.Errorf("FX Clearing MXN drill = %d, want %d", mxn, native(ids.FXClearing, "MXN"))
	}

	// --- PERIOD + NARROWED drills reconcile to INDEPENDENT toolkit oracles (not to
	// another DrillSplits call). Program Supplies carries MXN activity of 500,000 (two
	// splits: 300,000 Beca-Agua + 200,000 unrestricted, both Educacion / program
	// class) over the fixture window -- so a program-filtered drill reconciles to
	// ProgramActivity, a class-filtered drill to FunctionalMatrix, and a fund-filtered
	// drill to the hand-derived Beca-Agua leg. Each proves an optional filter AND the
	// From/To period date mode.
	from, to := fx.Expected.ActivityFrom, fx.Expected.ActivityTo
	prog := ids.Educacion
	class := "program"
	beca := ids.BecaAgua

	// PROGRAM oracle: ProgramActivity[Educacion][ProgramSupplies] MXN. Educacion is a
	// LEAF program, so the toolkit's tree rollup adds nothing below it -- the cell is
	// exactly the ProgramSupplies MXN activity tagged Educacion.
	progAct, err := tk.ProgramActivity(ctx, reports.Scope{Sub: ids.Root}, from, to, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("ProgramActivity: %v", err)
	}
	wantProg := curAmt(t, progAct[prog][ids.ProgramSupplies], "MXN")
	gotProg, nProg := drillSum(t, st, store.DrillFilter{
		Scope: ids.Root, AccountID: ids.ProgramSupplies, Currency: "MXN",
		From: from, To: to, ProgramID: &prog,
	})
	if gotProg != wantProg {
		t.Errorf("program-filtered drill = %d, want %d (ProgramActivity[Educacion][ProgramSupplies] MXN)", gotProg, wantProg)
	}
	if nProg == 0 {
		t.Errorf("program-filtered drill returned no rows")
	}

	// CLASS oracle: FunctionalMatrix[ProgramSupplies][program] MXN (a direct sum, no
	// rollup).
	fm, err := tk.FunctionalMatrix(ctx, reports.Scope{Sub: ids.Root}, from, to, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("FunctionalMatrix: %v", err)
	}
	wantClass := curAmt(t, fm[ids.ProgramSupplies][reports.Class(class)], "MXN")
	gotClass, _ := drillSum(t, st, store.DrillFilter{
		Scope: ids.Root, AccountID: ids.ProgramSupplies, Currency: "MXN",
		From: from, To: to, Class: &class,
	})
	if gotClass != wantClass {
		t.Errorf("class-filtered drill = %d, want %d (FunctionalMatrix[ProgramSupplies][program] MXN)", gotClass, wantClass)
	}

	// FUND oracle: no toolkit method filters by fund, so hand-derive from the fixture
	// (transactions.go: the ONE Beca-Agua-funded ProgramSupplies MXN split is +300,000
	// minor; the +200,000 leg is unrestricted). The fund filter must isolate exactly
	// the Beca-Agua leg.
	const wantFund int64 = 300_000
	gotFund, nFund := drillSum(t, st, store.DrillFilter{
		Scope: ids.Root, AccountID: ids.ProgramSupplies, Currency: "MXN",
		From: from, To: to, FundID: &beca,
	})
	if gotFund != wantFund {
		t.Errorf("fund-filtered drill = %d, want %d (hand-derived Beca-Agua ProgramSupplies MXN leg)", gotFund, wantFund)
	}
	if nFund == 0 {
		t.Errorf("fund-filtered drill returned no rows")
	}
}

// curAmt extracts the minor amount for ccy from a toolkit CurAmt slice (a report
// cell's per-currency amounts), failing the test if absent.
func curAmt(t *testing.T, amts []reports.CurAmt, ccy string) int64 {
	t.Helper()
	for _, a := range amts {
		if a.Currency == ccy {
			return a.Minor
		}
	}
	t.Fatalf("no %s amount in toolkit cell %v", ccy, amts)
	return 0
}

// TestDrillRouteRendersRows: the mounted drill route lists the drilled transactions,
// each linking to the txn editor + history (p12.4), and echoes the reconciled figure.
func TestDrillRouteRendersRows(t *testing.T) {
	h, _, sm, user, ids := drillFixtureApp(t)

	d := reports.Drill{
		Scope:      ids.Root,
		AccountIDs: []int64{ids.CheckingMX},
		Currency:   "MXN",
		Mode:       reports.DrillAsOf,
		AsOf:       "2026-06-30",
	}
	path := "/reports/" + reports.TrialBalanceReportID + "/drill?" + d.Encode()

	rec := asUser(t, h, sm, user, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drill route status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The drilled account name + a reconciled figure appear.
	if !strings.Contains(body, "Checking MX") {
		t.Errorf("drill page missing drilled account name; body:\n%s", body)
	}
	// A row links to the txn editor and history (p12.4).
	if !strings.Contains(body, "/edit") || !strings.Contains(body, "/history") {
		t.Errorf("drill rows missing editor/history links")
	}
	if !strings.Contains(body, `class="drill-row"`) {
		t.Errorf("drill page rendered no transaction rows")
	}
	// The figure header carries the reconciled MXN sum (39,500,000 minor = 395,000.00).
	if !strings.Contains(body, "MXN 395,000.00") {
		t.Errorf("drill page missing reconciled figure MXN 395,000.00; body:\n%s", body)
	}
}

// TestDrillRouteBareHit: a drill route hit with NO query (the permission-matrix's
// concrete /reports/{id}/drill) decodes to an empty drill and 200s with an empty
// list (not a 500), so the route is matrix-reachable.
func TestDrillRouteBareHit(t *testing.T) {
	h, _, sm, user, _ := drillFixtureApp(t)
	rec := asUser(t, h, sm, user, http.MethodGet, "/reports/"+reports.TrialBalanceReportID+"/drill", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare drill hit status = %d, want 200", rec.Code)
	}
}

// TestReportPageEmitsDrillLinks: the trial-balance report page renders its native
// money cells as DRILL links (the retrofit), pointing at the drill route.
func TestReportPageEmitsDrillLinks(t *testing.T) {
	h, _, sm, user, _ := drillFixtureApp(t)
	rec := asUser(t, h, sm, user, http.MethodGet, "/reports/"+reports.TrialBalanceReportID+"?scope=1&asof=2026-06-30", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("report page status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="report-drill-link"`) {
		t.Errorf("trial-balance page has no drill links (retrofit missing)")
	}
	if !strings.Contains(body, "/reports/trial_balance/drill?") {
		t.Errorf("drill link does not point at the drill route")
	}
}

// TestBalanceSheetDrillReconciles: the balance-sheet report attaches a Drill to its
// asset/liability ACCOUNT cells; the drill for a cell lists exactly the transactions
// whose signed NATIVE sum equals that account's native figure (p15.3d reconciliation,
// against the toolkit's BalancesAsOf oracle). This runs the REAL balance-sheet report,
// pulls the Drill off a drillable cell, and reconciles it -- so the report's actual
// drill wiring (not a hand-built Drill) is what is tested.
func TestBalanceSheetDrillReconciles(t *testing.T) {
	fx := fixture.New(t)
	ctx := context.Background()
	st := fx.Store
	ids := fx.IDs
	asof := fx.Expected.AsOf

	// Native toolkit oracle at root scope.
	tk := reports.NewToolkit(st, reports.Params{Scope: ids.Root, AsOf: asof})
	balances, err := tk.BalancesAsOf(ctx, reports.Scope{Sub: ids.Root}, asof, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("BalancesAsOf: %v", err)
	}

	// Run the real balance sheet (native mode: no target, so each account cell drills
	// its single currency directly). Detail=currency exposes a per-currency drill on
	// every native cell.
	rep, ok := reports.Default().Get(reports.BalanceSheetReportID)
	if !ok {
		t.Fatalf("balance-sheet report not registered")
	}
	p := reports.Params{Scope: ids.Root, AsOf: asof, Lang: "en", Detail: "currency"}
	table, err := rep.Run(ctx, reports.NewToolkit(st, p), p)
	if err != nil {
		t.Fatalf("run balance sheet: %v", err)
	}

	// Collect every drillable cell's Drill and reconcile it against the toolkit native
	// figure for (account, currency). Assets are stored net-debit positive; the drill
	// sum equals the STORED native figure (the balance sheet's sign-normalization of
	// liabilities does not change the drilled splits -- the drill lists the raw splits,
	// whose sum is the stored net-debit balance).
	nativeFor := func(acct int64, ccy string) (int64, bool) {
		for _, a := range balances[acct] {
			if a.Currency == ccy {
				return a.Minor, true
			}
		}
		return 0, false
	}

	drills := 0
	for _, row := range table.Rows {
		for _, c := range row.Cells {
			if c.Drill == nil {
				continue
			}
			d := c.Drill
			if len(d.AccountIDs) != 1 {
				continue
			}
			acct := d.AccountIDs[0]
			want, ok := nativeFor(acct, d.Currency)
			if !ok {
				t.Errorf("drill for account %d %s has no toolkit native figure", acct, d.Currency)
				continue
			}
			got, n := drillSum(t, st, store.DrillFilter{
				Scope:     d.Scope,
				AccountID: acct,
				Currency:  d.Currency,
				AsOf:      d.AsOf,
			})
			if got != want {
				t.Errorf("balance-sheet drill account %d %s: sum %d, want %d (toolkit native)", acct, d.Currency, got, want)
			}
			if n == 0 {
				t.Errorf("balance-sheet drill account %d %s returned no rows", acct, d.Currency)
			}
			drills++
		}
	}
	if drills == 0 {
		t.Fatalf("balance sheet emitted no drillable cells")
	}
	// Sanity: Checking MX MXN is among the drilled accounts and reconciles to 39,500,000.
	if got, _ := drillSum(t, st, store.DrillFilter{Scope: ids.Root, AccountID: ids.CheckingMX, Currency: "MXN", AsOf: asof}); got != 39_500_000 {
		t.Errorf("Checking MX drill = %d, want 39,500,000", got)
	}
}

// TestIncomeStatementDrillReconciles: every drillable income-statement cell (a single-
// currency R/E leaf, DrillPeriod over its column's sub-period range) lists exactly the
// transactions whose signed NATIVE sum equals the toolkit's NATIVE Activity figure for
// that (account, currency) over that same range -- the p15.5 flow-cell reconciliation.
// The report cell was CONVERTED (txn-date, D12) but the drill lists NATIVE splits; the
// invariant holds against the pre-conversion native figure (drill.go). Quarterly
// comparative columns exercise several distinct sub-period ranges.
func TestIncomeStatementDrillReconciles(t *testing.T) {
	fx := fixture.New(t)
	fx.ExtendRates(t)
	ctx := context.Background()
	st := fx.Store
	ids := fx.IDs
	from, to := fx.Expected.ActivityFrom, fx.Expected.ActivityTo

	rep, ok := reports.Default().Get(reports.IncomeStatementReportID)
	if !ok {
		t.Fatalf("income-statement report not registered")
	}
	p := reports.Params{
		Scope: ids.Root, From: from, To: to,
		Granularity: reports.GranQuarter, TargetCurrency: "USD", Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(st, p), p)
	if err != nil {
		t.Fatalf("run income statement: %v", err)
	}

	// nativeActivity returns the toolkit's NATIVE Activity figure for (account, currency)
	// over [pf,pt] at root scope -- the reconciliation oracle (never the drill's output).
	tk := reports.NewToolkit(st, p)
	nativeActivity := func(acct int64, ccy, pf, pt string) (int64, bool) {
		act, err := tk.Activity(ctx, reports.Scope{Sub: ids.Root}, pf, pt, reports.ConvertOpts{Mode: reports.RateNone})
		if err != nil {
			t.Fatalf("Activity native: %v", err)
		}
		for _, a := range act[acct] {
			if a.Currency == ccy {
				return a.Minor, true
			}
		}
		return 0, false // no activity in this sub-period => figure 0
	}

	drills, nonzero := 0, 0
	for _, row := range table.Rows {
		for _, c := range row.Cells {
			if c.Drill == nil {
				continue
			}
			d := c.Drill
			if len(d.AccountIDs) != 1 || d.Mode != reports.DrillPeriod {
				t.Errorf("income-statement drill not a single-account period drill: %+v", d)
				continue
			}
			acct := d.AccountIDs[0]
			want, _ := nativeActivity(acct, d.Currency, d.From, d.To)
			got, n := drillSum(t, st, store.DrillFilter{
				Scope:     d.Scope,
				AccountID: acct,
				Currency:  d.Currency,
				From:      d.From,
				To:        d.To,
			})
			if got != want {
				t.Errorf("income-statement drill account %d %s [%s..%s]: sum %d, want %d (toolkit native)",
					acct, d.Currency, d.From, d.To, got, want)
			}
			if want != 0 && n == 0 {
				t.Errorf("income-statement drill account %d %s [%s..%s] returned no rows for a nonzero figure",
					acct, d.Currency, d.From, d.To)
			}
			drills++
			if want != 0 {
				nonzero++
			}
		}
	}
	if drills == 0 {
		t.Fatalf("income statement emitted no drillable cells")
	}
	if nonzero == 0 {
		t.Fatalf("income statement emitted no NONZERO drillable cells (reconciliation vacuous)")
	}

	// Concrete anchor: Food Purchases (MXN) whole-range native activity is 360,000 (the
	// fixture R/E oracle), reconciled through the drill over the full span.
	if got, _ := drillSum(t, st, store.DrillFilter{Scope: ids.Root, AccountID: ids.FoodPurchases, Currency: "MXN", From: from, To: to}); got != 360_000 {
		t.Errorf("Food Purchases whole-range drill = %d, want 360,000", got)
	}
}
