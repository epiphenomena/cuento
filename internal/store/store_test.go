package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"cuento/internal/db/sqlc"
	"cuento/internal/testutil"
)

// countChanges reads the changes-table row count directly through sqlc — the
// funnel's audit trail is what these tests assert on. Using the generated query
// (not raw SQL) keeps the test honest to rule 6's read path.
func countChanges(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	n, err := sqlc.New(d).CountChanges(context.Background())
	if err != nil {
		t.Fatalf("CountChanges: %v", err)
	}
	return n
}

// TestWriteRequiresActor proves the actor check happens BEFORE any transaction
// begins: a ctx with no actor returns the typed ErrNoActor and writes nothing
// (the changes count stays at zero). This is the security spine of the funnel —
// no anonymous mutation is representable.
func TestWriteRequiresActor(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	fnCalled := false
	_, err := s.write(context.Background(), "test", "note",
		func(_ context.Context, _ *sqlc.Queries, _ int64) error {
			fnCalled = true
			return nil
		})
	if !errors.Is(err, ErrNoActor) {
		t.Fatalf("write without actor: err = %v, want ErrNoActor", err)
	}
	if fnCalled {
		t.Error("fn ran despite missing actor; the check must precede the tx")
	}
	if n := countChanges(t, d); n != 0 {
		t.Errorf("changes count = %d, want 0 (nothing written)", n)
	}
}

// TestWriteRecordsChange proves one funnel call with a valid actor inserts
// exactly one changes row carrying the right actor_id, kind, note and a
// parseable RFC3339Nano timestamp.
func TestWriteRecordsChange(t *testing.T) {
	d := testutil.NewDB(t)
	fixed := time.Date(2026, 7, 11, 12, 34, 56, 789_000_000, time.UTC)
	s := New(d, WithClock(func() time.Time { return fixed }))

	ctx := WithActor(context.Background(), Actor{ID: 1})
	changeID, err := s.write(ctx, "create.thing", "a note",
		func(_ context.Context, _ *sqlc.Queries, _ int64) error { return nil })
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if changeID <= 0 {
		t.Fatalf("write returned changeID %d, want a positive id", changeID)
	}
	if n := countChanges(t, d); n != 1 {
		t.Fatalf("changes count = %d, want exactly 1", n)
	}

	c, err := sqlc.New(d).GetChange(ctx, changeID)
	if err != nil {
		t.Fatalf("GetChange(%d): %v", changeID, err)
	}
	if c.ActorID != 1 {
		t.Errorf("actor_id = %d, want 1", c.ActorID)
	}
	if c.Kind != "create.thing" {
		t.Errorf("kind = %q, want %q", c.Kind, "create.thing")
	}
	if !c.Note.Valid || c.Note.String != "a note" {
		t.Errorf("note = %+v, want %q", c.Note, "a note")
	}
	at, err := time.Parse(time.RFC3339Nano, c.At)
	if err != nil {
		t.Fatalf("at %q not RFC3339Nano-parseable: %v", c.At, err)
	}
	if !at.Equal(fixed) {
		t.Errorf("at = %v, want %v", at, fixed)
	}
}

// TestWriteAtomicRollback proves the change-row insert and fn's own writes share
// one transaction that rolls back on fn error: fn writes a second changes row
// and then fails, yet zero changes rows survive.
func TestWriteAtomicRollback(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	sentinel := errors.New("boom")
	ctx := WithActor(context.Background(), Actor{ID: 1})
	_, err := s.write(ctx, "test", "",
		func(fnCtx context.Context, q *sqlc.Queries, changeID int64) error {
			// A real caller's live-table write happens here on the same tx-bound
			// q. Insert a side effect, then fail: the deferred rollback must undo
			// both this and the funnel's own change row.
			if changeID <= 0 {
				t.Errorf("fn got changeID %d, want positive", changeID)
			}
			if _, e := q.InsertChange(fnCtx, sqlc.InsertChangeParams{
				ActorID: 1,
				At:      s.now().Format(time.RFC3339Nano),
				Kind:    "side.effect",
			}); e != nil {
				t.Fatalf("fn InsertChange: %v", e)
			}
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("write err = %v, want the sentinel from fn", err)
	}
	if n := countChanges(t, d); n != 0 {
		t.Errorf("changes count = %d, want 0 (whole tx rolled back)", n)
	}
}
