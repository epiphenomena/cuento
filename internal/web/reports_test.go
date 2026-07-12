package web

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// p15.1 report-framework web tests. They drive the REAL mounted router (httptest)
// over a migrated temp db with the startup report-group sync run, so the auto-
// mounted /reports/{id} routes, the shared params form, and the CSV endpoint are
// exercised end to end (no handler-level store mocks -- AGENTS testing conventions).
//
// The PERMISSION-matrix requirement ("new reports appear in the matrix
// automatically") is covered with ZERO edits by routes_test.go: because report
// routes are appended to the SAME registry TestPermissionMatrix iterates, and the
// ReportsOnly persona is granted the smoke report's group, the matrix already
// asserts granted->200 / ungranted->403 on GET /reports/_smoke. These tests cover
// the p15.1-specific behaviors: unknown id -> 404, the scope selector on EVERY
// report, and the smoke report rendering typed cells + CSV.

// reportsApp builds a real app with the report groups synced (so a ReportGroup grant
// has a valid FK) and returns the handler + store + sessions. It seeds one account
// with a posted balance so the smoke report reads REAL data through the toolkit.
func reportsApp(t *testing.T) (http.Handler, *store.Store, *sql.DB, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}

	// Seed a couple of accounts + a balanced posted transaction so the smoke report
	// (SubtreeBalancesAsOf at root scope) returns non-empty typed cells.
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	mkAcct := func(name string) int64 {
		id, err := st.CreateAccount(ctx, store.CreateAccountInput{
			Type: "asset", DefaultCurrency: "USD",
			Names: map[string]string{"en": name}, Subsidiaries: []int64{1},
		})
		if err != nil {
			t.Fatalf("seed account %s: %v", name, err)
		}
		return id
	}
	a1, a2 := mkAcct("Cash"), mkAcct("Bank")
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: a1, Amount: 25000, Position: 0},
			{AccountID: a2, Amount: -25000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, db, app.sessions
}

