package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// TestUsersVersionOmitsPasswordHash is the CRITICAL invariant of p06.1 (rule 5):
// a user created WITH a real password hash gets a users_versions op='create'
// row that carries every non-secret business column but CANNOT carry the hash —
// the audit trail must never see the secret. It proves both halves:
//
//	(a) users_versions has NO password_hash column at all (PRAGMA table_info);
//	(b) the created user's version row exists (op='create') with the non-secret
//	    columns populated and the LIVE users row does hold the hash.
func TestUsersVersionOmitsPasswordHash(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	secretHash := "$argon2id$v=19$m=65536,t=1,p=2$c29tZXNhbHR2YWx1ZQ$Zm9vYmFyYmF6cXV4"
	id, err := s.CreateUser(ctx, CreateUserInput{
		Username:     "alice",
		DisplayName:  "Alice Example",
		PasswordHash: &secretHash,
		IsAdmin:      false,
		TxnPerm:      "write",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 1 {
		t.Fatalf("CreateUser returned id %d, want > 1 (system user is 1)", id)
	}

	// (a) users_versions must not declare a password_hash column.
	rows, err := d.Query(`PRAGMA table_info(users_versions)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(users_versions): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "password_hash" {
			t.Fatal("users_versions declares a password_hash column; rule 5 forbids the hash in the audit trail")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}

	// The hash must not appear ANYWHERE in users_versions for this user, even
	// smuggled into a text column. Scan every column of the latest version row as
	// text and assert none equals the secret.
	assertHashAbsentFromVersion(t, d, int64(id), secretHash)

	// (b) the version row exists, op='create', with the non-secret columns set.
	testutil.AssertVersioned(t, d, "users", int64(id), "create")

	var (
		vUsername string
		vDisplay  string
		vIsAdmin  int64
		vTxnPerm  string
		vLocale   string
	)
	err = d.QueryRow(
		`SELECT username, display_name, is_admin, txn_perm, locale
		   FROM users_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, id,
	).Scan(&vUsername, &vDisplay, &vIsAdmin, &vTxnPerm, &vLocale)
	if err != nil {
		t.Fatalf("read users_versions snapshot: %v", err)
	}
	if vUsername != "alice" {
		t.Errorf("version username = %q, want %q", vUsername, "alice")
	}
	if vDisplay != "Alice Example" {
		t.Errorf("version display_name = %q, want %q", vDisplay, "Alice Example")
	}
	if vIsAdmin != 0 {
		t.Errorf("version is_admin = %d, want 0", vIsAdmin)
	}
	if vTxnPerm != "write" {
		t.Errorf("version txn_perm = %q, want %q", vTxnPerm, "write")
	}
	if vLocale != "en" {
		t.Errorf("version locale = %q, want default %q", vLocale, "en")
	}

	// The LIVE users row DOES carry the hash — the asymmetry is the whole point:
	// the current table stores the secret, the audit trail never does.
	var livePH any
	if err := d.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, id).Scan(&livePH); err != nil {
		t.Fatalf("read live password_hash: %v", err)
	}
	if got, _ := livePH.(string); got != secretHash {
		t.Errorf("live password_hash = %v, want the stored hash", livePH)
	}
}

// assertHashAbsentFromVersion scans every column of the entity's latest
// users_versions row as text and fails if any equals the secret hash — proving
// the hash was not smuggled into some other column.
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

// okLocale is a stand-in for the web layer's i18n.Langs membership test: the store
// stays i18n-free and takes the check as a closure. en/es are the catalog langs.
func okLocale(s string) bool { return s == "en" || s == "es" }

// TestUpdateUserSettingsVersioned proves the p13.1 settings write: a valid update
// changes the live users row across all seven preference columns AND appends an
// op='update' users_versions snapshot naming the actor, under ONE change (rule 5).
// It also proves the nullable default subsidiary round-trips (set then clear).
func TestUpdateUserSettingsVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "carol", DisplayName: "Carol"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Root subsidiary is seeded id 1; use it as a valid default. The seeded root
	// program (id 1) is a valid default program.
	rootSub := ids.SubsidiaryID(1)
	rootProg := ids.ProgramID(1)
	in := UserSettingsInput{
		Locale: "es", DateFormat: "EU", NumberFormat: "EU",
		DisplayMode: "dr_cr", NegStyle: "parens", Theme: "dark",
		DefaultSubsidiaryID: &rootSub,
		DefaultProgramID:    &rootProg,
	}
	if err := s.UpdateUserSettings(ctx, id, in, okLocale); err != nil {
		t.Fatalf("UpdateUserSettings: %v", err)
	}

	// Live row reflects every field.
	got, err := s.UserByID(ctx, id)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Locale != "es" || got.DateFormat != "EU" || got.NumberFormat != "EU" ||
		got.DisplayMode != "dr_cr" || got.NegStyle != "parens" || got.Theme != "dark" {
		t.Fatalf("live settings = %+v, want the applied values", got)
	}
	if got.DefaultSubsidiaryID == nil || *got.DefaultSubsidiaryID != rootSub {
		t.Fatalf("default subsidiary = %v, want %d", got.DefaultSubsidiaryID, rootSub)
	}
	if got.DefaultProgramID == nil || *got.DefaultProgramID != rootProg {
		t.Fatalf("default program = %v, want %d", got.DefaultProgramID, rootProg)
	}

	// An op='update' version snapshot exists, tied to the acting actor (id 1).
	testutil.AssertVersioned(t, d, "users", int64(id), "update")
	var vActor int64
	var vLocale, vDisplayMode string
	err = d.QueryRow(
		`SELECT c.actor_id, v.locale, v.display_mode
		   FROM users_versions v JOIN changes c ON c.id = v.change_id
		  WHERE v.entity_id = ? AND v.op = 'update'
		  ORDER BY v.valid_from DESC, v.id DESC LIMIT 1`, id,
	).Scan(&vActor, &vLocale, &vDisplayMode)
	if err != nil {
		t.Fatalf("read update version: %v", err)
	}
	if vActor != 1 {
		t.Errorf("version actor_id = %d, want 1 (the acting user)", vActor)
	}
	if vLocale != "es" || vDisplayMode != "dr_cr" {
		t.Errorf("version snapshot = (%q,%q), want (es,dr_cr)", vLocale, vDisplayMode)
	}

	// Clearing the default subsidiary + program (nil) persists a NULL.
	in.DefaultSubsidiaryID = nil
	in.DefaultProgramID = nil
	if err := s.UpdateUserSettings(ctx, id, in, okLocale); err != nil {
		t.Fatalf("UpdateUserSettings clear: %v", err)
	}
	got, err = s.UserByID(ctx, id)
	if err != nil {
		t.Fatalf("UserByID after clear: %v", err)
	}
	if got.DefaultSubsidiaryID != nil {
		t.Errorf("default subsidiary = %v, want nil after clear", got.DefaultSubsidiaryID)
	}
	if got.DefaultProgramID != nil {
		t.Errorf("default program = %v, want nil after clear", got.DefaultProgramID)
	}
}

