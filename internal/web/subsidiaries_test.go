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

// p11.3 subsidiaries admin (/admin/subsidiaries). These handlers are driven
// through the REAL mounted router (httptest) over a real migrated db (AGENTS
// testing conventions) -- no handler-level store mocks. They reuse the shared
// web-package test helpers (accountsApp, asUser, mkUser, itoa) established by
// p11.1; this file only adds subsidiary-specific cases.
//
// The single root subsidiary (id 1, "Organization", USD) exists from the seed,
// so every created subsidiary is a child; the root is immovable and no second
// root may be created.

// subByName returns the id of the subsidiary with the given name, or 0.
func subByName(t *testing.T, st *store.Store, name string) ids.SubsidiaryID {
	t.Helper()
	rows, err := st.SubTree(context.Background())
	if err != nil {
		t.Fatalf("SubTree: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return 0
}

// TestSubsidiariesPageRenders: GET /admin/subsidiaries (Admin) renders the tree
// list including the seeded root.
func TestSubsidiariesPageRenders(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/subsidiaries", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/subsidiaries as admin: status=%d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Organization") {
		t.Errorf("page does not list the root subsidiary; body: %s", rec.Body.String())
	}
}

// TestSubsidiariesCreateChild: an Admin creates a child subsidiary with a base
// currency; it appears in the tree with that currency and the given parent.
func TestSubsidiariesCreateChild(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	form := url.Values{}
	form.Set("name", "West Branch")
	form.Set("parent_id", "1")
	form.Set("base_currency", "MXN")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries", form)
	if rec.Code >= 400 {
		t.Fatalf("create child returned %d, body: %s", rec.Code, rec.Body.String())
	}

	id := subByName(t, st, "West Branch")
	if id == 0 {
		t.Fatalf("created subsidiary not found; body: %s", rec.Body.String())
	}
	sub, err := st.GetSubsidiary(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubsidiary: %v", err)
	}
	if sub.BaseCurrency != "MXN" {
		t.Errorf("base currency = %q, want MXN", sub.BaseCurrency)
	}
	if !sub.ParentID.Valid || sub.ParentID.Int64 != 1 {
		t.Errorf("parent = %v, want 1", sub.ParentID)
	}
}

// TestSubsidiariesRename: editing a subsidiary's name changes it in the tree.
func TestSubsidiariesRename(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Old Name", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	form := url.Values{}
	form.Set("name", "New Name")
	form.Set("parent_id", "1")
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(id)), form)
	if rec.Code >= 400 {
		t.Fatalf("rename returned %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := subByName(t, st, "New Name"); got != id {
		t.Errorf("renamed subsidiary not found (want id %d); got %d", id, got)
	}
}

// TestSubsidiariesSetDefaultAPAccount: an Admin sets a subsidiary's default AP
// account through the edit form (the submitted account is an active in-scope
// liability leaf); the live row records it.
func TestSubsidiariesSetDefaultAPAccount(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	sub, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Branch", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	ap, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "liability", DefaultCurrency: "USD",
		Names:        map[string]string{"en": "Accounts Payable", "es": "Cuentas por pagar"},
		Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("seed AP account: %v", err)
	}

	form := url.Values{}
	form.Set("name", "Branch")
	form.Set("parent_id", "1")
	form.Set("base_currency", "USD")
	form.Set("default_ap_account_id", itoa(int64(ap)))

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(sub)), form)
	if rec.Code >= 400 {
		t.Fatalf("set AP returned %d, body: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetSubsidiary(context.Background(), sub)
	if !got.DefaultApAccountID.Valid || got.DefaultApAccountID.Int64 != int64(ap) {
		t.Errorf("default_ap_account_id = %+v, want %d", got.DefaultApAccountID, ap)
	}
}

// TestSubsidiariesRenamePreservesAP: an unrelated edit (rename) that OMITS the
// default_ap_account_id field must NOT clear a previously set AP account. The form
// omits the picker when a subsidiary has no candidate accounts, so an absent field
// must be treated as "leave as-is" -- not as a clear (which would wipe the field
// and write an unintended versioned snapshot, rule 14).
func TestSubsidiariesRenamePreservesAP(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	sub, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Branch", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	ap, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "liability", DefaultCurrency: "USD",
		Names:        map[string]string{"en": "AP", "es": "AP"},
		Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("seed AP account: %v", err)
	}
	if err := st.UpdateSubsidiary(ctx, sub, store.UpdateSubsidiaryInput{DefaultAPAccountID: &ap}); err != nil {
		t.Fatalf("set AP: %v", err)
	}

	// Rename WITHOUT resubmitting default_ap_account_id (as the picker-less form does).
	form := url.Values{}
	form.Set("name", "Renamed Branch")
	form.Set("parent_id", "1")
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(sub)), form)
	if rec.Code >= 400 {
		t.Fatalf("rename returned %d, body: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetSubsidiary(context.Background(), sub)
	if !got.DefaultApAccountID.Valid || got.DefaultApAccountID.Int64 != int64(ap) {
		t.Errorf("AP account wiped by an unrelated rename: got %+v, want %d", got.DefaultApAccountID, ap)
	}
}

// TestSubsidiariesMove: editing a subsidiary's parent moves it under a new
// parent.
func TestSubsidiariesMove(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	a, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "A", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	b, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "B", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Move B under A.
	form := url.Values{}
	form.Set("name", "B")
	form.Set("parent_id", itoa(int64(a)))
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(b)), form)
	if rec.Code >= 400 {
		t.Fatalf("move returned %d, body: %s", rec.Code, rec.Body.String())
	}
	sub, _ := st.GetSubsidiary(context.Background(), b)
	if !sub.ParentID.Valid || sub.ParentID.Int64 != int64(a) {
		t.Errorf("B parent = %v, want %d", sub.ParentID, a)
	}
}

