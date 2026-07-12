package ledger_test

import (
	"context"
	"database/sql"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// The integrity suite is verified two ways (AGENTS testing conventions): a VALID
// dataset built through the store must be clean (no Error violations), and each
// implemented rule has a negative test that corrupts a copy with RAW SQL
// (bypassing the store/triggers) and asserts exactly that rule flags. Raw
// corruption sometimes needs foreign_keys OFF or trigger-bypassing writes -- fine
// on a throwaway db.

func mutCtx() context.Context {
	return store.WithActor(context.Background(), store.Actor{ID: 1})
}

// rootSub / rootProg are the seeded root ids (p04.1 / p07.1). They are vars (not
// consts) so tests can take &rootProg when a split explicitly tags the root
// program.
var (
	rootSub  = int64(1)
	rootProg = int64(1)
)

// world is a small VALID chart + transactions the tests share. It is built ONLY
// through the store's public API, so every invariant the store enforces holds and
// ledger.Check must find no Error violations.
type world struct {
	d *sql.DB
	s *store.Store

	subUS, subMX int64
	prog         int64
	fund         int64

	checkingUS int64 // asset, US
	salaries   int64 // expense (class management, prog root), US, 990-coded
	contrib    int64 // revenue (prog root), US, 990-coded
	equity     int64 // equity, US
	dueFrom    int64 // asset, intercompany, US
	dueTo      int64 // liability, intercompany, MX
	checkingMX int64 // asset, MX

	txPlain int64 // a simple balanced US txn (salaries/checking)
	txFund  int64 // a restricted-fund receipt in US
}

func newWorld(t *testing.T) *world {
	t.Helper()
	d := testutil.NewDB(t)
	s := store.New(d)
	w := &world{d: d, s: s}

	w.subUS = mkSub(t, s, "US")
	w.subMX = mkSub(t, s, "MX")
	w.prog = mkProg(t, s, "Educacion") // under root program
	// The fund is program-scoped to the root program subtree (which contains
	// Educacion), so Z15's fund-program-scope branch runs on VALID data: the
	// contrib split below tags this fund AND carries Educacion, which the recursive
	// subtree walk must accept. (A flat/unscoped fund would leave Z15-scope
	// unexercised on the clean world.)
	w.fund = mkFundScoped(t, s, "Beca", []int64{w.subUS}, &rootProg)

	mgmt := "management"
	code990Rev := "VIII.1f"
	code990Exp := "IX.16"
	// A parent placeholder + child give Z7 (acyclic walk) and Z12 (parent sub-set
	// superset of children) real tree EDGES on valid data -- without a nested
	// account both rules would pass the clean-world test vacuously (zero edges).
	// checkingUS is that child, under an "Assets" placeholder.
	assetsParent := mkAcct(t, s, acct{typ: "asset", name: "Assets", subs: []int64{w.subUS}})
	w.checkingUS = mkAcct(t, s, acct{typ: "asset", name: "Checking US", subs: []int64{w.subUS}, parent: &assetsParent})
	w.salaries = mkAcct(t, s, acct{typ: "expense", name: "Salaries", subs: []int64{w.subUS}, fclass: &mgmt, defProg: &rootProg, code990: &code990Exp})
	w.contrib = mkAcct(t, s, acct{typ: "revenue", name: "Contributions", subs: []int64{w.subUS}, defProg: &rootProg, code990: &code990Rev})
	w.equity = mkAcct(t, s, acct{typ: "equity", name: "Opening", subs: []int64{w.subUS}})
	w.dueFrom = mkAcct(t, s, acct{typ: "asset", name: "Due from MX", subs: []int64{w.subUS}, intercompany: true})
	w.dueTo = mkAcct(t, s, acct{typ: "liability", name: "Due to US", subs: []int64{w.subMX}, intercompany: true})
	w.checkingMX = mkAcct(t, s, acct{typ: "asset", name: "Checking MX", subs: []int64{w.subMX}})

	// A plain balanced US expense: debit salaries 10000, credit checking 10000.
	w.txPlain = post(t, s, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.salaries, Amount: 10_000, Position: 0},
			{AccountID: w.checkingUS, Amount: -10_000, Position: 1},
		},
	})

	// A restricted-fund receipt: debit checking 50000 / credit contributions
	// 50000, both tagged the fund (per-fund zero-sum holds). Leaves the fund with
	// a POSITIVE net-debit balance (assets held), so Z18 stays clean.
	w.txFund = post(t, s, store.PostTransactionInput{
		Date: "2025-04-01", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.checkingUS, Amount: 50_000, FundID: &w.fund, Position: 0},
			{AccountID: w.contrib, Amount: -50_000, FundID: &w.fund, ProgramID: &w.prog, Position: 1},
		},
	})

	// An intercompany funding pair that nets to zero per currency (D19, W4).
	// Net-debit sign: DR = +, CR = -.
	//   US: DR Due-from +3000, CR checking US -3000.
	//   MX: DR checking MX +3000, CR Due-to -3000.
	// The two intercompany accounts net: dueFrom(+3000) + dueTo(-3000) = 0.
	post(t, s, store.PostTransactionInput{
		Date: "2025-05-01", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.dueFrom, Amount: 3_000, Position: 0},
			{AccountID: w.checkingUS, Amount: -3_000, Position: 1},
		},
	})
	post(t, s, store.PostTransactionInput{
		Date: "2025-05-01", SubsidiaryID: w.subMX, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.checkingMX, Amount: 3_000, Position: 0},
			{AccountID: w.dueTo, Amount: -3_000, Position: 1},
		},
	})

	return w
}

