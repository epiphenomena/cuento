package reports

import "context"

// SmokeReportID is the id of the p15.1 framework smoke report — a THROWAWAY,
// clearly-labeled report that exists ONLY to prove the framework end to end (typed
// cells + indent + a subtotal row + both renderers + the auto-mounted route + the
// permission matrix) BEFORE the real report suite (p15.3–p15.11) lands. The leading
// underscore marks it as internal/placeholder; a later step deletes this file and
// its registration, and the framework carries on unchanged. It is NOT the trial
// balance (p15.3 owns that id and shape).
const SmokeReportID = "_smoke"

// registerSmoke registers the framework smoke report into reg. It is a real, if
// minimal, report: it reads a genuine as-of balance through the Toolkit's store
// (so the store wiring is exercised, not stubbed), and emits a small Table that
// covers every framework feature the renderers must handle — a text label, a money
// cell, an indented child row, and a subtotal row. The group is "financial" (a
// real declared group), so mounting it gates the route on ReportGroup("financial")
// and the permission matrix proves per-group enforcement automatically.
func registerSmoke(reg *Registry) {
	reg.Register(Report{
		ID:         SmokeReportID,
		TitleKey:   "reports.smoke.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true, Currency: true},
		Run:        runSmoke,
	})
}

// runSmoke computes the smoke Table. It reads the scope's as-of account balances
// through the store (proving the Toolkit → store path), sums them per currency into
// a single visible aggregate, and lays out: a heading row, one indented data row
// per (account, currency) balance, and a subtotal row. The numbers are only as
// meaningful as a placeholder needs — the point is the SHAPE flowing through the
// renderers, not a correct financial statement (that is p15.3+).
func runSmoke(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	balances, err := tk.Store().SubtreeBalancesAsOf(ctx, p.AsOf, p.Scope)
	if err != nil {
		return Table{}, err
	}

	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.smoke.col.line", Align: AlignLeft},
			{HeaderKey: "reports.smoke.col.amount", Align: AlignRight},
		},
	}

	// A top-level heading row (localized label, blank amount).
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{LabelCell("reports.smoke.section.balances"), BlankMoneyCell()},
		Kind:  RowData,
	})

	// One indented data row per balance cell, accumulating a per-currency subtotal.
	subtotal := make(map[string]int64)
	var order []string
	seen := make(map[string]bool)
	for _, b := range balances {
		t.Rows = append(t.Rows, Row{
			Cells:  []Cell{TextCell(accountLineLabel(b.AccountID, b.Currency)), MoneyCell(b.Amount, b.Currency)},
			Indent: 1,
			Kind:   RowData,
		})
		subtotal[b.Currency] += b.Amount
		if !seen[b.Currency] {
			seen[b.Currency] = true
			order = append(order, b.Currency)
		}
	}

	// A subtotal row per currency (net-debit sum; a trial balance nets to zero, so
	// this is a real, checkable quantity even for a placeholder).
	for _, ccy := range order {
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{LabelCell("reports.smoke.subtotal"), MoneyCell(subtotal[ccy], ccy)},
			Kind:  RowSubtotal,
		})
	}

	return t, nil
}

// accountLineLabel builds a placeholder line label for the smoke report. Real
// reports resolve account NAMES via the store (p15.3+); the smoke report keeps it
// trivially self-describing without a name lookup, since its purpose is the render
// path, not a named statement.
func accountLineLabel(accountID int64, currency string) string {
	return "acct #" + itoa(accountID) + " (" + currency + ")"
}

// itoa is a tiny int64→string helper local to the smoke report (avoids importing
// strconv into a throwaway file). Handles the placeholder's small ids fine.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
