package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cuento/internal/budget"
	"cuento/internal/db/sqlc"
	"cuento/internal/i18n"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p19.3 budget management (/budgets). Budgets are PLANNING/BOOKKEEPING data (like
// funds p12.5 and D-conventions): the VIEW routes (list + detail + schedule library)
// are TxnRead (they feed the p19.4 reports), the create/edit/delete MUTATIONS are
// TxnWrite -- matching funds management (subsidiaries/users stay Admin). The store
// (p19.1, internal/store/budgets.go) owns ALL validation; these handlers only
// collect a form and TRANSLATE the store's typed errors to (field, i18n-key) pairs
// via the p10.3 form-error convention (422 + partial + autofocus).
//
// Three surfaces:
//   - SCHEDULE LIBRARY (/schedules): named, reusable recurrences with
//     KIND-SPECIFIC pickers (day-of-month, ordinal-weekday, semimonthly, biweekly/
//     weekly anchor, custom imported date list) + a weekend policy. All kind fields
//     render at once; a small CSP-safe ES module (budgetkind.js) SHOWS/HIDES the
//     relevant block on a kind change -- NO server round-trip, so no htmx settle race
//     on the kind select (DECISIONS p19.3). The store is the sole arbiter of field
//     consistency (ErrScheduleInvalid).
//   - BUDGET LIST + form (/budgets): a budget is a name + period bounds.
//   - LINE EDITOR (within a budget): sub/account(R-E leaf)/fund/program/amount/
//     currency/schedule. The fund/program selectors are SCOPED to the chosen
//     subsidiary exactly like the transaction editor (p12.2): a subsidiary change
//     re-fetches the line form via hx-get so the server re-filters the options
//     (server is the source of truth for fund scope, D20). Accounts are filtered to
//     revenue/expense LEAVES (a budget is of R/E flows, ErrBudgetLineAccountNotRE).
//
// Dates (anchor, custom list) render + parse through money.FormatDate/ParseDate
// honoring the user's date_format (rule 10 -- never input[type=date], never raw
// time.Parse/strconv on a date path). Every string via {{t}} (rule 9); no inline
// script (rule 12).

// ===========================================================================
// SCHEDULE LIBRARY
// ===========================================================================

// scheduleRow is one rendered schedule on the library list: id, name, a localized
// kind label key, and a human summary of its selectors.
type scheduleRow struct {
	ID      int64
	Name    string
	KindKey string // i18n key: budget.kind.<kind>
	Summary string // pre-localized one-line description of the selectors
}

// schedulesPageModel is the GET /schedules model.
type schedulesPageModel struct {
	Rows []scheduleRow
}

// scheduleKinds / weekdays / ordinals / weekendPolicies are the static enums the
// pickers offer (the engine's constants, mirrored for the template range).
var (
	scheduleKinds    = []string{budget.KindOnetime, budget.KindAnnual, budget.KindMonthly, budget.KindSemimonthly, budget.KindBiweekly, budget.KindWeekly, budget.KindCustom}
	weekendPolicies  = []string{budget.WeekendActual, budget.WeekendPrevBiz, budget.WeekendNextBiz}
	weekdayValues    = []int{0, 1, 2, 3, 4, 5, 6}
	ordinalValues    = []int{1, 2, 3, 4, -1} // 1st..4th, then last (-1)
	dayOfMonthValues = daysOneThroughLast()  // 1..31, then -1 (last)
)

func daysOneThroughLast() []int {
	out := make([]int, 0, 32)
	for d := 1; d <= 31; d++ {
		out = append(out, d)
	}
	return append(out, -1) // month-end sentinel
}

// schedulesPage handles GET /schedules (TxnRead): the schedule library list.
func (s *server) schedulesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	rows, err := s.store.ListSchedules(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	df := dateFormatFor(u)
	model := schedulesPageModel{}
	for _, sc := range rows {
		model.Rows = append(model.Rows, scheduleRow{
			ID:      sc.ID,
			Name:    sc.Name,
			KindKey: "budget.kind." + sc.Kind,
			Summary: scheduleSummary(langOf(ctx), sc, df),
		})
	}
	s.render(w, r, http.StatusOK, "schedules.tmpl", s.newShellPage(r, model))
}

// scheduleSummary builds a short description of a schedule's key selector for the
// list (e.g. "day 15", "anchor 2025-01-03"). Kept compact and mostly language-
// neutral (numbers/dates); the form is the authoritative editor. Uses the i18n
// weekday/ordinal labels for the ordinal-weekday case.
func scheduleSummary(lang string, sc sqlc.BudgetSchedule, df money.DateFormat) string {
	switch sc.Kind {
	case budget.KindMonthly:
		if sc.Ordinal.Valid && sc.Ordinal.Int64 != 0 {
			return i18n.T(lang, "budget.ordinal."+ordKey(int(sc.Ordinal.Int64))) + " " +
				i18n.T(lang, "budget.weekday."+strconv.FormatInt(sc.Weekday.Int64, 10))
		}
		if sc.DayOfMonth.Valid {
			return i18n.T(lang, "budget.summary.day") + " " + domLabel(int(sc.DayOfMonth.Int64), lang)
		}
	case budget.KindSemimonthly:
		return domLabel(int(sc.DayOfMonth.Int64), lang) + ", " + domLabel(int(sc.DayOfMonth2.Int64), lang)
	case budget.KindWeekly:
		return i18n.T(lang, "budget.weekday."+strconv.FormatInt(sc.Weekday.Int64, 10))
	case budget.KindBiweekly, budget.KindOnetime, budget.KindAnnual:
		if sc.AnchorDate.Valid && sc.AnchorDate.String != "" {
			return i18n.T(lang, "budget.summary.anchor") + " " +
				money.FormatDate(parseISOForDisplay(sc.AnchorDate.String), df)
		}
	}
	return ""
}

// domLabel renders a day-of-month value: the number, or the "last" label for the
// month-end sentinel (-1).
func domLabel(dom int, lang string) string {
	if dom == -1 {
		return i18n.T(lang, "budget.dom.last")
	}
	return strconv.Itoa(dom)
}

// ordKey maps an ordinal value to its i18n sub-key ("1".."4","last"). Exported to
// the template funcmap so schedule_form.tmpl can label the ordinal select options.
func ordKey(ord int) string {
	if ord == -1 {
		return "last"
	}
	return strconv.Itoa(ord)
}

// scheduleFormModel is the create/edit form model: the value fields (all kinds'
// fields rendered; the JS shows the active one), the option enums, and the errors.
type scheduleFormModel struct {
	ID   int64
	Name string
	Kind string

	DayOfMonth  int    // 0 = unset
	DayOfMonth2 int    // 0 = unset
	Ordinal     int    // 0 = unset
	Weekday     int    // 0..6 (default 0=Sunday)
	AnchorDate  string // formatted in the user's date format
	Weekend     string
	CustomDates string // one date per line, formatted in the user's date format
	Notes       string

	Kinds           []string
	WeekendPolicies []string
	Weekdays        []int
	Ordinals        []int
	DaysOfMonth     []int

	Errors formErrors
}

// buildScheduleForm returns the base form model with the option enums filled and
// sensible defaults (monthly, prev_business_day).
func (s *server) buildScheduleForm() scheduleFormModel {
	return scheduleFormModel{
		Kind:            budget.KindMonthly,
		Weekend:         budget.DefaultWeekend,
		Kinds:           scheduleKinds,
		WeekendPolicies: weekendPolicies,
		Weekdays:        weekdayValues,
		Ordinals:        ordinalValues,
		DaysOfMonth:     dayOfMonthValues,
	}
}

// scheduleNewForm handles GET /schedules/new (TxnWrite): the empty form.
func (s *server) scheduleNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "schedule-form", s.buildScheduleForm())
}

