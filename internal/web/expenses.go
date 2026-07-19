package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cuento/internal/db/sqlc"
	"cuento/internal/i18n"
	"cuento/internal/ids"
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
// REUSE of the phase-12 grid pieces (p25.4 OVERTURNS the p20.2 "NO bulk-save grid / NO
// new ES module" note): the LINE editor is now the SAME auto-row grid as the transaction
// editor -- a report detail = header + a bulk-submit line grid (account/amount/fund/
// program/memo) + Submit. The whole line set saves under ONE change via the store's
// ReplaceExpenseReportLines replace-set (diff-by-line-id, the p12 trap-1 reconcile
// mirrored from UpdateTransaction), replacing the old per-line Add/Update/Remove CRUD.
// The grid starts with one empty row; expensegrid.js auto-appends a fresh trailing empty
// row as the last row is edited (reusing the tested isRowEmpty predicate), and the server
// drops the trailing empty on save. Still honestly scoped vs the txn grid: NO balancing,
// NO min-2, NO DR/CR, NO functional-class. What IS reused: the sub-scoped account/fund/
// program option lists (accounts = R/E leaves in the report's sub, funds = ActiveFunds
// (sub), programs = all active -- expenseLineOptions), the per-row error convention (422
// re-render, p10.3), the .txn-grid CSS, and the money formatters (rule 10). NO balancing
// requirement: an unbalanced report submits fine (the reviewer balances it at convert,
// p20.3). Amounts are entered as a POSITIVE magnitude; the stored sign is derived from the
// account TYPE (expense positive, revenue negative) so the display reads naturally -- the
// reviewer re-resolves sign at convert, so correctness does not hinge here.
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
	Draft       bool // draft -> the discard affordance (p25.3)
}

// expensesPageModel is the GET /expenses model: the current user's OWN reports. The
// "new report" affordance is a plain button (p25.3: the subsidiary is chosen on the
// report page, no longer a list-page picker).
type expensesPageModel struct {
	Rows []expenseReportRow
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
			ID:        int64(rep.ID),
			SubName:   subNames[int64(rep.SubsidiaryID)],
			StatusKey: "expense.status." + rep.Status,
			Draft:     rep.Status == "draft",
		}
		if rep.Status == "rejected" {
			row.Rejected = true
			row.ReviewNotes = rep.ReviewNotes
		}
		model.Rows = append(model.Rows, row)
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
	id, err := s.store.CreateExpenseReport(s.actorCtx(ctx), u.ID, ids.SubsidiaryID(sub))
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
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(int64(id), 10))
}

