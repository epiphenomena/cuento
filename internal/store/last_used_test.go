package store

import (
	"context"
	"testing"

	"cuento/internal/ids"
)

// p26.37 last-used header account. The prefill for a NEW transaction opened from the
// top nav: the position-0 split account of the actor's most-recently-ENTERED (by create
// change id, not business date) non-deleted transaction, scoped to that actor.

// actorCtx returns a mutation context tagged with the given actor id (mutCtx uses id 1).
func actorCtx(id ids.UserID) context.Context {
	return WithActor(context.Background(), Actor{ID: id})
}

// postHeaderTxn posts a balanced 2-split txn whose POSITION-0 (header) split is `header`
// and body split is `body`, on `date`, as the given actor. Returns the transaction id.
func (e txnEnv) postHeaderTxn(t *testing.T, actor ids.UserID, date string, header, body, amount int64) int64 {
	t.Helper()
	in := PostTransactionInput{
		Date: date, SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: header, Amount: -amount, Position: 0},
			{AccountID: body, Amount: amount, Position: 1},
		},
	}
	id, err := e.s.PostTransaction(actorCtx(actor), in)
	if err != nil {
		t.Fatalf("post header txn: %v", err)
	}
	return id
}

func TestLastHeaderAccountForActor(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	// Two real users (the changes.actor_id FK requires them); user 1 is the seeded system.
	alice, err := s.CreateUser(mutCtx(), CreateUserInput{Username: "alice", DisplayName: "Alice", TxnPerm: "write"})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := s.CreateUser(mutCtx(), CreateUserInput{Username: "bob", DisplayName: "Bob", TxnPerm: "write"})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	// No prior transaction for alice -> 0 (leave the header blank).
	if got, err := s.LastHeaderAccountForActor(context.Background(), alice); err != nil || got != 0 {
		t.Fatalf("empty actor = %d err %v, want 0 nil", got, err)
	}
	// Actor id 0 short-circuits to 0.
	if got, err := s.LastHeaderAccountForActor(context.Background(), 0); err != nil || got != 0 {
		t.Fatalf("actor 0 = %d err %v, want 0 nil", got, err)
	}

	// Alice enters two transactions: an EARLIER-dated one with a LATER create (so recency
	// must be by create order, not business date). The header account of the last-created
	// transaction (checking) must win over the first-created one (credit).
	e.postHeaderTxn(t, alice, "2025-05-01", e.credit, e.salaries, 3000)   // created FIRST, later date
	e.postHeaderTxn(t, alice, "2025-01-01", e.checking, e.salaries, 4000) // created SECOND, earlier date

	got, err := s.LastHeaderAccountForActor(context.Background(), alice)
	if err != nil {
		t.Fatalf("LastHeaderAccountForActor: %v", err)
	}
	if got != e.checking {
		t.Fatalf("last header account = %d, want checking %d (most-recently-CREATED wins over later date)", got, e.checking)
	}

	// Scoped per actor: bob's transaction does not leak into alice's lookup. Bob enters one
	// with a different header (credit); alice's lookup is unchanged, bob sees credit.
	e.postHeaderTxn(t, bob, "2025-06-01", e.credit, e.salaries, 2000)
	if got, err := s.LastHeaderAccountForActor(context.Background(), alice); err != nil || got != e.checking {
		t.Fatalf("alice after bob posted = %d err %v, want checking %d", got, err, e.checking)
	}
	if got, err := s.LastHeaderAccountForActor(context.Background(), bob); err != nil || got != e.credit {
		t.Fatalf("bob = %d err %v, want credit %d", got, err, e.credit)
	}

	// A soft-deleted transaction does not count. Delete bob's only txn -> back to 0.
	id := e.postHeaderTxn(t, bob, "2025-07-01", e.checking, e.salaries, 1000)
	if err := s.DeleteTransaction(actorCtx(bob), id); err != nil {
		t.Fatalf("DeleteTransaction: %v", err)
	}
	// Bob's remaining most-recent non-deleted txn is the earlier credit-header one.
	if got, err := s.LastHeaderAccountForActor(context.Background(), bob); err != nil || got != e.credit {
		t.Fatalf("bob after delete = %d err %v, want credit %d (deleted txn excluded)", got, err, e.credit)
	}
}