// scheduleEditForm handles GET /schedules/{id}/edit (TxnWrite): the form
// prefilled from the schedule's current state (fields + imported custom dates).
func (s *server) scheduleEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	sc, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	df := dateFormatFor(u)
	form := s.buildScheduleForm()
	form.ID = id
	form.Name = sc.Name
	form.Kind = sc.Kind
	if sc.DayOfMonth.Valid {
		form.DayOfMonth = int(sc.DayOfMonth.Int64)
	}
	if sc.DayOfMonth2.Valid {
		form.DayOfMonth2 = int(sc.DayOfMonth2.Int64)
	}
	if sc.Ordinal.Valid {
		form.Ordinal = int(sc.Ordinal.Int64)
	}
	if sc.Weekday.Valid {
		form.Weekday = int(sc.Weekday.Int64)
	}
	if sc.AnchorDate.Valid && sc.AnchorDate.String != "" {
		form.AnchorDate = money.FormatDate(parseISOForDisplay(sc.AnchorDate.String), df)
	}
	form.Weekend = sc.WeekendAdjust
	form.Notes = sc.Notes
	if sc.Kind == budget.KindCustom {
		dates, err := s.store.ScheduleDates(ctx, id)
		if err != nil {
			s.serverError(w)
			return
		}
		var lines []string
		for _, d := range dates {
			lines = append(lines, money.FormatDate(parseISOForDisplay(d), df))
		}
		form.CustomDates = strings.Join(lines, "\n")
	}
	s.render(w, r, http.StatusOK, "schedule-form", form)
}

