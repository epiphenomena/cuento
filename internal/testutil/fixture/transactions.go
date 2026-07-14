package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
)

// sp is a concise split literal for the fixture's transactions. fund/program are
// pointers (nil = unset); the store defaults program on R/E splits and functional
// class on expense splits from the account, so we usually leave them nil and let
// the defaults apply -- except where a test needs a non-default program/fund.
type sp struct {
	acct   int64
	amount int64 // minor units, net-debit sign (D2)
	fund   *int64
	prog   *int64
	class  *string
	desc   string // per-split free-text description (p26.15); "" leaves it empty
}

// post inserts a balanced transaction and returns its id, failing the test on
// error. It is the workhorse of the transaction build.
func post(t *testing.T, ctx context.Context, s *store.Store, date string, sub int64, ccy, memo string, payee *int64, splits ...sp) int64 {
	t.Helper()
	in := store.PostTransactionInput{
		Date:         date,
		SubsidiaryID: sub,
		Currency:     ccy,
		Memo:         memo,
		PayeeID:      payee,
	}
	for i, x := range splits {
		in.Splits = append(in.Splits, store.SplitInput{
			AccountID:       x.acct,
			Amount:          x.amount,
			FundID:          x.fund,
			ProgramID:       x.prog,
			FunctionalClass: x.class,
			Description:     x.desc,
			Position:        int64(i),
		})
	}
	id, err := s.PostTransaction(ctx, in)
	if err != nil {
		t.Fatalf("post %q (%s %s): %v", memo, date, ccy, err)
	}
	return id
}

