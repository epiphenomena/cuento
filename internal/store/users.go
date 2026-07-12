package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"cuento/internal/db/sqlc"
)

// User operations (p06.1). CreateUser is the MINIMAL write needed to prove the
// security-critical invariant (rule 5): a user's live row carries its
// password_hash, but the users_versions audit snapshot NEVER does. It copies the
// versioned-entity discipline p04.2/p05.2 established -- one change through the
// funnel, live write first, then the snapshot-from-live version append -- with
// the ONE deliberate asymmetry that the version query selects every users column
// EXCEPT password_hash.
//
// Full user management (passwd/disable/admin toggles, report-group grants) is
// deferred to p06.4/p13.2; grant version-append writers land there. This step
// intentionally builds no grant surface.

// systemUserID is the seeded machine actor (id 1, migration 00002). It is never
// an operator: the admin surface refuses to disable, perm-change, grant, or reset
// it. Named here so the guard reads intent, not a magic number.
const systemUserID int64 = 1

// CreateUserInput is the desired state of a NEW user. PasswordHash is optional
// (nil = a passwordless user, like the system user). TxnPerm defaults to "none"
// when empty. The per-user settings columns are NOT set here -- their schema
// defaults apply and the version snapshot reads them back from the live row.
type CreateUserInput struct {
	Username     string
	DisplayName  string
	PasswordHash *string
	IsAdmin      bool
	TxnPerm      string
}

// CreateUser inserts a user (with its password_hash) and appends the op='create'
// users_versions row under ONE change, returning the new id. The version append
// is snapshot-from-live and omits password_hash by construction (rule 5): the
// secret is stored only in the live users table and never enters the audit
// trail. This is the critical path the version-omits-hash test exercises.
func (s *Store) CreateUser(ctx context.Context, in CreateUserInput) (int64, error) {
	txnPerm := in.TxnPerm
	if txnPerm == "" {
		txnPerm = "none"
	}

	var newID int64
	_, err := s.write(ctx, "user.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			id, err := q.InsertUser(ctx, sqlc.InsertUserParams{
				Username:     in.Username,
				DisplayName:  in.DisplayName,
				CreatedAt:    s.now().Format(time.RFC3339Nano),
				PasswordHash: nullStringPtr(in.PasswordHash),
				IsAdmin:      boolToInt(in.IsAdmin),
				TxnPerm:      txnPerm,
			})
			if err != nil {
				if isUniqueViolation(err) {
					return ErrUsernameTaken
				}
				return fmt.Errorf("insert user: %w", err)
			}
			newID = id

			return insertUserVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("create user: %w", err)
	}
	return newID, nil
}

// ErrUsernameTaken is returned by CreateUser when the username collides with an
// existing user (the users.username UNIQUE constraint). The web layer maps it to a
// username field error (a 422), rather than a 500 -- it is a routine user mistake,
// not a server fault.
var ErrUsernameTaken = errors.New("store: username already taken")

// ValidTxnPermPublic reports whether p is one of none/read/write. Exported so the
// web create form can reject a crafted value before calling CreateUser (whose
// TxnPerm defaults to "none" on empty and relies on the column CHECK otherwise).
func ValidTxnPermPublic(p string) bool { return validTxnPerm(p) }

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure. The
// modernc.org/sqlite driver surfaces it in the error text ("UNIQUE constraint
// failed: ..."); matching the text keeps the store driver-agnostic without pulling
// the driver's error type into this package.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ErrUserNotFound is returned by the username-keyed operations (SetUserPassword
// and DisableUser resolve ids from usernames; the CLI resolves the id first)
// when no user matches. Callers turn it into a clean CLI message.
var ErrUserNotFound = errors.New("store: user not found")