// expenseSetSubsidiary handles POST /expenses/{id}/subsidiary (ExpenseSubmit, p25.3):
// change a draft report's subsidiary in-page (the store rejects the change once the
// report has lines or is not editable). Always redirects back to the report page; a
// rejected change is a no-op the picker's guard already prevents on a well-behaved
// client, so there is no field-error surface.
func (s *server) expenseSetSubsidiary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	sub := parseID(r.PostFormValue("subsidiary_id"))
	if sub != 0 && ids.SubsidiaryID(sub) != rep.SubsidiaryID {
		if err := s.store.UpdateExpenseReportSubsidiary(s.actorCtx(ctx), ids.ExpenseReportID(id), ids.SubsidiaryID(sub)); err != nil {
			// Expected guards (locked/bad sub) just no-op back to the page; anything else
			// is a real fault.
			if !errors.Is(err, store.ErrExpenseReportHasLines) &&
				!errors.Is(err, store.ErrExpenseReportState) &&
				!errors.Is(err, store.ErrExpenseReportRefMissing) {
				s.serverError(w)
				return
			}
		}
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// expenseDiscard handles POST /expenses/{id}/discard (ExpenseSubmit, p25.3): hard-
// delete a DRAFT report and its lines (the store guards draft-only). Redirects to the
// reports list. The UI only offers discard on a draft, so a POST against a non-draft is
// an abnormal request: the store rejects it (ErrExpenseReportState) and the handler
// just routes back to the list (not a 500, not a 404) -- ownership is still enforced.
func (s *server) expenseDiscard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if _, ok := s.loadOwnedReport(w, r, id); !ok {
		return
	}
	if err := s.store.DiscardExpenseReport(s.actorCtx(ctx), ids.ExpenseReportID(id)); err != nil {
		if errors.Is(err, store.ErrExpenseReportState) {
			redirectAfterForm(w, r, "/expenses")
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/expenses")
}

// ===========================================================================
// REPORT DETAIL / EDITOR
// ===========================================================================

// expenseLineRow is one line on the report detail. When the report is EDITABLE it is
// rendered as a GRID ROW (the ids + positive-magnitude AmountInput prefill the account/
// amount/fund/program/memo controls, p25.4); when NOT editable it renders READ-ONLY from
// the resolved-name display fields.
type expenseLineRow struct {
	ID int64
	// Grid-edit prefill (Editable render): the chosen ids + the positive magnitude in
	// the user's number format.
	AccountID   int64
	FundID      int64 // 0 = unrestricted
	ProgramID   int64 // 0 = none
	AmountInput string
	// Read-only display (non-editable render): resolved proper-noun names + formatted
	// (positive-magnitude) amount.
	AcctName    string
	FundName    string // "" = unrestricted
	ProgName    string // "" = none
	AmountFmt   string
	Description string // per-line free-text (p26.19; autocomplete + prefill source)
	Memo        string
	// ErrorKey is this row's per-row error i18n key on a 422 grid re-render (p25.4);
	// "" = none. Rendered in the row's error cell via {{t}}.
	ErrorKey string
}

// expenseAccountOption is one account offered in the line grid's account combobox.
// Like the txn editor's txnAccountOption it carries a dotted Path (ancestor chain
// ending in Name, p26.1) so the combobox can display and fuzzy-match the account
// with its parent chain; value stays the account id.
type expenseAccountOption struct {
	ID   int64
	Name string
	Path string
	Type string // p26.74: the account's type, so the selector can <optgroup> by it
	// Unavailable marks an account force-included because an existing line references
	// it though it is inactive / a placeholder / out-of-subsidiary (p26.10); the
	// template appends a marker suffix + data-unavailable so the user sees why the
	// row's account is special. Value stays the real id, SELECTED.
	Unavailable bool
}

// expenseDetailModel is the GET /expenses/{id} model: the report header (its sub +
// status + editability + the reviewer's reason when rejected) and its lines.
type expenseDetailModel struct {
	ID          int64
	SubName     string
	SubID       int64       // the report's current subsidiary (selected in the picker)
	Subs        []subOption // subsidiary picker options (shown only when SubEditable)
	SubEditable bool        // editable AND no lines yet -> the sub picker is shown (p25.3)
	Status      string
	StatusKey   string
	Editable    bool   // draft|rejected -> the add/edit-line form + Submit show
	Draft       bool   // draft -> the discard affordance (p25.3)
	Rejected    bool   // rejected -> the resubmit affordance + reviewer reason show
	ReviewNotes string // the reviewer's rejection reason (rejected only)
	Lines       []expenseLineRow

	// Sub-scoped option lists for the editable grid (p25.4), loaded only when
	// .Editable: accounts = R/E leaves in the report's sub (each with a dotted Path,
	// p26.1), funds = ActiveFunds(sub), programs = all active. Empty on a read-only
	// render.
	Accounts []expenseAccountOption
	Funds    []txnOption
	Programs []txnOption

	// UserProgram is the submitter's default_program (p26.5); 0 = unset. The grid
	// prefills it as the program on a NEW/empty row (there is no per-account default
	// tier here, unlike the txn editor, so the user default applies unconditionally to
	// fresh rows). Server-rendered existing lines keep their stored program.
	UserProgram int64

	// ErrorMsg is a pre-localized page-level error (the zero-line submit case), "" =
	// none. Unlike a *.ErrorKey field it holds already-localized text (run through
	// {{t}} in the handler), so the template renders it verbatim.
	ErrorMsg string
}

// expenseDetail handles GET /expenses/{id} (ExpenseSubmit): the report editor. It
// ENFORCES OWNERSHIP -- a missing OR not-owned id is a 404 (uniform, no enumeration).
func (s *server) expenseDetail(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadOwnedReport(w, r, id)
	if !ok {
		return
	}
	model, ok := s.buildExpenseDetailModel(w, r, rep)
	if !ok {
		return
	}
	// The editable expense grid is the same wide .txn-grid the transaction editor uses
	// (account/amount/fund/program/memo + delete columns); it overflows the centered
	// reading column. Opt into the wide <main> like the register / txn editor so the grid
	// gets the same horizontal room.
	page := s.newShellPage(r, model)
	page.Shell.Wide = true
	s.render(w, r, http.StatusOK, "expense_detail.tmpl", page)
}

// buildExpenseDetailModel assembles the report-detail model shared by expenseDetail and
// the 422 re-render paths (renderReportError / the grid-save error re-render). It loads
// the header + status, the reviewer reason, the read-only line display fields AND -- when
// the report is EDITABLE -- the sub-scoped account/fund/program option lists plus each
// line's grid-edit prefill (ids + positive-magnitude AmountInput). When editable with NO
// lines it seeds ONE empty starter row so {{len .Lines}} == rendered rows (the client
// auto-appends further trailing rows, the server drops the trailing empty on save). ok=
// false means a 500 was already written.
func (s *server) buildExpenseDetailModel(w http.ResponseWriter, r *http.Request, rep sqlc.ExpenseReport) (expenseDetailModel, bool) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := rep.ID

	lines, err := s.store.ExpenseReportLines(ctx, id)
	if err != nil {
		s.serverError(w)
		return expenseDetailModel{}, false
	}
	acctNames, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		s.serverError(w)
		return expenseDetailModel{}, false
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return expenseDetailModel{}, false
	}
	progNames, err := s.programNameMap(ctx)
	if err != nil {
		s.serverError(w)
		return expenseDetailModel{}, false
	}
	fundNames, err := fundNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return expenseDetailModel{}, false
	}
	opts := formatOptsFor(u)
	exp := s.reportExponent(ctx, rep)
	ccy := s.reportCurrency(ctx, rep)

	model := expenseDetailModel{
		ID:        int64(id),
		SubName:   subNames[int64(rep.SubsidiaryID)],
		SubID:     int64(rep.SubsidiaryID),
		Status:    rep.Status,
		StatusKey: "expense.status." + rep.Status,
		Editable:  rep.Status == "draft" || rep.Status == "rejected",
		Draft:     rep.Status == "draft",
		Rejected:  rep.Status == "rejected",
	}
	if model.Rejected {
		model.ReviewNotes = rep.ReviewNotes
	}
	// p25.3: the subsidiary is editable in-page ONLY while the report is editable AND
	// has no lines (a line's account/fund options are sub-scoped; the store enforces
	// the same lock). Load the picker options only then.
	if model.Editable && len(lines) == 0 {
		model.SubEditable = true
		subs, err := s.store.AllSubsidiaries(ctx)
		if err != nil {
			s.serverError(w)
			return expenseDetailModel{}, false
		}
		for _, sb := range subs {
			model.Subs = append(model.Subs, subOption{ID: int64(sb.ID), Name: sb.Name})
		}
	}
	// p25.4: when editable, load the sub-scoped account/fund/program option lists the
	// grid selects offer (the SAME set reportAccountType gates on).
	if model.Editable {
		// Force-include every account a live line references (p26.10) so a line whose
		// account is now inactive / out-of-sub still renders as a SELECTED option.
		var include []ids.AccountID
		for _, l := range lines {
			include = append(include, l.AccountID)
		}
		accts, funds, progs, err := s.expenseLineOptions(ctx, rep.SubsidiaryID, include...)
		if err != nil {
			s.serverError(w)
			return expenseDetailModel{}, false
		}
		model.Accounts, model.Funds, model.Programs = accts, funds, progs
		// p26.5: prefill the submitter's default program on new/empty rows, but only when
		// it is one of the offered (active) program options -- a stale/inactive default
		// must not select a non-existent <option>.
		if up := derefID(userDefaultProgram(u)); up != 0 {
			for _, p := range progs {
				if p.ID == up {
					model.UserProgram = up
					break
				}
			}
		}
	}
	for _, l := range lines {
		row := expenseLineRow{
			ID:          int64(l.ID),
			AccountID:   int64(l.AccountID),
			AcctName:    acctNames[l.AccountID],
			AmountFmt:   money.FormatMoney(displayAmount(l.Amount), ccy, exp, opts),
			Description: l.Description,
			Memo:        l.Memo,
		}
		if model.Editable {
			row.AmountInput = money.Format(displayAmount(l.Amount), exp, opts)
		}
		if l.FundID.Valid {
			row.FundID = l.FundID.Int64
			row.FundName = fundNames[ids.FundID(l.FundID.Int64)]
		}
		if l.ProgramID.Valid {
			row.ProgramID = l.ProgramID.Int64
			row.ProgName = progNames[l.ProgramID.Int64]
		}
		model.Lines = append(model.Lines, row)
	}
	// Editable + no lines: seed ONE empty starter row so the rows-count matches the
	// rendered rows (the client auto-appends more; the server drops the trailing empty).
	if model.Editable && len(model.Lines) == 0 {
		model.Lines = []expenseLineRow{{}}
	}
	return model, true
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

