package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p11.5 programs management (/programs). These handlers are driven through the
// REAL mounted router (httptest) over a real migrated db (AGENTS testing
// conventions) -- no handler-level store mocks. They reuse the shared web-package
// test helpers (accountsApp, asUser, mkUser, itoa) established by p11.1; this file
// only adds program-specific cases.
//
// Programs are a dimension (D24): a single seeded root ("General", id 1) exists
// from the seed, so every created program is a child; the root is immovable and no
// second root may be created. Unlike subsidiaries (all Admin), programs are
// BOOKKEEPING: viewing is TxnRead, managing is TxnWrite.

// progByName returns the id of the program with the given name, or 0.
func progByName(t *testing.T, st *store.Store, name string) ids.ProgramID {
	t.Helper()
	rows, err := st.ProgramTree(context.Background())
	if err != nil {
		t.Fatalf("ProgramTree: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return 0
}

// TestProgramsPageRenders: GET /programs (TxnRead) renders the tree list
// including the seeded root program.
func TestProgramsPageRenders(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	rec := asUser(t, h, sm, book, http.MethodGet, "/programs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /programs as bookkeeper: status=%d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "General") {
		t.Errorf("page does not list the root program; body: %s", rec.Body.String())
	}
}

// TestProgramsCreateChild: a Bookkeeper (TxnWrite) creates a child program; it
// appears in the tree under the given parent.
func TestProgramsCreateChild(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	form := url.Values{}
	form.Set("name", "Outreach")
	form.Set("parent_id", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs", form)
	if rec.Code >= 400 {
		t.Fatalf("create child returned %d, body: %s", rec.Code, rec.Body.String())
	}
	id := progByName(t, st, "Outreach")
	if id == 0 {
		t.Fatalf("created program not found; body: %s", rec.Body.String())
	}
	prog, err := st.GetProgram(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProgram: %v", err)
	}
	if !prog.ParentID.Valid || prog.ParentID.Int64 != 1 {
		t.Errorf("parent = %v, want 1", prog.ParentID)
	}
}

// TestProgramsRename: editing a program's name changes it in the tree.
func TestProgramsRename(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Old Name"})
	if err != nil {
		t.Fatalf("seed program: %v", err)
	}

	form := url.Values{}
	form.Set("name", "New Name")
	form.Set("parent_id", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/"+itoa(int64(id)), form)
	if rec.Code >= 400 {
		t.Fatalf("rename returned %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := progByName(t, st, "New Name"); got != id {
		t.Errorf("renamed program not found (want id %d); got %d", id, got)
	}
}

// TestProgramsMove: editing a program's parent moves it under a new parent.
func TestProgramsMove(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	a, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "A"})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	b, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "B"})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Move B under A.
	form := url.Values{}
	form.Set("name", "B")
	form.Set("parent_id", itoa(int64(a)))

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/"+itoa(int64(b)), form)
	if rec.Code >= 400 {
		t.Fatalf("move returned %d, body: %s", rec.Code, rec.Body.String())
	}
	prog, _ := st.GetProgram(context.Background(), b)
	if !prog.ParentID.Valid || prog.ParentID.Int64 != int64(a) {
		t.Errorf("B parent = %v, want %d", prog.ParentID, a)
	}
}

// TestProgramsMoveOptionsExcludeDescendants: the edit form's parent select omits
// the subject AND its descendants, so a cycle is never offered (like the accounts
// parent-filter test). Asserted on buildProgramForm's option list directly.
func TestProgramsMoveOptionsExcludeDescendants(t *testing.T) {
	_, st, _ := accountsApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// General(1) -> parent -> child
	parent, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Parent"})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	child, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: parent, Name: "Child"})
	if err != nil {
		t.Fatalf("seed child: %v", err)
	}

	app := &server{store: st}
	form, err := app.buildProgramForm(ctx, parent)
	if err != nil {
		t.Fatalf("buildProgramForm: %v", err)
	}
	for _, opt := range form.Parents {
		if opt.ID == int64(parent) {
			t.Errorf("parent options include the subject %d", parent)
		}
		if opt.ID == int64(child) {
			t.Errorf("parent options include descendant %d", child)
		}
	}
	// The root (General) must still be offered as a valid target.
	sawRoot := false
	for _, opt := range form.Parents {
		if opt.ID == 1 {
			sawRoot = true
		}
	}
	if !sawRoot {
		t.Errorf("parent options omit the root program (id 1)")
	}
}

// TestProgramsSecondRootRejected: creating a program with no parent yields the
// localized second-root message (the store rejects it; the handler translates).
func TestProgramsSecondRootRejected(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	form := url.Values{}
	form.Set("name", "Rogue Root")
	form.Set("parent_id", "0")

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second-root create: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	want := "A program must have a parent; there is already a root."
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body missing localized second-root message %q; body: %s", want, rec.Body.String())
	}
}

// TestProgramsRootImmovable: giving the root program a parent is rejected with the
// localized root-immovable message.
func TestProgramsRootImmovable(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	child, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Child"})
	if err != nil {
		t.Fatalf("seed child: %v", err)
	}

	// POST an update to the ROOT (id 1) asking to move it under child.
	form := url.Values{}
	form.Set("name", "General")
	form.Set("parent_id", itoa(int64(child)))

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/1", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("root move: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	want := "The root program cannot be given a parent."
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body missing localized root-immovable message %q; body: %s", want, rec.Body.String())
	}
}

