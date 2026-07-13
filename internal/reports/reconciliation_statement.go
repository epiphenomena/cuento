package reports

import (
	"context"

	"cuento/internal/store"
)

// ReconciliationStatementReportID is the id (URL slug + registry key) of the
// reconciliation statement report (p16.4, group "reconciliation"): the printable
// STATEMENT DETAIL for ONE finalized bank reconciliation. It renders, in one columnar
// Table:
//
//   - a STATEMENT INFO preamble (account, statement date, statement balance, currency,
//     status) as label/value rows;
//   - the OPENING balance (the prior finalized statement balance for the same
//     account+currency -- the statement chain, PriorFinalizedStatementBalance);
//   - the INCLUDED (cleared) SPLITS -- every split cleared against this recon, in
//     date order, reusing the account-ledger register-row shape (date linked to its
//     txn, payee/description, fund, amount);
//   - the CLEARED TOTAL (the net-debit sum of those splits);
//   - the CLOSING balance = opening + cleared, ASSERTED to equal the statement balance
//     (the recon's own finalize gate / ledger Z9), so the report proves the chain.
//
// PARAMETER: a single finalized RECONCILIATION (report-specific, ParamsSpec.Reconciliation),
// parsed from ?reconciliation= (or ?recon=) and validated against the real finalized-recon
// set. NO subsidiary scope narrowing and NO currency conversion: a reconciliation is per
// (account, currency) across ALL funds and ALL subsidiaries (D13/D20), so the included set
// is fully identified by the recon id -- scoping it would shrink the set and break the
// chain. The scope selector still renders (every report is scoped, D18) but is inert.
//
// LINE -> TXN (p12.4): each split line's date cell carries WithTxn(txnID), which the web
// layer renders as a link to the transaction editor/history -- a reviewer clicks a cleared
// line to open its entry.
//
// NO RECONCILIATION CHOSEN (Reconciliation == 0): the report returns an empty Table (the
// framework's nothing-to-show rule), so a bare /reports/reconciliation_statement hit renders
// 200 with just the params form (the permission-matrix / scope-selector test path).
const ReconciliationStatementReportID = "reconciliation_statement"

// registerReconciliationStatement registers the reconciliation statement report (p16.4)
// into reg under the "reconciliation" group. It offers ONLY the report-specific
// RECONCILIATION selector (no period/currency -- the statement is a fixed, finalized
// artifact); the shared web params form renders it from the ParamsSpec.
func registerReconciliationStatement(reg *Registry) {
	reg.Register(Report{
		ID:         ReconciliationStatementReportID,
		TitleKey:   "reports.reconciliation_statement.title",
		Group:      "reconciliation",
		ParamsSpec: ParamsSpec{Reconciliation: true},
		Run:        runReconciliationStatement,
	})
}

