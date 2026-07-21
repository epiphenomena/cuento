package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p11.5b merge-programs handler tests. They drive the REAL mounted router (httptest)
// against a real migrated db (AGENTS testing conventions) -- the store is never
// mocked. Mirrors merge_test.go (the account-merge web tests):
//   - GET  /programs/merge   -> the merge form partial (source/destination selects)
//   - POST /programs/merge   -> WITHOUT confirm: a consequences PREVIEW that does NOT
//                               execute; WITH confirm=1: the merge.
// Typed store errors surface as localized validation messages (422 + partial),
// reusing the p10.3 form-error convention.

// progMergeEnv seeds two child programs plus a balanced transaction whose R/E split is
// tagged with the SOURCE program, so a merge has exactly one split to repoint.
type progMergeEnv struct {
	h    http.Handler
	st   *store.Store
	sm   *scs.SessionManager
	book ids.UserID
	src  ids.ProgramID
	dst  ids.ProgramID
}

func seedProgMergeEnv(t *testing.T) progMergeEnv {
	t.Helper()
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Two child programs under the seeded root (id 1).
	src, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Src Prog"})
	if err != nil {
		t.Fatalf("create src program: %v", err)
	}
	dst, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Dst Prog"})
	if err != nil {
		t.Fatalf("create dst program: %v", err)
	}

	// An expense leaf (R/E, so its splits carry a program) + a cash asset, both on
	// root sub 1. A balanced txn tags the expense split with src.
	mgmt := "management"
	exp, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Supplies"},
		Subsidiaries: []ids.SubsidiaryID{1}, FunctionalClass: &mgmt,
	})
	if err != nil {
		t.Fatalf("create expense: %v", err)
	}
	cash, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	p := src
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-02-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: exp, Amount: 5000, ProgramID: &p, FunctionalClass: &mgmt, Position: 0},
			{AccountID: cash, Amount: -5000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post txn: %v", err)
	}

	// Sanity: exactly one split sits on src.
	if n, err := st.SplitCountForProgram(context.Background(), src); err != nil || n != 1 {
		t.Fatalf("SplitCountForProgram src: n=%d err=%v (want 1)", n, err)
	}
	return progMergeEnv{h: h, st: st, sm: sm, book: book, src: src, dst: dst}
}

// TestProgramMergeHappyPath: a valid confirmed merge repoints the src split onto dst
// and deactivates src.
func TestProgramMergeHappyPath(t *testing.T) {
	e := seedProgMergeEnv(t)

	form := url.Values{}
	form.Set("src", itoa(int64(e.src)))
	form.Set("dst", itoa(int64(e.dst)))
	form.Set("confirm", "1")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/programs/merge", form)
	if rec.Code >= 400 {
		t.Fatalf("confirmed merge returned %d, body: %s", rec.Code, rec.Body.String())
	}
	// src has no live splits left; dst now carries it.
	if n, _ := e.st.SplitCountForProgram(context.Background(), e.src); n != 0 {
		t.Errorf("src still has %d live splits after merge, want 0", n)
	}
	if n, _ := e.st.SplitCountForProgram(context.Background(), e.dst); n != 1 {
		t.Errorf("dst has %d live splits after merge, want 1", n)
	}
	// src deactivated.
	prog, _ := e.st.GetProgram(context.Background(), e.src)
	if prog.Active != 0 {
		t.Errorf("src still active after merge")
	}
}

// TestProgramMergeConfirmRequired: a POST WITHOUT the confirm flag shows the
// consequences/confirmation and does NOT execute.
func TestProgramMergeConfirmRequired(t *testing.T) {
	e := seedProgMergeEnv(t)

	form := url.Values{}
	form.Set("src", itoa(int64(e.src)))
	form.Set("dst", itoa(int64(e.dst)))
	// no confirm flag

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/programs/merge", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// NOTHING executed: the split is still on src, src still active.
	if n, _ := e.st.SplitCountForProgram(context.Background(), e.src); n != 1 {
		t.Errorf("preview executed the merge: src has %d live splits, want 1", n)
	}
	prog, _ := e.st.GetProgram(context.Background(), e.src)
	if prog.Active == 0 {
		t.Errorf("preview deactivated src, want still active")
	}
	// The confirmation body must offer a confirm control + the split count.
	if !strings.Contains(rec.Body.String(), `name="confirm"`) {
		t.Errorf("preview body missing a confirm control; body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1 transaction line will move") {
		t.Errorf("preview does not surface the split count; body: %s", rec.Body.String())
	}
}

// TestProgramMergeCycleSurfaced: a confirmed merge into the source's own descendant
// returns the localized cycle message at 422 and does NOT execute.
func TestProgramMergeCycleSurfaced(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	src, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Cyc Src"})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	dst, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: src, Name: "Cyc Dst"})
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}

	form := url.Values{}
	form.Set("src", itoa(int64(src)))
	form.Set("dst", itoa(int64(dst)))
	form.Set("confirm", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/merge", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cycle merge status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "descendant") {
		t.Errorf("422 body missing the localized cycle message; body: %s", rec.Body.String())
	}
	// No execution: src still active.
	prog, _ := st.GetProgram(context.Background(), src)
	if prog.Active == 0 {
		t.Errorf("cycle merge executed (src deactivated), want no execution")
	}
}

// TestProgramMergePermissions: POST /programs/merge is TxnWrite; a ReadOnly user is
// forbidden and anon is bounced to login.
func TestProgramMergePermissions(t *testing.T) {
	h, st, sm := accountsApp(t)
	readOnly := mkUser(t, st, "ro", "read", false)

	form := url.Values{}
	form.Set("src", "2")
	form.Set("dst", "1")

	rec := asUser(t, h, sm, readOnly, http.MethodPost, "/programs/merge", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ReadOnly POST /programs/merge: status=%d, want 403", rec.Code)
	}
	// anon on the GET form -> login.
	rec = asUser(t, h, sm, 0, http.MethodGet, "/programs/merge", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon GET /programs/merge: status=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
}
