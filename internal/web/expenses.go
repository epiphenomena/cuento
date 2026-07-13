package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cuento/internal/db/sqlc"
	"cuento/internal/i18n"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p20.2 submitter workspace (/expenses, Perm ExpenseSubmit). A low-privilege
// SUBMITTER (can_submit_expenses, INDEPENDENT of txn_perm -- p20.1/D10) drafts an
// expense report of proposed revenue/expense lines that need NOT balance, submits
// it, sees its status, and -- after a reviewer rejects it (p20.3) with a reason --
// sees that reason and RESUBMITS after editing. A pure submitter reaches ONLY these
// routes: TxnRead/TxnWrite/ReportGroup all 403 (the ledger/reports are off-limits),
// and a submitter may see/edit ONLY THEIR OWN reports (ownership enforced in every
// id-taking handler -> 404 for a missing OR not-owned id, uniform, no enumeration).
//
// REUSE of the phase-12 grid pieces, honestly scoped (DECISIONS p20.2): the LINE
// editor is the LINE-AT-A-TIME shape of the budget-line editor (p19.3) -- a report
// detail = header + a lines table + one inline add/edit-line form + Submit. The
// per-line CRUD (Add/Update/RemoveExpenseReportLine, each its OWN change) is the
// store seam; there is NO bulk-save grid, so NO p12 trap-1 reconcile-by-id and NO
// new ES module. What IS reused: the sub-scoped account/fund/program selector row
// (accounts = R/E leaves in the report's sub, funds = ActiveFunds(sub), programs =
// all active -- exactly buildLineForm), the form-error convention (422 + partial +
// autofocus, p10.3), the .txn-grid CSS, and the money formatters (rule 10). NO
// balancing requirement: an unbalanced report submits fine (the reviewer balances it
// at convert, p20.3). Amounts are entered as a POSITIVE magnitude; the stored sign is
// derived from the account TYPE (expense positive, revenue negative) so the display
// reads naturally -- the reviewer re-resolves sign at convert, so correctness does
// not hinge here (DECISIONS p20.2).
//
// Every string via {{t}} (rule 9); amounts/dates through the money formatters (rule
// 10); account/fund/program/subsidiary names are stored proper nouns; no inline
// script (rule 12).

// ===========================================================================
// MY REPORTS LIST + create
// ===========================================================================

// expenseReportRow is one rendered report on the "my reports" list: id, the report's
// subsidiary name, a localized status key, and -- when rejected -- the reviewer's
// reason (review_notes) so the submitter sees WHY before resubmitting.
type expenseReportRow struct {
	ID          int64
	SubName     string
	StatusKey   string // i18n key: expense.status.<status>
	ReviewNotes string // the reviewer's rejection reason ("" unless rejected)
	Rejected    bool
}

// expensesPageModel is the GET /expenses model: the current user's OWN reports and
// the subsidiary picker for a new report.
type expensesPageModel struct {
	Rows []expenseReportRow
	Subs []subOption // the "new report" subsidiary picker
}

// expensesPage handles GET /expenses (ExpenseSubmit): the submitter's own reports
// (newest first) with status + reviewer reason, plus a "new report" affordance.
func (s *server) expensesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	reports, err := s.store.ExpenseReportsBySubmitter(ctx, u.ID)
	if err != nil {
		s.serverError(w)
		return
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}
	model := expensesPageModel{}
	for _, rep := range reports {
		row := expenseReportRow{
			ID:        rep.ID,
			SubName:   subNames[rep.SubsidiaryID],
			StatusKey: "expense.status." + rep.Status,
		}
		if rep.Status == "rejected" {
			row.Rejected = true
			row.ReviewNotes = rep.ReviewNotes
		}
		model.Rows = append(model.Rows, row)
	}
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	for _, sb := range subs {
		model.Subs = append(model.Subs, subOption{ID: sb.ID, Name: sb.Name})
	}
	s.render(w, r, http.StatusOK, "expenses.tmpl", s.newShellPage(r, model))
}

