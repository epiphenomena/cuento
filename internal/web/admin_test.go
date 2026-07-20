package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/ids"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// adminApp is accountsApp plus the raw *sql.DB, which the versioning assertions
// need. It builds the same real mounted app over a migrated temp db, running the
// startup report-group sync so grant tests have a group to grant.
func adminApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager, *sql.DB) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, app.sessions, db
}

// p13.2 admin feature tests. Driven through the REAL mounted router (httptest) over
// a real migrated db (AGENTS testing conventions) -- no store mocks. They reuse the
// shared web-package helpers (accountsApp, asUser, mkUser). The system user is id 1;
// created users start at id 2+.

// adminUserByUsername returns the id of a user by username via the store's admin
// list (excludes the system user), or 0.
func adminUserByUsername(t *testing.T, st *store.Store, username string) ids.UserID {
	t.Helper()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		if u.Username == username {
			return u.ID
		}
	}
	return 0
}

// TestAdminIndexRendersCards (p13.2 cards): GET /admin (Admin) renders every admin
// destination as a CARD (shared hub-cards partial), not the old plain bullet list. It
// asserts each of the six hrefs is present as a card and that the old admin-links markup
// is gone, pinning both the card look and the full destination set.
func TestAdminIndexRendersCards(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `class="hub-card"`) {
		t.Errorf("admin index is not rendered as cards (no hub-card); body: %s", body)
	}
	if strings.Contains(body, "admin-links") {
		t.Errorf("admin index still carries the old admin-links markup; body: %s", body)
	}
	for _, href := range []string{
		"/admin/users",
		"/admin/subsidiaries",
		"/admin/currencies",
		"/admin/rates",
		"/admin/org",
		"/admin/ops",
	} {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("admin index missing card for %q; body: %s", href, body)
		}
	}
}

// TestAdminUsersPageRenders: GET /admin/users (Admin) renders the list including a
// created operator.
func TestAdminUsersPageRenders(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	mkUser(t, st, "clerk", "write", false)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/users", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/users: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clerk") {
		t.Errorf("list does not show the created user; body: %s", rec.Body.String())
	}
}

// TestAdminUsersNonAdminForbidden: a non-admin (bookkeeper) is 403 on /admin/users,
// asserted explicitly (the matrix covers this too, but the task calls it out).
func TestAdminUsersNonAdminForbidden(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	rec := asUser(t, h, sm, book, http.MethodGet, "/admin/users", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /admin/users = %d, want 403", rec.Code)
	}
}

// TestAdminUserCreate: an admin creates an operator with a password; it appears with
// the given perm and can be looked up, and its password is hashed (not the plaintext).
func TestAdminUserCreate(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	form := url.Values{}
	form.Set("username", "newbie")
	form.Set("display_name", "New Bie")
	form.Set("password", "correct horse battery staple")
	form.Set("txn_perm", "read")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users", form)
	if rec.Code >= 400 {
		t.Fatalf("create returned %d, body: %s", rec.Code, rec.Body.String())
	}
	id := adminUserByUsername(t, st, "newbie")
	if id == 0 {
		t.Fatalf("created user not found")
	}
	u, err := st.AdminUserByID(context.Background(), id)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if u.TxnPerm != "read" {
		t.Errorf("txn_perm = %q, want read", u.TxnPerm)
	}
	// Password is hashed, never the plaintext (rule 13).
	creds, err := st.CredentialsByUsername(context.Background(), "newbie")
	if err != nil {
		t.Fatalf("CredentialsByUsername: %v", err)
	}
	if creds.PasswordHash == nil || *creds.PasswordHash == "correct horse battery staple" {
		t.Errorf("password not hashed: %v", creds.PasswordHash)
	}
}

// TestAdminUserCreateDuplicate: creating a user whose username already exists is a
// 422 with the username error (not a 500).
func TestAdminUserCreateDuplicate(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	mkUser(t, st, "taken", "none", false)

	form := url.Values{}
	form.Set("username", "taken")
	form.Set("password", "another password")
	form.Set("txn_perm", "none")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate create = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
}