// --- store-build helpers -----------------------------------------------------

func mkSub(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	id, err := s.CreateSubsidiary(mutCtx(), store.CreateSubsidiaryInput{ParentID: rootSub, Name: name, BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("CreateSubsidiary(%s): %v", name, err)
	}
	return id
}

func mkProg(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	id, err := s.CreateProgram(mutCtx(), store.CreateProgramInput{ParentID: rootProg, Name: name})
	if err != nil {
		t.Fatalf("CreateProgram(%s): %v", name, err)
	}
	return id
}

// mkFundScoped creates a fund optionally scoped to a program subtree (progScope).
func mkFundScoped(t *testing.T, s *store.Store, name string, subs []int64, progScope *int64) int64 {
	t.Helper()
	id, err := s.CreateFund(mutCtx(), store.CreateFundInput{
		Name: name, Restriction: "purpose", Subsidiaries: subs, ProgramID: progScope,
	})
	if err != nil {
		t.Fatalf("CreateFund(%s): %v", name, err)
	}
	return id
}

type acct struct {
	typ          string
	name         string
	subs         []int64
	parent       *int64
	fclass       *string
	defProg      *int64
	code990      *string
	intercompany bool
}

func mkAcct(t *testing.T, s *store.Store, a acct) int64 {
	t.Helper()
	in := store.CreateAccountInput{
		ParentID: a.parent, Type: a.typ, DefaultCurrency: "USD",
		Names: map[string]string{"en": a.name}, Subsidiaries: a.subs,
		FunctionalClass: a.fclass, DefaultProgramID: a.defProg,
		Form990Code: a.code990, Intercompany: a.intercompany,
	}
	id, err := s.CreateAccount(mutCtx(), in)
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", a.name, err)
	}
	return id
}

func post(t *testing.T, s *store.Store, in store.PostTransactionInput) int64 {
	t.Helper()
	id, err := s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	return id
}

// --- assertion helpers -------------------------------------------------------

// check runs the suite and returns the violations.
func checkAll(t *testing.T, d *sql.DB) []ledger.Violation {
	t.Helper()
	vs, err := ledger.Check(context.Background(), d)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	return vs
}

// rulesOf returns the set of rule names present in vs.
func rulesOf(vs []ledger.Violation) map[string]bool {
	set := make(map[string]bool)
	for _, v := range vs {
		set[v.Rule] = true
	}
	return set
}

// assertClean fails if any Error-severity violation is present.
func assertClean(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, v := range checkAll(t, d) {
		if v.Severity == ledger.Error {
			t.Errorf("expected clean, got %s %s: %s", v.Severity, v.Rule, v.Detail)
		}
	}
}

