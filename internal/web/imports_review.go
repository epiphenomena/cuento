package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cuento/internal/money"
	"cuento/internal/store"
)

// p17.3 bank-CSV review queue -> post. After p17.2 stages a batch's rows as pending,
// the reviewer works the queue: a per-batch PENDING list with a progress indicator,
// an "edit & post" that opens the phase-12 editor PREFILLED from the staged row (the
// batch account line + payee-template counter-splits, with the batch's SUBSIDIARY
// LOCKED), and a discard-with-reason action. Posting creates a real balanced,
// versioned ledger transaction and LINKS the row (store.PostImportRow, atomic); the
// posted transaction is the row's audit, and a discarded row's audit is the change
// carrying the reason.
//
// Routes (all TxnWrite -- this is an import-into-ledger workflow; even VIEWING the
// queue is a write-adjacent staging surface and the actions mutate the ledger, so the
// view perm is TxnWrite too, documented in DECISIONS p17.3):
//   GET  /import/batches/{id}         the batch queue + progress
//   GET  /import/rows/{id}/edit       the phase-12 editor prefilled, sub locked
//   POST /import/rows/{id}/post       create the balanced txn + link the row
//   POST /import/rows/{id}/discard    discard with a reason (writes a change)

// importQueueModel is the batch-queue page: the progress indicator + the pending rows
// (with the duplicate flag), plus any already-actioned rows for context.
type importQueueModel struct {
	BatchID  int64
	Filename string
	Account  string
	Sub      string
	Progress importProgressModel
	Rows     []importQueueRow
	ErrorKey string
	ErrorArg string
}

// importProgressModel is the "12 of 40 posted, 3 discarded, 25 pending" indicator.
type importProgressModel struct {
	Total     int
	Pending   int
	Posted    int
	Discarded int
}

// importQueueRow is one staged row in the queue: its display fields, status, the
// duplicate flag, and (when posted) the linked transaction id for a register link.
type importQueueRow struct {
	ID          int64
	Date        string
	AmountFmt   string
	Payee       string
	Memo        string
	Status      string
	Duplicate   bool
	PostedTxnID int64 // 0 unless posted
}

// importBatchQueue handles GET /import/batches/{id} (TxnWrite): the review queue.
func (s *server) importBatchQueue(w http.ResponseWriter, r *http.Request) {
	batchID := parseID(r.PathValue("id"))
	model, err := s.buildImportQueue(r, batchID, "", "")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "import_queue.tmpl", s.newShellPage(r, model))
}

// buildImportQueue assembles the batch queue model: batch header, progress counts
// (computed in Go from the rows -- batches are tiny), and the row list with the
// duplicate flag recomputed against the account's existing-dedupe set (the same
// advisory flagging the staging pass used, so a re-uploaded duplicate keeps showing).
func (s *server) buildImportQueue(r *http.Request, batchID int64, errKey, errArg string) (importQueueModel, error) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	batch, err := s.store.GetImportBatch(ctx, batchID)
	if err != nil {
		return importQueueModel{}, err
	}
	rows, err := s.store.ImportRowsForBatchFlagged(ctx, batchID)
	if err != nil {
		return importQueueModel{}, err
	}

	exp := s.currencyExponentForAccount(ctx, batch.AccountID)
	currency := s.accountCurrency(ctx, batch.AccountID)
	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	model := importQueueModel{
		BatchID:  batchID,
		Filename: batch.Filename,
		Account:  s.accountName(ctx, batch.AccountID, lang),
		Sub:      s.subsidiaryName(ctx, batch.SubsidiaryID),
		ErrorKey: errKey,
		ErrorArg: errArg,
	}
	model.Progress.Total = len(rows)
	for _, row := range rows {
		switch row.Status {
		case "posted":
			model.Progress.Posted++
		case "discarded":
			model.Progress.Discarded++
		default:
			model.Progress.Pending++
		}
		amt := int64(0)
		if row.AmountMinor != nil {
			amt = *row.AmountMinor
		}
		qr := importQueueRow{
			ID:        row.ID,
			Date:      money.FormatDate(parseISOForDisplay(row.Date), df),
			AmountFmt: currency + " " + money.Format(amt, exp, opts),
			Payee:     row.Payee,
			Memo:      row.Memo,
			Status:    row.Status,
			Duplicate: row.Duplicate,
		}
		if row.PostedTxnID != nil {
			qr.PostedTxnID = *row.PostedTxnID
		}
		model.Rows = append(model.Rows, qr)
	}
	return model, nil
}