// parseScheduleForm reads the POST form into a scheduleFormModel (echo for a 422
// re-render) and a store.ScheduleInput (for the store call). Dates are parsed
// through the user's date format (rule 10). A malformed date is NOT rejected here --
// it is passed to the store as-is-invalid so the store's engine surfaces
// ErrScheduleInvalid (single source of the date rules); but a custom-list line that
// fails to parse is flagged form-locally (there is no store field error for one bad
// custom line, so we validate the format at the edge and surface the kind error).
func (s *server) parseScheduleForm(r *http.Request, id int64) (scheduleFormModel, store.ScheduleInput, bool) {
	u := currentUser(r.Context())
	df := dateFormatFor(u)
	_ = r.ParseForm()

	form := s.buildScheduleForm()
	form.ID = id
	form.Name = strings.TrimSpace(r.PostFormValue("name"))
	form.Kind = r.PostFormValue("kind")
	form.Weekend = r.PostFormValue("weekend_adjust")
	form.CustomDates = r.PostFormValue("custom_dates")
	form.Notes = strings.TrimSpace(r.PostFormValue("notes"))

	// Read the KIND-SPECIFIC fields from the block that owns them for the submitted
	// kind. EVERY kind block is rendered at once (the JS only toggles visibility, which
	// does NOT stop submission), so several controls share a field NAME across blocks;
	// r.PostFormValue returns the FIRST in DOM order. To read the right block we switch
	// on kind and use the kind-unique names (sm_day_of_month, weekly_weekday,
	// weekly_anchor) where the shared name would otherwise pick the wrong block's value.
	switch form.Kind {
	case budget.KindMonthly:
		form.DayOfMonth = atoiOr(r.PostFormValue("day_of_month"), 0)
		form.Ordinal = atoiOr(r.PostFormValue("ordinal"), 0)
		form.Weekday = atoiOr(r.PostFormValue("weekday"), 0)
	case budget.KindSemimonthly:
		form.DayOfMonth = atoiOr(r.PostFormValue("sm_day_of_month"), 0)
		form.DayOfMonth2 = atoiOr(r.PostFormValue("day_of_month_2"), 0)
	case budget.KindWeekly:
		form.Weekday = atoiOr(r.PostFormValue("weekly_weekday"), 0)
		form.AnchorDate = strings.TrimSpace(r.PostFormValue("weekly_anchor"))
	case budget.KindOnetime, budget.KindAnnual, budget.KindBiweekly:
		form.AnchorDate = strings.TrimSpace(r.PostFormValue("anchor_date"))
	}

	in := store.ScheduleInput{
		Name:          form.Name,
		Kind:          form.Kind,
		WeekendAdjust: form.Weekend,
		Notes:         form.Notes,
	}
	if form.DayOfMonth != 0 {
		v := form.DayOfMonth
		in.DayOfMonth = &v
	}
	if form.DayOfMonth2 != 0 {
		v := form.DayOfMonth2
		in.DayOfMonth2 = &v
	}
	if form.Ordinal != 0 {
		v := form.Ordinal
		in.Ordinal = &v
	}
	// Weekday only matters for ordinal-weekday and weekly kinds; always pass it (the
	// engine ignores it for other kinds).
	if form.Kind == budget.KindMonthly || form.Kind == budget.KindWeekly {
		v := form.Weekday
		in.Weekday = &v
	}
	// Anchor date: parse through the user's format to ISO. A blank/unparseable anchor
	// is left empty -> the store's engine rejects it for the kinds that need it.
	if form.AnchorDate != "" {
		if iso, ok := parseUserDate(form.AnchorDate, df, s.now()); ok {
			in.AnchorDate = &iso
		}
	}
	// Custom dates: one per line; parse each through the user's format. A line that
	// fails to parse flags a form-local error (there is no store field for it).
	badDate := false
	if form.Kind == budget.KindCustom {
		var dates []string
		for _, raw := range strings.Split(form.CustomDates, "\n") {
			line := strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			iso, ok := parseUserDate(line, df, s.now())
			if !ok {
				badDate = true
				continue
			}
			dates = append(dates, iso)
		}
		in.CustomDates = dates
	}
	return form, in, badDate
}