// SetUserPassword replaces a user's password_hash on the live row and appends an
// op='update' users_versions row under ONE change (p06.4 `user passwd`). The
// version append is the SAME snapshot-from-live query CreateUser uses, so it
// omits password_hash by construction (rule 5): the new secret enters only the
// live table, never the audit trail.
func (s *Store) SetUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	_, err := s.write(ctx, "user.passwd", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := q.SetUserPassword(ctx, sqlc.SetUserPasswordParams{
				PasswordHash: sql.NullString{String: passwordHash, Valid: true},
				ID:           userID,
			}); err != nil {
				return fmt.Errorf("set password: %w", err)
			}
			return insertUserVersion(ctx, q, changeID, "update", userID)
		})
	if err != nil {
		return fmt.Errorf("set user password (id %d): %w", userID, err)
	}
	return nil
}

// ErrSystemUser is returned when an admin action targets the seeded system user
// (id 1): it is passwordless machinery, never an operator, so it cannot be
// disabled, perm-changed, granted, or password-reset. The web layer maps it to a
// page-level 422 guard.
var ErrSystemUser = errors.New("store: cannot manage the system user")

// ErrLastAdmin is returned by DisableUser when disabling the target would leave
// the org with no enabled admin (the target is the last enabled admin). The web
// layer maps it to a page-level 422 guard so the admin surface is never locked
// out. This is checked against CountOtherEnabledAdmins, NOT CountHumanUsers
// (which counts non-system users regardless of admin/enabled state).
var ErrLastAdmin = errors.New("store: cannot disable the last admin")

// DisableUser marks a user disabled (disabled_at = now) on the live row and
// appends an op='update' users_versions row under ONE change (p06.4 `user
// disable`). A disabled user cannot log in (the login handler enforces this).
// Unlike password_hash, disabled_at IS part of the snapshot, so the audit trail
// records who was disabled and when.
//
// Two guards protect the admin surface (p13.2): the system user (id 1) can never
// be disabled (ErrSystemUser), and an admin cannot be disabled when they are the
// last ENABLED admin (ErrLastAdmin) -- else no one could reach /admin/**. Both
// are checked BEFORE the write so a blocked disable leaves no trace. A non-admin
// user is never subject to the last-admin guard.
func (s *Store) DisableUser(ctx context.Context, userID int64) error {
	if userID == systemUserID {
		return ErrSystemUser
	}
	row, err := s.q.GetUserRow(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("disable user (id %d): load: %w", userID, err)
	}
	if row.IsAdmin != 0 {
		others, err := s.q.CountOtherEnabledAdmins(ctx, userID)
		if err != nil {
			return fmt.Errorf("disable user (id %d): count admins: %w", userID, err)
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	_, err = s.write(ctx, "user.disable", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{
				DisabledAt: sql.NullString{String: s.now().Format(time.RFC3339Nano), Valid: true},
				ID:         userID,
			}); err != nil {
				return fmt.Errorf("set disabled: %w", err)
			}
			return insertUserVersion(ctx, q, changeID, "update", userID)
		})
	if err != nil {
		return fmt.Errorf("disable user (id %d): %w", userID, err)
	}
	return nil
}

// ErrInvalidTxnPerm is returned by SetUserTxnPerm when the requested value is not
// one of none/read/write. The migration's CHECK is only a backstop; the store is
// the guard (mirroring ErrInvalidSetting / ErrInvalidTheme). The web layer maps it
// to a 422 form error (only reachable by a crafted request, the form is a fixed
// <select>).
var ErrInvalidTxnPerm = errors.New("store: invalid txn_perm value")

// validTxnPerm mirrors the migration 00006 CHECK (none/read/write) so the store
// rejects an unknown value before it reaches the db.
func validTxnPerm(p string) bool { return p == "none" || p == "read" || p == "write" }

