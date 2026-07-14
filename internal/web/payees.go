package web

import (
	"net/http"

	"cuento/internal/money"
)

// p12.3 payee autofill. One TxnWrite route feeding the transaction ENTRY flow
// (matching the editor's write-gating, since it exists only to author an entry): GET
// /payees/{id}/template?sub= returns the editor split-ROWS partial prefilled from the
// payee's last non-deleted transaction, with out-of-subsidiary splits dropped server
// -side (respects-subsidiary). The never-overwrites guard is CLIENT-side (txnpayee.js
// allRowsEmpty); the server always renders the template, the client decides to apply.
//
// p26.3: the ranked GET /payees/suggest fragment was REMOVED -- the header payee is now
// a single client-side combobox filtering the full payee option list, so the server
// suggest path was dead code.

// payeeTemplate handles GET /payees/{id}/template?sub=: the split-rows partial
// prefilled from the payee's last non-deleted transaction. Splits whose account is
// not valid in the selected subsidiary are DROPPED (respects-subsidiary) and a notice
// is set. Rows carry account, memo, amount (signed, user's number format), fund,
// program, and functional class -- exactly the fields txnEditForm prefills, mapped the
// same way. The client applies these rows only when the grid is empty (never
// overwrites, txnpayee.js).
func (s *server) payeeTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	payeeID := parseID(r.PathValue("id"))
	sub := parseID(r.URL.Query().Get("sub"))

	model, err := s.newEditorModel(ctx, u, sub)
	if err != nil {
		s.serverError(w)
		return
	}

	tpl, err := s.store.PayeeLastTransactionTemplate(ctx, payeeID)
	if err != nil {
		s.serverError(w)
		return
	}

	// The account set valid in the selected subsidiary (the SAME options the combobox
	// offers) -- template splits outside it are dropped.
	inSub := make(map[int64]bool, len(model.Accounts))
	for _, a := range model.Accounts {
		inSub[a.ID] = true
	}

	// Format amounts in the template transaction's currency (as txnEditForm does): the
	// prefill is display-only; the server re-validates the parsed amount on save.
	exp := s.currencyExponent(ctx, tpl.Currency)
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}

	var rows []txnRowModel
	dropped := false
	idx := 0
	for _, sp := range tpl.Splits {
		if !inSub[sp.AccountID] {
			dropped = true
			continue // account outside the selected subsidiary -> drop the row
		}
		row := txnRowModel{
			Index:   idx,
			Account: sp.AccountID,
			Amount:  money.Format(sp.Amount, exp, fmtOpts),
			Memo:    sp.Memo,
		}
		if sp.FundID.Valid {
			row.Fund = sp.FundID.Int64
		}
		if sp.ProgramID.Valid {
			row.Program = sp.ProgramID.Int64
		}
		if sp.FunctionalClass.Valid {
			row.Class = sp.FunctionalClass.String
		}
		if sp.Amount >= 0 {
			row.AmountDR = money.Format(sp.Amount, exp, fmtOpts)
		} else {
			row.AmountCR = money.Format(-sp.Amount, exp, fmtOpts)
		}
		rows = append(rows, row)
		idx++
	}
	model.Rows = rows
	if dropped {
		model.AutofillNotice = "txn.autofill.dropped_out_of_sub"
	}

	s.render(w, r, http.StatusOK, "payee-template", model)
}
