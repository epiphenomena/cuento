// Package store is cuento's single writer. Every database mutation in the whole
// program flows through the unexported write funnel (AGENTS rule 2): handlers,
// CLI, and reports never open a transaction or execute a write directly. The
// funnel binds one changes row (the audit anchor, rule 14) to the caller's live
// work in a single transaction, so an actor is recorded for every mutation and
// nothing is half-written.
//
// Versioning convention (AGENTS rule 5, D4). There is deliberately NO generic
// version-append helper here: a table-name-parameterized helper would need
// interpolated SQL, which rule 6 forbids in the query layer. Instead, every
// versioned business table gets its own sqlc InsertXVersion query, called inside
// the funnel's fn on the SAME tx-bound *sqlc.Queries and sharing the funnel's
// changeID (so the version row's change_id matches and valid_from = changes.at).
// The first concrete instance lands at p04.1 (subsidiaries); this step only
// establishes the funnel and the convention.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cuento/internal/db/sqlc"
)

// ErrNoActor is returned by the write funnel when the context carries no actor.
// It is checked BEFORE any transaction begins, so a missing actor writes
// nothing. Callers that branch on it (handlers turning it into 401/500) use
// errors.Is.
var ErrNoActor = errors.New("store: no actor in context")

// Actor identifies who is making a change. It holds only the user id the
// changes.actor_id foreign key needs — contexts carry the actor and nothing
// else (AGENTS Style). Speculative fields (name, roles) live elsewhere.
type Actor struct {
	ID int64
}

// actorKey is an unexported context-key type so no other package can collide
// with or read this context value by key.
type actorKey struct{}

// WithActor returns a child context carrying a for the write funnel to record.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom extracts the actor placed by WithActor. ok is false when absent.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// Store is the single writer. It holds the raw *sql.DB (needed to BeginTx) and a
// base *sqlc.Queries; each write binds a fresh tx-scoped Queries via WithTx.
type Store struct {
	db  *sql.DB
	q   *sqlc.Queries
	now func() time.Time
}

// Option configures a Store at construction.
type Option func(*Store)

// WithClock overrides the Store's time source (default time.Now). Injected so
// tests are deterministic and so timestamps stay controllable — changes.at is
// stored at sub-second (RFC3339Nano) precision precisely so rapid successive
// edits get distinct, orderable timestamps (a forward requirement of p08.2's
// as-of reconstruction between two quick edits).
func WithClock(clock func() time.Time) Option {
	return func(s *Store) { s.now = clock }
}

// New builds a Store over db.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{
		db:  db,
		q:   sqlc.New(db),
		now: time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// write is the single write funnel (AGENTS rule 2). In order it:
//
//  1. requires an actor in ctx, returning ErrNoActor BEFORE opening a tx so a
//     missing actor writes nothing;
//  2. begins a transaction (rolled back by a deferred no-op-after-commit call);
//  3. binds the tx to a sqlc.Queries — fn only ever touches sqlc queries, never
//     raw SQL, keeping rule 6 intact;
//  4. inserts the changes row (the audit anchor) at RFC3339Nano precision and
//     captures its id;
//  5. runs fn with that same tx-bound q and changeID, so the caller's live-table
//     write and its version append share one atomic unit and one change;
//  6. commits, returning the changeID (p04+ entity ops want it).
//
// An empty note is stored as SQL NULL (the column is nullable).
func (s *Store) write(
	ctx context.Context,
	kind, note string,
	fn func(ctx context.Context, q *sqlc.Queries, changeID int64) error,
) (int64, error) {
	actor, ok := ActorFrom(ctx)
	if !ok {
		return 0, ErrNoActor
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.write: begin: %w", err)
	}
	// Deferred rollback is a no-op once the tx has committed; on any error path
	// (including a panic) it undoes the change row and everything fn wrote.
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)

	changeID, err := q.InsertChange(ctx, sqlc.InsertChangeParams{
		ActorID: actor.ID,
		At:      s.now().Format(time.RFC3339Nano),
		Kind:    kind,
		Note:    nullString(note),
	})
	if err != nil {
		return 0, fmt.Errorf("store.write: insert change: %w", err)
	}

	if err := fn(ctx, q, changeID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.write: commit: %w", err)
	}
	return changeID, nil
}

// nullString maps "" to SQL NULL; a non-empty note is stored verbatim.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