// SetUserTxnPerm updates a user's transaction permission on the live row and
// appends an op='update' users_versions row under ONE change (p13.2 admin), so the
// audit trail names the acting admin (changes.actor_id) who changed the perm.
// txn_perm IS part of the users_versions snapshot. The value is validated against
// {none,read,write} first (ErrInvalidTxnPerm); the system user (id 1) is refused
// (ErrSystemUser) -- its perm is irrelevant machinery and must not be audited as an
// operator change.
func (s *Store) SetUserTxnPerm(ctx context.Context, userID int64, perm string) error {
	if userID == systemUserID {
		return ErrSystemUser
	}
	if !validTxnPerm(perm) {
		return ErrInvalidTxnPerm
	}
	_, err := s.write(ctx, "user.txn_perm", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := q.SetUserTxnPerm(ctx, sqlc.SetUserTxnPermParams{TxnPerm: perm, ID: userID}); err != nil {
				return fmt.Errorf("set txn_perm: %w", err)
			}
			return insertUserVersion(ctx, q, changeID, "update", userID)
		})
	if err != nil {
		return fmt.Errorf("set user txn_perm (id %d): %w", userID, err)
	}
	return nil
}

// AdminUser is one row of the admin user list (p13.2 /admin/users): the fields the
// list and per-user editor read. Excludes the system user by construction (the
// ListUsers query filters id <> 1). A read projection, not a live entity.
type AdminUser struct {
	ID          int64
	Username    string
	DisplayName string
	IsAdmin     bool
	TxnPerm     string
	Disabled    bool
}

// ListUsers returns the manageable operators (every user except the seeded system
// user), ordered by username. A read (rule 2).
func (s *Store) ListUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	out := make([]AdminUser, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminUser{
			ID: r.ID, Username: r.Username, DisplayName: r.DisplayName,
			IsAdmin: r.IsAdmin != 0, TxnPerm: r.TxnPerm, Disabled: r.DisabledAt.Valid,
		})
	}
	return out, nil
}

// AdminUserByID returns one manageable user for the per-user admin detail page
// (p13.2). The system user (id 1) is refused (ErrSystemUser) -- it is not an
// operator; a missing id returns ErrUserNotFound. A read.
func (s *Store) AdminUserByID(ctx context.Context, userID int64) (AdminUser, error) {
	if userID == systemUserID {
		return AdminUser{}, ErrSystemUser
	}
	r, err := s.q.GetUserRow(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AdminUser{}, ErrUserNotFound
		}
		return AdminUser{}, fmt.Errorf("store: get user %d: %w", userID, err)
	}
	return AdminUser{
		ID: r.ID, Username: r.Username, DisplayName: r.DisplayName,
		IsAdmin: r.IsAdmin != 0, TxnPerm: r.TxnPerm, Disabled: r.DisabledAt.Valid,
	}, nil
}

// UserIDByUsername resolves a username to its id for the CLI's passwd/disable
// (which take a username but the versioned store methods take an id). A missing
// username returns ErrUserNotFound. This is a read (rule 2 permits reads outside
// the write funnel via sqlc).
func (s *Store) UserIDByUsername(ctx context.Context, username string) (int64, error) {
	id, err := s.q.UserIDByUsername(ctx, username)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup user %q: %w", username, err)
	}
	return id, nil
}

// CountHumanUsers returns the number of real operators, excluding the seeded
// system user (id 1). serve uses it to decide whether to log the bootstrap hint
// (no human users -> tell the operator to run `cuento user add`). A read.
func (s *Store) CountHumanUsers(ctx context.Context) (int64, error) {
	n, err := s.q.CountHumanUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("count human users: %w", err)
	}
	return n, nil
}

// Credentials is what the login handler needs to authenticate a username: the
// user id, the stored argon2id hash (nil when the user is passwordless, e.g. the
// system user), whether the account is disabled, and the UI locale to bind after
// a successful login. It is a read projection, not a live entity.
type Credentials struct {
	ID           int64
	PasswordHash *string
	Disabled     bool
	Locale       string
}

// CurrentUser is the identity the auth middleware resolves from a session: who
// the request is acting as, plus the permission + locale fields handlers and
// templates read. It is deliberately small — contexts carry the actor, and this
// read carries only what request handling needs (AGENTS Style).
type CurrentUser struct {
	ID       int64
	Username string
	Disabled bool
	TxnPerm  string
	IsAdmin  bool
	Locale   string
	Theme    string
	// Money/date display settings (p11.1): carried so every render path can honor
	// per-user formatting (rule 10) without a second query. Raw column strings
	// (DB defaults US/signed/minus/ISO for an untouched session); the web layer
	// maps them to money.FormatOpts / money.DateFormat.
	DateFormat   string
	NumberFormat string
	DisplayMode  string
	NegStyle     string
	// DefaultSubsidiaryID is the user's preferred subsidiary for new transactions
	// (p12.2); nil = unset, so the editor falls back to the sole/root subsidiary.
	DefaultSubsidiaryID *int64
}

