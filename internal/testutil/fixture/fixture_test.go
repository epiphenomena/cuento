package fixture_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"cuento/internal/ids"
	"cuento/internal/ledger"
	"cuento/internal/testutil/fixture"
)

// parseInstant parses the RFC3339Nano timestamps the store stores in changes.at
// / valid_from (store.write formats with time.RFC3339Nano).
func parseInstant(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// TestFixtureIntegrity asserts the canonical fixture is CLEAN: ledger.Check
// returns ZERO Error-severity violations, and exactly the expected WARNING set
// -- at least Z19 for the one deliberately-unmapped active R/E leaf (Event
// Income). Any unexpected error (or an unexpected warning rule) fails.
func TestFixtureIntegrity(t *testing.T) {
	f := fixture.New(t)

	vs, err := ledger.Check(context.Background(), f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}

	var errs, warns []ledger.Violation
	for _, v := range vs {
		switch v.Severity {
		case ledger.Error:
			errs = append(errs, v)
		case ledger.Warning:
			warns = append(warns, v)
		}
	}

	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("unexpected Error violation: %s: %s", e.Rule, e.Detail)
		}
	}

	// The ONLY warnings the fixture intends: Z19 naming Event Income. (Z17
	// intercompany nets zero, Z18 no restricted fund goes negative -- both must
	// stay silent.)
	warnRules := map[string]int{}
	for _, w := range warns {
		warnRules[w.Rule]++
	}
	if warnRules["Z19"] == 0 {
		t.Errorf("expected a Z19 warning (unmapped active R/E leaf); got warnings %v", warnRules)
	}
	for rule := range warnRules {
		if rule != "Z19" {
			t.Errorf("unexpected warning rule %s (fixture should only trip Z19)", rule)
		}
	}

	// Z19's detail must name the unmapped Event Income account id.
	found := false
	want := itoa(int64(f.Expected.UnmappedRevenueLeaf))
	for _, w := range warns {
		if w.Rule == "Z19" && contains(w.Detail, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("Z19 warning does not name Event Income (id %d); warnings: %+v", f.Expected.UnmappedRevenueLeaf, warns)
	}
}

