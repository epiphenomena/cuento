package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
				return fmt.Errorf("insert user: %w", err)
			}
			newID = id

			return insertUserVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return newID, nil
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

// DisableUser marks a user disabled (disabled_at = now) on the live row and
// appends an op='update' users_versions row under ONE change (p06.4 `user
// disable`). A disabled user cannot log in (the login handler enforces this).
// Unlike password_hash, disabled_at IS part of the snapshot, so the audit trail
// records who was disabled and when.
func (s *Store) DisableUser(ctx context.Context, userID int64) error {
	_, err := s.write(ctx, "user.disable", "",
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
	return CurrentUser{
		ID:       row.ID,
		Username: row.Username,
		Disabled: row.DisabledAt.Valid,
		TxnPerm:  row.TxnPerm,
		IsAdmin:  row.IsAdmin != 0,
		Locale:   row.Locale,
	}, nil
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