// CredentialsByUsername returns the login credentials for username. A username
// that does not exist returns (zero, sql.ErrNoRows) — the login handler maps
// that to the SAME uniform error as a wrong password so unknown-user and
// wrong-password are indistinguishable (no user enumeration, rule 13). This is a
// read (rule 2 permits reads outside the write funnel via sqlc queries).
func (s *Store) CredentialsByUsername(ctx context.Context, username string) (Credentials, error) {
	row, err := s.q.UserByUsername(ctx, username)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		ID:           row.ID,
		PasswordHash: nullStringToPtr(row.PasswordHash),
		Disabled:     row.DisabledAt.Valid,
		Locale:       row.Locale,
	}, nil
}

// UserByID resolves a session's stored user id back into the current identity.
// Used by the auth middleware on every authenticated request; a missing id
// returns sql.ErrNoRows (a stale/forged session), which the middleware treats as
// anonymous.
func (s *Store) UserByID(ctx context.Context, id int64) (CurrentUser, error) {
	row, err := s.q.UserByID(ctx, id)
	if err != nil {
		return CurrentUser{}, err
	}
	cu := CurrentUser{
		ID:           row.ID,
		Username:     row.Username,
		Disabled:     row.DisabledAt.Valid,
		TxnPerm:      row.TxnPerm,
		IsAdmin:      row.IsAdmin != 0,
		Locale:       row.Locale,
		Theme:        row.Theme,
		DateFormat:   row.DateFormat,
		NumberFormat: row.NumberFormat,
		DisplayMode:  row.DisplayMode,
		NegStyle:     row.NegStyle,
	}
	if row.DefaultSubsidiaryID.Valid {
		v := row.DefaultSubsidiaryID.Int64
		cu.DefaultSubsidiaryID = &v
	}
	return cu, nil
}

// ErrInvalidTheme is returned by SetUserTheme when the requested theme is not one
// of the allowed values. The web layer maps it to a 400 (a bad theme is a client
// error, not a server fault).
var ErrInvalidTheme = errors.New("store: invalid theme")

// ValidTheme reports whether theme is one of the allowed data-theme values
// (light/dark/auto, matching the p06.1 default and the CSS token layer). Exposed
// so the web handler can reject a bad value before writing.
func ValidTheme(theme string) bool {
	switch theme {
	case "light", "dark", "auto":
		return true
	default:
		return false
	}
}

// The valid value sets for the per-user settings columns (p13.1). The web
// format.go maps these strings LENIENTLY (unknown -> default) so a stray value
// never breaks a render path; but the settings WRITE must REJECT unknowns (the
// migration puts a CHECK only on txn_perm, so these columns have no DB backstop --
// the store is the guard that keeps the columns to a known vocabulary). Each
// mirrors the switch arms format.go recognizes.
func validDateFormat(s string) bool   { return s == "ISO" || s == "US" || s == "EU" }
func validNumberFormat(s string) bool { return s == "US" || s == "EU" || s == "plain" }
func validDisplayMode(s string) bool  { return s == "signed" || s == "dr_cr" }
func validNegStyle(s string) bool     { return s == "minus" || s == "parens" }

// ErrInvalidSetting is returned by UpdateUserSettings when any field carries a
// value outside its known vocabulary (locale/date/number/display/neg/theme) or the
// requested default subsidiary does not exist. The web layer maps it to a 422 form
// error (a bad settings POST is a client error, only reachable by a crafted request
// since the form offers fixed <select> options).
var ErrInvalidSetting = errors.New("store: invalid setting value")