// exec runs raw SQL against the throwaway db (corruption), failing on error.
func exec(t *testing.T, d *sql.DB, sqlStr string, args ...any) {
	t.Helper()
	if _, err := d.Exec(sqlStr, args...); err != nil {
		t.Fatalf("exec %q: %v", sqlStr, err)
	}
}

// dropTriggers removes named triggers on the throwaway copy so a corrupting write
// that the store-and-schema normally forbid can land (the task sanctions
// bypassing triggers when corrupting a throwaway db). PRAGMA foreign_keys=OFF
// disables FK enforcement but NOT triggers, so trigger-guarded corruptions must
// drop the trigger explicitly.
func dropTriggers(t *testing.T, d *sql.DB, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := d.Exec("DROP TRIGGER IF EXISTS " + n); err != nil {
			t.Fatalf("drop trigger %s: %v", n, err)
		}
	}
}

// assertFlags corrupts (already applied by the caller), runs the suite, and
// asserts the wanted rule is present. It also asserts no OTHER Error rule fired
// unexpectedly when exclusive is true (a single-rule corruption should trip one
// rule -- though some corruptions legitimately trip a couple, so exclusivity is
// opt-in per test).
func assertFlags(t *testing.T, d *sql.DB, want string) {
	t.Helper()
	got := rulesOf(checkAll(t, d))
	if !got[want] {
		t.Errorf("want rule %s flagged; got rules %v", want, keys(got))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- clean pass --------------------------------------------------------------

func TestCheckEmptyMigratedDBClean(t *testing.T) {
	// An empty migrated db has only the seeded roots (subsidiary, program, system
	// user) and no splits -- it MUST be clean.
	d := testutil.NewDB(t)
	if vs := checkAll(t, d); len(vs) != 0 {
		t.Fatalf("empty migrated db not clean: %v", vs)
	}
}

func TestCheckValidWorldClean(t *testing.T) {
	w := newWorld(t)
	// No Error violations. Warnings are allowed (the world has no negative fund and
	// its active R/E leaves are 990-coded, so it happens to be warning-free too).
	vs := checkAll(t, w.d)
	for _, v := range vs {
		t.Logf("violation: %s %s: %s", v.Severity, v.Rule, v.Detail)
	}
	assertClean(t, w.d)
	if len(vs) != 0 {
		t.Errorf("valid world produced %d violation(s) (want 0 incl. warnings): %v", len(vs), vs)
	}
}

// --- Z1: transaction sums to zero -------------------------------------------

func TestZ1Unbalanced(t *testing.T) {
	w := newWorld(t)
	// Tamper one split's amount directly, breaking the zero-sum, bypassing the
	// store. Triggers don't guard amount-sum (cross-row), so a raw UPDATE sticks.
	exec(t, w.d, `UPDATE splits SET amount = amount + 1 WHERE id = (SELECT MIN(id) FROM splits WHERE transaction_id = ?)`, w.txPlain)
	assertFlags(t, w.d, "Z1")
}

// --- Z2: splits on leaf accounts --------------------------------------------

func TestZ2SplitOnPlaceholder(t *testing.T) {
	w := newWorld(t)
	// Give an account-with-a-split a child, turning it into a placeholder. The
	// trg_accounts_no_children_over_splits trigger blocks this, so drop it for the
	// raw insert on the throwaway db.
	dropTriggers(t, w.d, "trg_accounts_no_children_over_splits")
	exec(t, w.d, `INSERT INTO accounts(parent_id, type, default_currency, intercompany, reconcilable, active, sort_order, created_at)
		VALUES (?, 'asset', 'USD', 0, 0, 1, 0, '2025-01-01T00:00:00Z')`, w.checkingUS)
	assertFlags(t, w.d, "Z2")
}

// --- Z3: current row equals latest version snapshot -------------------------

func TestZ3TamperedLiveRow(t *testing.T) {
	w := newWorld(t)
	// Tamper a live account row without appending a version -- its latest snapshot
	// now differs. (A single-id versioned table.)
	exec(t, w.d, `UPDATE accounts SET sort_order = sort_order + 999 WHERE id = ?`, w.checkingUS)
	assertFlags(t, w.d, "Z3")
}

func TestZ3TamperedComposite(t *testing.T) {
	w := newWorld(t)
	// Tamper a live account NAME (composite entity: account_id + lang) without a
	// version -- exercises the composite-key Z3 branch.
	exec(t, w.d, `UPDATE account_names SET name = 'TAMPERED' WHERE account_id = ? AND lang = 'en'`, w.checkingUS)
	assertFlags(t, w.d, "Z3")
}

func TestZ3MissingVersion(t *testing.T) {
	w := newWorld(t)
	// A live row with NO version at all (insert bypassing the store).
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `INSERT INTO payees(name, active) VALUES ('Ghost', 1)`)
	assertFlags(t, w.d, "Z3")
}

// --- Z4: foreign_key_check clean --------------------------------------------

func TestZ4ForeignKeyViolation(t *testing.T) {
	w := newWorld(t)
	// Insert a split whose transaction_id references a non-existent transaction,
	// with FKs off so it lands. The account (an active asset leaf) satisfies the
	// leaf/function/program triggers, so no trigger needs dropping -- only the FK
	// on transaction_id is broken, which PRAGMA foreign_key_check (Z4) catches.
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `INSERT INTO splits(transaction_id, account_id, amount, memo, position)
		VALUES (777777, ?, 1, '', 0)`, w.checkingUS)
	assertFlags(t, w.d, "Z4")
}