// TestFixtureKnownAggregates asserts the exported hand-computed constants match
// what the p08.4 balance queries return at the specified dates, at ROOT scope
// (full consolidation), native currency:
//   - trial balance zero per subsidiary scope per currency;
//   - specific account balances;
//   - fund balances (incl. unrestricted);
//   - functional-matrix cells;
//   - per-program activity;
//   - 990 line rollups incl. the Unmapped bucket;
//   - intercompany net zero per currency.
func TestFixtureKnownAggregates(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	s := f.Store
	exp := f.Expected

	// --- trial balance zero per subsidiary scope per currency ---
	for scope, ccys := range exp.TrialBalanceCurrencies {
		bals, err := s.SubtreeBalancesAsOf(ctx, exp.AsOf, scope)
		if err != nil {
			t.Fatalf("SubtreeBalancesAsOf(scope %d): %v", scope, err)
		}
		byCcy := map[string]int64{}
		for _, b := range bals {
			byCcy[b.Currency] += b.Amount
		}
		for _, ccy := range ccys {
			if byCcy[ccy] != 0 {
				t.Errorf("trial balance scope %d %s = %d, want 0", scope, ccy, byCcy[ccy])
			}
		}
	}

	// --- specific account balances (root scope) ---
	bals, err := s.SubtreeBalancesAsOf(ctx, exp.AsOf, rootScope(f))
	if err != nil {
		t.Fatalf("SubtreeBalancesAsOf(root): %v", err)
	}
	gotAcct := map[key]int64{}
	for _, b := range bals {
		gotAcct[key{b.AccountID, b.Currency}] = b.Amount
	}
	for _, want := range exp.AccountBalances {
		got, ok := gotAcct[key{want.Account, want.Currency}]
		if !ok {
			t.Errorf("account %d/%s missing from balances", want.Account, want.Currency)
			continue
		}
		if got != want.Amount {
			t.Errorf("account %d/%s balance = %d, want %d", want.Account, want.Currency, got, want.Amount)
		}
	}

	// --- fund balances incl. unrestricted (root scope) ---
	fbals, err := s.FundBalancesAsOf(ctx, exp.AsOf, rootScope(f))
	if err != nil {
		t.Fatalf("FundBalancesAsOf(root): %v", err)
	}
	gotFund := map[fundkey]int64{}
	for _, b := range fbals {
		gotFund[fundkey{b.FundID, b.Currency}] = b.Amount
	}
	for _, want := range exp.FundBalances {
		got, ok := gotFund[fundkey{want.Fund, want.Currency}]
		if !ok {
			t.Errorf("fund %d/%s missing from fund balances", want.Fund, want.Currency)
			continue
		}
		if got != want.Amount {
			t.Errorf("fund %d/%s balance = %d, want %d", want.Fund, want.Currency, got, want.Amount)
		}
	}

	// --- functional matrix cells (root scope, whole window) ---
	fcells, err := s.FunctionalActivity(ctx, exp.ActivityFrom, exp.ActivityTo, rootScope(f))
	if err != nil {
		t.Fatalf("FunctionalActivity(root): %v", err)
	}
	gotFunc := map[fkey]int64{}
	for _, c := range fcells {
		gotFunc[fkey{c.AccountID, c.FunctionalClass, c.Currency}] = c.Amount
	}
	if len(gotFunc) != len(exp.Functional) {
		t.Errorf("functional cell count = %d, want %d", len(gotFunc), len(exp.Functional))
	}
	for _, want := range exp.Functional {
		got, ok := gotFunc[fkey{want.Account, want.Class, want.Currency}]
		if !ok {
			t.Errorf("functional %d/%s/%s missing", want.Account, want.Class, want.Currency)
			continue
		}
		if got != want.Amount {
			t.Errorf("functional %d/%s/%s = %d, want %d", want.Account, want.Class, want.Currency, got, want.Amount)
		}
	}

	// --- per-program activity (root scope, whole window) ---
	pcells, err := s.ProgramActivity(ctx, exp.ActivityFrom, exp.ActivityTo, rootScope(f))
	if err != nil {
		t.Fatalf("ProgramActivity(root): %v", err)
	}
	gotProg := map[pkey]int64{}
	for _, c := range pcells {
		gotProg[pkey{c.ProgramID, c.AccountID, c.Currency}] = c.Amount
	}
	if len(gotProg) != len(exp.Program) {
		t.Errorf("program cell count = %d, want %d", len(gotProg), len(exp.Program))
	}
	for _, want := range exp.Program {
		got, ok := gotProg[pkey{want.Program, want.Account, want.Currency}]
		if !ok {
			t.Errorf("program %d/acct %d/%s missing", want.Program, want.Account, want.Currency)
			continue
		}
		if got != want.Amount {
			t.Errorf("program %d/acct %d/%s = %d, want %d", want.Program, want.Account, want.Currency, got, want.Amount)
		}
	}

	// --- 990 line rollups incl. Unmapped bucket ---
	assert990Rollup(t, f)

	// --- intercompany net zero per currency (root scope) ---
	// Sum the intercompany-flagged accounts' balances per currency; each must be
	// zero (D19). Due from RV Mexico (+) and Due to RV Internacional (-) net out.
	interco := map[string]int64{}
	for _, b := range bals {
		if b.AccountID == f.IDs.DueFromMX || b.AccountID == f.IDs.DueToIntl {
			interco[b.Currency] += b.Amount
		}
	}
	for _, ccy := range exp.IntercompanyNetCurrencies {
		if interco[ccy] != 0 {
			t.Errorf("intercompany net %s = %d, want 0", ccy, interco[ccy])
		}
	}

	// --- twice-edited txn: the middle state is recoverable as-of EditedMidAsOf ---
	// At edit 1's instant the supply expense is Beca Agua-funded, program
	// Educacion (the middle state); the FINAL state is unrestricted, General. This
	// proves EditedMidAsOf lets an as-of test pick the middle T (D4/D5).
	mid, err := parseInstant(exp.EditedMidAsOf)
	if err != nil {
		t.Fatalf("parse EditedMidAsOf %q: %v", exp.EditedMidAsOf, err)
	}
	st, err := s.TransactionAsOf(ctx, f.IDs.EditedTxn, mid)
	if err != nil {
		t.Fatalf("TransactionAsOf(edited, mid): %v", err)
	}
	if !st.Present {
		t.Fatalf("edited txn not present at middle instant")
	}
	restricted := false
	for _, sp := range st.Splits {
		if sp.AccountID == f.IDs.ProgramSupplies {
			if sp.FundID.Valid && sp.FundID.Int64 == int64(f.IDs.BecaAgua) &&
				sp.ProgramID.Valid && sp.ProgramID.Int64 == int64(f.IDs.Educacion) {
				restricted = true
			}
		}
	}
	if !restricted {
		t.Errorf("middle state of edited txn is not the restricted Beca Agua/Educacion state; splits=%+v", st.Splits)
	}
}

