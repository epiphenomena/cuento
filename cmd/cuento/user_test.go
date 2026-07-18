package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuento/internal/store"
	"cuento/internal/testutil"
	"cuento/internal/web"
)

// postLogin drives the real web login handler end to end, mirroring the p06.2
// login tests: a same-origin POST /login (no Sec-Fetch-Site header, so
// cross-origin protection passes) from a fixed remote IP.
func postLogin(t *testing.T, h http.Handler, ip, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := "username=" + username + "&password=" + password
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "cuento_session" {
			return c
		}
	}
	return nil
}

// TestUserAddAndLogin proves `cuento user add` creates a user with a hashed
// password that the REAL login handler then accepts (end to end, same db), and
// that the resulting users_versions row is op='create' and carries NO
// password_hash (rule 5).
func TestUserAddAndLogin(t *testing.T) {
	db := testutil.NewDB(t)
	st := store.New(db)

	// user add carol --admin --display "Carol Admin" with a known password.
	if err := userAdd(context.Background(), st, "carol", "Carol Admin", true, "correct-horse-battery"); err != nil {
		t.Fatalf("userAdd: %v", err)
	}

	// Live row: is_admin set, password_hash present (and NOT the plaintext).
	var (
		isAdmin int64
		livePH  sql.NullString
		uid     int64
	)
	if err := db.QueryRow(`SELECT id, is_admin, password_hash FROM users WHERE username = ?`, "carol").
		Scan(&uid, &isAdmin, &livePH); err != nil {
		t.Fatalf("read live user: %v", err)
	}
	if isAdmin != 1 {
		t.Errorf("is_admin = %d, want 1 (--admin)", isAdmin)
	}
	if !livePH.Valid || livePH.String == "" {
		t.Fatal("live password_hash is empty; add must store a hash")
	}
	if livePH.String == "correct-horse-battery" {
		t.Fatal("password stored in plaintext; add must hash with auth.Hash")
	}

	// Version row exists (op='create') and omits the hash.
	testutil.AssertVersioned(t, db, "users", uid, "create")
	assertHashAbsentFromVersion(t, db, uid, livePH.String)

	// End to end: the real login handler accepts those credentials.
	h := web.Handler(web.Config{Version: "test"}, db, st)
	rec := postLogin(t, h, "10.0.0.1", "carol", "correct-horse-battery")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d; body=%q", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if sessionCookie(rec) == nil {
		t.Fatal("successful login set no session cookie")
	}
}