// scheduleCreate handles POST /schedules (TxnWrite).
func (s *server) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in, badDate := s.parseScheduleForm(r, 0)
	if badDate {
		form.Errors.add("custom_dates", "error.budget.custom_date_invalid")
		s.renderFormError(w, r, "schedule-form", form)
		return
	}
	if _, err := s.store.CreateSchedule(s.actorCtx(ctx), in); err != nil {
		s.renderScheduleFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/schedules")
}

// scheduleUpdate handles POST /schedules/{id} (TxnWrite).
func (s *server) scheduleUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in, badDate := s.parseScheduleForm(r, id)
	if badDate {
		form.Errors.add("custom_dates", "error.budget.custom_date_invalid")
		s.renderFormError(w, r, "schedule-form", form)
		return
	}
	if err := s.store.UpdateSchedule(s.actorCtx(ctx), id, in); err != nil {
		s.renderScheduleFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/schedules")
}

// renderScheduleFormError maps a store typed error to a field-error key and re-renders
// the schedule form at 422. ErrScheduleInvalid attaches to the kind select (the
// picker region) so focus lands on the kind; an unknown error is a real fault.
func (s *server) renderScheduleFormError(w http.ResponseWriter, r *http.Request, form scheduleFormModel, err error) {
	switch {
	case errors.Is(err, store.ErrScheduleInvalid):
		form.Errors.add("kind", "error.budget.schedule_invalid")
	case errors.Is(err, store.ErrScheduleNotFound):
		form.Errors.add("name", "error.budget.schedule_not_found")
	default:
		s.serverError(w)
		return
	}
	s.renderFormError(w, r, "schedule-form", form)
}

// ===========================================================================
// BUDGET LIST + form + detail
// ===========================================================================

// budgetRow is one rendered budget on the list: id, name, and formatted period.
type budgetRow struct {
	ID          int64
	Name        string
	PeriodStart string // formatted (rule 10)
	PeriodEnd   string
}

// budgetsPageModel is the GET /budgets model.
type budgetsPageModel struct {
	Rows []budgetRow
}

// budgetsPage handles GET /budgets (TxnRead): the budget list.
func (s *server) budgetsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	budgets, err := s.store.ListBudgets(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	df := dateFormatFor(u)
	model := budgetsPageModel{}
	for _, b := range budgets {
		model.Rows = append(model.Rows, budgetRow{
			ID:          b.ID,
			Name:        b.Name,
			PeriodStart: money.FormatDate(parseISOForDisplay(b.PeriodStart), df),
			PeriodEnd:   money.FormatDate(parseISOForDisplay(b.PeriodEnd), df),
		})
	}
	s.render(w, r, http.StatusOK, "budgets.tmpl", s.newShellPageControls(r, model, "budgets"))
}

