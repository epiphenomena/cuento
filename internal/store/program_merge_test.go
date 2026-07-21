package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
	"cuento/internal/ledger"
	"cuento/internal/testutil"
)

// Program merge tests (p11.5b). These mirror the account-merge tests (merge_test.go):
// they build tiny bespoke setups inline through the store, post R/E splits tagged with
// a SOURCE program, then exercise MergeProgram and assert the repoint + versioning +
// deactivation + ledger cleanliness.

// liveSplitProgram reads one split's current live program_id.
func liveSplitProgram(t *testing.T, d *sql.DB, splitID ids.SplitID) ids.ProgramID {
	t.Helper()
	var pid int64
	if err := d.QueryRow(`SELECT program_id FROM splits WHERE id = ?`, int64(splitID)).Scan(&pid); err != nil {
		t.Fatalf("liveSplitProgram(%d): %v", splitID, err)
	}
	return ids.ProgramID(pid)
}

// countSplitsOnProgram counts live splits currently tagged with a program.
func countSplitsOnProgram(t *testing.T, d *sql.DB, programID ids.ProgramID) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM splits WHERE program_id = ?`, int64(programID)).Scan(&n); err != nil {
		t.Fatalf("countSplitsOnProgram(%d): %v", programID, err)
	}
	return n
}

// programActiveFlag reads a program's live active flag.
func programActiveFlag(t *testing.T, d *sql.DB, programID ids.ProgramID) int64 {
	t.Helper()
	var a int64
	if err := d.QueryRow(`SELECT active FROM programs WHERE id = ?`, int64(programID)).Scan(&a); err != nil {
		t.Fatalf("programActiveFlag(%d): %v", programID, err)
	}
	return a
}

// programParentID reads a program's live parent_id (0 when NULL/root).
func programParentID(t *testing.T, d *sql.DB, programID ids.ProgramID) int64 {
	t.Helper()
	var p sql.NullInt64
	if err := d.QueryRow(`SELECT parent_id FROM programs WHERE id = ?`, int64(programID)).Scan(&p); err != nil {
		t.Fatalf("programParentID(%d): %v", programID, err)
	}
	if !p.Valid {
		return 0
	}
	return p.Int64
}

// mkProgram creates a child program under parent and returns its id.
func mkProgram(t *testing.T, s *Store, parent ids.ProgramID, name string) ids.ProgramID {
	t.Helper()
	id, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: parent, Name: name})
	if err != nil {
		t.Fatalf("CreateProgram(%s): %v", name, err)
	}
	return id
}

// postExpenseProg posts a balanced expense txn (debit `expense`, credit checking)
// tagging the expense split with the given program, and returns the txn id + splits.
func (e txnEnv) postExpenseProg(t *testing.T, expense ids.AccountID, prog ids.ProgramID, amount int64) (ids.TransactionID, []SplitState) {
	t.Helper()
	mgmt := "management"
	p := prog
	id, err := e.s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: expense, Amount: amount, ProgramID: &p, FunctionalClass: &mgmt, Position: 0},
			{AccountID: e.checking, Amount: -amount, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	return id, txnSplits(t, e.d, id)
}

// splitOnProgram returns the one split (from a list) tagged with program prog.
func splitOnProgram(t *testing.T, sps []SplitState, prog ids.ProgramID) SplitState {
	t.Helper()
	for _, sp := range sps {
		if sp.ProgramID.Valid && ids.ProgramID(sp.ProgramID.Int64) == prog {
			return sp
		}
	}
	t.Fatalf("no split on program %d", prog)
	return SplitState{}
}

// --- happy path: repointing splits + reparenting children + versioning ------

func TestMergeProgramRepointsSplitsChildrenGrants(t *testing.T) {
	e := newTxnEnv(t)
	// src and dst: two children of root. src also has a child (grandchild under root)
	// to prove the child reparents onto dst. supplies is an expense leaf with no default
	// program, so the split's program is exactly what we tag.
	src := mkProgram(t, e.s, rootProgramID, "Src Program")
	dst := mkProgram(t, e.s, rootProgramID, "Dst Program")
	srcKid := mkProgram(t, e.s, src, "Src Kid")

	// A report grant scoped to src (so the merge re-scopes it to dst).
	if err := e.s.SyncReportGroups(context.Background(), []string{"reports_x"}); err != nil {
		t.Fatalf("SyncReportGroups: %v", err)
	}
	uid, err := e.s.CreateUser(mutCtx(), CreateUserInput{Username: "grantee", DisplayName: "Grantee"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := e.s.GrantReportGroup(mutCtx(), uid, "reports_x", &src); err != nil {
		t.Fatalf("GrantReportGroup: %v", err)
	}

	// Two txns tag src, so there are two src splits to move.
	_, sps1 := e.postExpenseProg(t, e.supplies, src, 10_000)
	_, sps2 := e.postExpenseProg(t, e.supplies, src, 25_000)
	srcSplitA := splitOnProgram(t, sps1, src)
	srcSplitB := splitOnProgram(t, sps2, src)

	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(mutCtx(), src, dst); err != nil {
		t.Fatalf("MergeProgram: %v", err)
	}

	// Exactly ONE new change anchors the whole merge.
	if n := countChanges(t, e.d); n != before+1 {
		t.Fatalf("changes = %d, want %d (one change for the merge)", n, before+1)
	}

	// Every src split now lives on dst; src has no live splits left.
	for _, id := range []ids.SplitID{srcSplitA.ID, srcSplitB.ID} {
		if got := liveSplitProgram(t, e.d, id); got != dst {
			t.Errorf("split %d program = %d, want dst %d", id, got, dst)
		}
	}
	if n := countSplitsOnProgram(t, e.d, src); n != 0 {
		t.Errorf("src still has %d live splits, want 0", n)
	}

	// Each moved split got an op='update' version row (2 rows total: create + update).
	for _, id := range []ids.SplitID{srcSplitA.ID, srcSplitB.ID} {
		testutil.AssertVersioned(t, e.d, "splits", int64(id), "update")
		if n := splitVersionCount(t, e.d, id); n != 2 {
			t.Errorf("split %d version count = %d, want 2 (create + merge update)", id, n)
		}
	}

	// src's child reparented onto dst (versioned op='update').
	if got := programParentID(t, e.d, srcKid); got != int64(dst) {
		t.Errorf("src kid parent = %d, want dst %d (reparented)", got, dst)
	}
	testutil.AssertVersioned(t, e.d, "programs", int64(srcKid), "update")

	// The grant re-scoped from src to dst.
	scope, err := e.s.q.GetReportGrantScope(context.Background(), sqlc.GetReportGrantScopeParams{UserID: uid, GroupName: "reports_x"})
	if err != nil {
		t.Fatalf("GetReportGrantScope: %v", err)
	}
	if !scope.Valid || ids.ProgramID(scope.Int64) != dst {
		t.Errorf("grant scope = %+v, want dst %d (re-scoped)", scope, dst)
	}

	// src is deactivated (active=0, op='update').
	if a := programActiveFlag(t, e.d, src); a != 0 {
		t.Errorf("src active = %d, want 0 (source deactivated)", a)
	}
	testutil.AssertVersioned(t, e.d, "programs", int64(src), "update")

	// The post-merge books stay integrity-clean (no Z3/Z15/Z16 violation).
	vs, err := ledger.Check(context.Background(), e.d)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("post-merge integrity error: %s: %s", v.Rule, v.Detail)
		}
	}
}

// --- validations (each Blocked test satisfies EVERY other constraint so only the
// target error can fire) -----------------------------------------------------

func TestMergeProgramRejectsSelf(t *testing.T) {
	e := newTxnEnv(t)
	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(mutCtx(), e.educ, e.educ); !errors.Is(err, ErrProgramMergeSelf) {
		t.Fatalf("err = %v, want ErrProgramMergeSelf", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

func TestMergeProgramRejectsRootSource(t *testing.T) {
	e := newTxnEnv(t)
	// Merging the root away would leave the tree rootless (Z16). dst is a real child.
	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(mutCtx(), rootProgramID, e.educ); !errors.Is(err, ErrProgramMergeRoot) {
		t.Fatalf("err = %v, want ErrProgramMergeRoot", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

func TestMergeProgramRejectsCycle(t *testing.T) {
	e := newTxnEnv(t)
	// dst is a DESCENDANT of src: merging src into its own descendant would form a
	// cycle when src's children reparent under dst.
	src := mkProgram(t, e.s, rootProgramID, "Cycle Src")
	dst := mkProgram(t, e.s, src, "Cycle Dst")
	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(mutCtx(), src, dst); !errors.Is(err, ErrCycle) {
		t.Fatalf("err = %v, want ErrCycle", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

func TestMergeProgramRejectsInactiveDestination(t *testing.T) {
	e := newTxnEnv(t)
	src := mkProgram(t, e.s, rootProgramID, "Live Src")
	dst := mkProgram(t, e.s, rootProgramID, "Dead Dst")
	if err := e.s.DeactivateProgram(mutCtx(), dst); err != nil {
		t.Fatalf("DeactivateProgram: %v", err)
	}
	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(mutCtx(), src, dst); !errors.Is(err, ErrProgramMergeIntoInactive) {
		t.Fatalf("err = %v, want ErrProgramMergeIntoInactive", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

// TestMergeProgramBlockedFundScoped proves the Z15b block-guard (D20): merging a
// source program whose splits are tagged a FUND scoped to src is REFUSED with
// ErrProgramMergeFundScoped when dst lies OUTSIDE that fund's program subtree (writing
// nothing), because repointing the split to dst would push it out of the fund scope.
// A fund scoped to an ANCESTOR of dst (here the root, which covers everything) lets the
// same merge succeed. Mirrors TestMergeBlockedSourceReconciled.
func TestMergeProgramBlockedFundScoped(t *testing.T) {
	e := newTxnEnv(t)
	ctx := mutCtx()

	// src + a sibling dst, both children of root. dst is NOT in subtree(src).
	src := mkProgram(t, e.s, rootProgramID, "Scoped Src")
	dst := mkProgram(t, e.s, rootProgramID, "Scoped Dst")

	// A fund scoped to SRC's subtree, on the US sub. A split tagged this fund must
	// carry a program inside subtree(src); tagging src itself is valid.
	fund, err := e.s.CreateFund(ctx, CreateFundInput{
		Name: "Src Scoped Fund", Restriction: "purpose",
		ProgramID: &src, Subsidiaries: []ids.SubsidiaryID{e.subUS},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	mgmt := "management"
	p := src
	f := fund
	if _, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2025-04-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.supplies, Amount: 7_000, ProgramID: &p, FundID: &f, FunctionalClass: &mgmt, Position: 0},
			{AccountID: e.checking, Amount: -7_000, FundID: &f, Position: 1},
		},
	}); err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// Merge src -> dst is REFUSED: the fund's scope (subtree(src)) does not cover dst.
	before := countChanges(t, e.d)
	if err := e.s.MergeProgram(ctx, src, dst); !errors.Is(err, ErrProgramMergeFundScoped) {
		t.Fatalf("MergeProgram: err = %v, want ErrProgramMergeFundScoped", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
	if a := programActiveFlag(t, e.d, src); a != 1 {
		t.Errorf("src active = %d, want 1 (not deactivated)", a)
	}
	// The books stay integrity-clean -- the merge did not create a Z15b hole.
	vs, err := ledger.Check(context.Background(), e.d)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("post-reject integrity error: %s: %s", v.Rule, v.Detail)
		}
	}

	// A fund scoped to the ROOT (covers every program, incl. dst) does NOT block the
	// merge. Widen the fund's scope to the root, then the same merge SUCCEEDS.
	rootP := rootProgramID
	if err := e.s.UpdateFund(ctx, fund, UpdateFundInput{ProgramID: &rootP}); err != nil {
		t.Fatalf("UpdateFund widen scope: %v", err)
	}
	if err := e.s.MergeProgram(ctx, src, dst); err != nil {
		t.Fatalf("MergeProgram after widening fund scope: %v", err)
	}
	if n := countSplitsOnProgram(t, e.d, src); n != 0 {
		t.Errorf("src still has %d live splits, want 0 (merge ran)", n)
	}
	vs, err = ledger.Check(context.Background(), e.d)
	if err != nil {
		t.Fatalf("ledger.Check after merge: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("post-merge integrity error: %s: %s", v.Rule, v.Detail)
		}
	}
}

// --- dst untouched -----------------------------------------------------------

func TestMergeProgramDestinationUntouched(t *testing.T) {
	e := newTxnEnv(t)
	src := mkProgram(t, e.s, rootProgramID, "Untouched Src")
	dst := mkProgram(t, e.s, rootProgramID, "Untouched Dst")
	e.postExpenseProg(t, e.supplies, src, 5_000)

	before, err := e.s.GetProgram(mutCtx(), dst)
	if err != nil {
		t.Fatalf("GetProgram(dst) before: %v", err)
	}
	if err := e.s.MergeProgram(mutCtx(), src, dst); err != nil {
		t.Fatalf("MergeProgram: %v", err)
	}
	after, err := e.s.GetProgram(mutCtx(), dst)
	if err != nil {
		t.Fatalf("GetProgram(dst) after: %v", err)
	}
	if before != after {
		t.Errorf("dst row changed: before %+v after %+v", before, after)
	}
	if after.Active != 1 {
		t.Errorf("dst active = %d, want 1 (dst untouched)", after.Active)
	}
}