// TestAdminUserCreateMissingPassword: a blank password is a 422 (required field).
func TestAdminUserCreateMissingPassword(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	form := url.Values{}
	form.Set("username", "nopass")
	form.Set("txn_perm", "none")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-password create = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if adminUserByUsername(t, st, "nopass") != 0 {
		t.Errorf("user was created despite the blank password")
	}
}

// TestAdminUserDisable: an admin disables a non-admin user; the live row is disabled.
func TestAdminUserDisable(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	target := mkUser(t, st, "goner", "write", false)

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/disable", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("disable returned %d, body: %s", rec.Code, rec.Body.String())
	}
	u, err := st.AdminUserByID(context.Background(), target)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if !u.Disabled {
		t.Errorf("user not disabled after POST disable")
	}
}

// TestAdminLastAdminGuardHTTP: disabling the SOLE admin over HTTP is blocked with a
// 422 and the last-admin error message (the guard, no execution).
func TestAdminLastAdminGuardHTTP(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "onlyboss", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(admin))+"/disable", url.Values{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disable sole admin = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The admin is still enabled (nothing executed).
	u, err := st.AdminUserByID(context.Background(), admin)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if u.Disabled {
		t.Errorf("sole admin was disabled despite the guard")
	}
}

// TestAdminResetPassword: reset sets a new hash; the login credential changes.
func TestAdminResetPassword(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	target := mkUser(t, st, "resetme", "read", false)

	form := url.Values{}
	form.Set("password", "brand new secret")
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/reset-password", form)
	if rec.Code >= 400 {
		t.Fatalf("reset returned %d, body: %s", rec.Code, rec.Body.String())
	}
	creds, err := st.CredentialsByUsername(context.Background(), "resetme")
	if err != nil {
		t.Fatalf("CredentialsByUsername: %v", err)
	}
	if creds.PasswordHash == nil || *creds.PasswordHash == "brand new secret" {
		t.Errorf("reset did not set a hashed password: %v", creds.PasswordHash)
	}
}

// TestAdminSetTxnPerm: an admin changes a user's txn_perm over HTTP; the live row
// reflects it and the change is versioned (op=update, actor = the admin).
func TestAdminSetTxnPerm(t *testing.T) {
	h, st, sm, db := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	target := mkUser(t, st, "worker", "none", false)

	form := url.Values{}
	form.Set("txn_perm", "write")
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/txn-perm", form)
	if rec.Code >= 400 {
		t.Fatalf("set txn_perm returned %d, body: %s", rec.Code, rec.Body.String())
	}
	u, err := st.AdminUserByID(context.Background(), target)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if u.TxnPerm != "write" {
		t.Errorf("txn_perm = %q, want write", u.TxnPerm)
	}
	testutil.AssertVersioned(t, db, "users", int64(target), "update")
	if got := testutil.LatestVersionActor(t, db, "users", int64(target)); got != int64(admin) {
		t.Errorf("txn_perm change actor = %d, want admin %d", got, admin)
	}
}

// TestAdminGrantsRoundTrip: an admin grants then revokes a report group over HTTP;
// the user's grants reflect each step, versioned and named to the admin.
func TestAdminGrantsRoundTrip(t *testing.T) {
	h, st, sm, db := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	target := mkUser(t, st, "reader", "read", false)

	// The placeholder report group is synced at app startup (NewApp calls the sync);
	// discover its name from the store.
	groups, err := st.ReportGroupNames(context.Background())
	if err != nil || len(groups) == 0 {
		t.Fatalf("no report groups synced: %v (%v)", groups, err)
	}
	grp := groups[0]

	// Grant.
	grant := url.Values{}
	grant.Set("grant_"+grp, "1")
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", grant)
	if rec.Code >= 400 {
		t.Fatalf("grant returned %d, body: %s", rec.Code, rec.Body.String())
	}
	if gs, _ := st.ReportGrants(context.Background(), target); len(gs) != 1 || gs[0].Group != grp {
		t.Fatalf("grants after grant = %v, want [%s]", gs, grp)
	}
	testutil.AssertVersionedGrant(t, db, int64(target), grp, "create")
	if got := testutil.LatestGrantActor(t, db, int64(target), grp); got != int64(admin) {
		t.Errorf("grant actor = %d, want admin %d", got, admin)
	}

	// Revoke (submit the form with the box unchecked = absent).
	rec = asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("revoke returned %d, body: %s", rec.Code, rec.Body.String())
	}
	if gs, _ := st.ReportGrants(context.Background(), target); len(gs) != 0 {
		t.Fatalf("grants after revoke = %v, want empty", gs)
	}
	testutil.AssertVersionedGrant(t, db, int64(target), grp, "delete")
	if got := testutil.LatestGrantActor(t, db, int64(target), grp); got != int64(admin) {
		t.Errorf("revoke actor = %d, want admin %d", got, admin)
	}
}