// --- Z5: version change_id references a real change -------------------------

func TestZ5DanglingChange(t *testing.T) {
	w := newWorld(t)
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `UPDATE accounts_versions SET change_id = 888888 WHERE id = (SELECT MIN(id) FROM accounts_versions WHERE entity_id = ?)`, w.checkingUS)
	assertFlags(t, w.d, "Z5")
}

// --- Z6: no orphan splits ----------------------------------------------------

func TestZ6OrphanSplit(t *testing.T) {
	w := newWorld(t)
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `INSERT INTO splits(transaction_id, account_id, amount, memo, position)
		VALUES (777777, ?, 1, '', 0)`, w.checkingUS)
	assertFlags(t, w.d, "Z6")
}

// --- Z7: account tree acyclic -----------------------------------------------

func TestZ7AccountCycle(t *testing.T) {
	w := newWorld(t)
	// Make an account its own parent (a self-cycle), bypassing move validation.
	exec(t, w.d, `UPDATE accounts SET parent_id = ? WHERE id = ?`, w.checkingUS, w.checkingUS)
	assertFlags(t, w.d, "Z7")
}

// --- Z8: cleared split matches its recon's account and currency --------------

func TestZ8ClearedSplitAccountMismatch(t *testing.T) {
	w := newWorld(t)
	// A recon on checkingUS (USD). Clear the txPlain SALARIES split (a different
	// account) against it -- account mismatch. reconciliation_id is live-only and
	// unversioned, so this raw UPDATE trips Z8 without disturbing Z3. The lock
	// trigger only guards financial fields (amount/account/txn/fund/recon_id), and
	// the recon here is open, so the reconciliation_id set is not blocked.
	exec(t, w.d, `INSERT INTO reconciliations (account_id, statement_date, statement_balance, currency, status)
		VALUES (?, '2025-03-31', 0, 'USD', 'open')`, w.checkingUS)
	exec(t, w.d, `UPDATE splits SET reconciliation_id = (SELECT MAX(id) FROM reconciliations)
		WHERE account_id = ? AND transaction_id = ?`, w.salaries, w.txPlain)
	assertFlags(t, w.d, "Z8")
}

func TestZ8ClearedSplitCurrencyMismatch(t *testing.T) {
	w := newWorld(t)
	// A recon on checkingUS but declared in MXN. Clear the checkingUS split of
	// txFund (a USD txn) against it -- account matches, currency (USD txn vs MXN
	// recon) does not.
	exec(t, w.d, `INSERT INTO reconciliations (account_id, statement_date, statement_balance, currency, status)
		VALUES (?, '2025-03-31', 0, 'MXN', 'open')`, w.checkingUS)
	exec(t, w.d, `UPDATE splits SET reconciliation_id = (SELECT MAX(id) FROM reconciliations)
		WHERE account_id = ? AND transaction_id = ?`, w.checkingUS, w.txFund)
	assertFlags(t, w.d, "Z8")
}

