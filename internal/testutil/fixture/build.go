package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
)

// Seeded roots keep their migration ids.
const (
	seedRootSub     int64 = 1 // "Organization" (USD), renamed below
	seedRootProgram int64 = 1 // "General"
)

// ptr returns a pointer to v (concise optional-field construction).
func ptr[T any](v T) *T { return &v }

// build constructs Appendix D exactly, in dependency order, and returns the ids.
// It fails the test on any error. All amounts are minor units (cents; USD and
// MXN both have exponent 2). Net-debit signs (D2): asset/expense debits +,
// revenue/liability/equity credits -.
func build(t *testing.T, ctx context.Context, s *store.Store) IDs {
	t.Helper()
	var ids IDs

	buildOrg(t, ctx, s, &ids)
	buildAccounts(t, ctx, s, &ids)
	buildFunds(t, ctx, s, &ids)
	buildTransactions(t, ctx, s, &ids)

	return ids
}

// buildOrg renames the seeded root and creates the two child subsidiaries and
// the two child programs.
func buildOrg(t *testing.T, ctx context.Context, s *store.Store, ids *IDs) {
	t.Helper()

	// RENAME the seeded root (do NOT insert a second root -- the single-root
	// trigger forbids it). This produces an update version row on the seed.
	if err := s.UpdateSubsidiary(ctx, seedRootSub, store.UpdateSubsidiaryInput{
		Name: ptr("Rio Verde Internacional"),
	}); err != nil {
		t.Fatalf("rename root subsidiary: %v", err)
	}
	ids.Root = seedRootSub

	us, err := s.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{
		ParentID: ids.Root, Name: "RV Estados Unidos", BaseCurrency: "USD", SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("create US subsidiary: %v", err)
	}
	ids.US = us

	mx, err := s.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{
		ParentID: ids.Root, Name: "RV Mexico", BaseCurrency: "MXN", SortOrder: 2,
	})
	if err != nil {
		t.Fatalf("create MX subsidiary: %v", err)
	}
	ids.MX = mx

	ids.General = seedRootProgram
	edu, err := s.CreateProgram(ctx, store.CreateProgramInput{ParentID: ids.General, Name: "Educacion", SortOrder: 1})
	if err != nil {
		t.Fatalf("create Educacion program: %v", err)
	}
	ids.Educacion = edu

	fp, err := s.CreateProgram(ctx, store.CreateProgramInput{ParentID: ids.General, Name: "Food Pantry", SortOrder: 2})
	if err != nil {
		t.Fatalf("create Food Pantry program: %v", err)
	}
	ids.FoodPantry = fp
}

// acctSpec is one account to create (concise, named fields).
type acctSpec struct {
	dst          *int64
	parent       *int64
	typ          string
	nameEN       string
	nameES       string
	currency     string
	subs         []int64
	reconcilable bool
	intercompany bool
	fClass       *string
	defProgram   *int64
	code         *string
}

