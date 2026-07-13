package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cuento/internal/money"
	"cuento/internal/store"
)

// p12.4 void (delete = soft-delete with confirm, TxnWrite). Voiding a transaction is
// destructive-feeling and irreversible-in-the-UI, so it is a TWO-STEP flow mirroring
// the merge-accounts confirm UX (merge.go):
//
//   1. GET  /transactions/{id}/void  -> a REVIEW page summarizing the transaction
//      (date, payee, memo, split lines) and a Confirm button. ZERO writes.
//   2. POST /transactions/{id}/void with confirm=1 -> store.DeleteTransaction (rule
//      14 soft-delete: the header's deleted flag flips and a transactions_versions
//      op='delete' is appended; splits are left live -- the as-of/history queries
//      exclude the txn by its own delete row). A POST WITHOUT confirm re-renders the
//      review (defensive; the form always carries confirm=1).
//
// Void != a hard delete: nothing is removed, the audit trail (changes + versions)
// grows by one delete row, and the history panel still renders the txn (DECISIONS
// p12.4). GetTransaction guards both steps, so a re-void of an already-voided txn
// 404s (GetTransaction returns ErrTransactionNotFound for a soft-deleted row).

// voidReviewModel is the review page: the transaction summary shown before confirm.
type voidReviewModel struct {
	TxnID    int64
	Date     string // formatted per the user's date format (rule 10)
	Payee    string // "" = none
	Memo     string // "" = none
	Lines    []voidLine
	ErrorKey string // "" = none; an i18n key shown as a banner (p16.5 recon lock)
}

// voidLine is one split line in the void review (account + formatted amount + fund).
type voidLine struct {
	Account string
	Amount  string
	Fund    string // "" = unrestricted
}

// voidReview handles GET /transactions/{id}/void (TxnWrite): the confirm-review page.
// A soft-deleted or missing txn 404s (GetTransaction).
func (s *server) voidReview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := parseID(r.PathValue("id"))

	model, err := s.buildVoidReview(ctx, u, lang, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "void.tmpl", s.newShellPage(r, model))
}

// void handles POST /transactions/{id}/void (TxnWrite): with confirm=1 it executes
// the soft-delete; without confirm it re-renders the review (defensive). On success
// it redirects to the chart of accounts (the txn no longer appears in any register).
func (s *server) void(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := parseID(r.PathValue("id"))

	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}
	if r.PostFormValue("confirm") == "" {
		// No confirm flag: fall back to the review (never void without an explicit
		// confirm). A missing/voided txn 404s.
		model, err := s.buildVoidReview(ctx, u, lang, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.render(w, r, http.StatusOK, "void.tmpl", s.newShellPage(r, model))
		return
	}

	if err := s.store.DeleteTransaction(s.actorCtx(ctx), id); err != nil {
		if errors.Is(err, store.ErrTransactionNotFound) {
			http.NotFound(w, r)
			return
		}
		// A split of this txn is locked by a FINALIZED reconciliation (p16.5): voiding
		// would drop it from the statement. Re-render the review with a clean banner at
		// 409, not a 500 -- the user must reopen the reconciliation first. The txn is
		// still live (the store rolled the void back), so buildVoidReview succeeds.
		if errors.Is(err, store.ErrSplitReconciled) {
			model, berr := s.buildVoidReview(ctx, u, lang, id)
			if berr != nil {
				http.NotFound(w, r)
				return
			}
			model.ErrorKey = "void.error.reconciled"
			s.render(w, r, http.StatusConflict, "void.tmpl", s.newShellPage(r, model))
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/accounts")
}

// buildVoidReview assembles the review model from the LIVE transaction (header +
// splits), formatting the date/amounts (rule 10) and resolving account/payee/fund
// names (rule 9). It returns an error (the handler 404s) for a missing or already-
// voided transaction (GetTransaction).
func (s *server) buildVoidReview(ctx context.Context, u *store.CurrentUser, lang string, id int64) (voidReviewModel, error) {
	hdr, err := s.store.GetTransaction(ctx, id)
	if err != nil {
		return voidReviewModel{}, err
	}
	splits, err := s.store.TransactionSplits(ctx, id)
	if err != nil {
		return voidReviewModel{}, err
	}

	accounts, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		return voidReviewModel{}, err
	}
	funds, err := fundNameMap(ctx, s.store)
	if err != nil {
		return voidReviewModel{}, err
	}
	payees, err := payeeNameMap(ctx, s.store)
	if err != nil {
		return voidReviewModel{}, err
	}

	exp := s.currencyExponent(ctx, hdr.Currency)
	opts := formatOptsFor(u)

	model := voidReviewModel{
		TxnID: id,
		Date:  money.FormatDate(parseISOForDisplay(hdr.Date), dateFormatFor(u)),
		Memo:  hdr.Memo,
	}
	if hdr.PayeeID.Valid {
		model.Payee = payees[hdr.PayeeID.Int64]
	}
	for _, sp := range splits {
		line := voidLine{
			Account: accounts[sp.AccountID],
			Amount:  hdr.Currency + " " + money.Format(sp.Amount, exp, opts),
		}
		if sp.FundID.Valid {
			line.Fund = funds[sp.FundID.Int64]
		}
		model.Lines = append(model.Lines, line)
	}
	return model, nil
}

// txnDuplicate handles GET /transactions/{id}/duplicate (TxnWrite): it opens the
// editor prefilled from an existing transaction's header + splits as a NEW UNSAVED
// entry (no txn id, no split ids), so saving POSTs to /transactions (a create). The
// payee and memo are copied (a duplicate is usually the same payee/purpose); the
// date defaults to TODAY, not the original's, since a duplicate is a fresh entry made
// now (DECISIONS p12.4). It reuses the p12.2 editor model machinery.
func (s *server) txnDuplicate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))

	hdr, err := s.store.GetTransaction(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	splits, err := s.store.TransactionSplits(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}

	model, err := s.newEditorModel(ctx, u, hdr.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	// A NEW unsaved entry: no txn id, not an edit -> POST /transactions on save.
	model.TxnID = 0
	model.IsEdit = false
	model.Currency = hdr.Currency
	// Date defaults to today (a duplicate is a fresh entry made now, DECISIONS p12.4).
	model.Date = money.FormatDate(time.Now(), dateFormatFor(u))
	// Payee + memo copied from the source (same payee/purpose is the common case).
	if hdr.PayeeID.Valid {
		model.Payee = hdr.PayeeID.Int64
		for _, p := range model.Payees {
			if p.ID == model.Payee {
				model.PayeeName = p.Name
				break
			}
		}
	}
	model.Memo = hdr.Memo

	exp := s.currencyExponent(ctx, hdr.Currency)
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}
	for i, sp := range splits {
		// SplitID left "" so each row is a NEW split (no diff-by-id, trap 1) -> the
		// store inserts fresh splits under the new transaction.
		row := txnRowModel{
			Index:   i,
			SplitID: "",
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
		model.Rows = append(model.Rows, row)
	}
	s.renderEditor(w, r, model)
}