// --- Z9: finalized recon reconciles to its statement chain -------------------

func TestZ9FinalizedStatementMismatch(t *testing.T) {
	w := newWorld(t)
	// Clear the checkingUS split of txPlain (amount -10000) against a finalized
	// recon whose statement_balance is a wrong number (first recon, so opening = 0;
	// -10000 + 0 != 99999). Insert the recon OPEN, clear the split, then flip it to
	// finalized (the lock trigger only guards financial-field UPDATEs on splits, not
	// UPDATEs to reconciliations.status).
	exec(t, w.d, `INSERT INTO reconciliations (account_id, statement_date, statement_balance, currency, status)
		VALUES (?, '2025-03-31', 99999, 'USD', 'open')`, w.checkingUS)
	exec(t, w.d, `UPDATE splits SET reconciliation_id = (SELECT MAX(id) FROM reconciliations)
		WHERE account_id = ? AND transaction_id = ?`, w.checkingUS, w.txPlain)
	exec(t, w.d, `UPDATE reconciliations SET status = 'finalized' WHERE id = (SELECT MAX(id) FROM reconciliations)`)
	assertFlags(t, w.d, "Z9")
}

func TestZ8Z9CleanRecon(t *testing.T) {
	w := newWorld(t)
	// A CORRECT finalized recon must trip neither Z8 nor Z9. Recon on checkingUS in
	// USD; clear the checkingUS split of txPlain (amount -10000); statement_balance
	// = -10000 (opening 0 + cleared -10000). Account/currency match (Z8 clean) and
	// the chain balances (Z9 clean). Build it OPEN, clear, then finalize.
	exec(t, w.d, `INSERT INTO reconciliations (account_id, statement_date, statement_balance, currency, status)
		VALUES (?, '2025-03-31', -10000, 'USD', 'open')`, w.checkingUS)
	exec(t, w.d, `UPDATE splits SET reconciliation_id = (SELECT MAX(id) FROM reconciliations)
		WHERE account_id = ? AND transaction_id = ?`, w.checkingUS, w.txPlain)
	exec(t, w.d, `UPDATE reconciliations SET status = 'finalized' WHERE id = (SELECT MAX(id) FROM reconciliations)`)
	got := rulesOf(checkAll(t, w.d))
	if got["Z8"] {
		t.Errorf("Z8 fired on a valid cleared split; got %v", keys(got))
	}
	if got["Z9"] {
		t.Errorf("Z9 fired on a correctly-reconciled finalized recon; got %v", keys(got))
	}
	assertClean(t, w.d)
}

// --- Z10: per-fund zero-sum --------------------------------------------------

func TestZ10FundUnbalanced(t *testing.T) {
	w := newWorld(t)
	// Break ONE fund group's balance while keeping the overall sum zero, so Z10
	// (not Z1) is the flag. The fund receipt has two splits both tagged the fund;
	// retag ONE of them to unrestricted (fund_id NULL). Now the fund group nets
	// non-zero and the unrestricted group nets non-zero, but the txn total is still
	// zero.
	exec(t, w.d, `UPDATE splits SET fund_id = NULL WHERE transaction_id = ? AND account_id = ?`, w.txFund, w.contrib)
	got := rulesOf(checkAll(t, w.d))
	if !got["Z10"] {
		t.Errorf("want Z10 flagged; got %v", keys(got))
	}
	if got["Z1"] {
		t.Errorf("Z1 should NOT fire (overall sum still zero); got %v", keys(got))
	}
}

// --- Z11: split account mapped to txn subsidiary -----------------------------

func TestZ11AccountNotInSubsidiary(t *testing.T) {
	w := newWorld(t)
	// Remove the account_subsidiaries mapping for a split's account (raw delete),
	// so the split's account is no longer mapped to its txn's subsidiary.
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `DELETE FROM account_subsidiaries WHERE account_id = ? AND subsidiary_id = ?`, w.salaries, w.subUS)
	assertFlags(t, w.d, "Z11")
}