// reportCurrency resolves the ISO currency for a report's amounts (the report's
// SUBSIDIARY base currency, D18) so display cells can render the per-currency
// symbol via money.FormatMoney (rule 10). Best-effort: falls back to "" (which
// money.FormatMoney treats as an unmapped, code-prefixed currency) on a lookup
// miss.
func (s *server) reportCurrency(ctx context.Context, rep sqlc.ExpenseReport) string {
	sub, err := s.store.GetSubsidiary(ctx, rep.SubsidiaryID)
	if err != nil {
		return ""
	}
	return sub.BaseCurrency
}

// ===========================================================================
// LINE EDITOR (sub-scoped account/fund/program selectors)
// ===========================================================================

// expenseLineOptions assembles the sub-scoped option lists the grid selects offer for
// a report's subsidiary: accounts = the revenue/expense LEAVES in the sub (a report is
// of R/E flows -- a balance-sheet account is never offered, and the handler rejects one
// out of band); funds = ActiveFunds(sub); programs = all active (not sub-scoped, like
// the txn/budget editors). It is shared by the detail render and the 422 re-render so
// the options always match the report's subsidiary.
func (s *server) expenseLineOptions(ctx context.Context, sub ids.SubsidiaryID, include ...ids.AccountID) (accounts []expenseAccountOption, funds, programs []txnOption, err error) {
	lang := langOf(ctx)

	accts, err := s.store.AccountEditorOptionsWith(ctx, lang, sub, include)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range accts {
		// A normally-offered account must be an R/E leaf; a force-included one (p26.10)
		// is kept regardless so a line whose account is now inactive/out-of-sub still
		// renders SELECTED rather than blank.
		if !a.Unavailable && a.Type != "revenue" && a.Type != "expense" {
			continue
		}
		accounts = append(accounts, expenseAccountOption{ID: int64(a.ID), Name: a.Name, Path: a.Path, Type: a.Type, Unavailable: a.Unavailable})
	}

	fs, err := s.store.ActiveFunds(ctx, int64(sub))
	if err != nil {
		return nil, nil, nil, err
	}
	for _, f := range fs {
		funds = append(funds, txnOption{ID: int64(f.ID), Name: f.Name})
	}

	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	// p29.13: dotted hierarchy path per program for the expense grid's program combobox.
	progPaths, err := s.store.ProgramPaths(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, p := range progs {
		if p.Active == 0 {
			continue
		}
		programs = append(programs, txnOption{ID: int64(p.ID), Name: p.Name, Path: progPaths[p.ID]})
	}
	return accounts, funds, programs, nil
}

// reportAccountType returns an account's type and whether it is one of the R/E leaves
// offered for the report's subsidiary. Reuses AccountEditorOptions (the SAME set the
// grid offers), so the gate can never disagree with what the picker showed.
func (s *server) reportAccountType(ctx context.Context, sub ids.SubsidiaryID, acctID ids.AccountID) (string, bool) {
	if acctID == 0 {
		return "", false
	}
	accts, err := s.store.AccountEditorOptions(ctx, langOf(ctx), sub)
	if err != nil {
		return "", false
	}
	for _, a := range accts {
		if ids.AccountID(a.ID) == acctID && (a.Type == "revenue" || a.Type == "expense") {
			return a.Type, true
		}
	}
	return "", false
}

// expenseLinesSave handles POST /expenses/{id}/lines (ExpenseSubmit, p25.4): the
// auto-row grid's BULK submit. It parses the per-row fields (account_i, amount_i,
// fund_i, program_i, memo_i, line_id_i) up to the `rows` count -- like parseSplitForms
// -- SKIPPING fully-empty rows (which drops the trailing scaffold row). Each non-empty
// row's account must be an offered R/E leaf (reportAccountType, the web-edge gate the
// store does not enforce); its amount is a POSITIVE magnitude, and the stored sign is
// derived from the account type (revenue negative, expense positive -- the reviewer
// re-resolves at convert, so this is a display convention, DECISIONS p20.2). A bad row
// is a per-row error and a 422 grid re-render; all-valid rows go to
// ReplaceExpenseReportLines under ONE change, then a PRG redirect to the detail page.
func (s *server) expenseLinesSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	rep, ok := s.loadEditableReport(w, r, id)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	exp := s.reportExponent(ctx, rep)
	numFmt := numberFormatFor(u)
	n := int(parseID(r.PostFormValue("rows")))

	desired := make([]store.ExpenseReportLineDesired, 0, n)
	// echo holds every NON-EMPTY row as the user typed it (order preserved), so a 422
	// re-render shows the user's input, not a reload of the persisted set (mirrors the
	// txn grid's parseSplitForms echo). badRow indexes into echo; -1 = no row error.
	var echo []expenseLineRow
	rowErr := "" // i18n key of the first row error; "" = all rows valid
	badRow := -1
	for i := 0; i < n; i++ {
		si := strconv.Itoa(i)
		acct := ids.AccountID(parseID(r.PostFormValue("account_" + si)))
		amountStr := strings.TrimSpace(r.PostFormValue("amount_" + si))
		// Skip fully-empty rows (no account AND blank amount): the trailing scaffold row.
		if acct == 0 && amountStr == "" {
			continue
		}

		lineID := parseID(r.PostFormValue("line_id_" + si))
		fund := parseID(r.PostFormValue("fund_" + si))
		prog := parseID(r.PostFormValue("program_" + si))
		desc := strings.TrimSpace(r.PostFormValue("description_" + si))
		memo := strings.TrimSpace(r.PostFormValue("memo_" + si))

		// Echo the row exactly as typed (the ids + the raw amount text) so the re-render
		// re-enters the user's input.
		echoIdx := len(echo)
		echo = append(echo, expenseLineRow{
			ID:          lineID,
			AccountID:   int64(acct),
			FundID:      fund,
			ProgramID:   prog,
			AmountInput: amountStr,
			Description: desc,
			Memo:        memo,
		})

		acctType, offered := s.reportAccountType(ctx, rep.SubsidiaryID, acct)
		if acct == 0 || !offered {
			if rowErr == "" {
				rowErr, badRow = "error.expense.account_not_offered", echoIdx
			}
			continue
		}
		mag, perr := money.Parse(amountStr, exp, numFmt)
		if perr != nil || mag <= 0 {
			if rowErr == "" {
				rowErr, badRow = "error.expense.amount", echoIdx
			}
			continue
		}
		amount := mag
		if acctType == "revenue" {
			amount = -mag
		}
		d := store.ExpenseReportLineDesired{
			ID: ids.ExpenseReportLineID(lineID),
			ExpenseReportLineInput: store.ExpenseReportLineInput{
				AccountID:   acct,
				Amount:      amount,
				Description: desc,
				Memo:        memo,
			},
		}
		if fund > 0 {
			f := ids.FundID(fund)
			d.FundID = &f
		}
		if prog > 0 {
			p := ids.ProgramID(prog)
			d.ProgramID = &p
		}
		desired = append(desired, d)
	}

	if rowErr != "" {
		s.renderExpenseGridError(w, r, rep, echo, badRow, rowErr)
		return
	}

	if err := s.store.ReplaceExpenseReportLines(s.actorCtx(ctx), ids.ExpenseReportID(id), desired); err != nil {
		switch {
		case errors.Is(err, store.ErrExpenseReportState),
			errors.Is(err, store.ErrExpenseReportImmutable):
			// The report left the editable state under us -> back to the detail page.
			redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
			return
		case errors.Is(err, store.ErrExpenseReportRefMissing),
			errors.Is(err, store.ErrExpenseReportLineNotFound):
			// A race deleted a referenced account/line -> a page-level grid error.
			s.renderExpenseGridError(w, r, rep, echo, -1, "error.expense.account_not_offered")
			return
		default:
			s.serverError(w)
			return
		}
	}
	redirectAfterForm(w, r, "/expenses/"+strconv.FormatInt(id, 10))
}