// TestDisabledUserCannotLogin proves `cuento user disable` blocks login with the
// uniform auth error, and that the disable produced a version row (op='update')
// with disabled_at set (and still no password_hash).
func TestDisabledUserCannotLogin(t *testing.T) {
	db := testutil.NewDB(t)
	st := store.New(db)

	if err := userAdd(context.Background(), st, "dave", "Dave", false, "hunter2-hunter2"); err != nil {
		t.Fatalf("userAdd: %v", err)
	}
	var uid int64
	if err := db.QueryRow(`SELECT id FROM users WHERE username = ?`, "dave").Scan(&uid); err != nil {
		t.Fatalf("read user id: %v", err)
	}

	// Sanity: login works before disabling.
	h := web.Handler(web.Config{Version: "test"}, db, st)
	if rec := postLogin(t, h, "10.0.1.1", "dave", "hunter2-hunter2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("pre-disable login status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if err := userDisable(context.Background(), st, "dave"); err != nil {
		t.Fatalf("userDisable: %v", err)
	}

	// Live row: disabled_at set.
	var liveDisabled sql.NullString
	if err := db.QueryRow(`SELECT disabled_at FROM users WHERE id = ?`, uid).Scan(&liveDisabled); err != nil {
		t.Fatalf("read disabled_at: %v", err)
	}
	if !liveDisabled.Valid {
		t.Fatal("disabled_at is NULL after disable")
	}

	// Version row: op='update' with disabled_at set.
	testutil.AssertVersioned(t, db, "users", uid, "update")
	var vDisabled sql.NullString
	if err := db.QueryRow(
		`SELECT disabled_at FROM users_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, uid,
	).Scan(&vDisabled); err != nil {
		t.Fatalf("read version disabled_at: %v", err)
	}
	if !vDisabled.Valid {
		t.Error("version row disabled_at is NULL; disable snapshot should capture it")
	}

	// End to end: login is now rejected with the uniform 401 error.
	rec := postLogin(t, h, "10.0.1.2", "dave", "hunter2-hunter2")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled-user login status = %d, want %d (uniform rejection)", rec.Code, http.StatusUnauthorized)
	}
	if sessionCookie(rec) != nil {
		t.Error("disabled-user login set a session cookie")
	}
}

// TestSetUserPasswordVersionOmitsHash proves the passwd path is versioned
// (op='update') and that its snapshot still carries no password_hash (rule 5),
// while the live hash actually changed.
func TestSetUserPasswordVersionOmitsHash(t *testing.T) {
	db := testutil.NewDB(t)
	st := store.New(db)

	if err := userAdd(context.Background(), st, "erin", "Erin", false, "first-password-xyz"); err != nil {
		t.Fatalf("userAdd: %v", err)
	}
	var (
		uid   int64
		oldPH string
	)
	if err := db.QueryRow(`SELECT id, password_hash FROM users WHERE username = ?`, "erin").
		Scan(&uid, &oldPH); err != nil {
		t.Fatalf("read user: %v", err)
	}

	if err := userPasswd(context.Background(), st, "erin", "second-password-abc"); err != nil {
		t.Fatalf("userPasswd: %v", err)
	}

	var newPH string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, uid).Scan(&newPH); err != nil {
		t.Fatalf("read new hash: %v", err)
	}
	if newPH == oldPH {
		t.Fatal("password_hash unchanged after passwd")
	}
	if newPH == "second-password-abc" {
		t.Fatal("new password stored in plaintext")
	}

	testutil.AssertVersioned(t, db, "users", uid, "update")
	assertHashAbsentFromVersion(t, db, uid, newPH)

	// And the new password actually authenticates end to end.
	h := web.Handler(web.Config{Version: "test"}, db, st)
	if rec := postLogin(t, h, "10.0.2.1", "erin", "second-password-abc"); rec.Code != http.StatusSeeOther {
		t.Fatalf("post-passwd login status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
}

// TestParseUserAddInterspersed locks the flag-handling regression: stdlib flag
// stops at the first positional, so --admin/--display AFTER the username were
// silently dropped. parseUserAdd must accept flags in any position and never
// mistake a flag VALUE (e.g. the --display argument) for the username.
func TestParseUserAddInterspersed(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantUser    string
		wantDisplay string
		wantAdmin   bool
	}{
		{"flags after username", []string{"carol", "--admin", "--display", "Carol Admin"}, "carol", "Carol Admin", true},
		{"flags before username", []string{"--admin", "--display", "Carol Admin", "carol"}, "carol", "Carol Admin", true},
		{"display value not taken as username", []string{"--display", "Some Name", "dave"}, "dave", "Some Name", false},
		{"no admin", []string{"erin"}, "erin", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pa, err := parseUserAdd(tc.args)
			if err != nil {
				t.Fatalf("parseUserAdd(%v): %v", tc.args, err)
			}
			if pa.username != tc.wantUser {
				t.Errorf("username = %q, want %q", pa.username, tc.wantUser)
			}
			if pa.display != tc.wantDisplay {
				t.Errorf("display = %q, want %q", pa.display, tc.wantDisplay)
			}
			if pa.admin != tc.wantAdmin {
				t.Errorf("admin = %v, want %v", pa.admin, tc.wantAdmin)
			}
		})
	}

	if _, err := parseUserAdd(nil); err == nil {
		t.Error("parseUserAdd with no username should error")
	}

	// A second positional must be REJECTED, not silently dropped: `user add alice
	// bob` used to keep "alice" and discard "bob" (mirrors expense-report's guard).
	if _, err := parseUserAdd([]string{"alice", "bob"}); err == nil {
		t.Error("parseUserAdd with two usernames should error, not drop the second")
	}
}

// assertHashAbsentFromVersion scans every column of the entity's latest
// users_versions row as text and fails if any equals the secret hash.
func assertHashAbsentFromVersion(t *testing.T, d *sql.DB, entityID int64, secret string) {
	t.Helper()

	rows, err := d.Query(
		`SELECT * FROM users_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, entityID,
	)
	if err != nil {
		t.Fatalf("select users_versions row: %v", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("no users_versions row for entity_id=%d", entityID)
	}
	cells := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("scan version row: %v", err)
	}
	for i, c := range cells {
		if c.Valid && c.String == secret {
			t.Fatalf("column %q of the version row holds the password hash; rule 5 violated", cols[i])
		}
	}
}