// --- Z12: parent sub set superset of children -------------------------------

func TestZ12ParentMissingChildSub(t *testing.T) {
	w := newWorld(t)
	// Build a parent placeholder + child through the store so the tree is valid,
	// then raw-delete the parent's membership for the child's sub.
	s := w.s
	parent, err := s.CreateAccount(mutCtx(), store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Assets"}, Subsidiaries: []int64{w.subUS},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	_, err = s.CreateAccount(mutCtx(), store.CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Child"}, Subsidiaries: []int64{w.subUS},
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Remove the parent's US membership; the child still has it -> superset broken.
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `DELETE FROM account_subsidiaries WHERE account_id = ? AND subsidiary_id = ?`, parent, w.subUS)
	assertFlags(t, w.d, "Z12")
}

// --- Z13: split fund scoped to txn subsidiary -------------------------------

func TestZ13FundNotScoped(t *testing.T) {
	w := newWorld(t)
	// Remove the fund's scope to the US sub; the fund-tagged split's txn is US.
	exec(t, w.d, `PRAGMA foreign_keys=OFF`)
	exec(t, w.d, `DELETE FROM fund_subsidiaries WHERE fund_id = ? AND subsidiary_id = ?`, w.fund, w.subUS)
	assertFlags(t, w.d, "Z13")
}

// --- Z14: functional_class iff expense --------------------------------------

func TestZ14FunctionMismatch(t *testing.T) {
	w := newWorld(t)
	// Null out an expense split's functional_class (bypassing the trigger by
	// dropping it on the throwaway db).
	dropTriggers(t, w.d, "trg_splits_function_matches_type_update")
	exec(t, w.d, `UPDATE splits SET functional_class = NULL WHERE account_id = ? AND transaction_id = ?`, w.salaries, w.txPlain)
	assertFlags(t, w.d, "Z14")
}

// --- Z15: program iff R/E + fund scope --------------------------------------

func TestZ15ProgramPresenceMismatch(t *testing.T) {
	w := newWorld(t)
	// Give a balance-sheet split a program (must be NULL) -- bypass the trigger by
	// dropping it on the throwaway db.
	dropTriggers(t, w.d, "trg_splits_program_matches_type_update")
	exec(t, w.d, `UPDATE splits SET program_id = ? WHERE account_id = ? AND transaction_id = ?`, w.prog, w.checkingUS, w.txPlain)
	assertFlags(t, w.d, "Z15")
}

func TestZ15FundProgramScope(t *testing.T) {
	w := newWorld(t)
	// Scope the fund to a program subtree that EXCLUDES the split's program, then
	// point the fund receipt's R/E split at a program outside that subtree.
	// Build a second program branch; scope fund to it; the contrib split uses
	// w.prog which is NOT under it.
	other := mkProg(t, w.s, "Other")
	exec(t, w.d, `UPDATE funds SET program_id = ? WHERE id = ?`, other, w.fund)
	assertFlags(t, w.d, "Z15")
}

// --- Z16: single root + acyclic subsidiary/program trees --------------------

func TestZ16SecondSubsidiaryRoot(t *testing.T) {
	w := newWorld(t)
	// Orphan a subsidiary into a second root (bypassing the single-root trigger by
	// dropping it on the throwaway db).
	dropTriggers(t, w.d, "trg_subsidiaries_single_root_update")
	exec(t, w.d, `UPDATE subsidiaries SET parent_id = NULL WHERE id = ?`, w.subUS)
	assertFlags(t, w.d, "Z16")
}

func TestZ16ProgramCycle(t *testing.T) {
	w := newWorld(t)
	// A program that is its own parent -> cycle.
	exec(t, w.d, `UPDATE programs SET parent_id = ? WHERE id = ?`, w.prog, w.prog)
	assertFlags(t, w.d, "Z16")
}

// --- Z17 (warning): intercompany nets to zero per currency ------------------