// buildTransactions posts the ~36 Appendix D transactions in date order (grouped
// by scenario for readability). Every scenario's per-fund and overall zero-sum is
// verified by the store on post; ledger.Check then re-verifies the whole set.
//
// All amounts are minor units (cents). Payees: "City Utilities" repeats (autofill
// tests). The twice-edited txn and the deleted txn are captured for as-of/audit
// tests.
func buildTransactions(t *testing.T, ctx context.Context, s *store.Store, ids *IDs) {
	t.Helper()

	// Repeat payee for autofill tests (used by two MXN cash expenses).
	utilities, err := s.CreatePayee(ctx, "City Utilities")
	if err != nil {
		t.Fatalf("create payee: %v", err)
	}

	// ---------------------------------------------------------------------
	// Opening balances (one per subsidiary), via Equity:Opening Balances.
	// Unrestricted (NULL fund), no program (equity/asset splits carry none).
	// ---------------------------------------------------------------------

	// A split account must be mapped to the txn's subsidiary (D18). Savings is a
	// root-only account, so its opening balance is a ROOT-subsidiary transaction;
	// Checking US / Building / Credit Card (US-mapped) open under US; Checking MX
	// / Cash MXN (MX-mapped) under MX. Hence three opening transactions.

	// US opening: Checking US 50,000.00; Credit Card (liability, credit)
	// -3,000.00; Building 120,000.00; balanced by Opening Balances equity credit
	// -167,000.00. (USD)
	post(
		t, ctx, s, "2025-01-01", ids.US, "USD", "Opening balances RV Estados Unidos", nil,
		sp{acct: ids.CheckingUS, amount: 5_000_000, desc: "Opening bank balance"},
		sp{acct: ids.Building, amount: 12_000_000},
		sp{acct: ids.CreditCard, amount: -300_000},
		sp{acct: ids.OpeningBalances, amount: -16_700_000},
	)

	// Root opening: Savings 20,000.00 USD; balanced by Opening Balances
	// -20,000.00. (Savings is a root-scoped account.)
	post(
		t, ctx, s, "2025-01-01", ids.Root, "USD", "Opening balances Rio Verde Internacional", nil,
		sp{acct: ids.Savings, amount: 2_000_000},
		sp{acct: ids.OpeningBalances, amount: -2_000_000},
	)

	// MX opening: Checking MX 300,000.00 MXN; Cash MXN 15,000.00; balanced by
	// Opening Balances -315,000.00. (MXN)
	post(
		t, ctx, s, "2025-01-01", ids.MX, "MXN", "Saldos iniciales RV Mexico", nil,
		sp{acct: ids.CheckingMX, amount: 30_000_000},
		sp{acct: ids.CashMXN, amount: 1_500_000},
		sp{acct: ids.OpeningBalances, amount: -31_500_000},
	)

	// ---------------------------------------------------------------------
	// Everyday unrestricted activity.
	// ---------------------------------------------------------------------

	// Contribution received (US, USD): Checking US +2,000.00; Contributions
	// -2,000.00 (program General/root).
	post(
		t, ctx, s, "2025-02-05", ids.US, "USD", "General donation", nil,
		sp{acct: ids.CheckingUS, amount: 200_000, desc: "Donation deposit"},
		sp{acct: ids.Contributions, amount: -200_000},
	)

	// Salaries paid (US, USD): Salaries +8,000.00 (program, General); Checking US
	// -8,000.00.
	post(
		t, ctx, s, "2025-02-28", ids.US, "USD", "February salaries", nil,
		sp{acct: ids.Salaries, amount: 800_000},
		sp{acct: ids.CheckingUS, amount: -800_000, desc: "Payroll run Feb"},
	)

	// Occupancy / rent (US, USD): Occupancy +1,500.00 (management, General);
	// Checking US -1,500.00.
	post(
		t, ctx, s, "2025-03-01", ids.US, "USD", "March rent", nil,
		sp{acct: ids.Occupancy, amount: 150_000},
		sp{acct: ids.CheckingUS, amount: -150_000, desc: "Office rent check"},
	)

	// Insurance (US, USD): Insurance +600.00 (management, General); Checking US
	// -600.00.
	post(
		t, ctx, s, "2025-03-10", ids.US, "USD", "Annual insurance", nil,
		sp{acct: ids.Insurance, amount: 60_000},
		sp{acct: ids.CheckingUS, amount: -60_000, desc: "Liability insurance premium"},
	)

	// Bank fees (US, USD): Bank Fees +25.00 (management, General); Checking US
	// -25.00. (Leaf-override 990 code IX.11g exercised in reports.)
	post(
		t, ctx, s, "2025-03-31", ids.US, "USD", "Q1 bank fees", nil,
		sp{acct: ids.BankFees, amount: 2_500},
		sp{acct: ids.CheckingUS, amount: -2_500, desc: "Quarterly account fees"},
	)

	// MXN cash expenses (program General) via the repeat payee "City Utilities".
	// Food Purchases +1,200.00 MXN (program default = Food Pantry -> OVERRIDE to
	// General so this is a General-program expense); Cash MXN -1,200.00.
	genProg := ids.General
	post(
		t, ctx, s, "2025-03-15", ids.MX, "MXN", "Utilities", &utilities,
		sp{acct: ids.FoodPurchases, amount: 120_000, prog: &genProg},
		sp{acct: ids.CashMXN, amount: -120_000},
	)
	// Second occurrence of the repeat payee (autofill target). Food Purchases
	// +900.00 MXN (General); Cash MXN -900.00.
	post(
		t, ctx, s, "2025-04-15", ids.MX, "MXN", "Utilities", &utilities,
		sp{acct: ids.FoodPurchases, amount: 90_000, prog: &genProg},
		sp{acct: ids.CashMXN, amount: -90_000},
	)

	// ---------------------------------------------------------------------
	// Restricted grant lifecycle -- Beca Agua 2025 (program Educacion).
	// ---------------------------------------------------------------------

	// Grant receipt (MX, MXN, fund Beca Agua, revenue tagged Educacion):
	// Checking MX +100,000.00 MXN; Government Grants -100,000.00 (Educacion).
	// Both splits tagged the fund so the txn nets to zero WITHIN Beca Agua (MXN).
	post(
		t, ctx, s, "2025-04-01", ids.MX, "MXN", "Beca Agua grant receipt", nil,
		sp{acct: ids.CheckingMX, amount: 10_000_000, fund: &ids.BecaAgua},
		sp{acct: ids.GovernmentGrants, amount: -10_000_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
	)

	// Small USD receipt for Beca Agua in the US sub so the US-side (USD) spend
	// below does not drive the fund's USD asset balance negative (Z18 stays
	// silent). Checking US +2,000.00 USD (Beca Agua); Government Grants -2,000.00
	// (Beca Agua, Educacion).
	post(
		t, ctx, s, "2025-04-05", ids.US, "USD", "Beca Agua US contribution", nil,
		sp{acct: ids.CheckingUS, amount: 200_000, fund: &ids.BecaAgua, desc: "Restricted gift deposit"},
		sp{acct: ids.GovernmentGrants, amount: -200_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
	)

	// Restricted spend #1 -- MIXED 60/40 fund split (MX, MXN), proving per-fund
	// balancing. Program Supplies expense 5,000.00 MXN split 60% Beca Agua /
	// 40% unrestricted; the cash side splits correspondingly so EACH fund group
	// nets zero.
	//   Beca Agua group:  supplies +3,000.00 ; cash -3,000.00
	//   unrestricted group: supplies +2,000.00 ; cash -2,000.00
	post(
		t, ctx, s, "2025-05-10", ids.MX, "MXN", "Program supplies (mixed funding)", nil,
		sp{acct: ids.ProgramSupplies, amount: 300_000, fund: &ids.BecaAgua, prog: &ids.Educacion, desc: "Water filters (grant-funded portion)"},
		sp{acct: ids.CheckingMX, amount: -300_000, fund: &ids.BecaAgua, desc: "Water filters (grant-funded portion)"},
		sp{acct: ids.ProgramSupplies, amount: 200_000, prog: &ids.Educacion, desc: "Water filters (general portion)"},
		sp{acct: ids.CheckingMX, amount: -200_000, desc: "Water filters (general portion)"},
	)

	// Restricted spend #2 -- in RV Estados Unidos (USD), proving the multi-sub
	// fund scope. Program Supplies +1,500.00 USD (Beca Agua, Educacion);
	// Checking US -1,500.00 (Beca Agua).
	post(
		t, ctx, s, "2025-05-20", ids.US, "USD", "Program supplies (US)", nil,
		sp{acct: ids.ProgramSupplies, amount: 150_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -150_000, fund: &ids.BecaAgua, desc: "Filter parts payment"},
	)

	// ---------------------------------------------------------------------
	// Building Fund -- receipt applied to a Building ASSET PURCHASE (restricted
	// non-expense application; no program on asset splits).
	// ---------------------------------------------------------------------

	// Building Fund receipt (US, USD): Checking US +50,000.00 (Building Fund);
	// Contributions -50,000.00 (Building Fund, program General). Revenue split
	// carries a program (required); asset split does not.
	post(
		t, ctx, s, "2025-06-01", ids.US, "USD", "Building Fund gift", nil,
		sp{acct: ids.CheckingUS, amount: 5_000_000, fund: &ids.BuildingFund, desc: "Capital campaign gift"},
		sp{acct: ids.Contributions, amount: -5_000_000, fund: &ids.BuildingFund, prog: &genProg},
	)

	// Building asset purchase applying the Building Fund (US, USD): Building
	// +40,000.00 (Building Fund); Checking US -40,000.00 (Building Fund). No
	// program on either split (both A-class); nets zero within Building Fund.
	post(
		t, ctx, s, "2025-06-15", ids.US, "USD", "Building improvement", nil,
		sp{acct: ids.Building, amount: 4_000_000, fund: &ids.BuildingFund},
		sp{acct: ids.CheckingUS, amount: -4_000_000, fund: &ids.BuildingFund, desc: "Roof replacement payment"},
	)

	// ---------------------------------------------------------------------
	// Intercompany funding pair (US -> MX via due-to/due-from). Both legs USD,
	// equal and opposite on the flagged accounts, so Z17 nets zero per currency.
	// The MX-side USD debit lands on FX Clearing (the only USD-capable account in
	// MX besides Due-to) -- modelling both legs in USD keeps each txn
	// single-currency (D3).
	// ---------------------------------------------------------------------

	// Parent leg (US, USD): Due from RV Mexico +10,000.00; Checking US -10,000.00.
	post(
		t, ctx, s, "2025-07-01", ids.US, "USD", "Intercompany funding to MX", nil,
		sp{acct: ids.DueFromMX, amount: 1_000_000},
		sp{acct: ids.CheckingUS, amount: -1_000_000, desc: "Wire to RV Mexico"},
	)
	// Child leg (MX, USD): FX Clearing +10,000.00; Due to RV Internacional
	// -10,000.00. Due-from (+10,000) and Due-to (-10,000) net to zero in USD.
	post(
		t, ctx, s, "2025-07-02", ids.MX, "USD", "Intercompany funding from Intl", nil,
		sp{acct: ids.FXClearing, amount: 1_000_000},
		sp{acct: ids.DueToIntl, amount: -1_000_000},
	)

	// ---------------------------------------------------------------------
	// Cross-currency transfer via FX Clearing (two single-currency txns, D3).
	//   leg 1 (MX, MXN): Cash MXN -5,000.00 ; FX Clearing +5,000.00 MXN
	//   leg 2 (US, USD): FX Clearing -260.00 ; Checking US +260.00 USD
	// FX Clearing's converted balance is the cumulative FX gain/loss (native
	// here: +5,000.00 MXN and -260.00 USD sit side by side).
	// ---------------------------------------------------------------------
	post(
		t, ctx, s, "2025-08-01", ids.MX, "MXN", "FX transfer out (MXN)", nil,
		sp{acct: ids.CashMXN, amount: -500_000},
		sp{acct: ids.FXClearing, amount: 500_000},
	)
	post(
		t, ctx, s, "2025-08-01", ids.US, "USD", "FX transfer in (USD)", nil,
		sp{acct: ids.FXClearing, amount: -26_000},
		sp{acct: ids.CheckingUS, amount: 26_000, desc: "Converted MXN settlement"},
	)

	// ---------------------------------------------------------------------
	// Event income + costs (Event Income is the UNMAPPED revenue leaf -> Z19;
	// Event Costs is fundraising-class).
	// ---------------------------------------------------------------------
	// Fundraising event (US, USD): Checking US +3,000.00; Event Income -3,000.00
	// (program General). Event Income has NO effective 990 code -> Z19 + Unmapped.
	post(
		t, ctx, s, "2025-09-15", ids.US, "USD", "Gala ticket sales", nil,
		sp{acct: ids.CheckingUS, amount: 300_000, desc: "Gala ticket deposit"},
		sp{acct: ids.EventIncome, amount: -300_000, prog: &genProg},
	)
	// Event costs (US, USD): Event Costs +1,000.00 (fundraising, General);
	// Checking US -1,000.00.
	post(
		t, ctx, s, "2025-09-20", ids.US, "USD", "Gala catering", nil,
		sp{acct: ids.EventCosts, amount: 100_000},
		sp{acct: ids.CheckingUS, amount: -100_000, desc: "Caterer invoice payment"},
	)

	// ---------------------------------------------------------------------
	// A handful of 2026 transactions (so the fixture spans to 2026-06 and gives
	// the p16 reconciliation something to leave uncleared).
	// ---------------------------------------------------------------------
	// 2026 program fees (US, USD): Checking US +1,200.00; Program Service Fees
	// -1,200.00 (default program Educacion).
	post(
		t, ctx, s, "2026-01-10", ids.US, "USD", "Program fees Q1", nil,
		sp{acct: ids.CheckingUS, amount: 120_000, desc: "Tuition fees received"},
		sp{acct: ids.ProgramFees, amount: -120_000},
	)
	// 2026 salaries (US, USD): Salaries +8,500.00 (program, General); Checking US
	// -8,500.00.
	post(
		t, ctx, s, "2026-02-27", ids.US, "USD", "February 2026 salaries", nil,
		sp{acct: ids.Salaries, amount: 850_000},
		sp{acct: ids.CheckingUS, amount: -850_000, desc: "Payroll run Feb 2026"},
	)
	// 2026 food purchases (MX, MXN, Food Pantry program via account default).
	// Food Purchases +1,500.00 MXN (Food Pantry); Cash MXN -1,500.00.
	post(
		t, ctx, s, "2026-03-05", ids.MX, "MXN", "Pantry food restock", nil,
		sp{acct: ids.FoodPurchases, amount: 150_000},
		sp{acct: ids.CashMXN, amount: -150_000},
	)
	// 2026 occupancy (US, USD): Occupancy +1,550.00 (management); Checking US
	// -1,550.00. (An uncleared item after the 2026-05-31 reconciliation -- captured
	// so ExtendReconciliation leaves exactly this + June donation uncleared.)
	ids.MayRentTxn = post(
		t, ctx, s, "2026-05-25", ids.US, "USD", "May 2026 rent", nil,
		sp{acct: ids.Occupancy, amount: 155_000},
		sp{acct: ids.CheckingUS, amount: -155_000},
	)
	// 2026 contribution (US, USD): Checking US +750.00; Contributions -750.00
	// (General). A second uncleared item after the reconciliation.
	ids.JuneDonationTxn = post(
		t, ctx, s, "2026-06-10", ids.US, "USD", "June donation", nil,
		sp{acct: ids.CheckingUS, amount: 75_000},
		sp{acct: ids.Contributions, amount: -75_000},
	)

	// ---------------------------------------------------------------------
	// One transaction EDITED TWICE (as-of tests). Include fund + program changes
	// across the edits so as-of at a middle T reconstructs a distinct state.
	// ---------------------------------------------------------------------
	buildEditedTransaction(t, ctx, s, ids)

	// ---------------------------------------------------------------------
	// One transaction DELETED (soft). Posted then soft-deleted; excluded from
	// every aggregate.
	// ---------------------------------------------------------------------
	del := post(
		t, ctx, s, "2025-10-01", ids.US, "USD", "Erroneous entry (to delete)", nil,
		sp{acct: ids.Insurance, amount: 40_000},
		sp{acct: ids.CheckingUS, amount: -40_000},
	)
	if err := s.DeleteTransaction(ctx, del); err != nil {
		t.Fatalf("soft-delete transaction: %v", err)
	}
	ids.DeletedTxn = del
}

// buildEditedTransaction posts a transaction and edits it twice, with the store's
// monotonic clock giving each version a distinct increasing timestamp. The edits
// change the fund and program, so an as-of query at the middle timestamp sees a
// state distinct from both the original and the final. The FINAL state is what
// the aggregates below reflect.
//
// Original (2025-11-01, US, USD): Program Supplies +500.00 (unrestricted,
//
//	Educacion) / Checking US -500.00.
//
// Edit 1: retag the expense to Beca Agua and program Educacion, splitting the
//
//	cash side to match (per-fund balance).
//
// Edit 2 (FINAL): revert to a single unrestricted General-program supply of
//
//	+600.00 / Checking US -600.00.
func buildEditedTransaction(t *testing.T, ctx context.Context, s *store.Store, ids *IDs) {
	t.Helper()
	genProg := ids.General

	id := post(
		t, ctx, s, "2025-11-01", ids.US, "USD", "Supplies (v1)", nil,
		sp{acct: ids.ProgramSupplies, amount: 50_000, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -50_000},
	)
	ids.EditedTxn = id

	// Edit 1: Beca Agua-funded, Educacion. (Middle state for as-of tests.)
	editTxn(
		t, ctx, s, id, "2025-11-01", ids.US, "USD", "Supplies (v2, restricted)",
		sp{acct: ids.ProgramSupplies, amount: 50_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -50_000, fund: &ids.BecaAgua},
	)

	// Edit 2 (FINAL): unrestricted, General program, amount 600.00.
	editTxn(
		t, ctx, s, id, "2025-11-01", ids.US, "USD", "Supplies (v3, final)",
		sp{acct: ids.ProgramSupplies, amount: 60_000, prog: &genProg},
		sp{acct: ids.CheckingUS, amount: -60_000, desc: "Office supplies payment"},
	)
}

// editTxn replaces a transaction's header + split set (UpdateTransaction), failing
// the test on error.
func editTxn(t *testing.T, ctx context.Context, s *store.Store, id int64, date string, sub int64, ccy, memo string, splits ...sp) {
	t.Helper()
	in := store.PostTransactionInput{Date: date, SubsidiaryID: sub, Currency: ccy, Memo: memo}
	for i, x := range splits {
		in.Splits = append(in.Splits, store.SplitInput{
			AccountID:       x.acct,
			Amount:          x.amount,
			FundID:          x.fund,
			ProgramID:       x.prog,
			FunctionalClass: x.class,
			Description:     x.desc,
			Position:        int64(i),
		})
	}
	if err := s.UpdateTransaction(ctx, id, in); err != nil {
		t.Fatalf("edit transaction %q: %v", memo, err)
	}
}
