package store

import (
	"context"
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
