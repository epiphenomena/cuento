package synth

import (
	"context"
	"fmt"

	entids "cuento/internal/ids"
	"cuento/internal/store"
)

// sp is a concise split literal for the builder's transactions. fund/program are
// pointers (nil = unset); the store defaults program on R/E splits and functional
// class on expense splits from the account, so we usually leave them nil and let
// the defaults apply -- except where a scenario needs a non-default program/fund.
type sp struct {
	acct   int64
	amount int64 // minor units, net-debit sign (D2)
	fund   *entids.FundID
	prog   *int64
	class  *string
	desc   string // per-split free-text description; "" leaves it empty
}

// post inserts a balanced transaction and returns its id. It is the workhorse of
// the transaction build.
func post(ctx context.Context, s *store.Store, date string, sub int64, ccy, memo string, splits ...sp) (int64, error) {
	in := store.PostTransactionInput{
		Date:         date,
		SubsidiaryID: sub,
		Currency:     ccy,
		Memo:         memo,
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
		return 0, fmt.Errorf("post %q (%s %s): %w", memo, date, ccy, err)
	}
	return id, nil
}

// buildTransactions posts the ~36 Appendix D transactions in date order (grouped by
// scenario for readability). Every scenario's per-fund and overall zero-sum is
// verified by the store on post; ledger.Check then re-verifies the whole set.
//
// All amounts are minor units (cents). Two MXN cash expenses repeat the same
// "Utilities" split description. The twice-edited txn and the deleted txn are
// captured for as-of/audit tests.
func buildTransactions(ctx context.Context, s *store.Store, ids *IDs) error {
	// ---------------------------------------------------------------------
	// Opening balances (one per subsidiary), via Equity:Opening Balances.
	// Unrestricted (NULL fund), no program (equity/asset splits carry none).
	// ---------------------------------------------------------------------

	// A split account must be mapped to the txn's subsidiary (D18). Savings is a
	// root-only account, so its opening balance is a ROOT-subsidiary transaction;
	// Checking US / Building / Credit Card (US-mapped) open under US; Checking MX /
	// Cash MXN (MX-mapped) under MX. Hence three opening transactions.

	if _, err := post(
		ctx, s, "2025-01-01", ids.US, "USD", "Opening balances RV Estados Unidos",
		sp{acct: ids.CheckingUS, amount: 5_000_000, desc: "Opening bank balance"},
		sp{acct: ids.Building, amount: 12_000_000},
		sp{acct: ids.CreditCard, amount: -300_000},
		sp{acct: ids.OpeningBalances, amount: -16_700_000},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-01-01", ids.Root, "USD", "Opening balances Rio Verde Internacional",
		sp{acct: ids.Savings, amount: 2_000_000},
		sp{acct: ids.OpeningBalances, amount: -2_000_000},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-01-01", ids.MX, "MXN", "Saldos iniciales RV Mexico",
		sp{acct: ids.CheckingMX, amount: 30_000_000},
		sp{acct: ids.CashMXN, amount: 1_500_000},
		sp{acct: ids.OpeningBalances, amount: -31_500_000},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Everyday unrestricted activity.
	// ---------------------------------------------------------------------

	if _, err := post(
		ctx, s, "2025-02-05", ids.US, "USD", "General donation",
		sp{acct: ids.CheckingUS, amount: 200_000, desc: "Donation deposit"},
		sp{acct: ids.Contributions, amount: -200_000},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-02-28", ids.US, "USD", "February salaries",
		sp{acct: ids.Salaries, amount: 800_000},
		sp{acct: ids.CheckingUS, amount: -800_000, desc: "Payroll run Feb"},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-03-01", ids.US, "USD", "March rent",
		sp{acct: ids.Occupancy, amount: 150_000},
		sp{acct: ids.CheckingUS, amount: -150_000, desc: "Office rent check"},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-03-10", ids.US, "USD", "Annual insurance",
		sp{acct: ids.Insurance, amount: 60_000},
		sp{acct: ids.CheckingUS, amount: -60_000, desc: "Liability insurance premium"},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-03-31", ids.US, "USD", "Q1 bank fees",
		sp{acct: ids.BankFees, amount: 2_500},
		sp{acct: ids.CheckingUS, amount: -2_500, desc: "Quarterly account fees"},
	); err != nil {
		return err
	}

	// MXN cash expenses (program General) via the repeat description "Utilities".
	genProg := ids.General
	if _, err := post(
		ctx, s, "2025-03-15", ids.MX, "MXN", "Utilities",
		sp{acct: ids.FoodPurchases, amount: 120_000, prog: &genProg},
		sp{acct: ids.CashMXN, amount: -120_000},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2025-04-15", ids.MX, "MXN", "Utilities",
		sp{acct: ids.FoodPurchases, amount: 90_000, prog: &genProg},
		sp{acct: ids.CashMXN, amount: -90_000},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Restricted grant lifecycle -- Beca Agua 2025 (program Educacion).
	// ---------------------------------------------------------------------

	if _, err := post(
		ctx, s, "2025-04-01", ids.MX, "MXN", "Beca Agua grant receipt",
		sp{acct: ids.CheckingMX, amount: 10_000_000, fund: &ids.BecaAgua},
		sp{acct: ids.GovernmentGrants, amount: -10_000_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-04-05", ids.US, "USD", "Beca Agua US contribution",
		sp{acct: ids.CheckingUS, amount: 200_000, fund: &ids.BecaAgua, desc: "Restricted gift deposit"},
		sp{acct: ids.GovernmentGrants, amount: -200_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
	); err != nil {
		return err
	}

	// Restricted spend #1 -- MIXED 60/40 fund split (MX, MXN), proving per-fund
	// balancing.
	if _, err := post(
		ctx, s, "2025-05-10", ids.MX, "MXN", "Program supplies (mixed funding)",
		sp{acct: ids.ProgramSupplies, amount: 300_000, fund: &ids.BecaAgua, prog: &ids.Educacion, desc: "Water filters (grant-funded portion)"},
		sp{acct: ids.CheckingMX, amount: -300_000, fund: &ids.BecaAgua, desc: "Water filters (grant-funded portion)"},
		sp{acct: ids.ProgramSupplies, amount: 200_000, prog: &ids.Educacion, desc: "Water filters (general portion)"},
		sp{acct: ids.CheckingMX, amount: -200_000, desc: "Water filters (general portion)"},
	); err != nil {
		return err
	}

	// Restricted spend #2 -- in RV Estados Unidos (USD), proving the multi-sub fund
	// scope.
	if _, err := post(
		ctx, s, "2025-05-20", ids.US, "USD", "Program supplies (US)",
		sp{acct: ids.ProgramSupplies, amount: 150_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -150_000, fund: &ids.BecaAgua, desc: "Filter parts payment"},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Building Fund -- receipt applied to a Building ASSET PURCHASE.
	// ---------------------------------------------------------------------

	if _, err := post(
		ctx, s, "2025-06-01", ids.US, "USD", "Building Fund gift",
		sp{acct: ids.CheckingUS, amount: 5_000_000, fund: &ids.BuildingFund, desc: "Capital campaign gift"},
		sp{acct: ids.Contributions, amount: -5_000_000, fund: &ids.BuildingFund, prog: &genProg},
	); err != nil {
		return err
	}

	if _, err := post(
		ctx, s, "2025-06-15", ids.US, "USD", "Building improvement",
		sp{acct: ids.Building, amount: 4_000_000, fund: &ids.BuildingFund},
		sp{acct: ids.CheckingUS, amount: -4_000_000, fund: &ids.BuildingFund, desc: "Roof replacement payment"},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Intercompany funding pair (US -> MX via due-to/due-from). Both legs USD.
	// ---------------------------------------------------------------------

	if _, err := post(
		ctx, s, "2025-07-01", ids.US, "USD", "Intercompany funding to MX",
		sp{acct: ids.DueFromMX, amount: 1_000_000},
		sp{acct: ids.CheckingUS, amount: -1_000_000, desc: "Wire to RV Mexico"},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2025-07-02", ids.MX, "USD", "Intercompany funding from Intl",
		sp{acct: ids.FXClearing, amount: 1_000_000},
		sp{acct: ids.DueToIntl, amount: -1_000_000},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Cross-currency transfer via FX Clearing (two single-currency txns, D3).
	// ---------------------------------------------------------------------
	if _, err := post(
		ctx, s, "2025-08-01", ids.MX, "MXN", "FX transfer out (MXN)",
		sp{acct: ids.CashMXN, amount: -500_000},
		sp{acct: ids.FXClearing, amount: 500_000},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2025-08-01", ids.US, "USD", "FX transfer in (USD)",
		sp{acct: ids.FXClearing, amount: -26_000},
		sp{acct: ids.CheckingUS, amount: 26_000, desc: "Converted MXN settlement"},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// Event income + costs (Event Income is the UNMAPPED revenue leaf -> Z19).
	// ---------------------------------------------------------------------
	if _, err := post(
		ctx, s, "2025-09-15", ids.US, "USD", "Gala ticket sales",
		sp{acct: ids.CheckingUS, amount: 300_000, desc: "Gala ticket deposit"},
		sp{acct: ids.EventIncome, amount: -300_000, prog: &genProg},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2025-09-20", ids.US, "USD", "Gala catering",
		sp{acct: ids.EventCosts, amount: 100_000},
		sp{acct: ids.CheckingUS, amount: -100_000, desc: "Caterer invoice payment"},
	); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// A handful of 2026 transactions (so the fixture spans to 2026-06 and gives the
	// reconciliation something to leave uncleared).
	// ---------------------------------------------------------------------
	if _, err := post(
		ctx, s, "2026-01-10", ids.US, "USD", "Program fees Q1",
		sp{acct: ids.CheckingUS, amount: 120_000, desc: "Tuition fees received"},
		sp{acct: ids.ProgramFees, amount: -120_000},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2026-02-27", ids.US, "USD", "February 2026 salaries",
		sp{acct: ids.Salaries, amount: 850_000},
		sp{acct: ids.CheckingUS, amount: -850_000, desc: "Payroll run Feb 2026"},
	); err != nil {
		return err
	}
	if _, err := post(
		ctx, s, "2026-03-05", ids.MX, "MXN", "Pantry food restock",
		sp{acct: ids.FoodPurchases, amount: 150_000},
		sp{acct: ids.CashMXN, amount: -150_000},
	); err != nil {
		return err
	}
	// An uncleared item after the 2026-05-31 reconciliation.
	mayRent, err := post(
		ctx, s, "2026-05-25", ids.US, "USD", "May 2026 rent",
		sp{acct: ids.Occupancy, amount: 155_000},
		sp{acct: ids.CheckingUS, amount: -155_000},
	)
	if err != nil {
		return err
	}
	ids.MayRentTxn = mayRent
	// A second uncleared item after the reconciliation.
	juneDon, err := post(
		ctx, s, "2026-06-10", ids.US, "USD", "June donation",
		sp{acct: ids.CheckingUS, amount: 75_000},
		sp{acct: ids.Contributions, amount: -75_000},
	)
	if err != nil {
		return err
	}
	ids.JuneDonationTxn = juneDon

	// ---------------------------------------------------------------------
	// One transaction EDITED TWICE (as-of tests).
	// ---------------------------------------------------------------------
	if err := buildEditedTransaction(ctx, s, ids); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// One transaction DELETED (soft). Posted then soft-deleted.
	// ---------------------------------------------------------------------
	del, err := post(
		ctx, s, "2025-10-01", ids.US, "USD", "Erroneous entry (to delete)",
		sp{acct: ids.Insurance, amount: 40_000},
		sp{acct: ids.CheckingUS, amount: -40_000},
	)
	if err != nil {
		return err
	}
	if err := s.DeleteTransaction(ctx, del); err != nil {
		return fmt.Errorf("soft-delete transaction: %w", err)
	}
	ids.DeletedTxn = del
	return nil
}

// buildEditedTransaction posts a transaction and edits it twice, with the store's
// monotonic clock giving each version a distinct increasing timestamp. The edits
// change the fund and program, so an as-of query at the middle timestamp sees a
// state distinct from both the original and the final. The FINAL state is what the
// aggregates reflect.
func buildEditedTransaction(ctx context.Context, s *store.Store, ids *IDs) error {
	genProg := ids.General

	id, err := post(
		ctx, s, "2025-11-01", ids.US, "USD", "Supplies (v1)",
		sp{acct: ids.ProgramSupplies, amount: 50_000, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -50_000},
	)
	if err != nil {
		return err
	}
	ids.EditedTxn = id

	// Edit 1: Beca Agua-funded, Educacion. (Middle state for as-of tests.)
	if err := editTxn(
		ctx, s, id, "2025-11-01", ids.US, "USD", "Supplies (v2, restricted)",
		sp{acct: ids.ProgramSupplies, amount: 50_000, fund: &ids.BecaAgua, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -50_000, fund: &ids.BecaAgua},
	); err != nil {
		return err
	}

	// Edit 2 (FINAL): unrestricted, General program, amount 600.00.
	return editTxn(
		ctx, s, id, "2025-11-01", ids.US, "USD", "Supplies (v3, final)",
		sp{acct: ids.ProgramSupplies, amount: 60_000, prog: &genProg},
		sp{acct: ids.CheckingUS, amount: -60_000, desc: "Office supplies payment"},
	)
}

// editTxn replaces a transaction's header + split set (UpdateTransaction).
func editTxn(ctx context.Context, s *store.Store, id int64, date string, sub int64, ccy, memo string, splits ...sp) error {
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
		return fmt.Errorf("edit transaction %q: %w", memo, err)
	}
	return nil
}
