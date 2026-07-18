package web

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cuento/internal/db/sqlc"
	"cuento/internal/i18n"
	"cuento/internal/money"
	"cuento/internal/store"
)

// parseUserDate parses a display-format date (the user's date_format) to canonical
// ISO "YYYY-MM-DD" via money.ParseDate (rule 10). ok=false on a blank/malformed
// value so the caller leaves the field empty (the store then rejects it).
func parseUserDate(s string, df money.DateFormat, now time.Time) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	t, err := money.ParseDate(s, df, now)
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// p27.2 budget-PLAN management (/budget-plans) -- the NEW split-derived budget model
// (DECISIONS "Budget redesign"). A plan is a name + subsidiary; its SPLITS are
// projected, dated rows [description, date, account, fund, program, amount] entered
// through an auto-row GRID (reusing the phase-12/expense-grid pieces: combobox,
// descfield, datefield, the money formatters) with a CADENCE helper (client-side row
// generation) and a flat-CSV import. ADDITIVE: the old /budgets schedule model is
// UNTOUCHED (a distinct URL namespace) until p27.3.
//
// Budget plans are PLANNING data like funds/budgets: VIEW routes are TxnRead, MUTATIONS
// TxnWrite (mirrors /budgets). The store (internal/store/budget_plans.go) owns ALL
// validation; these handlers collect a form and TRANSLATE the store's typed errors to
// per-row i18n keys (the p10.3 / p25.4 grid-error convention: 422 + re-render + echo).
//
// A budget-split amount is a plain POSITIVE magnitude entered by the user and stored
// AS-ENTERED (no sign flip -- unlike the expense grid; inflow/outflow direction is a
// REPORT concern derived from account type in p27.3, DECISIONS). Program is REQUIRED on
// R/E rows (prefilled from the account default) and FORBIDDEN on A/L rows -- the store
// enforces this; the handler maps the two sentinels to a per-row field key.
//
// Every string via {{t}} (rule 9); dates through money.ParseDate/FormatDate honoring
// the user's format (rule 10, never input[type=date]); amounts via money.Parse/Format
// (rule 3/10); no inline script (rule 12).

// ===========================================================================
// PLAN LIST + create
// ===========================================================================

// budgetPlanRow is one rendered plan on the list.
type budgetPlanRow struct {
	ID      int64
	Name    string
	SubName string
}

type budgetPlansPageModel struct {
	Rows []budgetPlanRow
}

// budgetPlansPage handles GET /budget-plans (TxnRead): the plan list + a "new plan"
// affordance.
func (s *server) budgetPlansPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plans, err := s.store.ListBudgetPlans(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}
	model := budgetPlansPageModel{}
	for _, p := range plans {
		model.Rows = append(model.Rows, budgetPlanRow{ID: p.ID, Name: p.Name, SubName: subNames[p.SubsidiaryID]})
	}
	s.render(w, r, http.StatusOK, "budget_plans.tmpl", s.newShellPage(r, model))
}

// budgetPlanFormModel backs the new-plan form (create only; a plan's subsidiary is
// fixed once splits exist, so there is no in-page edit here -- rename/notes edits are
// out of this step's scope).
type budgetPlanFormModel struct {
	Name   string
	SubID  int64
	Subs   []subOption
	Notes  string
	Errors formErrors
}

// budgetPlanNewForm handles GET /budget-plans/new (TxnWrite).
func (s *server) budgetPlanNewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	model := budgetPlanFormModel{}
	for _, sb := range subs {
		model.Subs = append(model.Subs, subOption{ID: sb.ID, Name: sb.Name})
	}
	if u := currentUser(ctx); u != nil {
		model.SubID = s.defaultSubsidiary(ctx, u)
	}
	s.render(w, r, http.StatusOK, "budget_plan_form.tmpl", s.newShellPage(r, model))
}

