package sqlc_test

import (
	"context"
	"strings"
	"testing"

	"cuento/internal/db/sqlc"
	"cuento/internal/testutil"
)

// TestSystemUserSeeded proves the 00002 migration seeds the system user
// (id 1, username "system") and that sqlc types the minimal users table:
// GetUser round-trips the seeded row through the generated query layer.
func TestSystemUserSeeded(t *testing.T) {
	d := testutil.NewDB(t)
	q := sqlc.New(d)

	u, err := q.GetUser(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetUser(1): %v", err)
	}
	if u.Username != "system" {
		t.Errorf("system user Username = %q, want %q", u.Username, "system")
	}
	if u.DisplayName != "System" {
		t.Errorf("system user DisplayName = %q, want %q", u.DisplayName, "System")
	}
	if u.DisabledAt.Valid {
		t.Errorf("system user DisabledAt = %v, want NULL", u.DisabledAt)
	}
}

// TestCountUsersAfterSeed confirms the seed lands exactly one user (id 1).
func TestCountUsers(t *testing.T) {
	d := testutil.NewDB(t)
	q := sqlc.New(d)

	n, err := q.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 1 {
		t.Errorf("CountUsers = %d, want 1 (system user only)", n)
	}
}

// TestChangesRoundTrip inserts a changes row referencing the seeded system
// user and reads it back — proving the changes table exists, sqlc types it,
// and the FK-satisfied insert path works. note is nullable (Appendix A).
func TestChangesRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := testutil.NewDB(t)
	q := sqlc.New(d)

	id, err := q.InsertChange(ctx, sqlc.InsertChangeParams{
		ActorID: 1,
		At:      "2026-07-11T00:00:00Z",
		Kind:    "test",
	})
	if err != nil {
		t.Fatalf("InsertChange: %v", err)
	}

	c, err := q.GetChange(ctx, id)
	if err != nil {
		t.Fatalf("GetChange(%d): %v", id, err)
	}
	if c.ActorID != 1 {
		t.Errorf("change ActorID = %d, want 1", c.ActorID)
	}
	if c.Kind != "test" {
		t.Errorf("change Kind = %q, want %q", c.Kind, "test")
	}
	if c.Note.Valid {
		t.Errorf("change Note = %v, want NULL", c.Note)
	}
}

// TestChangesRequiresActor proves the FK from changes.actor_id to users(id) is
// enforced: inserting a changes row whose actor_id references no user fails
// (foreign_keys is ON via db.Open). The assertion is FK-specific — a generic
// err != nil would pass for the wrong reason (e.g. "no such table") before the
// migration exists; matching the FK constraint message keeps tests-first honest.
func TestChangesRequiresActor(t *testing.T) {
	d := testutil.NewDB(t)
	q := sqlc.New(d)

	_, err := q.InsertChange(context.Background(), sqlc.InsertChangeParams{
		ActorID: 999, // no such user
		At:      "2026-07-11T00:00:00Z",
		Kind:    "test",
	})
	if err == nil {
		t.Fatal("InsertChange with dangling actor_id succeeded, want FK violation")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("InsertChange error = %v, want a FOREIGN KEY constraint failure", err)
	}
}