// budgetFormModel is the budget create/edit form. Dates are in the user's format.
type budgetFormModel struct {
	ID          int64
	Name        string
	PeriodStart string
	PeriodEnd   string
	Notes       string
	Errors      formErrors
}

// budgetNewForm handles GET /budgets/new (TxnWrite).
func (s *server) budgetNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "budget-form", budgetFormModel{})
}

// budgetEditForm handles GET /budgets/{id}/edit (TxnWrite).
func (s *server) budgetEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))
	b, err := s.store.GetBudget(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	df := dateFormatFor(u)
	s.render(w, r, http.StatusOK, "budget-form", budgetFormModel{
		ID:          id,
		Name:        b.Name,
		PeriodStart: money.FormatDate(parseISOForDisplay(b.PeriodStart), df),
		PeriodEnd:   money.FormatDate(parseISOForDisplay(b.PeriodEnd), df),
		Notes:       b.Notes,
	})
}

// parseBudgetForm reads the POST form into a budgetFormModel + a store.BudgetInput.
// Period dates parse through the user's format; a bad date flags a form error.
func (s *server) parseBudgetForm(r *http.Request, id int64) (budgetFormModel, store.BudgetInput) {
	df := dateFormatFor(currentUser(r.Context()))
	_ = r.ParseForm()
	form := budgetFormModel{
		ID:          id,
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		PeriodStart: strings.TrimSpace(r.PostFormValue("period_start")),
		PeriodEnd:   strings.TrimSpace(r.PostFormValue("period_end")),
		Notes:       strings.TrimSpace(r.PostFormValue("notes")),
	}
	in := store.BudgetInput{Name: form.Name, Notes: form.Notes}
	if iso, ok := parseUserDate(form.PeriodStart, df, s.now()); ok {
		in.PeriodStart = iso
	}
	if iso, ok := parseUserDate(form.PeriodEnd, df, s.now()); ok {
		in.PeriodEnd = iso
	}
	return form, in
}

// budgetCreate handles POST /budgets (TxnWrite).
func (s *server) budgetCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in := s.parseBudgetForm(r, 0)
	if key := validateBudgetDates(in); key != "" {
		form.Errors.add("period_start", key)
		s.renderFormError(w, r, "budget-form", form)
		return
	}
	id, err := s.store.CreateBudget(s.actorCtx(ctx), in)
	if err != nil {
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budgets/"+strconv.FormatInt(id, 10))
}

