package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p20.3 reviewer queue -> convert / reject (TxnWrite). This is the MIRROR of the
// bank-import review->post (p17.3, imports_review.go): a submitter (p20.2) files an
// expense report of proposed R/E splits that need NOT balance; an EDITING user (the
// reviewer) works a queue of SUBMITTED reports across ALL submitters, opens one in the
// phase-12 editor PREFILLED from the report's lines with its subsidiary LOCKED, adjusts
// the splits to BALANCE (adding the cash/bank counter-side), and POSTS -- creating a
// real, balanced, versioned ledger transaction AND converting the report to it in ONE
// atomic change (store.PostAndConvertExpenseReport); OR REJECTS it with a reason routing
// it back to the submitter (p20.2 shows the reason, the submitter resubmits). A
// converted report is TERMINAL/immutable and shows a link to its posted transaction.
//
// PERM: the reviewer queue + convert + reject = TxnWrite (editing the books). This is a
// DISTINCT surface from the p20.2 submitter workspace (ExpenseSubmit): the literal
// "/expenses/review" beats the "/expenses/{id}" wildcard (Go 1.22+ mux), so a PURE
// submitter hitting the review routes is gated by TxnWrite -> 403 (the two roles are
// separate; the permission matrix auto-covers this). The reviewer reads EVERY
// submitter's reports (ExpenseReportsByStatus), so these handlers do NOT use the p20.2
// loadOwnedReport (which enforces submitter==self).
//
// Routes (all TxnWrite):
//   GET  /expenses/review                the queue (all submitted reports, + history)
//   GET  /expenses/review/{id}           the p12 editor prefilled, subsidiary locked
//   POST /expenses/review/{id}/post      create the balanced txn + convert (atomic)
//   POST /expenses/review/{id}/reject    reject with a reason (routes back to submitter)
//
// Strict CSP, i18n both catalogs, form-error convention (unbalanced post -> 422 partial;
// missing reject reason -> 422). All money/dates via the formatters (rule 10); every
// string via {{t}} (rule 9); account/fund/subsidiary names are stored proper nouns.

// ===========================================================================
// REVIEW QUEUE
// ===========================================================================

// reviewQueueRow is one report on the reviewer queue: its id, submitter + subsidiary
// names, line count + formatted total (the sum of its lines' magnitudes in the sub's
// base currency), a localized status key, and -- when converted -- the posted txn id
// for a "view transaction" link.
type reviewQueueRow struct {
	ID          int64
	Submitter   string
	SubName     string
	LineCount   int
	TotalFmt    string
	StatusKey   string // expense.status.<status>
	ReviewNotes string // the reviewer's reason (rejected only)
	Rejected    bool
	Converted   bool
	PostedTxnID int64 // 0 unless converted
}

// reviewQueueModel is the GET /expenses/review page: the submitted reports awaiting
// review (the working set) plus a count indicator, and -- for context -- the recently
// rejected/converted reports (history, "if clean").
type reviewQueueModel struct {
	Pending    []reviewQueueRow
	History    []reviewQueueRow
	NumPending int
}

// expenseReview handles GET /expenses/review (TxnWrite): the reviewer queue. Submitted
// reports are the working set; rejected + converted reports are shown as history.
func (s *server) expenseReview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	submitted, err := s.store.ExpenseReportsByStatus(ctx, "submitted")
	if err != nil {
		s.serverError(w)
		return
	}
	model, err := s.buildReviewQueue(r, submitted)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "expense_review.tmpl", s.newShellPage(r, model))
}

// buildReviewQueue assembles the queue model: the submitted reports (the working set)
// plus the rejected + converted reports as history, each row carrying submitter +
// subsidiary names, the line count + summed magnitude, and (converted) the posted txn
// id.
func (s *server) buildReviewQueue(r *http.Request, submitted []sqlc.ExpenseReport) (reviewQueueModel, error) {
	ctx := r.Context()
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		return reviewQueueModel{}, err
	}
	userNames, err := s.submitterNameMap(ctx)
	if err != nil {
		return reviewQueueModel{}, err
	}

	model := reviewQueueModel{NumPending: len(submitted)}
	for _, rep := range submitted {
		row, err := s.buildReviewRow(ctx, rep, subNames, userNames)
		if err != nil {
			return reviewQueueModel{}, err
		}
		model.Pending = append(model.Pending, row)
	}
	// History (context, "if clean"): converted + rejected reports. Kept minimal.
	for _, status := range []string{"converted", "rejected"} {
		reports, err := s.store.ExpenseReportsByStatus(ctx, status)
		if err != nil {
			return reviewQueueModel{}, err
		}
		for _, rep := range reports {
			row, err := s.buildReviewRow(ctx, rep, subNames, userNames)
			if err != nil {
				return reviewQueueModel{}, err
			}
			model.History = append(model.History, row)
		}
	}
	return model, nil
}

