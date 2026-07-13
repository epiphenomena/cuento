package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cuento/internal/store"
)

// p18.3 ops page tests. Driven through the REAL mounted router (httptest) over a
// real migrated temp db (AGENTS testing conventions) -- no store mocks. They reuse
// the shared web-package helpers (adminApp, asUser, mkUser). Coverage:
//
//   - admin-only: a non-admin is 403 on GET /admin/ops AND on POST /admin/ops/backup
//     (the matrix auto-covers the registry too; the task calls the backup out).
//   - snapshot validity (THE key p18.3 test): POST /admin/ops/backup returns an
//     octet-stream whose bytes, written to a temp file and opened with the sqlite
//     driver, pass PRAGMA quick_check AND contain the schema/data (the users table).
//   - integrity check rendering: a clean db shows "no violations"; an induced Z17
//     warning renders with its rule + detail under the warnings group.
//   - audit: a backup writes exactly one ops.backup change naming the acting admin.

// TestOpsPageRenders: GET /admin/ops (Admin) renders build info (version + Go
// version) and, on a clean db, the "no violations" notice.
func TestOpsPageRenders(t *testing.T) {
	h, st, sm, _ := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/ops", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ops: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Build info: the app is constructed with Version "test" (adminApp), and the Go
	// runtime version is rendered verbatim.
	if !strings.Contains(body, "test") {
		t.Errorf("ops page does not show the build version; body: %s", body)
	}
	if !strings.Contains(body, runtime.Version()) {
		t.Errorf("ops page does not show the Go runtime version %q; body: %s", runtime.Version(), body)
	}
	// A freshly migrated db is integrity-clean -> the "no violations" notice.
	if !strings.Contains(body, "ops-check-clean") {
		t.Errorf("clean db did not render the no-violations notice; body: %s", body)
	}
}

// TestOpsPageNonAdminForbidden: a non-admin (bookkeeper) is 403 on GET /admin/ops,
// asserted explicitly (the matrix covers this too, but the task calls it out).
func TestOpsPageNonAdminForbidden(t *testing.T) {
	h, st, sm, _ := adminApp(t)
	book := mkUser(t, st, "book", "write", false)

	rec := asUser(t, h, sm, book, http.MethodGet, "/admin/ops", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /admin/ops = %d, want 403", rec.Code)
	}
}

// TestOpsBackupNonAdminForbidden: a non-admin is 403 on POST /admin/ops/backup, and
// no snapshot/audit is produced (the handler never runs). Asserted explicitly per
// the task (the mutating action must be Admin-only).
func TestOpsBackupNonAdminForbidden(t *testing.T) {
	h, st, sm, db := adminApp(t)
	book := mkUser(t, st, "book", "write", false)

	rec := asUser(t, h, sm, book, http.MethodPost, "/admin/ops/backup", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin POST /admin/ops/backup = %d, want 403", rec.Code)
	}
	if n := countChangesByKind(t, db, "ops.backup"); n != 0 {
		t.Errorf("forbidden backup still wrote %d ops.backup change(s)", n)
	}
}

// TestOpsBackupSnapshotIsValidSQLite is THE key p18.3 test: the backup handler's
// output is a valid SQLite database. We POST as an admin, write the octet-stream
// body to a temp file, open it with the sqlite driver, run PRAGMA quick_check (must
// be "ok"), and assert it carries the schema+data (the seeded system user is there).
func TestOpsBackupSnapshotIsValidSQLite(t *testing.T) {
	h, st, sm, _ := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/ops/backup", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/ops/backup = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, ".db") {
		t.Errorf("Content-Disposition = %q, want an attachment .db filename", cd)
	}

	raw := rec.Body.Bytes()
	if len(raw) == 0 {
		t.Fatalf("backup body is empty")
	}
	// The first 16 bytes of any SQLite file are the "SQLite format 3\0" magic.
	if !strings.HasPrefix(string(raw), "SQLite format 3\x00") {
		t.Fatalf("backup does not start with the SQLite magic header")
	}

	// Persist the bytes and open them as a database.
	dir := t.TempDir()
	snap := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(snap, raw, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	sdb, err := sql.Open("sqlite", "file:"+snap)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = sdb.Close() }()

	var quick string
	if err := sdb.QueryRow("PRAGMA quick_check").Scan(&quick); err != nil {
		t.Fatalf("PRAGMA quick_check: %v", err)
	}
	if quick != "ok" {
		t.Fatalf("PRAGMA quick_check = %q, want ok", quick)
	}
	// The snapshot carries the schema + data: the seeded system user (id 1) is there.
	var username string
	if err := sdb.QueryRow("SELECT username FROM users WHERE id = 1").Scan(&username); err != nil {
		t.Fatalf("read users from snapshot: %v", err)
	}
	if username != "system" {
		t.Errorf("snapshot users id=1 username = %q, want system", username)
	}
}