// budgetUpdate handles POST /budgets/{id} (TxnWrite).
func (s *server) budgetUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in := s.parseBudgetForm(r, id)
	if key := validateBudgetDates(in); key != "" {
		form.Errors.add("period_start", key)
		s.renderFormError(w, r, "budget-form", form)
		return
	}
	if err := s.store.UpdateBudget(s.actorCtx(ctx), id, in); err != nil {
		if errors.Is(err, store.ErrBudgetNotFound) {
			form.Errors.add("name", "error.budget.not_found")
			s.renderFormError(w, r, "budget-form", form)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/budgets/"+strconv.FormatInt(id, 10))
}

// validateBudgetDates checks the period bounds parsed and are ordered (the store
// does not enforce ordering; this is the only edge check). It reads the ISO bounds on
// the parsed store input -- an empty ISO means the corresponding display value was
// blank/unparseable. Returns "" when valid; else an i18n error key. Since canonical
// ISO "YYYY-MM-DD" sorts lexicographically, a plain string compare orders the bounds.
func validateBudgetDates(in store.BudgetInput) string {
	if in.PeriodStart == "" || in.PeriodEnd == "" {
		return "error.budget.period_required"
	}
	if in.PeriodEnd < in.PeriodStart {
		return "error.budget.period_order"
	}
	return ""
}

// budgetDetailModel is the GET /budgets/{id} model: the budget header + its lines,
// each with resolved names (account/sub/fund/program/schedule) and formatted amount.
type budgetDetailModel struct {
	ID          int64
	Name        string
	PeriodStart string
	PeriodEnd   string
	Notes       string
	Lines       []budgetLineRow
}

// budgetLineRow is one rendered budget line on the detail page.
type budgetLineRow struct {
	ID        int64
	SubName   string
	AcctName  string
	FundName  string // "" = unrestricted
	ProgName  string
	AmountFmt string
	SchedName string
}

// budgetDetail handles GET /budgets/{id} (TxnRead): the budget + its lines.
func (s *server) budgetDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := parseID(r.PathValue("id"))
	b, err := s.store.GetBudget(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	lines, err := s.store.BudgetLines(ctx, id)
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
	schedNames, err := s.scheduleNameMap(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	df := dateFormatFor(u)
	opts := formatOptsFor(u)
	model := budgetDetailModel{
		ID:          id,
		Name:        b.Name,
		PeriodStart: money.FormatDate(parseISOForDisplay(b.PeriodStart), df),
		PeriodEnd:   money.FormatDate(parseISOForDisplay(b.PeriodEnd), df),
		Notes:       b.Notes,
	}
	for _, l := range lines {
		row := budgetLineRow{
			ID:        l.ID,
			SubName:   subNames[l.SubsidiaryID],
			AcctName:  acctNames[l.AccountID],
			ProgName:  progNames[l.ProgramID],
			AmountFmt: l.Currency + " " + money.Format(l.Amount, exps[l.Currency], opts),
			SchedName: schedNames[l.ScheduleID],
		}
		if l.FundID.Valid {
			row.FundName = fundNames[l.FundID.Int64]
		}
		model.Lines = append(model.Lines, row)
	}
	s.render(w, r, http.StatusOK, "budget_detail.tmpl", s.newShellPage(r, model))
}

// fundNameMap returns id->name for every fund (active and closed), for the line
// detail + editor.
func (s *server) fundNameMap(ctx context.Context) (map[int64]string, error) {
	funds, err := s.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(funds))
	for _, f := range funds {
		m[f.ID] = f.Name
	}
	return m, nil
}

// scheduleNameMap returns id->name for every schedule (for the line detail + editor).
func (s *server) scheduleNameMap(ctx context.Context) (map[int64]string, error) {
	scheds, err := s.store.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(scheds))
	for _, sc := range scheds {
		m[sc.ID] = sc.Name
	}
	return m, nil
}

// ===========================================================================
// LINE EDITOR (sub-scoped fund/program selectors)
// ===========================================================================

// lineFormModel is the budget-line create/edit form. The account/fund/program option
// lists are SCOPED to the chosen subsidiary (a subsidiary change re-fetches the form
// via hx-get, so the server re-filters -- exactly like the transaction editor). ID 0
// = create.
type lineFormModel struct {
	BudgetID   int64
	ID         int64
	Subsidiary int64
	AccountID  int64
	FundID     int64 // 0 = unrestricted
	ProgramID  int64
	Amount     string // in the user's number format
	Currency   string
	ScheduleID int64

	Subs       []subOption
	Accounts   []txnOption // R/E leaves scoped to Subsidiary
	Funds      []txnOption // ActiveFunds(Subsidiary)
	Programs   []txnOption
	Currencies []currencyOption
	Schedules  []txnOption

	Errors formErrors
}