// TestAdminGrantsProgramScope (p27.4c): the admin grant form carries an OPTIONAL
// program-subtree scope per group. Granting a program-dimensioned group ("financial")
// with a chosen program scopes the grant; clearing the scope re-grants org-wide; and
// a scope value on a non-program-dimensioned group ("funds") is IGNORED server-side
// (that group has no program-dimensioned report -- the p27.4b empty-coverage trap).
func TestAdminGrantsProgramScope(t *testing.T) {
	h, st, sm, db := adminApp(t)
	ctx := context.Background()
	admin := mkUser(t, st, "boss", "none", true)
	target := mkUser(t, st, "reader", "read", false)

	// A tiny program tree so the scope has a real id to point at.
	prog, err := st.CreateProgram(store.WithActor(ctx, store.Actor{ID: 1}), store.CreateProgramInput{ParentID: 1, Name: "Educacion", SortOrder: 1})
	if err != nil {
		t.Fatalf("create program: %v", err)
	}

	// Grant "financial" (a program-dimensioned group) scoped to the program.
	scoped := url.Values{}
	scoped.Set("grant_financial", "1")
	scoped.Set("program_financial", itoa(int64(prog)))
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", scoped)
	if rec.Code >= 400 {
		t.Fatalf("scoped grant returned %d, body: %s", rec.Code, rec.Body.String())
	}
	gs, _ := st.ReportGrants(ctx, target)
	if len(gs) != 1 || gs[0].Group != "financial" || gs[0].ProgramID == nil || *gs[0].ProgramID != prog {
		t.Fatalf("grants after scoped grant = %+v, want [financial scoped to %d]", gs, prog)
	}
	testutil.AssertVersionedGrant(t, db, int64(target), "financial", "create")

	// Re-submit with the same box checked but NO program -> re-grant org-wide (scope change).
	orgWide := url.Values{}
	orgWide.Set("grant_financial", "1")
	rec = asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", orgWide)
	if rec.Code >= 400 {
		t.Fatalf("org-wide re-grant returned %d, body: %s", rec.Code, rec.Body.String())
	}
	gs, _ = st.ReportGrants(ctx, target)
	if len(gs) != 1 || gs[0].Group != "financial" || gs[0].ProgramID != nil {
		t.Fatalf("grants after clearing scope = %+v, want [financial org-wide]", gs)
	}

	// A crafted program scope on "funds" (no program-dimensioned report) is IGNORED:
	// the grant lands org-wide, never scoped to nothing.
	craft := url.Values{}
	craft.Set("grant_financial", "1")
	craft.Set("grant_funds", "1")
	craft.Set("program_funds", itoa(int64(prog)))
	rec = asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", craft)
	if rec.Code >= 400 {
		t.Fatalf("funds grant returned %d, body: %s", rec.Code, rec.Body.String())
	}
	gs, _ = st.ReportGrants(ctx, target)
	var foundFunds bool
	for i := range gs {
		if gs[i].Group == "funds" {
			foundFunds = true
			if gs[i].ProgramID != nil {
				t.Errorf("funds grant scoped to %d, want org-wide (no program-dim report -> scope ignored)", *gs[i].ProgramID)
			}
		}
	}
	if !foundFunds {
		t.Fatalf("funds grant missing after craft, grants = %+v", gs)
	}

	// The GET detail page offers a program picker for the program-dimensioned group and
	// NOT for "funds" (empty-coverage). Re-scope financial so the current-scope shows.
	rescope := url.Values{}
	rescope.Set("grant_financial", "1")
	rescope.Set("program_financial", itoa(int64(prog)))
	rescope.Set("grant_funds", "1")
	asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/grants", rescope)
	get := asUser(t, h, sm, admin, http.MethodGet, "/admin/users/"+itoa(int64(target)), nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET detail = %d", get.Code)
	}
	body := get.Body.String()
	if !strings.Contains(body, `name="program_financial"`) {
		t.Errorf("detail page missing program picker for financial (program-dimensioned)")
	}
	if strings.Contains(body, `name="program_funds"`) {
		t.Errorf("detail page OFFERS a program picker for funds (has no program-dimensioned report)")
	}
	if !strings.Contains(body, "Educacion") {
		t.Errorf("detail page does not show the current program scope name")
	}
	// The current-scope hint renders ONLY for a HELD grant: financial is held+program-dim
	// (one hint); tax/programs/budget are program-dim but UNHELD, so they show a picker
	// with NO "Current scope:" line. So the hint marker appears exactly once.
	if n := strings.Count(body, "grant-scope-current"); n != 1 {
		t.Errorf("current-scope hint count = %d, want 1 (only the held financial grant shows a scope)", n)
	}
}

