package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/store"
	"cuento/internal/testutil"
	"cuento/internal/testutil/fixture"
)

// p19.3 budget-management handler tests. Driven through the REAL mounted router
// (httptest) against a real migrated db (AGENTS testing conventions) -- no
// handler-level store mocks. Schedules/budgets are validated by the store (p19.1);
// these tests assert the handlers persist (and version) through the funnel, that
// validation surfaces cleanly (422 + i18n field error), and that the perm gate
// (manage = TxnWrite) holds beyond the matrix.

// budgetsFixtureApp builds an app over the synthetic fixture and a write-capable
// user, returning the handler, store, session manager, ids, and the writer id.
func budgetsFixtureApp(t *testing.T) (http.Handler, *store.Store, *sql.DB, *scs.SessionManager, fixture.IDs, int64) {
	t.Helper()
	fx := fixture.New(t)
	app := NewApp(Config{Version: "test"}, fx.DB, fx.Store)
	writer := mkUser(t, fx.Store, "bwriter", "write", false)
	return app.handler, fx.Store, fx.DB, app.sessions, fx.IDs, writer
}

// budgetsSimpleApp builds a bare app (no fixture) for the perm test.
func budgetsSimpleApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, app.sessions
}

// ctxBG is the background context for the tests' direct store reads (no actor
// needed for reads).
func ctxBG() context.Context { return context.Background() }

// --- SCHEDULES ---------------------------------------------------------------

// TestScheduleCreateMonthlyDoM: a writer creates a monthly day-of-month schedule
// (day 15, prev_business_day); it persists and versions, and appears on the list.
func TestScheduleCreateMonthlyDoM(t *testing.T) {
	h, st, db, sm, _, writer := budgetsFixtureApp(t)

	form := url.Values{}
	form.Set("name", "Payroll 15th")
	form.Set("kind", "monthly")
	form.Set("day_of_month", "15")
	form.Set("weekend_adjust", "prev_business_day")
	rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST schedule = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}

	id := scheduleIDByName(t, st, "Payroll 15th")
	if id == 0 {
		t.Fatal("schedule not persisted")
	}
	testutil.AssertVersioned(t, db, "budget_schedules", id, "create")

	// It appears on the library list.
	list := asUser(t, h, sm, writer, http.MethodGet, "/schedules", nil)
	if !strings.Contains(list.Body.String(), "Payroll 15th") {
		t.Errorf("schedule list missing the new schedule")
	}
}

// TestScheduleCreateEveryKind: smoke-cover EVERY schedule kind through the handler,
// posting the way the real DOM serializes -- because every kind block is rendered at
// once (the JS only toggles visibility), a browser submits the OTHER blocks' controls
// too. So for each kind we also Add the colliding monthly/anchor default controls that
// precede the target block in DOM order (day_of_month, weekday, ordinal, anchor_date),
// proving the handler reads the KIND-UNIQUE names (sm_day_of_month, weekly_weekday,
// weekly_anchor) rather than the first-in-DOM shared name. Each kind persists+versions.
func TestScheduleCreateEveryKind(t *testing.T) {
	h, st, db, sm, _, writer := budgetsFixtureApp(t)

	// domDefaults mimics the always-rendered monthly + onetime-anchor block controls a
	// browser posts regardless of the chosen kind (their "None"/default values).
	domDefaults := func(v url.Values) {
		v.Add("day_of_month", "0") // monthly block, first day_of_month in DOM
		v.Add("ordinal", "0")
		v.Add("weekday", "0")    // monthly block weekday, first weekday in DOM
		v.Add("anchor_date", "") // onetime/annual/biweekly block, first anchor in DOM
	}

	cases := []struct {
		name  string
		build func(url.Values)
	}{
		{"OneTime Gala", func(v url.Values) {
			v.Set("kind", "onetime")
			v.Set("anchor_date", "2025-06-15")
		}},
		{"Annual Audit", func(v url.Values) {
			v.Set("kind", "annual")
			v.Set("anchor_date", "2025-03-31")
		}},
		{"Monthly 15th", func(v url.Values) {
			v.Set("kind", "monthly")
			v.Set("day_of_month", "15")
		}},
		{"2nd Monday", func(v url.Values) {
			v.Set("kind", "monthly")
			v.Set("ordinal", "2")
			v.Set("weekday", "1")
		}},
		{"Semimonthly 15/last", func(v url.Values) {
			v.Set("kind", "semimonthly")
			v.Set("sm_day_of_month", "15") // kind-unique name (NOT the monthly day_of_month)
			v.Set("day_of_month_2", "-1")
		}},
		{"Biweekly Payroll", func(v url.Values) {
			v.Set("kind", "biweekly")
			v.Set("anchor_date", "2025-01-03")
		}},
		{"Every Friday", func(v url.Values) {
			v.Set("kind", "weekly")
			v.Set("weekly_weekday", "5") // kind-unique name (NOT the monthly weekday)
			v.Set("weekly_anchor", "2025-01-03")
		}},
	}
	for _, c := range cases {
		v := url.Values{}
		v.Set("name", c.name)
		domDefaults(v)
		c.build(v)
		rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules", v)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s schedule = %d, want 303; body:\n%s", c.name, rec.Code, rec.Body.String())
		}
		id := scheduleIDByName(t, st, c.name)
		if id == 0 {
			t.Fatalf("%s schedule not persisted", c.name)
		}
		testutil.AssertVersioned(t, db, "budget_schedules", id, "create")
	}
}