// submitterNameMap maps a user id -> a display name (falling back to the username),
// for labeling a report's submitter on the queue.
func (s *server) submitterNameMap(ctx context.Context) (map[ids.UserID]string, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[ids.UserID]string, len(users))
	for _, u := range users {
		name := u.DisplayName
		if name == "" {
			name = u.Username
		}
		m[u.ID] = name
	}
	return m, nil
}

// buildReviewRow assembles one queue row: submitter + subsidiary names, the line count,
// the summed magnitude formatted in the report's subsidiary base currency, the status
// key, and (converted) the posted txn id.
func (s *server) buildReviewRow(ctx context.Context, rep sqlc.ExpenseReport, subNames map[int64]string, userNames map[ids.UserID]string) (reviewQueueRow, error) {
	u := currentUser(ctx)

	lines, err := s.store.ExpenseReportLines(ctx, rep.ID)
	if err != nil {
		return reviewQueueRow{}, err
	}
	var total int64
	for _, l := range lines {
		total += displayAmount(l.Amount)
	}
	exp := s.reportExponent(ctx, rep)
	row := reviewQueueRow{
		ID:        int64(rep.ID),
		Submitter: userNames[rep.SubmitterID],
		SubName:   subNames[int64(rep.SubsidiaryID)],
		LineCount: len(lines),
		TotalFmt:  money.FormatMoney(total, s.reportCurrency(ctx, rep), exp, formatOptsFor(u)),
		StatusKey: "expense.status." + rep.Status,
	}
	switch rep.Status {
	case "rejected":
		row.Rejected = true
		row.ReviewNotes = rep.ReviewNotes
	case "converted":
		row.Converted = true
		if rep.PostedTransactionID.Valid {
			row.PostedTxnID = rep.PostedTransactionID.Int64
		}
	}
	return row, nil
}

// ===========================================================================
// REVIEW & POST (the phase-12 editor, prefilled + sub locked)
// ===========================================================================