// runReconciliationStatement computes the statement-detail Table (p16.4). It loads the
// chosen recon, its opening balance (prior finalized statement -- the chain), and its
// included (cleared) splits, then renders: a statement-info preamble, the opening row,
// one data row per cleared split, the cleared total, and the closing row (opening +
// cleared) -- which EQUALS the statement balance by the recon's finalize gate. The
// closing == statement-balance identity is asserted here (and re-derived from the emitted
// cells by the golden test), so a broken chain surfaces rather than rendering a wrong
// figure silently.
func runReconciliationStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.reconciliation_statement.col.date", Align: AlignLeft},
			{HeaderKey: "reports.reconciliation_statement.col.description", Align: AlignLeft},
			{HeaderKey: "reports.reconciliation_statement.col.fund", Align: AlignLeft},
			{HeaderKey: "reports.reconciliation_statement.col.amount", Align: AlignRight},
		},
	}

	// No reconciliation chosen: an empty table (the framework's nothing-to-show rule),
	// so a bare hit renders 200 with just the params form.
	if p.Reconciliation == 0 {
		return t, nil
	}

	st := tk.Store()
	recon, err := st.GetReconciliation(ctx, p.Reconciliation)
	if err != nil {
		return Table{}, err
	}
	ccy := recon.Currency

	// Opening = the prior finalized statement balance for this (account, currency) --
	// the statement chain (the SAME query Finalize uses). Reuse the store's summary,
	// which reads opening + cleared from Finalize's own queries, so the report's figures
	// are byte-identical to the finalize gate.
	sum, err := st.ReconciliationSummaryFor(ctx, p.Reconciliation)
	if err != nil {
		return Table{}, err
	}

	// Included (cleared) splits, chronological. Sum(these) == sum.Cleared by construction
	// (same predicate as ReconciliationClearedSum).
	splits, err := st.ReconciliationStatementSplits(ctx, p.Reconciliation)
	if err != nil {
		return Table{}, err
	}

	payees, err := payeeNames(ctx, st)
	if err != nil {
		return Table{}, err
	}
	funds, err := fundNames(ctx, st)
	if err != nil {
		return Table{}, err
	}
	acctName, err := st.AccountName(ctx, recon.AccountID, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// --- Statement info preamble: label/value rows (date/description columns carry the
	// label + value; the fund/amount columns are blank except statement balance, which
	// sits in the amount column so it aligns with the money figures below).
	t.Rows = append(
		t.Rows,
		infoRow("reports.reconciliation_statement.info.account", TextCell(acctName)),
		infoRow("reports.reconciliation_statement.info.statement_date", DateCell(recon.StatementDate)),
		infoRow("reports.reconciliation_statement.info.currency", TextCell(ccy)),
		infoRow("reports.reconciliation_statement.info.status",
			LabelCell("reports.reconciliation_statement.status."+recon.Status)),
	)
	// Statement balance in the amount column (aligned with the money figures).
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			LabelCell("reports.reconciliation_statement.info.statement_balance"),
			TextCell(""),
			TextCell(""),
			MoneyCell(recon.StatementBalance, ccy),
		},
		Kind: RowSubtotal,
	})

	// --- Opening row (the prior finalized statement balance -- the chain).
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			DateCell(recon.StatementDate),
			LabelCell("reports.reconciliation_statement.opening"),
			TextCell(""),
			MoneyCell(sum.Opening, ccy),
		},
		Kind: RowSubtotal,
	})

	// --- Included (cleared) split lines, in date order (register-row shape).
	for _, sp := range splits {
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				DateCell(sp.Date).WithTxn(sp.TxnID),
				TextCell(statementDescription(sp, payees)),
				statementFundCell(sp.FundID, funds),
				MoneyCell(sp.Amount, ccy),
			},
			Kind: RowData,
		})
	}

	// --- Cleared total (net-debit sum of the included splits).
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			DateCell(recon.StatementDate),
			LabelCell("reports.reconciliation_statement.cleared_total"),
			TextCell(""),
			MoneyCell(sum.Cleared, ccy),
		},
		Kind: RowSubtotal,
	})

	// --- Closing = opening + cleared. By the recon's finalize gate (ledger Z9) this
	// EQUALS the statement balance; the golden test re-derives the same equality from the
	// emitted cells. We render the computed opening+cleared (not the stored statement
	// balance) so a broken chain would show a visible divergence rather than being masked.
	closing := sum.Opening + sum.Cleared
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			DateCell(recon.StatementDate),
			LabelCell("reports.reconciliation_statement.closing"),
			TextCell(""),
			MoneyCell(closing, ccy),
		},
		Kind: RowTotal,
	})

	return t, nil
}

// infoRow builds a statement-info preamble row: a localized LABEL in the description
// column with a value cell beside it, the other columns blank. The date column carries
// the label so the preamble reads label-then-value left to right.
func infoRow(labelKey string, value Cell) Row {
	return Row{
		Cells: []Cell{
			LabelCell(labelKey),
			value,
			TextCell(""),
			BlankMoneyCell(),
		},
		Kind: RowSubtotal,
	}
}

// statementDescription is a cleared line's Description cell text: the payee name when
// the split's transaction names one, else the split memo, else the transaction memo --
// the same fallback the account-ledger register uses.
func statementDescription(sp store.ReconciliationStatementSplit, payees map[int64]string) string {
	if sp.PayeeID != nil {
		if name := payees[*sp.PayeeID]; name != "" {
			return name
		}
	}
	if sp.SplitMemo != "" {
		return sp.SplitMemo
	}
	return sp.TxnMemo
}

// statementFundCell builds the FUND column cell for a cleared line: the fund's name (a
// stored proper noun) for a restricted split, or the localized "Unrestricted" label for
// a nil-fund split (D20). Mirrors the account-ledger's fundCell.
func statementFundCell(fundID *int64, funds map[int64]string) Cell {
	if fundID == nil {
		return LabelCell("reports.reconciliation_statement.unrestricted")
	}
	return TextCell(funds[*fundID])
}