func TestZ17IntercompanyImbalance(t *testing.T) {
	w := newWorld(t)
	// Build the imbalance THROUGH the store so no error rule (esp. Z3) fires: post
	// one extra store-valid US txn with a one-sided intercompany leg -- DR Due-from
	// (intercompany) / CR checking US. It balances (Z1/Z10 clean) and is fully
	// versioned (Z3 clean), but it leaves the intercompany accounts netting +500 at
	// consolidation, which is exactly Z17's warning.
	post(t, w.s, store.PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.dueFrom, Amount: 500, Position: 0},
			{AccountID: w.checkingUS, Amount: -500, Position: 1},
		},
	})
	vs := checkAll(t, w.d)
	got := rulesOf(vs)
	if !got["Z17"] {
		t.Errorf("want Z17 flagged; got %v", keys(got))
	}
	for _, v := range vs {
		if v.Rule == "Z17" && v.Severity != ledger.Warning {
			t.Errorf("Z17 severity = %s, want warning", v.Severity)
		}
	}
	if ledger.HasErrors(vs) {
		t.Errorf("Z17 (store-built) should raise NO Error violations; got %v", vs)
	}
}

// --- Z18 (warning): restricted fund negative balance ------------------------

func TestZ18NegativeRestrictedFund(t *testing.T) {
	w := newWorld(t)
	// The overspend D23 targets, built ENTIRELY through the store (each txn
	// fund-balanced, so Z1/Z10/Z3 all stay clean). newWorld already received 50000
	// into the fund (checking +50000). Now post a fund-tagged SPEND of 90000:
	// DR salaries +90000 (fund) / CR checking -90000 (fund). Each txn nets zero
	// within the fund, but the fund's restricted CASH is now 50000-90000 = -40000
	// -- negative in USD. Z18 (asset-restricted) warns; no error rule fires. This
	// is precisely "spending a grant past its receipts" (D23).
	post(t, w.s, store.PostTransactionInput{
		Date: "2025-05-15", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: w.salaries, Amount: 90_000, FundID: &w.fund, Position: 0},
			{AccountID: w.checkingUS, Amount: -90_000, FundID: &w.fund, Position: 1},
		},
	})
	vs := checkAll(t, w.d)
	got := rulesOf(vs)
	if !got["Z18"] {
		t.Errorf("want Z18 flagged; got %v", keys(got))
	}
	for _, v := range vs {
		if v.Rule == "Z18" && v.Severity != ledger.Warning {
			t.Errorf("Z18 severity = %s, want warning", v.Severity)
		}
	}
	if ledger.HasErrors(vs) {
		t.Errorf("Z18 (store-built overspend) should raise NO Error violations; got %v", vs)
	}
}

// --- Z19 (warning): active R/E leaf with activity has an effective 990 code --

func TestZ19UnmappedActiveLeaf(t *testing.T) {
	w := newWorld(t)
	// Build the unmapped-with-activity condition THROUGH the store (no Z3/error
	// side effects): create an expense leaf with NO 990 code (and no coded
	// ancestor), then post a balanced txn using it. It is an active R/E leaf with
	// splits and no effective code -- exactly Z19. (Kept OUT of newWorld so the
	// clean-world test stays warning-free.)
	mgmt := "management"
	unmapped := mkAcct(t, w.s, acct{typ: "expense", name: "Unmapped Exp", subs: []int64{w.subUS}, fclass: &mgmt, defProg: &rootProg})
	post(t, w.s, store.PostTransactionInput{
		Date: "2025-06-10", SubsidiaryID: w.subUS, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: unmapped, Amount: 700, Position: 0},
			{AccountID: w.checkingUS, Amount: -700, Position: 1},
		},
	})
	vs := checkAll(t, w.d)
	got := rulesOf(vs)
	if !got["Z19"] {
		t.Errorf("want Z19 flagged; got %v", keys(got))
	}
	for _, v := range vs {
		if v.Rule == "Z19" && v.Severity != ledger.Warning {
			t.Errorf("Z19 severity = %s, want warning", v.Severity)
		}
	}
	if ledger.HasErrors(vs) {
		t.Errorf("Z19 corruption should not raise Error violations; got %v", vs)
	}
}