// TestOpsBackupWritesAuditChange: a successful backup writes exactly one ops.backup
// change naming the acting admin (rule 14 -- who took a snapshot, and when).
func TestOpsBackupWritesAuditChange(t *testing.T) {
	h, st, sm, db := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	before := countChangesByKind(t, db, "ops.backup")
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/ops/backup", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/ops/backup = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	after := countChangesByKind(t, db, "ops.backup")
	if after != before+1 {
		t.Fatalf("ops.backup change count = %d, want %d (exactly one new change)", after, before+1)
	}
	if actor := latestChangeActorByKind(t, db, "ops.backup"); actor != admin {
		t.Errorf("ops.backup actor = %d, want the admin %d", actor, admin)
	}
}

// TestOpsCheckRendersViolation: an induced Z17 warning (an intercompany account
// whose splits do not net to zero per currency) renders on the ops page under the
// warnings group, with its rule + detail. The state is built through the store (a
// balanced transfer), then ONE account leg is flagged intercompany via raw SQL so
// only that side is counted -> Z17's per-currency net is non-zero. Raw SQL in a test
// is in-convention (p05.3) for crafting a violating state the store would reject.
func TestOpsCheckRendersViolation(t *testing.T) {
	h, st, sm, db := adminApp(t)
	admin := mkUser(t, st, "boss", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	a1, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Bank A"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create account a1: %v", err)
	}
	a2, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Bank B"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create account a2: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-01-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: a1, Amount: 1000, Position: 0},
			{AccountID: a2, Amount: -1000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post transaction: %v", err)
	}
	// Flag ONLY a1 intercompany: its lone +1000 split now nets non-zero for USD, so
	// Z17 (intercompany net per currency) fires. a2 stays normal.
	if _, err := db.ExecContext(context.Background(),
		"UPDATE accounts SET intercompany = 1 WHERE id = ?", a1); err != nil {
		t.Fatalf("flag intercompany: %v", err)
	}

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/ops", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ops: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "ops-check-clean") {
		t.Fatalf("ops page shows clean despite an induced Z17 violation; body: %s", body)
	}
	// The violation renders with its rule id under the warnings group.
	if !strings.Contains(body, `data-rule="Z17"`) {
		t.Errorf("Z17 violation not rendered with its rule; body: %s", body)
	}
	if !strings.Contains(body, `data-severity="warning"`) {
		t.Errorf("Z17 not rendered as a warning; body: %s", body)
	}
	// The detail text (naming the currency) renders too.
	if !strings.Contains(body, "intercompany net for USD") {
		t.Errorf("Z17 detail not rendered; body: %s", body)
	}
}

// countChangesByKind returns how many changes rows carry the given kind. Raw read in
// a test is in-convention (the store exposes no changes-by-kind read; p05.3).
func countChangesByKind(t *testing.T, db *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM changes WHERE kind = ?", kind).Scan(&n); err != nil {
		t.Fatalf("count changes kind=%s: %v", kind, err)
	}
	return n
}

// latestChangeActorByKind returns the actor_id of the most recent change of the
// given kind (highest id), or 0 if none.
func latestChangeActorByKind(t *testing.T, db *sql.DB, kind string) int64 {
	t.Helper()
	var actor int64
	err := db.QueryRowContext(context.Background(),
		"SELECT actor_id FROM changes WHERE kind = ? ORDER BY id DESC LIMIT 1", kind).Scan(&actor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		t.Fatalf("latest change actor kind=%s: %v", kind, err)
	}
	return actor
}