// expenseReviewForm handles GET /expenses/review/{id} (TxnWrite): open the phase-12
// editor PREFILLED with the report's lines as splits, subsidiary LOCKED to the report's
// sub. A non-submitted report (converted/rejected/draft) has nothing to post -> back to
// the queue.
func (s *server) expenseReviewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))

	rep, err := s.store.GetExpenseReport(ctx, ids.ExpenseReportID(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if rep.Status != "submitted" {
		// Only a submitted report is reviewable; everything else -> the queue.
		http.Redirect(w, r, "/expenses/review", http.StatusSeeOther)
		return
	}
	model, ok := s.buildReviewEditorModel(w, r, u, rep)
	if !ok {
		return
	}
	s.renderEditor(w, r, model)
}

// buildReviewEditorModel assembles the phase-12 editor model for a SUBMITTED report:
// the sub-scoped options (newEditorModel), the ExpenseReportID marker (review & post
// mode -> subsidiary locked), today's date, and the report's lines prefilled as rows
// with a trailing empty counter-side row. It is shared by expenseReviewForm (the GET)
// and the reject-422 re-render so a blank-reason reject re-renders the IDENTICAL review
// page. ok=false means a 500 was already written.
func (s *server) buildReviewEditorModel(w http.ResponseWriter, r *http.Request, u *store.CurrentUser, rep sqlc.ExpenseReport) (txnEditorModel, bool) {
	ctx := r.Context()
	lines, err := s.store.ExpenseReportLines(ctx, rep.ID)
	if err != nil {
		s.serverError(w)
		return txnEditorModel{}, false
	}
	model, err := s.newEditorModel(ctx, u, rep.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return txnEditorModel{}, false
	}
	model.ExpenseReportID = int64(rep.ID)
	model.FirstErrorRow = -1
	model.Date = money.FormatDate(s.now(), dateFormatFor(u))
	model.Rows = s.prefillExpenseRows(r, model, rep, lines)
	if err := s.injectRowAccounts(ctx, &model); err != nil { // p26.10: never blank a referenced account
		s.serverError(w)
		return txnEditorModel{}, false
	}
	return model, true
}

// prefillExpenseRows builds the editor rows from the report's lines: one row per line
// (account, signed amount, fund, program, memo), with program + functional class
// DEFAULTED from the account so the store's Z15/Z16 gates (R/E rows need a program; an
// expense row needs a functional class) don't reject an untouched prefill. A trailing
// EMPTY row is appended for the reviewer's counter-side (cash/bank) so the txn can be
// balanced without adding a row by hand (mirrors prefillImportRows). The store
// re-validates authoritatively on POST.
func (s *server) prefillExpenseRows(r *http.Request, model txnEditorModel, rep sqlc.ExpenseReport, lines []sqlc.ExpenseReportLine) []txnRowModel {
	ctx := r.Context()
	u := currentUser(ctx)
	exp := s.reportExponent(ctx, rep)
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}

	// Index the offered accounts for default program/class + type lookup.
	type acctMeta struct {
		typ            string
		defaultProgram int64
		defaultClass   string
	}
	meta := make(map[int64]acctMeta, len(model.Accounts))
	for _, a := range model.Accounts {
		meta[a.ID] = acctMeta{typ: a.Type, defaultProgram: a.DefaultProgram, defaultClass: a.DefaultClass}
	}

	rows := make([]txnRowModel, 0, len(lines)+1)
	for i, l := range lines {
		m := meta[l.AccountID]
		row := txnRowModel{
			Index:   i,
			Account: l.AccountID,
			Amount:  money.Format(l.Amount, exp, fmtOpts),
			// p26.19: carry the line's free-text description into the review editor row so
			// it round-trips (description_i -> parseSplitForms -> SplitInput.Description)
			// into the CREATED split on convert -- the payee->description migration's
			// "conversion carries description" requirement (p26.15 left this inert).
			Description: l.Description,
			Memo:        l.Memo,
		}
		if l.Amount >= 0 {
			row.AmountDR = money.Format(l.Amount, exp, fmtOpts)
		} else {
			row.AmountCR = money.Format(-l.Amount, exp, fmtOpts)
		}
		// Fund: the line's if set.
		if l.FundID.Valid {
			row.Fund = l.FundID.Int64
		}
		// Program: the line's if set, else the account's default, else the root (R/E rows
		// require a program, Z15). Balance-sheet rows carry no program (the store rejects
		// one there), but a report line is always R/E (p20.2 gates that), so default it.
		switch {
		case l.ProgramID.Valid:
			row.Program = l.ProgramID.Int64
		case m.defaultProgram != 0:
			row.Program = m.defaultProgram
		default:
			row.Program = model.RootProgram
		}
		// Functional class: expense rows need one (Z16); default from the account.
		if m.typ == "expense" {
			row.Class = m.defaultClass
		}
		rows = append(rows, row)
	}
	// A trailing empty row for the reviewer's counter-side (cash/bank).
	rows = append(rows, txnRowModel{Index: len(lines)})
	return rows
}