// expenseCreate handles POST /expenses (ExpenseSubmit): create a draft report for
// the current user in the chosen subsidiary (one sub per report; the submitter picks
// it here, defaulting to their default sub). Redirects to the report editor.
func (s *server) expenseCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	sub := parseID(r.PostFormValue("subsidiary_id"))
	if sub == 0 {
		sub = s.defaultSubsidiary(ctx, u)
	}
	id, err := s.store.CreateExpenseReport(s.actorCtx(ctx), u.ID, sub)
	if err != nil {
		// A bad subsidiary id is the only expected failure (ErrExpenseReportRefMissing);
		// send the submitter back to the list rather than a dead 500.
		if errors.Is(err, store.ErrExpenseReportRefMissing) {
			redirectAfterForm(w, r, "/expenses")
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// ===========================================================================
// REPORT DETAIL / EDITOR
// ===========================================================================

// expenseLineRow is one rendered line on the report detail: resolved account/fund/
// program names + the formatted (positive-magnitude) amount + memo.
type expenseLineRow struct {
	ID        int64
	AcctName  string
	FundName  string // "" = unrestricted
	ProgName  string // "" = none
	AmountFmt string
	Memo      string
}

// expenseDetailModel is the GET /expenses/{id} model: the report header (its sub +
// status + editability + the reviewer's reason when rejected) and its lines.
type expenseDetailModel struct {
	ID          int64
	SubName     string
	Status      string
	StatusKey   string
	Editable    bool   // draft|rejected -> the add/edit-line form + Submit show
	Rejected    bool   // rejected -> the resubmit affordance + reviewer reason show
	ReviewNotes string // the reviewer's rejection reason (rejected only)
	Lines       []expenseLineRow

	// ErrorKey is a pre-localized page-level error (the zero-line submit case), "" =
	// none. It is already run through {{t}} in the handler so the template renders it
	// verbatim (like admin_user_detail's page-level error).
	ErrorKey string
}

// expenseDetail handles GET /expenses/{id} (ExpenseSubmit): the report editor. It
// ENFORCES OWNERSHIP -- a missing OR not-owned id is a 404 (uniform, no enumeration).
func (s *server) expenseDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := parseID(r.PathValue("id"))

	rep, ok := s.loadOwnedReport(w, r, id)
	if !ok {
		return
	}
	lines, err := s.store.ExpenseReportLines(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	acctNames, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		s.serverError(w)
		return
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}
	progNames, err := s.programNameMap(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	fundNames, err := s.fundNameMap(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	opts := formatOptsFor(u)
	exp := s.reportExponent(ctx, rep)

	model := expenseDetailModel{
		ID:        id,
		SubName:   subNames[rep.SubsidiaryID],
		Status:    rep.Status,
		StatusKey: "expense.status." + rep.Status,
		Editable:  rep.Status == "draft" || rep.Status == "rejected",
		Rejected:  rep.Status == "rejected",
	}
	if model.Rejected {
		model.ReviewNotes = rep.ReviewNotes
	}
	for _, l := range lines {
		row := expenseLineRow{
			ID:        l.ID,
			AcctName:  acctNames[l.AccountID],
			AmountFmt: money.Format(displayAmount(l.Amount), exp, opts),
			Memo:      l.Memo,
		}
		if l.FundID.Valid {
			row.FundName = fundNames[l.FundID.Int64]
		}
		if l.ProgramID.Valid {
			row.ProgName = progNames[l.ProgramID.Int64]
		}
		model.Lines = append(model.Lines, row)
	}
	s.render(w, r, http.StatusOK, "expense_detail.tmpl", s.newShellPage(r, model))
}

// displayAmount returns the positive magnitude of a signed stored amount (the UI
// enters + shows positive magnitudes; the sign is a derived storage detail).
func displayAmount(a int64) int64 {
	if a < 0 {
		return -a
	}
	return a
}

// reportExponent resolves the minor-unit exponent for a report's amounts. An
// expense-report line stores no currency (p20.1), so the implicit currency is the
// report's SUBSIDIARY base currency (D18) -- the same convention the txn editor uses
// to default an entry's currency. This keeps parse/format honoring rule 3/10 for a
// non-2-exponent base (e.g. a 0-exponent currency), not a hardcoded 2. Best-effort:
// falls back to 2 on any lookup miss (a blank base / unknown currency).
func (s *server) reportExponent(ctx context.Context, rep sqlc.ExpenseReport) int {
	sub, err := s.store.GetSubsidiary(ctx, rep.SubsidiaryID)
	if err != nil || sub.BaseCurrency == "" {
		return 2
	}
	return s.currencyExponent(ctx, sub.BaseCurrency)
}

// ===========================================================================
// LINE EDITOR (sub-scoped account/fund/program selectors)
// ===========================================================================

// expenseLineFormModel is the add/edit-line form. The account/fund options are
// SCOPED to the report's subsidiary (accounts = R/E leaves in the sub, funds =
// ActiveFunds(sub)); programs are all active -- exactly buildLineForm (p19.3). ID 0
// = create. The report's sub is FIXED (one sub per report), so there is NO sub picker
// and NO sub-change re-filter -- the report already bound its subsidiary at creation.
type expenseLineFormModel struct {
	ReportID  int64
	ID        int64
	AccountID int64
	FundID    int64 // 0 = unrestricted
	ProgramID int64 // 0 = none
	Amount    string
	Memo      string

	Accounts []txnOption // R/E leaves scoped to the report's sub
	Funds    []txnOption // ActiveFunds(sub)
	Programs []txnOption

	Errors formErrors
}

// buildExpenseLineForm assembles the sub-scoped option lists for a report's line
// form. Accounts are the revenue/expense LEAVES in the report's sub (a report is of
// R/E flows -- a balance-sheet account is never offered, and the handler rejects one
// out of band); funds are ActiveFunds(sub); programs are all active (not sub-scoped,
// like the txn/budget editors). It is shared by the new/edit forms and the 422
// re-render so the options always match the report's subsidiary.
func (s *server) buildExpenseLineForm(ctx context.Context, reportID, sub int64) (expenseLineFormModel, error) {
	lang := langOf(ctx)
	form := expenseLineFormModel{ReportID: reportID}

	accts, err := s.store.AccountEditorOptions(ctx, lang, sub)
	if err != nil {
		return form, err
	}
	for _, a := range accts {
		if a.Type != "revenue" && a.Type != "expense" {
			continue
		}
		form.Accounts = append(form.Accounts, txnOption{ID: a.ID, Name: a.Name})
	}

	funds, err := s.store.ActiveFunds(ctx, sub)
	if err != nil {
		return form, err
	}
	for _, f := range funds {
		form.Funds = append(form.Funds, txnOption{ID: f.ID, Name: f.Name})
	}

	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return form, err
	}
	for _, p := range progs {
		if p.Active == 0 {
			continue
		}
		form.Programs = append(form.Programs, txnOption{ID: p.ID, Name: p.Name})
	}
	return form, nil
}

// expenseLineNewForm handles GET /expenses/{id}/lines/new (ExpenseSubmit): a blank
// line form for the report (ownership enforced). Only a draft|rejected report is
// editable; a submitted/converted report's editor renders read-only (no form), so
// this GET should not be reachable for those -- but we guard anyway (404 on a
// non-editable report keeps the surface honest).
func (s *server) expenseLineNewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	form, err := s.buildExpenseLineForm(ctx, id, rep.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "expense-line-form", form)
}

// expenseLineEditForm handles GET /expenses/{id}/lines/{lid}/edit (ExpenseSubmit):
// the line form prefilled (ownership + editability enforced).
func (s *server) expenseLineEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	line, ok := s.loadReportLine(w, r, id, lineID)
	if !ok {
		return
	}
	form, err := s.buildExpenseLineForm(ctx, id, rep.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	form.ID = lineID
	form.AccountID = line.AccountID
	if line.FundID.Valid {
		form.FundID = line.FundID.Int64
	}
	if line.ProgramID.Valid {
		form.ProgramID = line.ProgramID.Int64
	}
	form.Amount = money.Format(displayAmount(line.Amount), s.reportExponent(ctx, rep), formatOptsFor(u))
	form.Memo = line.Memo
	s.render(w, r, http.StatusOK, "expense-line-form", form)
}

// parseExpenseLineForm reads the POST form into an expenseLineFormModel (echo for a
// 422 re-render) and resolves the SIGNED store amount from the entered positive
// magnitude + the chosen account's type (expense positive, revenue negative). It
// validates the account is in the report's sub-scoped R/E option set (the store does
// NOT check subsidiary/type on a report line, p20.1 -- that gate lives here); an
// out-of-sub / non-R/E account is flagged as a field error. A bad amount is left 0.
func (s *server) parseExpenseLineForm(r *http.Request, rep sqlc.ExpenseReport, lineID int64) (expenseLineFormModel, store.ExpenseReportLineInput, string) {
	ctx := r.Context()
	u := currentUser(ctx)
	_ = r.ParseForm()

	form, err := s.buildExpenseLineForm(ctx, rep.ID, rep.SubsidiaryID)
	if err != nil {
		// A build failure is a server fault; signal via a generic key so the caller 500s.
		return expenseLineFormModel{ReportID: rep.ID, ID: lineID}, store.ExpenseReportLineInput{}, "__server__"
	}
	form.ID = lineID
	form.AccountID = parseID(r.PostFormValue("account_id"))
	form.FundID = parseID(r.PostFormValue("fund_id"))
	form.ProgramID = parseID(r.PostFormValue("program_id"))
	form.Amount = strings.TrimSpace(r.PostFormValue("amount"))
	form.Memo = strings.TrimSpace(r.PostFormValue("memo"))

	// The account must be one of the offered R/E leaves in the report's sub (the store
	// does not enforce this for a report line, so it is the web edge's job).
	acctType, offered := s.reportAccountType(ctx, rep.SubsidiaryID, form.AccountID)
	if form.AccountID == 0 || !offered {
		return form, store.ExpenseReportLineInput{}, "account_id"
	}

	// Amount: entered as a POSITIVE magnitude in the user's number format; a parse
	// failure yields 0 -> the balance-free store accepts it, but a 0 line is pointless,
	// so flag an empty/zero magnitude as an amount error.
	mag, perr := money.Parse(form.Amount, s.reportExponent(ctx, rep), numberFormatFor(u))
	if perr != nil || mag <= 0 {
		return form, store.ExpenseReportLineInput{}, "amount"
	}
	// Derive the stored sign from the account type: an expense is a positive outflow;
	// revenue is a negative (credit) magnitude. The reviewer re-resolves at convert, so
	// this is a display convention, not a correctness constraint (DECISIONS p20.2).
	amount := mag
	if acctType == "revenue" {
		amount = -mag
	}

	in := store.ExpenseReportLineInput{
		AccountID: form.AccountID,
		Amount:    amount,
		Memo:      form.Memo,
	}
	if form.FundID > 0 {
		fid := form.FundID
		in.FundID = &fid
	}
	if form.ProgramID > 0 {
		pid := form.ProgramID
		in.ProgramID = &pid
	}
	return form, in, ""
}

// reportAccountType returns an account's type and whether it is one of the R/E leaves
// offered for the report's subsidiary. Reuses AccountEditorOptions (the SAME set the
// form offers), so the gate can never disagree with what the picker showed.
func (s *server) reportAccountType(ctx context.Context, sub, acctID int64) (string, bool) {
	if acctID == 0 {
		return "", false
	}
	accts, err := s.store.AccountEditorOptions(ctx, langOf(ctx), sub)
	if err != nil {
		return "", false
	}
	for _, a := range accts {
		if a.ID == acctID && (a.Type == "revenue" || a.Type == "expense") {
			return a.Type, true
		}
	}
	return "", false
}

// expenseLineCreate handles POST /expenses/{id}/lines (ExpenseSubmit).
func (s *server) expenseLineCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	form, in, field := s.parseExpenseLineForm(r, rep, 0)
	if field != "" {
		s.renderExpenseLineError(w, r, form, field)
		return
	}
	if _, err := s.store.AddExpenseReportLine(s.actorCtx(ctx), id, in); err != nil {
		s.renderExpenseLineStoreError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// expenseLineUpdate handles POST /expenses/{id}/lines/{lid} (ExpenseSubmit).
func (s *server) expenseLineUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	if _, ok := s.loadReportLine(w, r, id, lineID); !ok {
		return
	}
	form, in, field := s.parseExpenseLineForm(r, rep, lineID)
	if field != "" {
		s.renderExpenseLineError(w, r, form, field)
		return
	}
	if err := s.store.UpdateExpenseReportLine(s.actorCtx(ctx), lineID, in); err != nil {
		s.renderExpenseLineStoreError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// expenseLineDelete handles POST /expenses/{id}/lines/{lid}/delete (ExpenseSubmit): a
// hard delete with an audit version (rule 14). Ownership + editability enforced.
func (s *server) expenseLineDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	if _, ok := s.loadEditableReport(w, r, id); !ok {
		return
	}
	if _, ok := s.loadReportLine(w, r, id, lineID); !ok {
		return
	}
	if err := s.store.RemoveExpenseReportLine(s.actorCtx(ctx), lineID); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/expenses/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// renderExpenseLineError re-renders the line form at 422 with a field error (the
// p10.3 convention). A "__server__" field is a build fault -> 500.
func (s *server) renderExpenseLineError(w http.ResponseWriter, r *http.Request, form expenseLineFormModel, field string) {
	if field == "__server__" {
		s.serverError(w)
		return
	}
	key := "error.expense.account_not_offered"
	if field == "amount" {
		key = "error.expense.amount"
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "expense-line-form", form)
}

// renderExpenseLineStoreError maps a store typed error to a field error (the store
// re-validates ref existence; a race that deleted the account surfaces here).
func (s *server) renderExpenseLineStoreError(w http.ResponseWriter, r *http.Request, form expenseLineFormModel, err error) {
	switch {
	case errors.Is(err, store.ErrExpenseReportRefMissing):
		form.Errors.add("account_id", "error.expense.account_not_offered")
	case errors.Is(err, store.ErrExpenseReportState),
		errors.Is(err, store.ErrExpenseReportImmutable):
		// The report left the editable state under us -> back to the detail page.
		redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(form.ReportID, 10))
		return
	default:
		s.serverError(w)
		return
	}
	s.renderFormError(w, r, "expense-line-form", form)
}

// ===========================================================================
// SUBMIT / RESUBMIT
// ===========================================================================

// expenseSubmit handles POST /expenses/{id}/submit (ExpenseSubmit): move a draft ->
// submitted (need NOT balance). A zero-line report is a clean 422 (ErrExpenseReport
// Empty -> i18n), not a 500. Ownership enforced.
func (s *server) expenseSubmit(w http.ResponseWriter, r *http.Request) {
	s.transitionReport(w, r, false)
}

// expenseResubmit handles POST /expenses/{id}/resubmit (ExpenseSubmit): move a
// rejected -> submitted after the submitter edits (the reviewer's reason is preserved
// and was shown on the detail page). Ownership enforced.
func (s *server) expenseResubmit(w http.ResponseWriter, r *http.Request) {
	s.transitionReport(w, r, true)
}

// transitionReport is the shared submit/resubmit path: it enforces ownership, calls
// the matching store transition, maps ErrExpenseReportEmpty to a 422 (with the detail
// page re-rendered so the error shows in context), and on success redirects to the
// detail page (PRG) where the new status shows.
func (s *server) transitionReport(w http.ResponseWriter, r *http.Request, resubmit bool) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadOwnedReport(w, r, id)
	if !ok {
		return
	}
	var err error
	if resubmit {
		err = s.store.ResubmitExpenseReport(s.actorCtx(ctx), id)
	} else {
		err = s.store.SubmitExpenseReport(s.actorCtx(ctx), id)
	}
	if err != nil {
		if errors.Is(err, store.ErrExpenseReportEmpty) {
			s.renderReportError(w, r, rep, "error.expense.empty")
			return
		}
		if errors.Is(err, store.ErrExpenseReportState) {
			// The report is not in a state that permits this transition (e.g. a double
			// submit) -> back to the detail page, which shows the true current status.
			redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// renderReportError re-renders the report detail page at 422 with a page-level error
// (the zero-line submit case). It reloads the lines so the page is intact.
func (s *server) renderReportError(w http.ResponseWriter, r *http.Request, rep sqlc.ExpenseReport, key string) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	subNames, _ := subNameMap(ctx, s.store)
	acctNames, _ := accountNameMap(ctx, s.store, lang)
	progNames, _ := s.programNameMap(ctx)
	fundNames, _ := s.fundNameMap(ctx)
	lines, _ := s.store.ExpenseReportLines(ctx, rep.ID)
	opts := formatOptsFor(u)
	exp := s.reportExponent(ctx, rep)

	model := expenseDetailModel{
		ID:        rep.ID,
		SubName:   subNames[rep.SubsidiaryID],
		Status:    rep.Status,
		StatusKey: "expense.status." + rep.Status,
		Editable:  rep.Status == "draft" || rep.Status == "rejected",
		Rejected:  rep.Status == "rejected",
	}
	if model.Rejected {
		model.ReviewNotes = rep.ReviewNotes
	}
	for _, l := range lines {
		row := expenseLineRow{
			ID:        l.ID,
			AcctName:  acctNames[l.AccountID],
			AmountFmt: money.Format(displayAmount(l.Amount), exp, opts),
			Memo:      l.Memo,
		}
		if l.FundID.Valid {
			row.FundName = fundNames[l.FundID.Int64]
		}
		if l.ProgramID.Valid {
			row.ProgName = progNames[l.ProgramID.Int64]
		}
		model.Lines = append(model.Lines, row)
	}
	// A page-level error above the form: reuse the detail template, with the error key
	// stamped so it renders. We surface it via a dedicated field on the model.
	model.ErrorKey = i18n.T(lang, key)
	s.render(w, r, http.StatusUnprocessableEntity, "expense_detail.tmpl", s.newShellPage(r, model))
}

// ===========================================================================
// ownership + editability guards
// ===========================================================================

// loadOwnedReport loads a report and enforces OWNERSHIP: it returns ok=false (after
// writing a 404) unless the report exists AND belongs to the current user. A missing
// id and a not-owned id are BOTH 404 (uniform, no enumeration). It never 500s on a
// missing id (ErrExpenseReportNotFound -> 404).
func (s *server) loadOwnedReport(w http.ResponseWriter, r *http.Request, id int64) (sqlc.ExpenseReport, bool) {
	ctx := r.Context()
	u := currentUser(ctx)
	rep, err := s.store.GetExpenseReport(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrExpenseReportNotFound) {
			http.NotFound(w, r)
			return sqlc.ExpenseReport{}, false
		}
		s.serverError(w)
		return sqlc.ExpenseReport{}, false
	}
	if u == nil || rep.SubmitterID != u.ID {
		// Not the owner: 404 (same as missing -- a submitter must not learn another's
		// report id exists).
		http.NotFound(w, r)
		return sqlc.ExpenseReport{}, false
	}
	return rep, true
}

// loadEditableReport is loadOwnedReport plus the editability gate: a submitted or
// converted report cannot be line-edited, so its editor mutations 404 (the detail
// page renders read-only for those states, so a well-behaved client never posts).
func (s *server) loadEditableReport(w http.ResponseWriter, r *http.Request, id int64) (sqlc.ExpenseReport, bool) {
	rep, ok := s.loadOwnedReport(w, r, id)
	if !ok {
		return sqlc.ExpenseReport{}, false
	}
	if rep.Status != "draft" && rep.Status != "rejected" {
		http.NotFound(w, r)
		return sqlc.ExpenseReport{}, false
	}
	return rep, true
}

// loadReportLine loads a line and confirms it belongs to reportID (a submitter must
// not edit a line of another report via a mismatched path). Missing / mismatched -> 404.
func (s *server) loadReportLine(w http.ResponseWriter, r *http.Request, reportID, lineID int64) (sqlc.ExpenseReportLine, bool) {
	ctx := r.Context()
	lines, err := s.store.ExpenseReportLines(ctx, reportID)
	if err != nil {
		s.serverError(w)
		return sqlc.ExpenseReportLine{}, false
	}
	for _, l := range lines {
		if l.ID == lineID {
			return l, true
		}
	}
	http.NotFound(w, r)
	return sqlc.ExpenseReportLine{}, false
}