// TestProgramsDeactivateChildless: deactivating a childless program flips its
// active flag (history intact -- op is update, not delete).
func TestProgramsDeactivateChildless(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Temp"})
	if err != nil {
		t.Fatalf("seed program: %v", err)
	}

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/"+itoa(int64(id))+"/deactivate", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("deactivate returned %d, body: %s", rec.Code, rec.Body.String())
	}
	prog, _ := st.GetProgram(context.Background(), id)
	if prog.Active != 0 {
		t.Errorf("program %d still active after deactivate", id)
	}
}

// TestProgramsDeactivateBlockedWithActiveChildren: deactivating a program with an
// active child is BLOCKED; the list re-renders at 422 with the localized message
// and the program stays active (the guard is shown; nothing executed).
func TestProgramsDeactivateBlockedWithActiveChildren(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	parent, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Parent"})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if _, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: parent, Name: "Child"}); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	rec := asUser(t, h, sm, book, http.MethodPost, "/programs/"+itoa(int64(parent))+"/deactivate", url.Values{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blocked deactivate: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	want := "This program still has active children and cannot be deactivated."
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body missing localized blocked message %q; body: %s", want, rec.Body.String())
	}
	prog, _ := st.GetProgram(context.Background(), parent)
	if prog.Active != 1 {
		t.Errorf("program %d deactivated despite active child", parent)
	}
}

// TestProgramsMatrixPersonas: /programs GET is TxnRead, mutations are TxnWrite
// (D24: program structure is bookkeeping). ReadOnly and Bookkeeper both view;
// ReadOnly cannot create/edit; anon is bounced to login. (The auto-matrix also
// covers this; this asserts the key personas.)
func TestProgramsMatrixPersonas(t *testing.T) {
	h, st, sm := accountsApp(t)
	read := mkUser(t, st, "ro", "read", false)
	book := mkUser(t, st, "book", "write", false)

	// anon -> login redirect.
	rec := asUser(t, h, sm, 0, http.MethodGet, "/programs", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon GET /programs: status=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
	// ReadOnly (read) can VIEW.
	rec = asUser(t, h, sm, read, http.MethodGet, "/programs", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("ReadOnly GET /programs: status=%d, want 200", rec.Code)
	}
	// Bookkeeper (write) can VIEW.
	rec = asUser(t, h, sm, book, http.MethodGet, "/programs", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("Bookkeeper GET /programs: status=%d, want 200", rec.Code)
	}
	// ReadOnly (read, not write) cannot CREATE -> 403.
	form := url.Values{}
	form.Set("name", "X")
	form.Set("parent_id", "1")
	rec = asUser(t, h, sm, read, http.MethodPost, "/programs", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ReadOnly POST /programs: status=%d, want 403", rec.Code)
	}
	// Bookkeeper (write) CAN create -> not 403.
	rec = asUser(t, h, sm, book, http.MethodPost, "/programs", form)
	if rec.Code == http.StatusForbidden {
		t.Errorf("Bookkeeper POST /programs: status=403, want allowed")
	}
}

// TestProgramActivityTotalsMatchQuery: the per-program R/E activity totals the page
// renders equal exactly the p08.4 ProgramActivity output for the same period and
// scope, aggregated flat per (program, currency). The assembly is exposed as
// programActivityTotals(from, to, scopeSub) so this asserts on the data structure,
// not scraped HTML (mirrors TestBalancesColumnMatchesQuery).
func TestProgramActivityTotalsMatchQuery(t *testing.T) {
	_, st, _ := accountsApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A revenue account tagged with a program, and a cash side, so ProgramActivity
	// has a non-empty per-program result.
	prog, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Outreach"})
	if err != nil {
		t.Fatalf("create program: %v", err)
	}
	cash, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	rev, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: map[string]string{"en": "Donations"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: cash, Amount: 50000},
			{AccountID: rev, Amount: -50000, ProgramID: &prog},
		},
	}); err != nil {
		t.Fatalf("post txn: %v", err)
	}

	const from, to = "2025-01-01", "2025-12-31"

	// Expected: flat aggregate of ProgramActivity by (program, currency).
	cells, err := st.ProgramActivity(ctx, from, to, 1)
	if err != nil {
		t.Fatalf("ProgramActivity: %v", err)
	}
	wantMap := map[[2]string]int64{}
	for _, c := range cells {
		wantMap[[2]string{itoa(int64(c.ProgramID)), c.Currency}] += c.Amount
	}

	got, err := programActivityTotals(ctx, st, from, to, 1)
	if err != nil {
		t.Fatalf("programActivityTotals: %v", err)
	}
	gotMap := map[[2]string]int64{}
	for pid, list := range got {
		for _, cell := range list {
			gotMap[[2]string{itoa(int64(pid)), cell.Currency}] += cell.Minor
		}
	}

	if len(gotMap) != len(wantMap) {
		t.Fatalf("program total cell count = %d, want %d", len(gotMap), len(wantMap))
	}
	for k, v := range wantMap {
		if gotMap[k] != v {
			t.Errorf("program total[%v] = %d, want %d", k, gotMap[k], v)
		}
	}
}