// TestScheduleCustomImportRoundTrips: a custom schedule imports a list of dates; the
// dates round-trip (the edit form re-renders them).
func TestScheduleCustomImportRoundTrips(t *testing.T) {
	h, st, _, sm, _, writer := budgetsFixtureApp(t)

	form := url.Values{}
	form.Set("name", "Grant tranches")
	form.Set("kind", "custom")
	// Dates entered in the user's format (ISO default) one per line.
	form.Set("custom_dates", "2025-03-01\n2025-06-01\n2025-09-01")
	if rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("custom schedule = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	id := scheduleIDByName(t, st, "Grant tranches")
	if id == 0 {
		t.Fatal("custom schedule not persisted")
	}
	dates, err := st.ScheduleDates(ctxBG(), id)
	if err != nil {
		t.Fatalf("ScheduleDates: %v", err)
	}
	if len(dates) != 3 || dates[0] != "2025-03-01" || dates[2] != "2025-09-01" {
		t.Fatalf("custom dates did not round-trip: %v", dates)
	}
	// The edit form re-renders the imported dates (round-trip through the UI).
	edit := asUser(t, h, sm, writer, http.MethodGet, "/schedules/"+strconv.FormatInt(id, 10)+"/edit", nil)
	if !strings.Contains(edit.Body.String(), "2025-06-01") {
		t.Errorf("edit form missing imported date; body:\n%s", edit.Body.String())
	}
}

// TestScheduleEdit: editing a schedule's fields persists and versions (op=update).
func TestScheduleEdit(t *testing.T) {
	h, st, db, sm, _, writer := budgetsFixtureApp(t)

	create := url.Values{}
	create.Set("name", "Rent")
	create.Set("kind", "monthly")
	create.Set("day_of_month", "1")
	asUser(t, h, sm, writer, http.MethodPost, "/schedules", create)
	id := scheduleIDByName(t, st, "Rent")

	edit := url.Values{}
	edit.Set("name", "Rent (last day)")
	edit.Set("kind", "monthly")
	edit.Set("day_of_month", "-1")
	edit.Set("weekend_adjust", "next_business_day")
	if rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules/"+strconv.FormatInt(id, 10), edit); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit schedule = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	sched, err := st.GetSchedule(ctxBG(), id)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if sched.Name != "Rent (last day)" {
		t.Errorf("schedule name not updated: %q", sched.Name)
	}
	testutil.AssertVersioned(t, db, "budget_schedules", id, "update")
}

// TestScheduleInvalidKind: a kind-inconsistent schedule (monthly with neither
// day-of-month nor ordinal) surfaces a 422 + i18n field error (ErrScheduleInvalid).
func TestScheduleInvalidKind(t *testing.T) {
	h, _, _, sm, _, writer := budgetsFixtureApp(t)

	form := url.Values{}
	form.Set("name", "Broken")
	form.Set("kind", "monthly")
	// Neither day_of_month nor ordinal -> ErrScheduleInvalid.
	rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid schedule = %d, want 422; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field-error") {
		t.Errorf("422 body missing a field error; body:\n%s", rec.Body.String())
	}
}

// --- BUDGETS -----------------------------------------------------------------