// importRowEditForm handles GET /import/rows/{id}/edit (TxnWrite): open the phase-12
// editor PREFILLED from the staged row. Row 0 is the batch-account line (signed
// parsed amount, date, payee name, memo); the counter-splits are prefilled via the
// payee template (fund + program + functional class carried), with the batch's
// SUBSIDIARY LOCKED. A non-pending row is redirected back to the queue.
func (s *server) importRowEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	rowID := parseID(r.PathValue("id"))

	row, err := s.store.GetImportRow(ctx, rowID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if row.Status != "pending" {
		// Already posted/discarded: nothing to edit -- back to the queue.
		http.Redirect(w, r, "/import/batches/"+strconv.FormatInt(row.BatchID, 10), http.StatusSeeOther)
		return
	}

	model, err := s.newEditorModel(ctx, u, row.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	model.ImportRowID = rowID
	model.ImportBatchID = row.BatchID
	model.FirstErrorRow = -1
	model.Date = money.FormatDate(parseISOForDisplay(row.Date), dateFormatFor(u))
	model.Memo = row.Memo

	// Resolve the parsed payee to an existing payee by name (READ-ONLY -- never create
	// at GET; creation happens on POST via create-on-save). A match seeds the payee id
	// + the template counter-splits.
	var payeeID int64
	if row.Payee != "" {
		if id, err := s.store.LookupPayeeByName(ctx, row.Payee); err == nil {
			payeeID = id
		}
	}
	model.Payee = payeeID
	model.PayeeName = row.Payee

	model.Rows = s.prefillImportRows(ctx, u, model, row, payeeID)
	if err := s.injectRowAccounts(ctx, &model); err != nil { // p26.10: never blank a referenced account
		s.serverError(w)
		return
	}
	s.renderEditor(w, r, model)
}

// prefillImportRows builds the editor rows for an "edit & post": the batch-account
// line first (the bank side, authoritative from the staged row), then the payee's
// template counter-splits (p12.3) EXCLUDING any split already on the batch account
// (else the bank line is doubled). The template splits carry fund + program +
// functional class; splits whose account is outside the locked subsidiary are dropped
// (respects-subsidiary). If a counter split carries a restricted fund, the bank line
// is given the SAME fund so per-fund zero-sum can hold (D20) -- the user still adjusts
// and the server re-validates.
func (s *server) prefillImportRows(ctx context.Context, u *store.CurrentUser, model txnEditorModel, row store.ImportRow, payeeID int64) []txnRowModel {
	exp := s.currencyExponentForAccount(ctx, row.AccountID)
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}

	// The bank line: the batch account, the signed parsed amount, memo.
	bankFund := int64(0)
	bank := txnRowModel{
		Index:   0,
		Account: row.AccountID,
		Amount:  money.Format(row.AmountMinor, exp, fmtOpts),
		Memo:    row.Memo,
	}
	if row.AmountMinor >= 0 {
		bank.AmountDR = money.Format(row.AmountMinor, exp, fmtOpts)
	} else {
		bank.AmountCR = money.Format(-row.AmountMinor, exp, fmtOpts)
	}

	rows := []txnRowModel{bank}

	if payeeID == 0 {
		// No known payee -> just the bank line and one empty counter row to fill in.
		rows = append(rows, txnRowModel{Index: 1})
		return rows
	}

	tpl, err := s.store.PayeeLastTransactionTemplate(ctx, payeeID)
	if err != nil || !tpl.Found {
		rows = append(rows, txnRowModel{Index: 1})
		return rows
	}

	inSub := make(map[int64]bool, len(model.Accounts))
	for _, a := range model.Accounts {
		inSub[a.ID] = true
	}

	idx := 1
	for _, sp := range tpl.Splits {
		if sp.AccountID == row.AccountID {
			// A template split on the batch account: don't double the bank line. Carry
			// its fund onto the bank line so a restricted counter balances per-fund.
			if sp.FundID.Valid {
				bankFund = sp.FundID.Int64
			}
			continue
		}
		if !inSub[sp.AccountID] {
			continue // outside the locked subsidiary -> drop
		}
		cr := txnRowModel{
			Index:   idx,
			Account: sp.AccountID,
			Amount:  money.Format(sp.Amount, exp, fmtOpts),
			Memo:    sp.Memo,
		}
		if sp.FundID.Valid {
			cr.Fund = sp.FundID.Int64
			if bankFund == 0 {
				bankFund = sp.FundID.Int64
			}
		}
		if sp.ProgramID.Valid {
			cr.Program = sp.ProgramID.Int64
		}
		if sp.FunctionalClass.Valid {
			cr.Class = sp.FunctionalClass.String
		}
		if sp.Amount >= 0 {
			cr.AmountDR = money.Format(sp.Amount, exp, fmtOpts)
		} else {
			cr.AmountCR = money.Format(-sp.Amount, exp, fmtOpts)
		}
		rows = append(rows, cr)
		idx++
	}
	if idx == 1 {
		// Every template split was the batch account or out-of-sub -> one empty counter.
		rows = append(rows, txnRowModel{Index: 1})
	}
	// Apply the carried fund onto the bank line (D20: restricted counter forces the
	// cash side into the fund so per-fund zero-sum can hold).
	if bankFund != 0 {
		rows[0].Fund = bankFund
	}
	return rows
}