// TestSubsidiariesDeactivateChildless: deactivating a childless subsidiary flips
// its active flag.
func TestSubsidiariesDeactivateChildless(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Temp", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(id))+"/deactivate", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("deactivate returned %d, body: %s", rec.Code, rec.Body.String())
	}
	sub, _ := st.GetSubsidiary(context.Background(), id)
	if sub.Active != 0 {
		t.Errorf("subsidiary %d still active after deactivate", id)
	}
}

// TestSubsidiariesMatrixPersonas: /admin/subsidiaries is Admin-only. Only an
// admin reaches it; Bookkeeper (write), ReportsOnly (read+grant), and anon are
// denied. (The auto-matrix also covers this; this asserts the key personas.)
func TestSubsidiariesMatrixPersonas(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)
	book := mkUser(t, st, "book", "write", false)
	reports := mkUser(t, st, "ro", "read", false)

	// anon -> login redirect.
	rec := asUser(t, h, sm, 0, http.MethodGet, "/admin/subsidiaries", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon GET /admin/subsidiaries: status=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
	// Bookkeeper (write, not admin) -> 403.
	rec = asUser(t, h, sm, book, http.MethodGet, "/admin/subsidiaries", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("Bookkeeper GET /admin/subsidiaries: status=%d, want 403", rec.Code)
	}
	// ReportsOnly (read, not admin) -> 403.
	rec = asUser(t, h, sm, reports, http.MethodGet, "/admin/subsidiaries", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ReportsOnly GET /admin/subsidiaries: status=%d, want 403", rec.Code)
	}
	// Admin -> 200.
	rec = asUser(t, h, sm, admin, http.MethodGet, "/admin/subsidiaries", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("Admin GET /admin/subsidiaries: status=%d, want 200", rec.Code)
	}
	// A mutating route is Admin-only too: Bookkeeper POST -> 403.
	form := url.Values{}
	form.Set("name", "X")
	form.Set("parent_id", "1")
	form.Set("base_currency", "USD")
	rec = asUser(t, h, sm, book, http.MethodPost, "/admin/subsidiaries", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("Bookkeeper POST /admin/subsidiaries: status=%d, want 403", rec.Code)
	}
}

// TestSubsidiariesSecondRootRejected: creating a subsidiary with no parent is
// rejected -- the store returns ErrSecondRoot, mapped to the localized key, and
// no subsidiary is created (no-trace).
func TestSubsidiariesSecondRootRejected(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	form := url.Values{}
	form.Set("name", "Rogue Root")
	form.Set("parent_id", "0") // no parent -> second root
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second-root create status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "parent") && !strings.Contains(rec.Body.String(), "root") {
		t.Errorf("422 body missing the localized second-root message; body: %s", rec.Body.String())
	}
	if subByName(t, st, "Rogue Root") != 0 {
		t.Errorf("a second root was created despite the guard")
	}
}

// TestSubsidiariesRootImmovable: editing the ROOT to give it a parent is rejected
// -- the store returns ErrRootImmovable, mapped to the localized key, and the
// root keeps its NULL parent.
func TestSubsidiariesRootImmovable(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	child, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Child", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed child: %v", err)
	}

	// Try to reparent the ROOT (id 1) under its own child.
	form := url.Values{}
	form.Set("name", "Organization")
	form.Set("parent_id", itoa(int64(child)))
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/1", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("root-move status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	root, _ := st.GetSubsidiary(context.Background(), 1)
	if root.ParentID.Valid {
		t.Errorf("root gained a parent despite ErrRootImmovable: %v", root.ParentID)
	}
}

// TestSubsidiariesMoveCycleRejected: moving a subsidiary under its own descendant
// is rejected with the localized cycle message; the parent is unchanged.
func TestSubsidiariesMoveCycleRejected(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	a, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "A", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	b, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: a, Name: "B", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Move A under B (its own descendant) -> ErrCycle.
	form := url.Values{}
	form.Set("name", "A")
	form.Set("parent_id", itoa(int64(b)))
	form.Set("base_currency", "USD")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(a)), form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cycle move status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	sub, _ := st.GetSubsidiary(context.Background(), a)
	if !sub.ParentID.Valid || sub.ParentID.Int64 != 1 {
		t.Errorf("A parent changed despite ErrCycle: %v", sub.ParentID)
	}
}

// TestSubsidiariesDeactivateGuard: deactivating a subsidiary that still has an
// active child is blocked -- the store returns ErrHasActiveChildren, the handler
// shows the localized blocked message, and the subsidiary stays active (no
// execution).
func TestSubsidiariesDeactivateGuard(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	parent, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Parent", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if _, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: parent, Name: "Kid", BaseCurrency: "USD"}); err != nil {
		t.Fatalf("seed kid: %v", err)
	}

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/subsidiaries/"+itoa(int64(parent))+"/deactivate", url.Values{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("guarded deactivate status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "child") && !strings.Contains(rec.Body.String(), "children") {
		t.Errorf("422 body missing the localized active-children message; body: %s", rec.Body.String())
	}
	sub, _ := st.GetSubsidiary(context.Background(), parent)
	if sub.Active != 1 {
		t.Errorf("parent %d was deactivated despite an active child", parent)
	}
}