// grantGroup gives userID a read grant on group via direct SQL (grant writers are
// p13.2; raw SQL in tests is in-convention). The group must already be synced (FK).
func grantGroup(t *testing.T, db *sql.DB, userID int64, group string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (?, ?)`,
		userID, group); err != nil {
		t.Fatalf("grant %q to user %d: %v", group, userID, err)
	}
}

// TestReportGroupsSynced is the "registry sync creates groups" listed test at the
// DB layer: after the startup SyncReportGroups (run by reportsApp), report_groups
// holds EXACTLY the code-declared set reports.Groups() -- not just the one group the
// smoke report references. So a group declared before any report uses it (funds /
// programs / tax) still lands in the table (its grant FK is valid the moment p15.3+
// or an admin grants it).
func TestReportGroupsSynced(t *testing.T) {
	_, st, _, _ := reportsApp(t)

	got, err := st.ReportGroupNames(context.Background())
	if err != nil {
		t.Fatalf("ReportGroupNames: %v", err)
	}
	want := reports.Groups()
	if len(got) != len(want) {
		t.Fatalf("synced groups = %v, want %v", got, want)
	}
	// SyncReportGroups syncs in declared order and ListReportGroups returns sort
	// order, so the sets match positionally.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("synced group[%d] = %q, want %q (full: got %v want %v)", i, got[i], want[i], got, want)
		}
	}
}

// TestReportUnknownID404: a /reports/{id} for an id that is not registered never
// matches a mounted route, so the mux 404s. (Admin persona: is_admin implies all,
// so a 404 here is the mux's, not a permission bounce.)
func TestReportUnknownID404(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown report id status = %d, want 404", rec.Code)
	}
	// The CSV variant of an unknown id likewise 404s.
	rec = asUser(t, h, sm, admin, http.MethodGet, "/reports/does-not-exist.csv", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown report CSV id status = %d, want 404", rec.Code)
	}
}

// TestScopeSelectorOnEveryReport: the params form on EVERY registered report page
// includes the subsidiary SCOPE selector (D18 -- every report is scoped). Iterating
// reports.Default().All() means a report added in p15.3+ is covered automatically.
func TestScopeSelectorOnEveryReport(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true) // is_admin reaches every report

	all := reports.Default().All()
	if len(all) == 0 {
		t.Fatal("no reports registered; expected at least the smoke report")
	}
	for _, rep := range all {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+rep.ID, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /reports/%s status = %d, want 200", rep.ID, rec.Code)
			continue
		}
		body := rec.Body.String()
		// The scope selector: a <select name="scope"> with the scope option marker
		// class. Assert the name attr (language-independent) is present.
		if !strings.Contains(body, `name="scope"`) {
			t.Errorf("report %s page missing subsidiary scope selector (name=\"scope\")", rep.ID)
		}
		if !strings.Contains(body, `class="report-scope-select"`) {
			t.Errorf("report %s page missing the scope select element", rep.ID)
		}
		// And a real subsidiary option (the seeded root, id 1) is present.
		if !strings.Contains(body, `<option value="1"`) {
			t.Errorf("report %s scope selector has no subsidiary option", rep.ID)
		}
	}
}

// TestSmokeReportRenders: the smoke report renders its typed cells (a money cell
// formatted with a currency prefix, an indented data row, and a subtotal row) into
// the HTML page. Proves the framework is end-to-end: route -> params -> toolkit ->
// store -> Table -> renderer.
func TestSmokeReportRenders(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.SmokeReportID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke report status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "report-table") {
		t.Errorf("smoke report page missing the report table")
	}
	// A money cell with the currency prefix (the seeded 250.00 balance).
	if !strings.Contains(body, "USD 250.00") {
		t.Errorf("smoke report missing formatted money cell USD 250.00; body:\n%s", body)
	}
	// The subtotal row emphasis class (a subtotal row was emitted).
	if !strings.Contains(body, "report-subtotal") {
		t.Errorf("smoke report missing the subtotal row")
	}
	// An indented data row (indent level 1).
	if !strings.Contains(body, "report-indent-1") {
		t.Errorf("smoke report missing the indented data row")
	}
}

// TestSmokeReportCSV: the CSV endpoint returns text/csv, an attachment filename, and
// a parseable body whose header + rows reflect the report (proving the CSV renderer
// is wired through the route with the same params).
func TestSmokeReportCSV(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.SmokeReportID+".csv", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke report CSV status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("CSV Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "_smoke.csv") {
		t.Errorf("CSV Content-Disposition = %q, want attachment filename _smoke.csv", cd)
	}
	body := rec.Body.String()
	// Machine-plain money (no grouping separators): 250.00 for the seeded balance.
	if !strings.Contains(body, "250.00") {
		t.Errorf("CSV body missing machine-plain amount 250.00; body:\n%s", body)
	}
}

// TestReportPermissionThroughGrant: a report route enforces its group grant like any
// registry route -- a user WITH the group grant gets 200, a user WITHOUT gets 403.
// This is what "appears in the matrix automatically" gives at the HTTP level for the
// concrete smoke report (routes_test.go's matrix asserts it across all personas; this
// pins it explicitly for the framework's example report).
func TestReportPermissionThroughGrant(t *testing.T) {
	h, st, db, sm := reportsApp(t)

	granted := mkUser(t, st, "granted", "none", false)
	grantGroup(t, db, granted, "financial") // the smoke report's group
	ungranted := mkUser(t, st, "ungranted", "none", false)

	rec := asUser(t, h, sm, granted, http.MethodGet, "/reports/"+reports.SmokeReportID, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("granted user status = %d, want 200", rec.Code)
	}
	rec = asUser(t, h, sm, ungranted, http.MethodGet, "/reports/"+reports.SmokeReportID, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ungranted user status = %d, want 403", rec.Code)
	}
}