// buildLineForm assembles the sub-scoped option lists for a budget line form. The
// subsidiary select offers all subs; accounts are the R/E leaves in `sub`; funds are
// ActiveFunds(sub); programs are all active; currencies + schedules are global. It is
// shared by the new/edit forms and the sub-change re-filter so the options always
// match the chosen subsidiary.
func (s *server) buildLineForm(ctx context.Context, budgetID, sub int64) (lineFormModel, error) {
	lang := langOf(ctx)
	form := lineFormModel{BudgetID: budgetID, Subsidiary: sub}

	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		return form, err
	}
	for _, sb := range subs {
		form.Subs = append(form.Subs, subOption{ID: sb.ID, Name: sb.Name})
	}

	// Accounts: leaf+active in `sub`, filtered to revenue/expense (a budget is of R/E
	// flows -- ErrBudgetLineAccountNotRE rejects anything else, so we never offer it).
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

	// Funds scoped to `sub` (D20), like the transaction editor.
	funds, err := s.store.ActiveFunds(ctx, sub)
	if err != nil {
		return form, err
	}
	for _, f := range funds {
		form.Funds = append(form.Funds, txnOption{ID: f.ID, Name: f.Name})
	}

	// Programs: all active (the tree, like the transaction editor -- programs are not
	// subsidiary-scoped, DECISIONS p19.3).
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

	// Currencies (enabled).
	curs, err := s.store.Currencies(ctx)
	if err != nil {
		return form, err
	}
	for _, c := range curs {
		if c.Active == 0 {
			continue
		}
		form.Currencies = append(form.Currencies, currencyOption{Code: c.Code, Name: c.Name})
	}
	// Default the currency to the subsidiary's base currency (D18).
	for _, sb := range subs {
		if sb.ID == sub && sb.BaseCurrency != "" {
			form.Currency = sb.BaseCurrency
		}
	}

	scheds, err := s.store.ListSchedules(ctx)
	if err != nil {
		return form, err
	}
	for _, sc := range scheds {
		form.Schedules = append(form.Schedules, txnOption{ID: sc.ID, Name: sc.Name})
	}
	return form, nil
}

// lineNewForm handles GET /budgets/{id}/lines/new (TxnWrite): a blank line form. It
// ALSO serves the subsidiary-change re-filter -- a `subsidiary` query param means the
// hx-get from the sub select re-fetched to re-scope fund/program/account options.
func (s *server) lineNewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	budgetID := parseID(r.PathValue("id"))
	sub := s.defaultSubsidiary(ctx, u)
	if q := r.URL.Query().Get("subsidiary"); q != "" {
		sub = parseID(q)
	}
	form, err := s.buildLineForm(ctx, budgetID, sub)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "line-form", form)
}

// lineEditForm handles GET /budgets/{id}/lines/{lid}/edit (TxnWrite): the line form
// prefilled. A `subsidiary` query param (the sub-change re-filter) overrides the
// stored subsidiary so the re-scoped options render.
func (s *server) lineEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	budgetID := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	line, err := s.store.GetBudgetLine(ctx, lineID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub := line.SubsidiaryID
	reFilter := r.URL.Query().Get("subsidiary") != ""
	if reFilter {
		sub = parseID(r.URL.Query().Get("subsidiary"))
	}
	form, err := s.buildLineForm(ctx, budgetID, sub)
	if err != nil {
		s.serverError(w)
		return
	}
	form.ID = lineID
	form.AccountID = line.AccountID
	if line.FundID.Valid {
		form.FundID = line.FundID.Int64
	}
	form.ProgramID = line.ProgramID
	form.ScheduleID = line.ScheduleID
	form.Amount = money.Format(line.Amount, currencyExp(ctx, s.store, line.Currency), formatOptsFor(u))
	if !reFilter {
		form.Currency = line.Currency
	}
	s.render(w, r, http.StatusOK, "line-form", form)
}

// currencyExp returns a currency's minor-unit exponent (best-effort; 2 on miss).
func currencyExp(ctx context.Context, st *store.Store, code string) int {
	exps, err := currencyExponents(ctx, st)
	if err != nil {
		return 2
	}
	if e, ok := exps[code]; ok {
		return e
	}
	return 2
}