// renderExpenseGridError re-renders the report detail (with its editable grid) at 422,
// ECHOING the user's typed rows (not a reload of the persisted set -- a bad save is
// all-or-nothing, so re-rendering the last-good persisted lines would discard the
// user's edits). rows is the parsed non-empty rows in submit order; badRow is the index
// (into rows) the error attaches to, or -1 for a page-level error. It reuses the shared
// builder ONLY for the sub-scoped option lists + header, then substitutes the echoed
// rows. A trailing empty starter row is appended so the client's auto-append has one to
// grow from (and {{len .Lines}} == rendered rows).
func (s *server) renderExpenseGridError(w http.ResponseWriter, r *http.Request, rep sqlc.ExpenseReport, rows []expenseLineRow, badRow int, key string) {
	model, ok := s.buildExpenseDetailModel(w, r, rep)
	if !ok {
		return
	}
	lang := langOf(r.Context())
	if badRow >= 0 && badRow < len(rows) {
		rows[badRow].ErrorKey = key
	} else {
		model.ErrorMsg = i18n.T(lang, key)
	}
	// Substitute the echoed rows for the persisted ones + a trailing empty starter row.
	model.Lines = append(rows, expenseLineRow{})
	s.render(w, r, http.StatusUnprocessableEntity, "expense_detail.tmpl", s.newShellPage(r, model))
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
		err = s.store.ResubmitExpenseReport(s.actorCtx(ctx), ids.ExpenseReportID(id))
	} else {
		err = s.store.SubmitExpenseReport(s.actorCtx(ctx), ids.ExpenseReportID(id))
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
// (the zero-line submit case). It rebuilds the model via the shared builder so the
// editable grid (its options + starter row) is intact on the re-render.
func (s *server) renderReportError(w http.ResponseWriter, r *http.Request, rep sqlc.ExpenseReport, key string) {
	model, ok := s.buildExpenseDetailModel(w, r, rep)
	if !ok {
		return
	}
	// A page-level error above the grid: reuse the detail template, with the error
	// message stamped (already localized) so it renders verbatim.
	model.ErrorMsg = i18n.T(langOf(r.Context()), key)
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
	rep, err := s.store.GetExpenseReport(ctx, ids.ExpenseReportID(id))
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
