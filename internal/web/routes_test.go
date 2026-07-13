package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/bankimport"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// The route registry is the whole security surface (rule 8): every route is
// declared once in routes.go with an explicit Perm, Mount is the only mounting
// site, and the permission matrix is generated FROM the registry so a route added
// there is covered automatically. These tests prove all three: the registry is
// the sole source of routes (TestRouteRegistryComplete), the enforcement policy is
// correct for every Perm x persona (TestDecidePolicy, on the pure decision fn),
// and real requests through the mounted handler match that policy for every route
// x persona (TestPermissionMatrix). The matrix iterates routes() -- never a
// hardcoded list -- so p11+'s routes are enforced the moment they are declared.

// newMatrixApp builds the real app over a migrated temp db, runs the startup
// report-group sync (so the placeholder group exists for grants), and returns the
// handler, the LIVE route registry (srv.routes()), the store+db for persona
// setup, and the scs session manager for session injection. Both registry tests
// iterate that exact registry -- never a hardcoded list -- so a route added to
// routes.go is picked up automatically (rule 8).
func newMatrixApp(t *testing.T) (http.Handler, []Route, *store.Store, *sql.DB, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	// Seed one account so that {id}-parameterized routes (p11.1's
	// /accounts/{id}/edit, /accounts/{id}/deactivate; p12's register/history)
	// resolve to a real resource when the reachability check substitutes {id} -> 1.
	// Its id is not asserted; the routes only need SOME account to exist so an
	// authorized persona reaches the handler rather than a legitimate 404.
	seedCtx := store.WithActor(context.Background(), store.Actor{ID: 1})
	seedAcct := func(name string, reconcilable bool) int64 {
		id, err := st.CreateAccount(seedCtx, store.CreateAccountInput{
			Type: "asset", DefaultCurrency: "USD",
			Names: map[string]string{"en": name}, Subsidiaries: []int64{1},
			Reconcilable: reconcilable,
		})
		if err != nil {
			t.Fatalf("seed account %s: %v", name, err)
		}
		return id
	}
	// a1 is RECONCILABLE so p16.3's /reconciliations/{id}... routes resolve to a real
	// open reconciliation when the reachability check substitutes {id} -> 1.
	a1 := seedAcct("Seed", true)
	a2 := seedAcct("Seed 2", false)

	// Seed one transaction so p12.2's /transactions/{id}/edit resolves to a real
	// resource when the reachability check substitutes {id} -> 1 (a balanced 2-split
	// transfer between the two seed accounts). Its id is not asserted; the route only
	// needs SOME transaction to exist so an authorized persona reaches the handler.
	if _, err := st.PostTransaction(seedCtx, store.PostTransactionInput{
		Date: "2025-01-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: a1, Amount: 1000, Position: 0},
			{AccountID: a2, Amount: -1000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	// Seed one fund so p12.5's /funds/{id} and /funds/{id}/edit resolve to a real
	// resource when the reachability check substitutes {id} -> 1. Its id is not
	// asserted; the routes only need SOME fund to exist so an authorized persona
	// reaches the handler rather than a legitimate 404.
	if _, err := st.CreateFund(seedCtx, store.CreateFundInput{
		Name: "Seed Fund", Restriction: "purpose", Subsidiaries: []int64{1},
	}); err != nil {
		t.Fatalf("seed fund: %v", err)
	}

	// Seed one OPEN reconciliation (id 1) on the reconcilable seed account so p16.3's
	// /reconciliations/{id}, /reconciliations/{id}/splits/{sid}/toggle,
	// /reconciliations/{id}/finalize, /reconciliations/{id}/reopen resolve to a real
	// resource when the reachability check substitutes {id} -> 1 and {sid} -> 1 (split
	// id 1 is the a1 leg of the seed transaction, so the toggle finds it). The statement
	// balance is nonzero so finalize returns a clean 422 guard (not a 404), and reopen
	// on an open recon returns 409 (not a 404) -- both count as "reachable".
	if _, err := st.StartReconciliation(seedCtx, a1, "USD", "2025-01-31", 1000); err != nil {
		t.Fatalf("seed reconciliation: %v", err)
	}

	// Seed one import batch + a pending row (row id 1) on the reconcilable seed account
	// so p17.3's /import/batches/{id}, /import/rows/{id}/edit, /import/rows/{id}/post,
	// /import/rows/{id}/discard resolve to a real resource when the reachability check
	// substitutes {id} -> 1. The batch's account maps to sub 1. The routes only need
	// SOME batch/row to exist so an authorized persona reaches the handler (post/discard
	// then 422 on the empty/invalid body -- non-404, so "reachable").
	if pid, err := st.CreateMappingProfile(seedCtx, "seed", bankimport.Config{}); err != nil {
		t.Fatalf("seed mapping profile: %v", err)
	} else if bid, err := st.CreateImportBatch(seedCtx, "seed.csv", a1, 1, pid, "2025-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed import batch: %v", err)
	} else if _, err := st.StageImportRows(seedCtx, bid, a1, []bankimport.ParsedRow{
		{Date: "2025-01-01", AmountMinor: 100, Payee: "Seed", Memo: "Seed", Raw: []string{"2025-01-01", "1.00", "Seed", "Seed"}},
	}); err != nil {
		t.Fatalf("seed import row: %v", err)
	}

	// Seed one schedule (id 1), one budget (id 1), and one budget line (id 1) so
	// p19.3's /schedules/{id}/edit, /budgets/{id}, /budgets/{id}/edit, and
	// /budgets/{id}/lines/{lid}/edit resolve to a real resource when the reachability
	// check substitutes {id}/{lid} -> 1. The line needs an R/E account (a budget is of
	// R/E flows), so a revenue leaf is seeded for it; the route only needs the row to
	// exist so an authorized persona reaches the handler (non-404).
	dom1 := 1
	if _, err := st.CreateSchedule(seedCtx, store.ScheduleInput{Name: "Seed sched", Kind: "monthly", DayOfMonth: &dom1}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	seedBudget, err := st.CreateBudget(seedCtx, store.BudgetInput{Name: "Seed budget", PeriodStart: "2025-01-01", PeriodEnd: "2025-12-31"})
	if err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	rev, err := st.CreateAccount(seedCtx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Seed Revenue"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("seed revenue account: %v", err)
	}
	if _, err := st.CreateBudgetLine(seedCtx, seedBudget, store.BudgetLineInput{
		SubsidiaryID: 1, AccountID: rev, ProgramID: 1, Amount: 10000, Currency: "USD", ScheduleID: 1,
	}); err != nil {
		t.Fatalf("seed budget line: %v", err)
	}

	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, app.srv.routes(), st, db, app.sessions
}

// persona is one of the six identities the matrix drives requests as. anon has a
// nil user (no cookie); the rest are real users with distinct permission shapes.
// grants mirrors the user's report-group grants so the expected outcome (decide)
// and the enforced outcome (which reads the DB via ReportGrants) share one truth.
type persona struct {
	name   string
	user   *store.CurrentUser
	grants []string
}

// buildPersonas creates the six matrix personas over st. They are driven by
// SESSION INJECTION (mintCookie), not by logging in through the handler -- so the
// matrix pays no argon2 cost and never trips the login rate limiter, and logout
// (which destroys a session) cannot contaminate later requests because every
// request mints its OWN token (the task sanctions "inject the session/user"). Note
// there is no password: injection never verifies one. ReportsOnly is granted the
// placeholder report group via direct SQL (grant WRITERS land in p13.2; raw SQL in
// tests is in-convention, p05.3); the group must exist first (FK), which the
// startup sync guarantees. That real grant is what the ReportGroup enforcement
// reads; p.grants feeds the expected decision.
func buildPersonas(t *testing.T, st *store.Store, db *sql.DB) []persona {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	mk := func(username string, in store.CreateUserInput) *store.CurrentUser {
		in.Username = username
		in.DisplayName = username
		id, err := st.CreateUser(ctx, in)
		if err != nil {
			t.Fatalf("create user %s: %v", username, err)
		}
		cu, err := st.UserByID(ctx, id)
		if err != nil {
			t.Fatalf("read user %s: %v", username, err)
		}
		return &cu
	}

	noAccess := mk("noaccess", store.CreateUserInput{TxnPerm: "none"})
	readOnly := mk("readonly", store.CreateUserInput{TxnPerm: "read"})
	bookkeeper := mk("bookkeeper", store.CreateUserInput{TxnPerm: "write"})
	reportsOnly := mk("reportsonly", store.CreateUserInput{TxnPerm: "none"})
	admin := mk("admin", store.CreateUserInput{IsAdmin: true})

	// Grant the ReportsOnly persona the "financial" report group -- the group the
	// p15.3 trial-balance report (the first real mounted report route) is gated by.
	// This is what makes the permission matrix prove per-group report enforcement with
	// ZERO extra test code: ReportsOnly reaches GET /reports/trial_balance (200),
	// NoAccess is forbidden (403), all via the existing matrix mechanism. p13.2's grant
	// WRITERS land later; raw SQL here is in-convention (p05.3). The group must exist
	// first (FK), which the startup SyncReportGroups (newMatrixApp) guarantees.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (?, ?)`,
		reportsOnly.ID, grantedReportGroup); err != nil {
		t.Fatalf("grant report group to reportsonly: %v", err)
	}

	return []persona{
		{name: "anon"},
		{name: "NoAccess", user: noAccess},
		{name: "ReadOnly", user: readOnly},
		{name: "Bookkeeper", user: bookkeeper},
		{name: "ReportsOnly", user: reportsOnly, grants: []string{grantedReportGroup}},
		{name: "Admin", user: admin},
	}
}

// grantedReportGroup is the report group the ReportsOnly matrix persona holds: the
// group the p15.3 trial-balance report is mounted under, so the matrix covers
// "granted -> 200, ungranted -> 403" on a real report route automatically. It stays a
// valid grant for any p15.4+ report in the same group.
const grantedReportGroup = "financial"

// mintCookie fabricates a fresh authenticated session for userID by writing a new
// scs session row (user_id bound under the SAME key authMiddleware reads) and
// returning its cookie. A fresh session per call means logout's Destroy can never
// poison a later request in the sweep, and no login/argon2/rate-limit is involved.
func mintCookie(t *testing.T, sm *scs.SessionManager, userID int64) *http.Cookie {
	t.Helper()
	ctx, err := sm.Load(context.Background(), "") // empty token => brand-new session
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	sm.Put(ctx, sessionUserKey, userID)
	token, _, err := sm.Commit(ctx) // persists the row, returns its token
	if err != nil {
		t.Fatalf("session commit: %v", err)
	}
	return &http.Cookie{Name: "cuento_session", Value: token, Path: "/"}
}

// concreteURL substitutes a placeholder for every {wildcard} segment so a route
// pattern becomes a hittable path. There are no wildcard routes today, but the
// matrix must auto-extend to p12's /accounts/{id}/register etc. without edits.
func concreteURL(pattern string) string {
	// {$} is the exact-match anchor (GET /{$} matches only "/"): its concrete path
	// drops the anchor. Handle it before the generic wildcard substitution, which
	// would otherwise mistake {$} for a path variable and yield "/1".
	pattern = strings.ReplaceAll(pattern, "{$}", "")
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "1"
		}
	}
	return strings.Join(segs, "/")
}