// parseLineForm reads the POST form into a lineFormModel (echo) + a store
// .BudgetLineInput. The amount parses through the user's number format for the chosen
// currency; a bad amount is left as 0 so the store rejects it (ErrBudgetAmount).
func (s *server) parseLineForm(r *http.Request, budgetID, lineID int64) (lineFormModel, store.BudgetLineInput, error) {
	ctx := r.Context()
	u := currentUser(ctx)
	_ = r.ParseForm()
	sub := parseID(r.PostFormValue("subsidiary_id"))
	form, err := s.buildLineForm(ctx, budgetID, sub)
	if err != nil {
		return lineFormModel{}, store.BudgetLineInput{}, err
	}
	form.ID = lineID
	form.AccountID = parseID(r.PostFormValue("account_id"))
	form.FundID = parseID(r.PostFormValue("fund_id"))
	form.ProgramID = parseID(r.PostFormValue("program_id"))
	form.Currency = r.PostFormValue("currency")
	form.ScheduleID = parseID(r.PostFormValue("schedule_id"))
	form.Amount = strings.TrimSpace(r.PostFormValue("amount"))

	in := store.BudgetLineInput{
		SubsidiaryID: sub,
		AccountID:    form.AccountID,
		ProgramID:    form.ProgramID,
		Currency:     form.Currency,
		ScheduleID:   form.ScheduleID,
	}
	if form.FundID > 0 {
		fid := form.FundID
		in.FundID = &fid
	}
	// Amount: parse through the user's number format for the currency's exponent. A
	// parse failure yields 0 -> the store rejects it with ErrBudgetAmount (surfaced as
	// the amount field error).
	amt, perr := money.Parse(form.Amount, currencyExp(ctx, s.store, form.Currency), numberFormatFor(u))
	if perr == nil {
		in.Amount = amt
	}
	return form, in, nil
}

// lineCreate handles POST /budgets/{id}/lines (TxnWrite).
func (s *server) lineCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	budgetID := parseID(r.PathValue("id"))
	form, in, err := s.parseLineForm(r, budgetID, 0)
	if err != nil {
		s.serverError(w)
		return
	}
	if _, err := s.store.CreateBudgetLine(s.actorCtx(ctx), budgetID, in); err != nil {
		s.renderLineFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/budgets/"+strconv.FormatInt(budgetID, 10))
}

// lineUpdate handles POST /budgets/{id}/lines/{lid} (TxnWrite).
func (s *server) lineUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	budgetID := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	form, in, err := s.parseLineForm(r, budgetID, lineID)
	if err != nil {
		s.serverError(w)
		return
	}
	if err := s.store.UpdateBudgetLine(s.actorCtx(ctx), lineID, in); err != nil {
		s.renderLineFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/budgets/"+strconv.FormatInt(budgetID, 10))
}

// lineDelete handles POST /budgets/{id}/lines/{lid}/delete (TxnWrite): a hard delete
// with an audit version (rule 14). Redirects to the budget detail.
func (s *server) lineDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	budgetID := parseID(r.PathValue("id"))
	lineID := parseID(r.PathValue("lid"))
	if err := s.store.DeleteBudgetLine(s.actorCtx(ctx), lineID); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/budgets/"+strconv.FormatInt(budgetID, 10), http.StatusSeeOther)
}

// renderLineFormError maps a store typed error to a (field, key) pair and re-renders
// the line form at 422 (the p10.3 convention). An unknown error is a real fault.
func (s *server) renderLineFormError(w http.ResponseWriter, r *http.Request, form lineFormModel, err error) {
	field, key := budgetLineErrorField(err)
	if key == "" {
		s.serverError(w)
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "line-form", form)
}

// budgetLineErrorField maps a store typed error to the (form field, i18n key) pair
// the form-error convention needs (drives autofocus). Mirrors fundErrorField.
func budgetLineErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrBudgetLineAccountNotRE):
		return "account_id", "error.budget.account_not_re"
	case errors.Is(err, store.ErrBudgetAmount):
		return "amount", "error.budget.amount"
	case errors.Is(err, store.ErrBudgetRefMissing):
		return "schedule_id", "error.budget.ref_missing"
	case errors.Is(err, store.ErrBudgetNotFound):
		return "account_id", "error.budget.not_found"
	case errors.Is(err, store.ErrBudgetLineNotFound):
		return "account_id", "error.budget.line_not_found"
	default:
		return "", ""
	}
}

// ===========================================================================
// small parse helpers
// ===========================================================================

// atoiOr parses s as an int, returning def on failure/empty (the schedule pickers'
// selects always submit a valid int; def is the "unset" sentinel 0).
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// parseUserDate parses a display-format date (the user's date_format) to canonical
// ISO "YYYY-MM-DD" via money.ParseDate (rule 10). ok=false on a blank/malformed
// value so the caller leaves the field empty (the store/engine then rejects it).
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