// budgetPlanCreate handles POST /budget-plans (TxnWrite): create a plan and redirect
// to its editor.
func (s *server) budgetPlanCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	sub := parseID(r.PostFormValue("subsidiary_id"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	if name == "" || sub == 0 {
		s.renderBudgetPlanFormError(w, r, name, sub, notes, "name", "error.budget_plan.name")
		return
	}
	id, err := s.store.CreateBudgetPlan(s.actorCtx(ctx), store.BudgetPlanInput{Name: name, SubsidiaryID: sub, Notes: notes})
	if err != nil {
		if errors.Is(err, store.ErrBudgetSplitRefMissing) {
			s.renderBudgetPlanFormError(w, r, name, sub, notes, "subsidiary_id", "error.budget_plan.subsidiary")
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budget-plans/"+strconv.FormatInt(id, 10))
}

// renderBudgetPlanFormError re-renders the new-plan form at 422 with a field error.
func (s *server) renderBudgetPlanFormError(w http.ResponseWriter, r *http.Request, name string, sub int64, notes, field, key string) {
	ctx := r.Context()
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	model := budgetPlanFormModel{Name: name, SubID: sub, Notes: notes}
	for _, sb := range subs {
		model.Subs = append(model.Subs, subOption{ID: sb.ID, Name: sb.Name})
	}
	model.Errors.add(field, key)
	s.render(w, r, http.StatusUnprocessableEntity, "budget_plan_form.tmpl", s.newShellPage(r, model))
}

// budgetPlanUpdate handles POST /budget-plans/{id} (TxnWrite): rename + notes edit
// (p27.3c). The plan's SUBSIDIARY is FIXED (splits resolve against it), so only the
// name and notes are editable; the existing subsidiary is carried through. A blank
// name re-renders the detail at 422 with a page-level error.
func (s *server) budgetPlanUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	plan, err := s.store.GetBudgetPlan(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	if name == "" {
		s.renderBudgetPlanDetailError(w, r, plan, i18n.T(langOf(ctx), "error.budget_plan.name"))
		return
	}
	if err := s.store.UpdateBudgetPlan(s.actorCtx(ctx), id, store.BudgetPlanInput{
		Name: name, SubsidiaryID: plan.SubsidiaryID, Notes: notes,
	}); err != nil {
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budget-plans/"+strconv.FormatInt(id, 10))
}

// budgetPlanDelete handles POST /budget-plans/{id}/delete (TxnWrite): hard-delete the
// plan and all its splits (versioned cascade, rule 14) and redirect to the list.
func (s *server) budgetPlanDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.DeleteBudgetPlan(s.actorCtx(ctx), id); err != nil {
		if errors.Is(err, store.ErrBudgetPlanNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budget-plans")
}

// renderBudgetPlanDetailError re-renders the plan detail at 422 with a pre-localized
// page-level error (the same seam the CSV import uses).
func (s *server) renderBudgetPlanDetailError(w http.ResponseWriter, r *http.Request, plan sqlc.BudgetPlan, msg string) {
	model, ok := s.buildBudgetPlanDetailModel(w, r, plan)
	if !ok {
		return
	}
	model.ErrorMsg = msg
	page := s.newShellPage(r, model)
	page.Shell.Wide = true
	s.render(w, r, http.StatusUnprocessableEntity, "budget_plan_detail.tmpl", page)
}

// ===========================================================================
// PLAN DETAIL / SPLIT-ENTRY GRID
// ===========================================================================

// budgetSplitRow is one grid row: the split's edit prefill (ids + a positive-magnitude
// AmountInput + the display-formatted date) or a per-row error key on a 422 re-render.
type budgetSplitRow struct {
	ID          int64
	Description string
	DateInput   string // display-formatted (user's date format)
	AccountID   int64
	FundID      int64 // 0 = unrestricted
	ProgramID   int64 // 0 = none
	AmountInput string
	ErrorKey    string // per-row i18n key on a 422 re-render; "" = none
}

// budgetPlanDetailModel is the GET /budget-plans/{id} model.
type budgetPlanDetailModel struct {
	ID       int64
	Name     string
	Notes    string
	SubName  string
	SubID    int64
	Splits   []budgetSplitRow
	Accounts []expenseAccountOption
	Funds    []txnOption
	Programs []txnOption
	// ErrorMsg is a pre-localized page-level error (e.g. a bad CSV), "" = none.
	ErrorMsg string
	// Cadence intervals for the client-side row generator.
	Intervals []cadenceIntervalOption
}

// cadenceIntervalOption is one option in the cadence interval <select> (value + label
// key). The values MUST match budgetcadence.js's INTERVAL_* constants.
type cadenceIntervalOption struct {
	Value    string
	LabelKey string
}

var cadenceIntervals = []cadenceIntervalOption{
	{"weekly", "budget_plan.cadence.weekly"},
	{"biweekly", "budget_plan.cadence.biweekly"},
	{"monthly", "budget_plan.cadence.monthly"},
}

// budgetPlanDetail handles GET /budget-plans/{id} (TxnRead): the plan editor.
func (s *server) budgetPlanDetail(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	plan, err := s.store.GetBudgetPlan(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	model, ok := s.buildBudgetPlanDetailModel(w, r, plan)
	if !ok {
		return
	}
	page := s.newShellPage(r, model)
	page.Shell.Wide = true
	s.render(w, r, http.StatusOK, "budget_plan_detail.tmpl", page)
}

// buildBudgetPlanDetailModel assembles the plan-detail model shared by the GET and the
// 422 re-render paths. It loads the header + the sub-scoped account/fund/program option
// lists + each split's grid-edit prefill, and seeds ONE empty starter row when the plan
// has no splits. ok=false means a 500 was already written.
func (s *server) buildBudgetPlanDetailModel(w http.ResponseWriter, r *http.Request, plan sqlc.BudgetPlan) (budgetPlanDetailModel, bool) {
	ctx := r.Context()
	u := currentUser(ctx)

	splits, err := s.store.BudgetSplits(ctx, plan.ID)
	if err != nil {
		s.serverError(w)
		return budgetPlanDetailModel{}, false
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return budgetPlanDetailModel{}, false
	}
	var include []int64
	for _, sp := range splits {
		include = append(include, sp.AccountID)
	}
	accts, funds, progs, err := s.budgetSplitOptions(ctx, plan.SubsidiaryID, include...)
	if err != nil {
		s.serverError(w)
		return budgetPlanDetailModel{}, false
	}

	df := dateFormatFor(u)
	opts := formatOptsFor(u)
	exp := s.subsidiaryExponent(ctx, plan.SubsidiaryID)

	model := budgetPlanDetailModel{
		ID:        plan.ID,
		Name:      plan.Name,
		Notes:     plan.Notes,
		SubName:   subNames[plan.SubsidiaryID],
		SubID:     plan.SubsidiaryID,
		Accounts:  accts,
		Funds:     funds,
		Programs:  progs,
		Intervals: cadenceIntervals,
	}
	for _, sp := range splits {
		row := budgetSplitRow{
			ID:          sp.ID,
			Description: sp.Description,
			DateInput:   money.FormatDate(parseISOForDisplay(sp.Date), df),
			AccountID:   sp.AccountID,
			AmountInput: money.Format(displayAmount(sp.Amount), exp, opts),
		}
		if sp.FundID.Valid {
			row.FundID = sp.FundID.Int64
		}
		if sp.ProgramID.Valid {
			row.ProgramID = sp.ProgramID.Int64
		}
		model.Splits = append(model.Splits, row)
	}
	if len(model.Splits) == 0 {
		model.Splits = []budgetSplitRow{{}}
	}
	return model, true
}

// budgetSplitOptions assembles the sub-scoped option lists the grid selects offer:
// accounts = R/E leaves OR open_item asset/liability leaves in the plan's subsidiary
// (a budget-split projects an R/E flow or an open-item A/R-A/P line; a plain balance-
// sheet account is never offered, and the store rejects one out of band); funds =
// ActiveFunds(sub); programs = all active. Mirrors expenseLineOptions but with the
// wider account gate.
func (s *server) budgetSplitOptions(ctx context.Context, sub int64, include ...int64) (accounts []expenseAccountOption, funds, programs []txnOption, err error) {
	lang := langOf(ctx)
	accts, err := s.store.AccountEditorOptionsWith(ctx, lang, sub, include)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range accts {
		offerable := a.Type == "revenue" || a.Type == "expense" ||
			(a.OpenItem && (a.Type == "asset" || a.Type == "liability"))
		if !a.Unavailable && !offerable {
			continue
		}
		accounts = append(accounts, expenseAccountOption{ID: a.ID, Name: a.Name, Path: a.Path, Type: a.Type, Unavailable: a.Unavailable})
	}
	fs, err := s.store.ActiveFunds(ctx, sub)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, f := range fs {
		funds = append(funds, txnOption{ID: f.ID, Name: f.Name})
	}
	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, p := range progs {
		if p.Active == 0 {
			continue
		}
		programs = append(programs, txnOption{ID: p.ID, Name: p.Name})
	}
	return accounts, funds, programs, nil
}

// budgetSplitAccountOffered reports whether acctID is an offered (R/E or open_item A/L)
// leaf in sub -- the web-edge gate mirroring what the grid showed, so a bad account is a
// per-row error rather than a store 500.
func (s *server) budgetSplitAccountOffered(ctx context.Context, sub, acctID int64) bool {
	if acctID == 0 {
		return false
	}
	accts, _, _, err := s.budgetSplitOptions(ctx, sub)
	if err != nil {
		return false
	}
	for _, a := range accts {
		if a.ID == acctID {
			return true
		}
	}
	return false
}

// budgetSplitsSave handles POST /budget-plans/{id}/splits (TxnWrite): the auto-row
// grid's BULK submit. It parses each row [description, date, account, fund, program,
// amount] up to the `rows` count, SKIPS fully-empty rows (the trailing scaffold), and
// REPLACES the plan's split set. A bad row is a per-row error + a 422 grid re-render
// (echoing the user's input); all-valid rows are written via a full replace (delete the
// existing set, insert the desired), each through the store funnel.
func (s *server) budgetSplitsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	plan, err := s.store.GetBudgetPlan(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	df := dateFormatFor(u)
	numFmt := numberFormatFor(u)
	exp := s.subsidiaryExponent(ctx, plan.SubsidiaryID)
	ccy := s.subsidiaryCurrency(ctx, plan.SubsidiaryID)
	n := int(parseID(r.PostFormValue("rows")))

	desired := make([]store.BudgetSplitInput, 0, n)
	var echo []budgetSplitRow
	rowErr := ""
	badRow := -1
	for i := 0; i < n; i++ {
		si := strconv.Itoa(i)
		desc := strings.TrimSpace(r.PostFormValue("description_" + si))
		dateStr := strings.TrimSpace(r.PostFormValue("date_" + si))
		acct := parseID(r.PostFormValue("account_" + si))
		fund := parseID(r.PostFormValue("fund_" + si))
		prog := parseID(r.PostFormValue("program_" + si))
		amountStr := strings.TrimSpace(r.PostFormValue("amount_" + si))

		// Skip a fully-empty row (no account AND blank amount AND blank date): the
		// trailing scaffold row.
		if acct == 0 && amountStr == "" && dateStr == "" {
			continue
		}
		echoIdx := len(echo)
		echo = append(echo, budgetSplitRow{
			Description: desc, DateInput: dateStr, AccountID: acct,
			FundID: fund, ProgramID: prog, AmountInput: amountStr,
		})

		if !s.budgetSplitAccountOffered(ctx, plan.SubsidiaryID, acct) {
			if rowErr == "" {
				rowErr, badRow = "error.budget_plan.account", echoIdx
			}
			continue
		}
		iso, ok := parseUserDate(dateStr, df, s.now())
		if !ok {
			if rowErr == "" {
				rowErr, badRow = "error.budget_plan.date", echoIdx
			}
			continue
		}
		mag, perr := money.Parse(amountStr, exp, numFmt)
		if perr != nil || mag <= 0 {
			if rowErr == "" {
				rowErr, badRow = "error.budget_plan.amount", echoIdx
			}
			continue
		}
		in := store.BudgetSplitInput{
			Description: desc,
			Date:        iso,
			AccountID:   acct,
			Amount:      mag, // stored AS-ENTERED (positive magnitude; no sign flip)
			Currency:    ccy,
		}
		if fund > 0 {
			f := fund
			in.FundID = &f
		}
		if prog > 0 {
			p := prog
			in.ProgramID = &p
		}
		desired = append(desired, in)
	}

	if rowErr != "" {
		s.renderBudgetGridError(w, r, plan, echo, badRow, rowErr)
		return
	}

	// ATOMIC replace the plan's split set (delete existing + insert desired) in ONE store
	// change: a mid-set rejection rolls the WHOLE change back, so the prior splits are
	// never lost (a per-call delete-then-insert would permanently drop them). On a store
	// rejection the failing desired index maps back to the echoed row's field error --
	// desired is built from the SAME non-empty rows as echo in order, so index i aligns.
	if failedIdx, err := s.store.ReplaceBudgetSplits(s.actorCtx(ctx), plan.ID, desired); err != nil {
		if key, ok := budgetSplitErrorKey(err); ok {
			s.renderBudgetGridError(w, r, plan, echo, failedIdx, key)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budget-plans/"+strconv.FormatInt(id, 10))
}

// budgetSplitErrorKey maps a store budget-split rejection to a per-row i18n key.
func budgetSplitErrorKey(err error) (string, bool) {
	switch {
	case errors.Is(err, store.ErrBudgetSplitProgramRequired):
		return "error.budget_plan.program_required", true
	case errors.Is(err, store.ErrBudgetSplitProgramForbidden):
		return "error.budget_plan.program_forbidden", true
	case errors.Is(err, store.ErrBudgetSplitAccountType),
		errors.Is(err, store.ErrBudgetSplitAccountNotLeaf),
		errors.Is(err, store.ErrBudgetSplitAccountSub):
		return "error.budget_plan.account", true
	case errors.Is(err, store.ErrBudgetSplitFundScope):
		return "error.budget_plan.fund", true
	case errors.Is(err, store.ErrBudgetSplitProgramScope):
		return "error.budget_plan.program_scope", true
	}
	return "", false
}

// renderBudgetGridError re-renders the plan detail at 422, echoing the user's rows and
// attaching the error to badRow (or page-level when badRow<0), plus a trailing empty
// starter row.
func (s *server) renderBudgetGridError(w http.ResponseWriter, r *http.Request, plan sqlc.BudgetPlan, rows []budgetSplitRow, badRow int, key string) {
	model, ok := s.buildBudgetPlanDetailModel(w, r, plan)
	if !ok {
		return
	}
	lang := langOf(r.Context())
	if badRow >= 0 && badRow < len(rows) {
		rows[badRow].ErrorKey = key
	} else {
		model.ErrorMsg = i18n.T(lang, key)
	}
	model.Splits = append(rows, budgetSplitRow{})
	s.render(w, r, http.StatusUnprocessableEntity, "budget_plan_detail.tmpl", s.newShellPage(r, model))
}

// subsidiaryExponent resolves a subsidiary's base-currency minor-unit exponent for
// amount parse/format (rule 3/10). Best-effort: falls back to 2 on a lookup miss.
func (s *server) subsidiaryExponent(ctx context.Context, subID int64) int {
	sub, err := s.store.GetSubsidiary(ctx, subID)
	if err != nil || sub.BaseCurrency == "" {
		return 2
	}
	return s.currencyExponent(ctx, sub.BaseCurrency)
}

// subsidiaryCurrency resolves a subsidiary's base ISO currency (a budget-split stores
// the plan's subsidiary base currency, mirroring the expense-line convention, D18).
// Best-effort: falls back to "" (which money.FormatMoney treats as unmapped).
func (s *server) subsidiaryCurrency(ctx context.Context, subID int64) string {
	sub, err := s.store.GetSubsidiary(ctx, subID)
	if err != nil {
		return ""
	}
	return sub.BaseCurrency
}

// ===========================================================================
// FLAT-CSV IMPORT
// ===========================================================================

// budgetPlanImport handles POST /budget-plans/{id}/import (TxnWrite): import a flat CSV
// [description, date, account, fund, optional program, amount] into the plan's splits
// (ADDITIVE -- appends to any existing splits). account/fund/program resolve by name (or
// account by its dotted path / fund/program by name). A bad row is an IN-PLACE page-level
// hint (row number + reason), not a page-nuking 500; a clean import redirects to the
// editor. Amounts are positive magnitudes stored as-entered.
func (s *server) budgetPlanImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	plan, err := s.store.GetBudgetPlan(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, _, err := r.FormFile("csv")
	if err != nil {
		s.renderBudgetImportError(w, r, plan, "error.budget_plan.csv_missing")
		return
	}
	defer file.Close()

	df := dateFormatFor(u)
	numFmt := numberFormatFor(u)
	exp := s.subsidiaryExponent(ctx, plan.SubsidiaryID)
	ccy := s.subsidiaryCurrency(ctx, plan.SubsidiaryID)

	// Resolve account/fund/program name lookups for the plan's subsidiary.
	accts, funds, progs, err := s.budgetSplitOptions(ctx, plan.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	acctByName := make(map[string]int64, len(accts)*2)
	for _, a := range accts {
		acctByName[strings.ToLower(a.Name)] = a.ID
		acctByName[strings.ToLower(a.Path)] = a.ID
	}
	fundByName := make(map[string]int64, len(funds))
	for _, f := range funds {
		fundByName[strings.ToLower(f.Name)] = f.ID
	}
	progByName := make(map[string]int64, len(progs))
	for _, p := range progs {
		progByName[strings.ToLower(p.Name)] = p.ID
	}

	cr := csv.NewReader(file)
	cr.FieldsPerRecord = -1 // program column is optional; validate arity per row
	cr.TrimLeadingSpace = true

	var parsed []store.BudgetSplitInput
	lineNo := 0
	for {
		rec, rerr := cr.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			s.renderBudgetImportError(w, r, plan, "error.budget_plan.csv_malformed")
			return
		}
		lineNo++
		// Skip a blank line and an optional header row (first field "description"/"desc").
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		if lineNo == 1 {
			first := strings.ToLower(strings.TrimSpace(rec[0]))
			if first == "description" || first == "desc" {
				continue
			}
		}
		if len(rec) < 5 {
			s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.csv_columns")
			return
		}
		desc := strings.TrimSpace(rec[0])
		iso, ok := parseUserDate(strings.TrimSpace(rec[1]), df, s.now())
		if !ok {
			s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.date")
			return
		}
		acctID := acctByName[strings.ToLower(strings.TrimSpace(rec[2]))]
		if acctID == 0 {
			s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.account")
			return
		}
		in := store.BudgetSplitInput{Description: desc, Date: iso, AccountID: acctID, Currency: ccy}
		if fn := strings.TrimSpace(rec[3]); fn != "" {
			fid := fundByName[strings.ToLower(fn)]
			if fid == 0 {
				s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.fund")
				return
			}
			in.FundID = &fid
		}
		// The amount is the LAST field; the optional program sits between fund and amount.
		amountStr := strings.TrimSpace(rec[len(rec)-1])
		if len(rec) >= 6 {
			if pn := strings.TrimSpace(rec[4]); pn != "" {
				pid := progByName[strings.ToLower(pn)]
				if pid == 0 {
					s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.program_scope")
					return
				}
				in.ProgramID = &pid
			}
		}
		mag, perr := money.Parse(amountStr, exp, numFmt)
		if perr != nil || mag <= 0 {
			s.renderBudgetImportRowError(w, r, plan, lineNo, "error.budget_plan.amount")
			return
		}
		in.Amount = mag
		parsed = append(parsed, in)
	}

	// Persist the whole batch ATOMICALLY (one store change): a mid-batch store rejection
	// (e.g. an R/E row missing a program) rolls the ENTIRE append back, so a retry after
	// the user fixes the CSV cannot duplicate the already-good rows. The failing desired
	// index maps back to the CSV row number for the hint.
	if failedIdx, err := s.store.AppendBudgetSplits(s.actorCtx(ctx), plan.ID, parsed); err != nil {
		if key, ok := budgetSplitErrorKey(err); ok {
			row := failedIdx + 1
			if failedIdx < 0 {
				row = 0
			}
			s.renderBudgetImportRowError(w, r, plan, row, key)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budget-plans/"+strconv.FormatInt(id, 10))
}

// renderBudgetImportError re-renders the plan detail at 422 with a page-level import
// error (a whole-file problem: missing/malformed CSV).
func (s *server) renderBudgetImportError(w http.ResponseWriter, r *http.Request, plan sqlc.BudgetPlan, key string) {
	model, ok := s.buildBudgetPlanDetailModel(w, r, plan)
	if !ok {
		return
	}
	model.ErrorMsg = i18n.T(langOf(r.Context()), key)
	s.render(w, r, http.StatusUnprocessableEntity, "budget_plan_detail.tmpl", s.newShellPage(r, model))
}

// renderBudgetImportRowError re-renders the plan detail at 422 with a page-level hint
// naming the offending CSV row number + reason (the bank-import in-place error-fragment
// convention: a bad row is a hint, not a 500).
func (s *server) renderBudgetImportRowError(w http.ResponseWriter, r *http.Request, plan sqlc.BudgetPlan, row int, key string) {
	model, ok := s.buildBudgetPlanDetailModel(w, r, plan)
	if !ok {
		return
	}
	lang := langOf(r.Context())
	model.ErrorMsg = i18n.T(lang, "error.budget_plan.csv_row", row, i18n.T(lang, key))
	s.render(w, r, http.StatusUnprocessableEntity, "budget_plan_detail.tmpl", s.newShellPage(r, model))
}