// doAs issues an httptest request for route as persona p and returns the
// recorder. GET/HEAD are safe; other methods carry no cross-origin headers so the
// stdlib CrossOriginProtection lets them pass (default httptest RemoteAddr is
// same-origin-clean) and the matrix observes the PERMISSION status, not a CSRF
// 403. It never follows redirects, so a 302/303 Location is observable.
func doAs(t *testing.T, h http.Handler, sm *scs.SessionManager, p persona, r Route) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(r.Method, concreteURL(r.Pattern), nil)
	if p.user != nil {
		req.AddCookie(mintCookie(t, sm, p.user.ID))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestDecidePolicy proves the pure enforcement decision (decide) implements the
// p06.3 policy for EVERY Perm x persona -- including ReportGroup, whose HTTP
// coverage waits for p15's report routes but whose LOGIC must be provable now.
// The HTTP matrix asserts real requests match decide; here we assert decide
// itself is correct, so the two share one source of truth with no duplicated
// expectation math.
func TestDecidePolicy(t *testing.T) {
	// Personas as (user, grants) pairs; anon is a nil user.
	anon := (*store.CurrentUser)(nil)
	noAccess := &store.CurrentUser{TxnPerm: "none"}
	readOnly := &store.CurrentUser{TxnPerm: "read"}
	bookkeeper := &store.CurrentUser{TxnPerm: "write"}
	reportsOnly := &store.CurrentUser{TxnPerm: "none"}
	admin := &store.CurrentUser{IsAdmin: true}

	grantsOf := map[*store.CurrentUser][]string{reportsOnly: {"placeholder"}}
	hasGrant := func(u *store.CurrentUser) func(string) bool {
		return func(name string) bool {
			for _, g := range grantsOf[u] {
				if g == name {
					return true
				}
			}
			return false
		}
	}

	const grp = "placeholder"
	cases := []struct {
		perm Perm
		user *store.CurrentUser
		want outcome
	}{
		// Public: everyone, including anon.
		{Public, anon, outcomeAllow},
		{Public, noAccess, outcomeAllow},
		{Public, admin, outcomeAllow},

		// AnyUser: any logged-in user; anon redirects to login.
		{AnyUser, anon, outcomeRedirectLogin},
		{AnyUser, noAccess, outcomeAllow},
		{AnyUser, admin, outcomeAllow},

		// TxnRead: read or write, or admin; none -> forbid; anon -> login.
		{TxnRead, anon, outcomeRedirectLogin},
		{TxnRead, noAccess, outcomeForbid},
		{TxnRead, readOnly, outcomeAllow},
		{TxnRead, bookkeeper, outcomeAllow},
		{TxnRead, admin, outcomeAllow},

		// TxnWrite: write or admin; read/none -> forbid; anon -> login.
		{TxnWrite, anon, outcomeRedirectLogin},
		{TxnWrite, noAccess, outcomeForbid},
		{TxnWrite, readOnly, outcomeForbid},
		{TxnWrite, bookkeeper, outcomeAllow},
		{TxnWrite, admin, outcomeAllow},

		// ReportGroup: a grant for that group, or admin; else forbid; anon -> login.
		{ReportGroup(grp), anon, outcomeRedirectLogin},
		{ReportGroup(grp), noAccess, outcomeForbid},
		{ReportGroup(grp), reportsOnly, outcomeAllow},
		{ReportGroup("other"), reportsOnly, outcomeForbid}, // granted a DIFFERENT group
		{ReportGroup(grp), admin, outcomeAllow},            // is_admin implies everything

		// Admin: is_admin only; else forbid; anon -> login.
		{Admin, anon, outcomeRedirectLogin},
		{Admin, noAccess, outcomeForbid},
		{Admin, bookkeeper, outcomeForbid},
		{Admin, admin, outcomeAllow},
	}

	for _, c := range cases {
		got := decide(c.perm, c.user, hasGrant(c.user))
		if got != c.want {
			t.Errorf("decide(%v, %s) = %v, want %v", c.perm, personaLabel(c.user), got, c.want)
		}
	}
}

func personaLabel(u *store.CurrentUser) string {
	switch {
	case u == nil:
		return "anon"
	case u.IsAdmin:
		return "admin"
	default:
		return "txn=" + u.TxnPerm
	}
}

// TestRouteRegistryComplete proves mounting happens ONLY through the registry
// (rule 8). Structurally, Mount iterates routes() and nothing else, so no route
// can exist outside the registry. Behaviorally: every registered pattern is
// reachable for an authorized persona (never a 404), while an unregistered path
// 404s -- so a route added to the mux-but-not-registry (impossible by
// construction) or a registry entry whose pattern the mux never serves both fail.
func TestRouteRegistryComplete(t *testing.T) {
	h, registry, st, db, sm := newMatrixApp(t)
	personas := buildPersonas(t, st, db)

	// Admin can reach every route regardless of Perm (is_admin implies all), so it
	// is the authorized persona for the reachability check.
	admin := personas[len(personas)-1]
	if admin.name != "Admin" {
		t.Fatalf("expected last persona to be Admin, got %q", admin.name)
	}

	for _, r := range registry {
		rec := doAs(t, h, sm, admin, r)
		if rec.Code == http.StatusNotFound {
			t.Errorf("registered route %s %s 404s for Admin -- not actually mounted",
				r.Method, r.Pattern)
		}
	}

	// An unregistered path must 404: proves GET /{$} is not a catch-all and no
	// stray subtree handler exists.
	req := httptest.NewRequest(http.MethodGet, "/definitely-not-registered", nil)
	req.AddCookie(mintCookie(t, sm, admin.user.ID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unregistered path status = %d, want 404 (a catch-all route leaked)", rec.Code)
	}
}

// TestPermissionMatrix is generated from the registry: for every Route x persona
// it asserts the real handler's response matches the outcome decide computes from
// the Route's Perm and the persona. Because it iterates routes() (not a hardcoded
// list) and reuses decide (not a re-expressed expectation), a new route declared
// in routes.go is enforced-and-checked automatically, and the policy lives in one
// place.
func TestPermissionMatrix(t *testing.T) {
	h, registry, st, db, sm := newMatrixApp(t)
	personas := buildPersonas(t, st, db)

	for _, r := range registry {
		for _, p := range personas {
			hasGrant := func(name string) bool {
				for _, g := range p.grants {
					if g == name {
						return true
					}
				}
				return false
			}
			want := decide(r.Perm, p.user, hasGrant)
			rec := doAs(t, h, sm, p, r)

			if !outcomeMatches(want, rec) {
				loc := rec.Header().Get("Location")
				t.Errorf("%s %s as %s: status=%d location=%q, want %s",
					r.Method, r.Pattern, p.name, rec.Code, loc, want)
			}
		}
	}
}

// outcomeMatches reports whether the recorded response satisfies the expected
// outcome. The 302/303 split is load-bearing: enforcement's anon->login redirect
// uses 302, while every real handler redirect (logout, already-authed /login)
// uses 303 -- so "authorized user wrongly bounced to /login" (a 302 to /login) is
// caught by outcomeAllow while a legitimate 303->/login (logout) passes.
func outcomeMatches(want outcome, rec *httptest.ResponseRecorder) bool {
	redirectToLogin := rec.Code == http.StatusFound && rec.Header().Get("Location") == "/login"
	switch want {
	case outcomeForbid:
		return rec.Code == http.StatusForbidden
	case outcomeRedirectLogin:
		return redirectToLogin
	case outcomeAllow:
		return rec.Code != http.StatusForbidden && !redirectToLogin
	default:
		return false
	}
}