// expenseReviewPost handles POST /expenses/review/{id}/post (TxnWrite): parse the
// reviewer's (possibly adjusted) editor splits, create the balanced ledger transaction,
// and CONVERT the report to it -- IN ONE atomic change (store.PostAndConvertExpense
// Report). The subsidiary is LOCKED to the report's (any posted value is ignored). The
// store is the SOLE validator: an unbalanced/invalid post re-renders the editor at 422
// with the error routed to its slot (reusing txnSubmit's machinery), and the report
// stays submitted. A report that left the submitted state under us (a race, or a
// re-post of a converted report) -> back to the queue. On success, HX-Redirect to the
// created transaction's history (p12.4) so the reviewer sees the posted txn.
func (s *server) expenseReviewPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))

	rep, err := s.store.GetExpenseReport(ctx, ids.ExpenseReportID(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}

	// The subsidiary is LOCKED to the report's -- ignore any posted value.
	subID := rep.SubsidiaryID
	currency := r.FormValue("currency")

	model, err := s.newEditorModel(ctx, u, subID)
	if err != nil {
		s.serverError(w)
		return
	}
	model.ExpenseReportID = id
	model.Currency = currency
	model.Memo = r.FormValue("memo")
	model.Notes = r.FormValue("notes")
	model.FirstErrorRow = -1

	dateISO, dateOK := parseEditorDate(r.FormValue("date"), dateFormatFor(u), s.now())
	model.Date = r.FormValue("date")

	rows, splits := s.parseSplitForms(r, s.currencyExponent(ctx, currency), model.acctTypeMap())
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

	txnID, err := s.store.PostAndConvertExpenseReport(s.actorCtx(ctx), ids.ExpenseReportID(id), in)
	if err != nil {
		if errors.Is(err, store.ErrExpenseReportState) || errors.Is(err, store.ErrExpenseReportImmutable) {
			// The report left the submitted state (a race, or a converted report) -> back
			// to the queue, which shows the true current status.
			s.redirectReview(w, r, "/expenses/review")
			return
		}
		// Any ledger-validation error (unbalanced, etc.) re-renders the editor at 422; the
		// report stays submitted (the store rolled the whole change back).
		s.routeTxnError(&model, err, splits, 0)
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	// Success: the report is converted and linked; send the reviewer to the posted txn.
	s.redirectReview(w, r, "/transactions/"+strconv.FormatInt(txnID, 10)+"/history")
}

// ===========================================================================
// REJECT WITH REASON
// ===========================================================================

// expenseReviewReject handles POST /expenses/review/{id}/reject (TxnWrite): reject a
// submitted report with a REASON (required), routing it back to the submitter (p20.2
// shows the reason; the submitter resubmits). p26.27: the reject form now lives on the
// review PAGE (transaction_form.tmpl's ExpenseReportID branch), so a missing/blank reason
// re-renders THAT page at 422 with the reject-reason error (the report still shown), NOT
// the queue. A successful reject 303-redirects back to the queue. A report that is not
// submitted (a race, or a converted report) -> back to the queue.
func (s *server) expenseReviewReject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))

	rep, err := s.store.GetExpenseReport(ctx, ids.ExpenseReportID(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}
	reason := trimSpace(r.FormValue("reason"))
	if reason == "" {
		// A missing reason: re-render the review PAGE at 422 with the reject-reason error
		// (the report is still submitted, so the prefilled editor rebuilds cleanly). The
		// store also rejects an empty reason, but trimming/short-circuiting here handles a
		// whitespace-only reason (the store checks == "" without trimming).
		s.renderRejectError(w, r, u, rep, "expreview.reject_reason_required")
		return
	}

	if err := s.store.RejectExpenseReport(s.actorCtx(ctx), ids.ExpenseReportID(id), reason); err != nil {
		if errors.Is(err, store.ErrExpenseReportState) || errors.Is(err, store.ErrExpenseReportImmutable) {
			s.redirectReview(w, r, "/expenses/review")
			return
		}
		if errors.Is(err, store.ErrExpenseReportReasonRequired) {
			s.renderRejectError(w, r, u, rep, "expreview.reject_reason_required")
			return
		}
		s.serverError(w)
		return
	}
	s.redirectReview(w, r, "/expenses/review")
}

// ===========================================================================
// helpers
// ===========================================================================

// renderRejectError re-renders the review PAGE (the prefilled editor + the reject form)
// at 422 with the reject-reason error stamped next to the reject form (p26.27). It reuses
// buildReviewEditorModel so the page is IDENTICAL to the GET, then sets RejectError to the
// error KEY (the template localizes it via {{t}}). The report is still submitted here, so
// the prefill rebuilds cleanly.
func (s *server) renderRejectError(w http.ResponseWriter, r *http.Request, u *store.CurrentUser, rep sqlc.ExpenseReport, errKey string) {
	model, ok := s.buildReviewEditorModel(w, r, u, rep)
	if !ok {
		return
	}
	model.RejectError = errKey
	s.render(w, r, http.StatusUnprocessableEntity, "transaction_form.tmpl", s.newWideShellPage(r, model))
}

// redirectReview sends a review action's response: an HX-Redirect for htmx (a client
// navigation, so a full-page destination is not nested in the swapped form region) or a
// plain 303 for a no-JS submit.
func (s *server) redirectReview(w http.ResponseWriter, r *http.Request, dest string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