// buildAccounts creates the ~22 accounts exactly per Appendix D, capturing each
// id into ids. Revenue and Expenses are placeholder parents (990 codes at
// parents to exercise D25 inheritance); Event Income is deliberately left
// UNMAPPED (no own code, revenue parent has none) to exercise Z19 + the Unmapped
// bucket; Bank Fees overrides the Expenses parent code (leaf override).
func buildAccounts(t *testing.T, ctx context.Context, s *store.Store, ids *IDs) {
	t.Helper()

	mgmt := "management"
	fundr := "fundraising"
	prog := "program"
	all := []int64{ids.Root, ids.US, ids.MX}

	// Create parents first so children can reference them.
	specs := []acctSpec{
		// --- Assets (natural leaves; no grouping parent) ---
		{dst: &ids.CheckingUS, typ: "asset", nameEN: "Checking US", nameES: "Cuenta corriente EE. UU.", currency: "USD", subs: []int64{ids.US}, reconcilable: true},
		{dst: &ids.CheckingMX, typ: "asset", nameEN: "Checking MX", nameES: "Cuenta corriente MX", currency: "MXN", subs: []int64{ids.MX}, reconcilable: true},
		{dst: &ids.Savings, typ: "asset", nameEN: "Savings", nameES: "Ahorros", currency: "USD", subs: []int64{ids.Root}},
		{dst: &ids.CashMXN, typ: "asset", nameEN: "Cash MXN", nameES: "Efectivo MXN", currency: "MXN", subs: []int64{ids.MX}},
		{dst: &ids.Building, typ: "asset", nameEN: "Building", nameES: "Edificio", currency: "USD", subs: []int64{ids.US}, code: ptr("X.10")},
		{dst: &ids.DueFromMX, typ: "asset", nameEN: "Due from RV Mexico", nameES: "Por cobrar de RV Mexico", currency: "USD", subs: []int64{ids.US}, intercompany: true},
		{dst: &ids.FXClearing, typ: "asset", nameEN: "FX Clearing", nameES: "Compensacion de cambio", currency: "USD", subs: all},

		// --- Liabilities ---
		{dst: &ids.CreditCard, typ: "liability", nameEN: "Credit Card", nameES: "Tarjeta de credito", currency: "USD", subs: []int64{ids.US}, reconcilable: true},
		{dst: &ids.DueToIntl, typ: "liability", nameEN: "Due to RV Internacional", nameES: "Por pagar a RV Internacional", currency: "USD", subs: []int64{ids.MX}, intercompany: true},

		// --- Equity ---
		{dst: &ids.OpeningBalances, typ: "equity", nameEN: "Opening Balances", nameES: "Saldos iniciales", currency: "USD", subs: all},

		// --- Revenue (placeholder parent, NO own code) ---
		{dst: &ids.Revenue, typ: "revenue", nameEN: "Revenue", nameES: "Ingresos", currency: "USD", subs: all},
	}
	for i := range specs {
		createAccount(t, ctx, s, specs[i])
	}

	// Revenue children (own codes; Event Income deliberately none).
	createAccount(t, ctx, s, acctSpec{dst: &ids.Contributions, parent: &ids.Revenue, typ: "revenue", nameEN: "Contributions", nameES: "Donaciones", currency: "USD", subs: all, code: ptr("VIII.1f")})
	createAccount(t, ctx, s, acctSpec{dst: &ids.GovernmentGrants, parent: &ids.Revenue, typ: "revenue", nameEN: "Government Grants", nameES: "Subvenciones del gobierno", currency: "USD", subs: all, code: ptr("VIII.1e")})
	createAccount(t, ctx, s, acctSpec{dst: &ids.ProgramFees, parent: &ids.Revenue, typ: "revenue", nameEN: "Program Service Fees", nameES: "Cuotas de servicios del programa", currency: "USD", subs: all, code: ptr("VIII.2"), defProgram: &ids.Educacion})
	createAccount(t, ctx, s, acctSpec{dst: &ids.EventIncome, parent: &ids.Revenue, typ: "revenue", nameEN: "Event Income", nameES: "Ingresos por eventos", currency: "USD", subs: all}) // UNMAPPED (Z19)

	// Expenses (placeholder parent WITH a code -> leaves inherit it).
	createAccount(t, ctx, s, acctSpec{dst: &ids.Expenses, typ: "expense", nameEN: "Expenses", nameES: "Gastos", currency: "USD", subs: all, code: ptr("IX.24e")})
	createAccount(t, ctx, s, acctSpec{dst: &ids.Salaries, parent: &ids.Expenses, typ: "expense", nameEN: "Salaries", nameES: "Salarios", currency: "USD", subs: all, fClass: &prog, code: ptr("IX.7")})
	createAccount(t, ctx, s, acctSpec{dst: &ids.ProgramSupplies, parent: &ids.Expenses, typ: "expense", nameEN: "Program Supplies", nameES: "Suministros del programa", currency: "USD", subs: all, fClass: &prog, defProgram: &ids.Educacion})
	createAccount(t, ctx, s, acctSpec{dst: &ids.FoodPurchases, parent: &ids.Expenses, typ: "expense", nameEN: "Food Purchases", nameES: "Compras de alimentos", currency: "MXN", subs: all, fClass: &prog, defProgram: &ids.FoodPantry})
	createAccount(t, ctx, s, acctSpec{dst: &ids.Occupancy, parent: &ids.Expenses, typ: "expense", nameEN: "Occupancy", nameES: "Ocupacion", currency: "USD", subs: all, fClass: &mgmt, code: ptr("IX.16")})
	createAccount(t, ctx, s, acctSpec{dst: &ids.Insurance, parent: &ids.Expenses, typ: "expense", nameEN: "Insurance", nameES: "Seguro", currency: "USD", subs: all, fClass: &mgmt})
	createAccount(t, ctx, s, acctSpec{dst: &ids.BankFees, parent: &ids.Expenses, typ: "expense", nameEN: "Bank Fees", nameES: "Comisiones bancarias", currency: "USD", subs: all, fClass: &mgmt, code: ptr("IX.11g")}) // LEAF OVERRIDE
	createAccount(t, ctx, s, acctSpec{dst: &ids.EventCosts, parent: &ids.Expenses, typ: "expense", nameEN: "Event Costs", nameES: "Costos de eventos", currency: "USD", subs: all, fClass: &fundr})
}

// createAccount posts one account with an en + es name and stores its id.
func createAccount(t *testing.T, ctx context.Context, s *store.Store, sp acctSpec) {
	t.Helper()
	id, err := s.CreateAccount(ctx, store.CreateAccountInput{
		ParentID:         sp.parent,
		Type:             sp.typ,
		DefaultCurrency:  sp.currency,
		Names:            map[string]string{"en": sp.nameEN, "es": sp.nameES},
		Subsidiaries:     sp.subs,
		FunctionalClass:  sp.fClass,
		Form990Code:      sp.code,
		DefaultProgramID: sp.defProgram,
		Intercompany:     sp.intercompany,
		Reconcilable:     sp.reconcilable,
	})
	if err != nil {
		t.Fatalf("create account %q: %v", sp.nameEN, err)
	}
	*sp.dst = id
}

// buildFunds creates the two restricted funds.
func buildFunds(t *testing.T, ctx context.Context, s *store.Store, ids *IDs) {
	t.Helper()

	beca, err := s.CreateFund(ctx, store.CreateFundInput{
		Name:         "Beca Agua 2025",
		Funder:       "Fundacion Agua Limpia",
		Purpose:      "Clean-water education scholarships",
		Restriction:  "purpose",
		ProgramID:    &ids.Educacion,
		StartDate:    ptr("2025-01-01"),
		EndDate:      ptr("2025-12-31"),
		Subsidiaries: []int64{ids.MX, ids.US},
	})
	if err != nil {
		t.Fatalf("create Beca Agua fund: %v", err)
	}
	ids.BecaAgua = beca

	bf, err := s.CreateFund(ctx, store.CreateFundInput{
		Name:         "Building Fund",
		Funder:       "Anonymous Donor",
		Purpose:      "New community building",
		Restriction:  "purpose",
		Subsidiaries: []int64{ids.US},
	})
	if err != nil {
		t.Fatalf("create Building Fund: %v", err)
	}
	ids.BuildingFund = bf
}