// assert990Rollup rolls the fixture's R/E leaf activity to effective 990 codes
// (own or nearest ancestor, D25) and compares to the expected rollup incl. the
// Unmapped bucket. It uses the store's Effective990Codes to resolve inheritance,
// exactly as the p15 990 reports will.
func assert990Rollup(t *testing.T, f *fixture.Fixture) {
	t.Helper()
	ctx := context.Background()
	exp := f.Expected

	eff, err := f.Store.Effective990Codes(ctx)
	if err != nil {
		t.Fatalf("Effective990Codes: %v", err)
	}

	// Roll program-activity R/E leaves (covers every revenue + expense leaf with
	// activity) to their effective code per currency; unmapped -> "".
	pcells, err := f.Store.ProgramActivity(ctx, exp.ActivityFrom, exp.ActivityTo, rootScope(f))
	if err != nil {
		t.Fatalf("ProgramActivity(root): %v", err)
	}
	got := map[ckey]int64{}
	for _, c := range pcells {
		code := eff[c.AccountID] // "" when unmapped
		got[ckey{code, c.Currency}] += c.Amount
	}

	want := map[ckey]int64{}
	for _, l := range exp.Rollup990 {
		want[ckey{l.Code, l.Currency}] = l.Amount
	}
	if len(got) != len(want) {
		t.Errorf("990 rollup cell count = %d, want %d (got %v)", len(got), len(want), sortedCkeys(got))
	}
	for k, w := range want {
		if got[k] != w {
			label := k.code
			if label == "" {
				label = "(Unmapped)"
			}
			t.Errorf("990 rollup %s/%s = %d, want %d", label, k.currency, got[k], w)
		}
	}
	// The Unmapped bucket must exist and be non-empty (the whole point of Z19).
	if got[ckey{"", "USD"}] == 0 {
		t.Errorf("990 rollup Unmapped/USD is empty; expected Event Income activity")
	}
}

// rootScope returns the root subsidiary id (full consolidation).
func rootScope(f *fixture.Fixture) ids.SubsidiaryID { return f.IDs.Root }

type key struct {
	id  ids.AccountID
	ccy string
}

type fundkey struct {
	id  ids.FundID
	ccy string
}

type fkey struct {
	acct  ids.AccountID
	class string
	ccy   string
}

type pkey struct {
	prog ids.ProgramID
	acct ids.AccountID
	ccy  string
}

type ckey struct {
	code     string
	currency string
}

func sortedCkeys(m map[ckey]int64) []ckey {
	out := make([]ckey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].code != out[j].code {
			return out[i].code < out[j].code
		}
		return out[i].currency < out[j].currency
	})
	return out
}

// contains / itoa are tiny local helpers (avoid importing strconv/strings for a
// single use each in the assertion path).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