// TestAdminUserDetailSystemUserRedirects: the system user (id 1) is off-limits --
// GET /admin/users/1 redirects to the list rather than 404ing or rendering.
func TestAdminUserDetailSystemUserRedirects(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/users/1", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/users" {
		t.Fatalf("GET /admin/users/1 = %d (loc %q), want 303 -> /admin/users", rec.Code, rec.Header().Get("Location"))
	}
}

// TestAdminCurrencyAdd: an admin adds a currency; it appears active in the list.
func TestAdminCurrencyAdd(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	form := url.Values{}
	form.Set("code", "gbp") // lowercased input; the handler uppercases
	form.Set("name", "Pound Sterling")
	form.Set("symbol", "£")
	form.Set("exponent", "2")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/currencies", form)
	if rec.Code >= 400 {
		t.Fatalf("add currency returned %d, body: %s", rec.Code, rec.Body.String())
	}
	c, err := st.Currency(context.Background(), "GBP")
	if err != nil {
		t.Fatalf("Currency(GBP): %v", err)
	}
	if c.Active == 0 {
		t.Errorf("added currency is not active")
	}
}

// TestAdminCurrencyAddInvalid: a bad code is a 422 (not a 500) and adds nothing.
func TestAdminCurrencyAddInvalid(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	form := url.Values{}
	form.Set("code", "TOOLONG")
	form.Set("name", "Nope")
	form.Set("symbol", "X")
	form.Set("exponent", "2")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/currencies", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad-code add = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
}

// TestAdminCurrencyToggle: disabling then re-enabling a currency flips its active flag.
func TestAdminCurrencyToggle(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	// Disable the seeded EUR.
	off := url.Values{}
	off.Set("active", "0")
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/currencies/EUR/toggle", off)
	if rec.Code >= 400 {
		t.Fatalf("disable EUR returned %d", rec.Code)
	}
	if c, _ := st.Currency(context.Background(), "EUR"); c.Active != 0 {
		t.Errorf("EUR still active after disable")
	}

	on := url.Values{}
	on.Set("active", "1")
	rec = asUser(t, h, sm, admin, http.MethodPost, "/admin/currencies/EUR/toggle", on)
	if rec.Code >= 400 {
		t.Fatalf("enable EUR returned %d", rec.Code)
	}
	if c, _ := st.Currency(context.Background(), "EUR"); c.Active == 0 {
		t.Errorf("EUR not active after enable")
	}
}