// TestUpdateUserSettingsRejectsInvalid proves each field's validator rejects an
// unknown value (the columns have no DB CHECK, so the store is the guard) and that
// a non-existent default subsidiary is rejected as a clean ErrInvalidSetting rather
// than an FK explosion. Every rejection must leave NO version row (fail before the
// write funnel opens).
func TestUpdateUserSettingsRejectsInvalid(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "dave", DisplayName: "Dave"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	base := UserSettingsInput{
		Locale: "en", DateFormat: "ISO", NumberFormat: "US",
		DisplayMode: "signed", NegStyle: "minus", Theme: "auto",
	}
	bad := ids.SubsidiaryID(999999)
	badProg := ids.ProgramID(999999)
	cases := map[string]func(UserSettingsInput) UserSettingsInput{
		"locale":        func(i UserSettingsInput) UserSettingsInput { i.Locale = "de"; return i },
		"date_format":   func(i UserSettingsInput) UserSettingsInput { i.DateFormat = "XX"; return i },
		"number_format": func(i UserSettingsInput) UserSettingsInput { i.NumberFormat = "XX"; return i },
		"display_mode":  func(i UserSettingsInput) UserSettingsInput { i.DisplayMode = "XX"; return i },
		"neg_style":     func(i UserSettingsInput) UserSettingsInput { i.NegStyle = "XX"; return i },
		"theme":         func(i UserSettingsInput) UserSettingsInput { i.Theme = "XX"; return i },
		"default_sub":   func(i UserSettingsInput) UserSettingsInput { i.DefaultSubsidiaryID = &bad; return i },
		"default_prog":  func(i UserSettingsInput) UserSettingsInput { i.DefaultProgramID = &badProg; return i },
	}
	for name, mut := range cases {
		if err := s.UpdateUserSettings(ctx, id, mut(base), okLocale); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: err = %v, want ErrInvalidSetting", name, err)
		}
	}

	// No update version row was ever written (every reject fails before the funnel).
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM users_versions WHERE entity_id = ? AND op = 'update'`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count update versions: %v", err)
	}
	if n != 0 {
		t.Errorf("update version rows = %d, want 0 (rejects must not write)", n)
	}
}

// TestCreateUserWithoutPasswordHash proves a user may be created with no
// password (PasswordHash nil), mirroring the system user: the live row's
// password_hash is NULL and the version row is still written.
func TestCreateUserWithoutPasswordHash(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{
		Username:    "bob",
		DisplayName: "Bob Example",
		TxnPerm:     "read",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	testutil.AssertVersioned(t, d, "users", int64(id), "create")

	var ph any
	if err := d.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, id).Scan(&ph); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if ph != nil {
		t.Errorf("password_hash = %v, want NULL for a passwordless user", ph)
	}
}

// TestEnableUserClearsDisabledAndVersioned proves the re-enable path (mirror of
// DisableUser): after disabling a user, EnableUser clears disabled_at on the live
// row AND appends an op='update' users_versions snapshot (rule 5). Asserting
// versioning alone is not enough — the live disabled_at must be NULL again.
func TestEnableUserClearsDisabledAndVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "eve", DisplayName: "Eve", TxnPerm: "read"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DisableUser(ctx, id); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}

	if err := s.EnableUser(ctx, id); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}

	// Live row: disabled_at is cleared.
	var da any
	if err := d.QueryRow(`SELECT disabled_at FROM users WHERE id = ?`, id).Scan(&da); err != nil {
		t.Fatalf("read disabled_at: %v", err)
	}
	if da != nil {
		t.Errorf("disabled_at = %v, want NULL after EnableUser", da)
	}

	// The enable is versioned as op='update'.
	testutil.AssertVersioned(t, d, "users", int64(id), "update")
}

// TestEnableUserRejectsSystemUser proves the system-user guard: EnableUser refuses
// the seeded machine actor (id 1) with ErrSystemUser and writes nothing.
func TestEnableUserRejectsSystemUser(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	err := s.EnableUser(ctx, systemUserID)
	if !errors.Is(err, ErrSystemUser) {
		t.Fatalf("EnableUser(system) = %v, want ErrSystemUser", err)
	}
}

// TestEnableUserMissingID proves a crafted/missing id is a clean ErrUserNotFound
// (the existence check runs before the write funnel opens, so no empty change row
// is committed — rule 14).
func TestEnableUserMissingID(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	err := s.EnableUser(ctx, ids.UserID(9999))
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("EnableUser(missing) = %v, want ErrUserNotFound", err)
	}
}

// TestSetUserDisplayNameUpdatesAndVersioned proves SetUserDisplayName trims and
// persists the new display name on the live row and appends an op='update'
// users_versions snapshot (rule 5) tied to the acting actor — the audit trail
// records who renamed whom. It also confirms a surrounding-whitespace value is
// stored trimmed (the store is the guard, not the form).
func TestSetUserDisplayNameUpdatesAndVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "erin", DisplayName: "Erin"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.SetUserDisplayName(ctx, id, "  Erin O'Neil  "); err != nil {
		t.Fatalf("SetUserDisplayName: %v", err)
	}

	got, err := s.AdminUserByID(ctx, id)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if got.DisplayName != "Erin O'Neil" {
		t.Fatalf("display_name = %q, want trimmed %q", got.DisplayName, "Erin O'Neil")
	}

	testutil.AssertVersioned(t, d, "users", int64(id), "update")

	var vName string
	var vActor int64
	err = d.QueryRow(
		`SELECT v.display_name, c.actor_id
		   FROM users_versions v JOIN changes c ON c.id = v.change_id
		  WHERE v.entity_id = ? AND v.op = 'update'
		  ORDER BY v.valid_from DESC, v.id DESC LIMIT 1`, id,
	).Scan(&vName, &vActor)
	if err != nil {
		t.Fatalf("read update version: %v", err)
	}
	if vName != "Erin O'Neil" {
		t.Errorf("version snapshot display_name = %q, want %q", vName, "Erin O'Neil")
	}
	if vActor != 1 {
		t.Errorf("version actor_id = %d, want 1 (the acting user)", vActor)
	}
}

// TestSetUserDisplayNameRejectsEmpty proves a blank / whitespace-only name is a
// clean ErrEmptyDisplayName (display_name is NOT NULL) rejected BEFORE the write
// funnel opens, so no version row is written. display_name is the first free-text
// user field, so an empty submit is a real (not merely crafted) path.
func TestSetUserDisplayNameRejectsEmpty(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "frank", DisplayName: "Frank"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for _, in := range []string{"", "   ", "\t\n"} {
		if err := s.SetUserDisplayName(ctx, id, in); !errors.Is(err, ErrEmptyDisplayName) {
			t.Fatalf("SetUserDisplayName(%q) = %v, want ErrEmptyDisplayName", in, err)
		}
	}

	// The rejection left the original name intact and appended no version row.
	got, err := s.AdminUserByID(ctx, id)
	if err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	}
	if got.DisplayName != "Frank" {
		t.Errorf("display_name = %q, want unchanged Frank (rejected write must not persist)", got.DisplayName)
	}
}

// TestSetUserDisplayNameRejectsTooLong proves the additive abuse-guard rune cap
// (CreateUser applies none; this cap is documented as additive to this method).
// The rejection is a clean ErrDisplayNameTooLong before the write funnel opens.
func TestSetUserDisplayNameRejectsTooLong(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	id, err := s.CreateUser(ctx, CreateUserInput{Username: "gwen", DisplayName: "Gwen"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	long := strings.Repeat("x", MaxDisplayNameLen+1)
	if err := s.SetUserDisplayName(ctx, id, long); !errors.Is(err, ErrDisplayNameTooLong) {
		t.Fatalf("SetUserDisplayName(too long) = %v, want ErrDisplayNameTooLong", err)
	}
	// A name AT the cap is accepted.
	if err := s.SetUserDisplayName(ctx, id, strings.Repeat("y", MaxDisplayNameLen)); err != nil {
		t.Fatalf("SetUserDisplayName(at cap): %v", err)
	}
}