// importRowPost handles POST /import/rows/{id}/post (TxnWrite): parse the (possibly
// user-adjusted) editor splits, create the balanced ledger transaction, and LINK the
// staged row (store.PostImportRow, atomic). The store is the sole validator: an
// unbalanced/invalid post re-renders the editor at 422 with the error routed to its
// slot (reusing txnSubmit's machinery). On success, HX-Redirect to the batch queue.
func (s *server) importRowPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	rowID := parseID(r.PathValue("id"))

	row, err := s.store.GetImportRow(ctx, rowID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}

	// The subsidiary is LOCKED to the batch's -- ignore any posted value, use the row's.
	subID := row.SubsidiaryID
	currency := r.FormValue("currency")

	model, err := s.newEditorModel(ctx, u, subID)
	if err != nil {
		s.serverError(w)
		return
	}
	model.ImportRowID = rowID
	model.ImportBatchID = row.BatchID
	model.Currency = currency
	model.Payee = parseID(r.FormValue("payee"))
	model.PayeeName = trimSpace(r.FormValue("payee_name"))
	model.Memo = r.FormValue("memo")
	model.Notes = r.FormValue("notes")
	model.FirstErrorRow = -1

	// Create-on-save payee (p12.3): a picked id wins; else find-or-create by typed name
	// (its own change, before the txn write, so a validation failure leaves it reusable).
	payeeID := model.Payee
	if payeeID == 0 && model.PayeeName != "" {
		payeeID, err = s.store.EnsurePayee(s.actorCtx(ctx), model.PayeeName)
		if err != nil {
			s.serverError(w)
			return
		}
		model.Payee = payeeID
	}

	dateISO, dateOK := parseEditorDate(r.FormValue("date"), dateFormatFor(u), s.now())
	model.Date = r.FormValue("date")

	rows, splits := s.parseSplitForms(r, s.currencyExponent(ctx, currency))
	model.Rows = rows
	_ = s.injectRowAccounts(ctx, &model) // p26.10: keep a referenced account SELECTED on 422

	if !dateOK {
		model.TotalsError = "error.txn.bad_date"
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	in := store.PostTransactionInput{
		Date:         dateISO,
		SubsidiaryID: subID,
		Memo:         model.Memo,
		Notes:        model.Notes,
		Currency:     currency,
		Splits:       splits,
	}
	if payeeID != 0 {
		p := payeeID
		in.PayeeID = &p
	}

	if _, err := s.store.PostImportRow(s.actorCtx(ctx), rowID, in); err != nil {
		if errors.Is(err, store.ErrImportRowNotPending) {
			// Someone posted/discarded it meanwhile: send them back to the queue.
			dest := "/import/batches/" + strconv.FormatInt(row.BatchID, 10)
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", dest)
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		s.routeTxnError(&model, err, splits)
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	dest := "/import/batches/" + strconv.FormatInt(row.BatchID, 10)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// importRowDiscard handles POST /import/rows/{id}/discard (TxnWrite): discard the
// staged row with a REASON (store.DiscardImportRow records the reason as the change's
// note). A missing reason re-renders the queue at 422 with the discard-reason error.
func (s *server) importRowDiscard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rowID := parseID(r.PathValue("id"))

	row, err := s.store.GetImportRow(ctx, rowID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}
	reason := trimSpace(r.FormValue("reason"))

	if err := s.store.DiscardImportRow(s.actorCtx(ctx), rowID, reason); err != nil {
		if errors.Is(err, store.ErrDiscardReasonRequired) {
			model, berr := s.buildImportQueue(r, row.BatchID, "import.discard_reason_required", "")
			if berr != nil {
				s.serverError(w)
				return
			}
			s.render(w, r, http.StatusUnprocessableEntity, "import_queue.tmpl", s.newShellPage(r, model))
			return
		}
		if errors.Is(err, store.ErrImportRowNotPending) {
			http.Redirect(w, r, "/import/batches/"+strconv.FormatInt(row.BatchID, 10), http.StatusSeeOther)
			return
		}
		s.serverError(w)
		return
	}

	dest := "/import/batches/" + strconv.FormatInt(row.BatchID, 10)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// --- small helpers --------------------------------------------------------

// currencyExponentForAccount returns the minor-unit exponent for an account's default
// currency (for amount format), defaulting to 2 on any miss.
func (s *server) currencyExponentForAccount(ctx context.Context, accountID int64) int {
	return s.currencyExponent(ctx, s.accountCurrency(ctx, accountID))
}

// accountCurrency returns an account's default currency code, or "USD" on any miss.
func (s *server) accountCurrency(ctx context.Context, accountID int64) string {
	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return "USD"
	}
	return acct.DefaultCurrency
}

func trimSpace(s string) string { return strings.TrimSpace(s) }