// TestBudgetCreateEdit: create then edit a budget; both persist and version.
func TestBudgetCreateEdit(t *testing.T) {
	h, st, db, sm, _, writer := budgetsFixtureApp(t)

	create := url.Values{}
	create.Set("name", "FY2025")
	create.Set("period_start", "2025-01-01")
	create.Set("period_end", "2025-12-31")
	if rec := asUser(t, h, sm, writer, http.MethodPost, "/budgets", create); rec.Code != http.StatusSeeOther {
		t.Fatalf("create budget = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	id := budgetIDByName(t, st, "FY2025")
	if id == 0 {
		t.Fatal("budget not persisted")
	}
	testutil.AssertVersioned(t, db, "budgets", id, "create")

	edit := url.Values{}
	edit.Set("name", "FY2025 (revised)")
	edit.Set("period_start", "2025-01-01")
	edit.Set("period_end", "2025-12-31")
	if rec := asUser(t, h, sm, writer, http.MethodPost, "/budgets/"+strconv.FormatInt(id, 10), edit); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit budget = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	b, err := st.GetBudget(ctxBG(), id)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if b.Name != "FY2025 (revised)" {
		t.Errorf("budget name not updated: %q", b.Name)
	}
	testutil.AssertVersioned(t, db, "budgets", id, "update")
}

// --- BUDGET LINES ------------------------------------------------------------

// seedBudgetAndSchedule creates a budget + a schedule for the line tests, returning
// their ids.
func seedBudgetAndSchedule(t *testing.T, h http.Handler, st *store.Store, sm *scs.SessionManager, writer int64) (int64, int64) {
	t.Helper()
	b := url.Values{}
	b.Set("name", "Line Budget")
	b.Set("period_start", "2025-01-01")
	b.Set("period_end", "2025-12-31")
	asUser(t, h, sm, writer, http.MethodPost, "/budgets", b)
	budgetID := budgetIDByName(t, st, "Line Budget")

	s := url.Values{}
	s.Set("name", "Monthly-1")
	s.Set("kind", "monthly")
	s.Set("day_of_month", "1")
	asUser(t, h, sm, writer, http.MethodPost, "/schedules", s)
	schedID := scheduleIDByName(t, st, "Monthly-1")
	return budgetID, schedID
}

// TestBudgetLineCreateEditDelete: add a line (sub/R-E account/fund/program/amount/
// schedule), edit it, delete it -- each persists and versions.
func TestBudgetLineCreateEditDelete(t *testing.T) {
	h, st, db, sm, ids, writer := budgetsFixtureApp(t)
	budgetID, schedID := seedBudgetAndSchedule(t, h, st, sm, writer)
	bp := "/budgets/" + strconv.FormatInt(budgetID, 10)

	line := url.Values{}
	line.Set("subsidiary_id", strconv.FormatInt(ids.US, 10))
	line.Set("account_id", strconv.FormatInt(ids.Contributions, 10)) // revenue leaf
	line.Set("fund_id", strconv.FormatInt(ids.BuildingFund, 10))     // scoped to US
	line.Set("program_id", strconv.FormatInt(ids.General, 10))
	line.Set("amount", "500.00")
	line.Set("currency", "USD")
	line.Set("schedule_id", strconv.FormatInt(schedID, 10))
	if rec := asUser(t, h, sm, writer, http.MethodPost, bp+"/lines", line); rec.Code != http.StatusSeeOther {
		t.Fatalf("create line = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	lines, err := st.BudgetLines(ctxBG(), budgetID)
	if err != nil {
		t.Fatalf("BudgetLines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	lineID := lines[0].ID
	if lines[0].Amount != 50000 {
		t.Errorf("amount = %d, want 50000 minor", lines[0].Amount)
	}
	testutil.AssertVersioned(t, db, "budget_lines", lineID, "create")

	// The budget detail lists the line.
	detail := asUser(t, h, sm, writer, http.MethodGet, bp, nil)
	if !strings.Contains(detail.Body.String(), "Contributions") {
		t.Errorf("budget detail missing the line's account; body:\n%s", detail.Body.String())
	}

	// Edit the amount.
	edit := url.Values{}
	edit.Set("subsidiary_id", strconv.FormatInt(ids.US, 10))
	edit.Set("account_id", strconv.FormatInt(ids.Contributions, 10))
	edit.Set("fund_id", strconv.FormatInt(ids.BuildingFund, 10))
	edit.Set("program_id", strconv.FormatInt(ids.General, 10))
	edit.Set("amount", "750.00")
	edit.Set("currency", "USD")
	edit.Set("schedule_id", strconv.FormatInt(schedID, 10))
	if rec := asUser(t, h, sm, writer, http.MethodPost, bp+"/lines/"+strconv.FormatInt(lineID, 10), edit); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit line = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	lines, _ = st.BudgetLines(ctxBG(), budgetID)
	if lines[0].Amount != 75000 {
		t.Errorf("edited amount = %d, want 75000", lines[0].Amount)
	}
	testutil.AssertVersioned(t, db, "budget_lines", lineID, "update")

	// Delete the line.
	if rec := asUser(t, h, sm, writer, http.MethodPost, bp+"/lines/"+strconv.FormatInt(lineID, 10)+"/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete line = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	lines, _ = st.BudgetLines(ctxBG(), budgetID)
	if len(lines) != 0 {
		t.Errorf("line not deleted: %d remain", len(lines))
	}
	testutil.AssertVersioned(t, db, "budget_lines", lineID, "delete")
}

// TestBudgetLineBalanceSheetAccountRejected: a balance-sheet account on a line maps
// to the ErrBudgetLineAccountNotRE field error (422 + i18n on the account field).
func TestBudgetLineBalanceSheetAccountRejected(t *testing.T) {
	h, st, _, sm, ids, writer := budgetsFixtureApp(t)
	budgetID, schedID := seedBudgetAndSchedule(t, h, st, sm, writer)
	bp := "/budgets/" + strconv.FormatInt(budgetID, 10)

	line := url.Values{}
	line.Set("subsidiary_id", strconv.FormatInt(ids.US, 10))
	line.Set("account_id", strconv.FormatInt(ids.CheckingUS, 10)) // ASSET -> not R/E
	line.Set("program_id", strconv.FormatInt(ids.General, 10))
	line.Set("amount", "100.00")
	line.Set("currency", "USD")
	line.Set("schedule_id", strconv.FormatInt(schedID, 10))
	rec := asUser(t, h, sm, writer, http.MethodPost, bp+"/lines", line)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("balance-sheet account line = %d, want 422; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field-error") {
		t.Errorf("422 body missing a field error; body:\n%s", rec.Body.String())
	}
}

// TestBudgetLineBadAmount: a non-positive amount surfaces the ErrBudgetAmount field
// error (422).
func TestBudgetLineBadAmount(t *testing.T) {
	h, st, _, sm, ids, writer := budgetsFixtureApp(t)
	budgetID, schedID := seedBudgetAndSchedule(t, h, st, sm, writer)
	bp := "/budgets/" + strconv.FormatInt(budgetID, 10)

	line := url.Values{}
	line.Set("subsidiary_id", strconv.FormatInt(ids.US, 10))
	line.Set("account_id", strconv.FormatInt(ids.Contributions, 10))
	line.Set("program_id", strconv.FormatInt(ids.General, 10))
	line.Set("amount", "0.00") // not positive
	line.Set("currency", "USD")
	line.Set("schedule_id", strconv.FormatInt(schedID, 10))
	rec := asUser(t, h, sm, writer, http.MethodPost, bp+"/lines", line)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad-amount line = %d, want 422; body:\n%s", rec.Code, rec.Body.String())
	}
}

// --- PERMS (beyond the matrix) -----------------------------------------------

// TestBudgetManagePermTxnWrite: a TxnRead user cannot create a schedule/budget/line
// (403); a TxnWrite user can. This asserts the manage-perm choice (TxnWrite) directly.
func TestBudgetManagePermTxnWrite(t *testing.T) {
	h, st, sm := budgetsSimpleApp(t)
	reader := mkUser(t, st, "reader", "read", false)
	writer := mkUser(t, st, "writer", "write", false)

	form := url.Values{}
	form.Set("name", "Perm sched")
	form.Set("kind", "monthly")
	form.Set("day_of_month", "1")

	// TxnRead: forbidden on the manage POST.
	if rec := asUser(t, h, sm, reader, http.MethodPost, "/schedules", form); rec.Code != http.StatusForbidden {
		t.Errorf("TxnRead POST schedule = %d, want 403", rec.Code)
	}
	// TxnRead: forbidden on the manage GET form too.
	if rec := asUser(t, h, sm, reader, http.MethodGet, "/schedules/new", nil); rec.Code != http.StatusForbidden {
		t.Errorf("TxnRead GET schedule form = %d, want 403", rec.Code)
	}
	// TxnRead CAN view the (read) list.
	if rec := asUser(t, h, sm, reader, http.MethodGet, "/schedules", nil); rec.Code != http.StatusOK {
		t.Errorf("TxnRead GET schedule list = %d, want 200", rec.Code)
	}
	// TxnWrite: allowed (303 redirect after the write).
	if rec := asUser(t, h, sm, writer, http.MethodPost, "/schedules", form); rec.Code != http.StatusSeeOther {
		t.Errorf("TxnWrite POST schedule = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
}

// --- helpers -----------------------------------------------------------------

func scheduleIDByName(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	rows, err := st.ListSchedules(ctxBG())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return 0
}

func budgetIDByName(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	rows, err := st.ListBudgets(ctxBG())
	if err != nil {
		t.Fatalf("ListBudgets: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return 0
}