// UserSettingsInput is the desired state of a user's personal preferences (p13.1
// /settings). DefaultSubsidiaryID is nil to CLEAR the preference (a user may have
// none); a non-nil id must reference a real subsidiary. Locale must be a known
// catalog language (validated by the caller against i18n.Langs, passed as
// localeOK) -- the store stays free of an i18n import while still rejecting an
// unknown locale.
type UserSettingsInput struct {
	Locale              string
	DateFormat          string
	NumberFormat        string
	DisplayMode         string
	NegStyle            string
	Theme               string
	DefaultSubsidiaryID *int64
}

// UpdateUserSettings persists a user's personal preferences on the live row and
// appends an op='update' users_versions row under ONE change (p13.1 POST
// /settings), so the audit trail records who changed which settings. Every field
// is validated against its known vocabulary FIRST (rejecting unknowns with
// ErrInvalidSetting) since the columns carry no DB CHECK; localeOK is the caller's
// i18n.Langs membership test (keeping the store i18n-free). A non-nil
// DefaultSubsidiaryID must reference an existing subsidiary (existence-checked so a
// bad id is a clean 422, not an FK-triggered 500); nil clears it (NULL). The
// version append is the same snapshot-from-live query CreateUser uses, so all seven
// columns are captured.
func (s *Store) UpdateUserSettings(ctx context.Context, userID int64, in UserSettingsInput, localeOK func(string) bool) error {
	if !localeOK(in.Locale) ||
		!validDateFormat(in.DateFormat) ||
		!validNumberFormat(in.NumberFormat) ||
		!validDisplayMode(in.DisplayMode) ||
		!validNegStyle(in.NegStyle) ||
		!ValidTheme(in.Theme) {
		return ErrInvalidSetting
	}

	if in.DefaultSubsidiaryID != nil {
		if _, err := s.q.GetSubsidiary(ctx, *in.DefaultSubsidiaryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidSetting
			}
			return fmt.Errorf("verify default subsidiary %d: %w", *in.DefaultSubsidiaryID, err)
		}
	}

	_, err := s.write(ctx, "user.settings", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := q.UpdateUserSettings(ctx, sqlc.UpdateUserSettingsParams{
				Locale:              in.Locale,
				DateFormat:          in.DateFormat,
				NumberFormat:        in.NumberFormat,
				DisplayMode:         in.DisplayMode,
				NegStyle:            in.NegStyle,
				Theme:               in.Theme,
				DefaultSubsidiaryID: nullInt64Ptr(in.DefaultSubsidiaryID),
				ID:                  userID,
			}); err != nil {
				return fmt.Errorf("update settings: %w", err)
			}
			return insertUserVersion(ctx, q, changeID, "update", userID)
		})
	if err != nil {
		return fmt.Errorf("update user settings (id %d): %w", userID, err)
	}
	return nil
}

// SetUserTheme persists a user's theme preference on the live row and appends an
// op='update' users_versions row under ONE change (p10.2 POST /theme). theme is
// validated against ValidTheme first so a bad value never reaches the db. The
// version append is the same snapshot-from-live query CreateUser uses (theme IS
// in the snapshot), so the change is audited.
func (s *Store) SetUserTheme(ctx context.Context, userID int64, theme string) error {
	if !ValidTheme(theme) {
		return ErrInvalidTheme
	}
	_, err := s.write(ctx, "user.theme", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := q.SetUserTheme(ctx, sqlc.SetUserThemeParams{Theme: theme, ID: userID}); err != nil {
				return fmt.Errorf("set theme: %w", err)
			}
			return insertUserVersion(ctx, q, changeID, "update", userID)
		})
	if err != nil {
		return fmt.Errorf("set user theme (id %d): %w", userID, err)
	}
	return nil
}

// insertUserVersion appends the users snapshot-from-live version row. It hides
// the generated positional-param names (ID=change_id, ID_2=entity_id) behind one
// call site. It MUST run after the live insert so the snapshot captures the new
// row -- and by design the query omits password_hash (rule 5).
func insertUserVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertUserVersion(ctx, sqlc.InsertUserVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append user version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}
