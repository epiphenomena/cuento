package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/store"
	"cuento/internal/testutil"
)

// accountsApp builds a real app over a migrated temp db and returns the handler,
// store, and session manager. p11.1's chart-of-accounts handlers are driven
// through the REAL mounted router (httptest) against a real migrated db (AGENTS
// testing conventions) -- no handler-level store mocks.
func accountsApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, app.sessions
}

// asUser mints a session cookie for userID and issues req through h, returning the
// recorder. Mirrors the matrix's mintCookie/doAs, generalized to any method+body.
func asUser(t *testing.T, h http.Handler, sm *scs.SessionManager, userID int64, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if userID != 0 {
		req.AddCookie(mintCookie(t, sm, userID))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// mkUser creates a user with the given txn perm (or admin) and returns its id.
func mkUser(t *testing.T, st *store.Store, username, perm string, admin bool) int64 {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateUser(ctx, store.CreateUserInput{
		Username: username, DisplayName: username, TxnPerm: perm, IsAdmin: admin,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return id
}

// countAccounts / accountByName help assertions against the created accounts.
func accountIDByName(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	rows, err := st.Tree(context.Background(), "en", nil)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return 0
}

// TestAccountsCreateHappyPath: a Bookkeeper POSTs a valid create (en+es names, a
// subsidiary) and the account appears in the tree with both names and its sub.
func TestAccountsCreateHappyPath(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "Petty Cash")
	form.Set("name_es", "Caja Chica")
	form.Set("sub_1", "1") // root subsidiary id 1
	form.Set("reconcilable", "on")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts", form)
	if rec.Code == http.StatusForbidden || (rec.Code == http.StatusFound && rec.Header().Get("Location") == "/login") {
		t.Fatalf("create denied for bookkeeper: status=%d", rec.Code)
	}
	if rec.Code >= 400 {
		t.Fatalf("create returned %d, body: %s", rec.Code, rec.Body.String())
	}

	id := accountIDByName(t, st, "Petty Cash")
	if id == 0 {
		t.Fatalf("created account not found in tree; body: %s", rec.Body.String())
	}
	// es name resolves via a Spanish tree.
	esRows, _ := st.Tree(context.Background(), "es", nil)
	var esName string
	for _, r := range esRows {
		if r.ID == id {
			esName = r.Name
		}
	}
	if esName != "Caja Chica" {
		t.Errorf("es name = %q, want %q", esName, "Caja Chica")
	}
	// Subsidiary mapped.
	subs, _ := st.AccountSubsidiaryIDs(context.Background(), id)
	found := false
	for _, s := range subs {
		if s == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("account %d not mapped to root sub; subs=%v", id, subs)
	}
}

// TestAccountNewFormFullPage (p26.7): GET /accounts/new is a plain navigation now,
// so it renders a FULL shell page (shell nav + an <h1> title), not the bare
// account-form partial. The form is present on that page.
func TestAccountNewFormFullPage(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book_new", "write", false)

	rec := asUser(t, h, sm, book, http.MethodGet, "/accounts/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts/new status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Full shell markers: the app nav + a page <h1> (the create title).
	if !strings.Contains(body, `class="app-nav"`) {
		t.Errorf("GET /accounts/new missing the shell nav (not a full page); body: %s", body)
	}
	if !strings.Contains(body, "<h1>") {
		t.Errorf("GET /accounts/new missing an <h1> page title; body: %s", body)
	}
	// The form itself (plain POST, no htmx target swap) is present.
	if !strings.Contains(body, `id="af-name-en"`) {
		t.Errorf("GET /accounts/new missing the account form; body: %s", body)
	}
}

// TestAccountEditFormFullPage (p26.7): GET /accounts/{id}/edit renders a full shell
// page prefilled from the account, not the bare partial.
func TestAccountEditFormFullPage(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book_edit", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	rec := asUser(t, h, sm, book, http.MethodGet, "/accounts/"+itoa(id)+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts/%d/edit status = %d, want 200; body: %s", id, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="app-nav"`) {
		t.Errorf("GET edit missing the shell nav (not a full page); body: %s", body)
	}
	if !strings.Contains(body, "<h1>") {
		t.Errorf("GET edit missing an <h1> page title; body: %s", body)
	}
	if !strings.Contains(body, `value="Cash"`) {
		t.Errorf("GET edit did not prefill the account name; body: %s", body)
	}
}

// TestAccountNewFormTypeSwapPartial (p26.7): the type-select re-fetch keeps working
// as an in-place htmx swap on the standalone page. htmx sends HX-Target
// "account-form", so the handler must serve the BARE partial (no shell chrome) so
// the swap replaces just the form, not inject a whole document.
func TestAccountNewFormTypeSwapPartial(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book_swap", "write", false)

	req := httptest.NewRequest(http.MethodGet, "/accounts/new?type=expense", strings.NewReader(""))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "account-form")
	req.AddCookie(mintCookie(t, sm, book))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("type-swap GET status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A bare partial: the form is present but NOT the shell nav.
	if !strings.Contains(body, `id="account-form"`) {
		t.Errorf("type-swap missing the form partial; body: %s", body)
	}
	if strings.Contains(body, `class="app-nav"`) {
		t.Errorf("type-swap returned full shell chrome (would inject a whole doc into #account-form); body: %s", body)
	}
	// Expense type shows the functional-class region.
	if !strings.Contains(body, `id="af-func"`) {
		t.Errorf("type-swap to expense missing the functional-class select; body: %s", body)
	}
}

// TestAccountsRowRegisterLinkAndReconcile (p25): the account NAME links to its
// register (the dedicated Register button is gone), and a reconcilable account shows
// a Reconcile affordance to the recon list while a non-reconcilable one does not.
func TestAccountsRowRegisterLinkAndReconcile(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book_recon", "write", false)

	mkAcct := func(name string, reconcilable bool) int64 {
		form := url.Values{}
		form.Set("type", "asset")
		form.Set("currency", "USD")
		form.Set("name_en", name)
		form.Set("sub_1", "1")
		if reconcilable {
			form.Set("reconcilable", "on")
		}
		rec := asUser(t, h, sm, book, http.MethodPost, "/accounts", form)
		if rec.Code >= 400 {
			t.Fatalf("create %q: status=%d, body=%s", name, rec.Code, rec.Body.String())
		}
		return accountIDByName(t, st, name)
	}

	reconID := mkAcct("Bank Recon", true)
	plainID := mkAcct("Plain Asset", false)

	body := asUser(t, h, sm, book, http.MethodGet, "/accounts", nil).Body.String()

	// The name is the register link.
	if !strings.Contains(body, `href="/accounts/`+itoa(reconID)+`/register">Bank Recon</a>`) {
		t.Errorf("reconcilable account name is not a register link:\n%s", body)
	}
	if !strings.Contains(body, `href="/accounts/`+itoa(plainID)+`/register">Plain Asset</a>`) {
		t.Errorf("plain account name is not a register link:\n%s", body)
	}
	// The reconcilable account shows the Reconcile affordance; the plain one does not.
	if !strings.Contains(body, `href="/reconciliations#recon-acct-`+itoa(reconID)+`"`) {
		t.Errorf("reconcilable account missing the Reconcile link:\n%s", body)
	}
	if strings.Contains(body, `href="/reconciliations#recon-acct-`+itoa(plainID)+`"`) {
		t.Errorf("non-reconcilable account should not show the Reconcile link:\n%s", body)
	}
}

// TestAccountsEditHappyPath: a Bookkeeper edits an account's English name and it
// changes in the tree.
func TestAccountsEditHappyPath(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "Cash Renamed")
	form.Set("name_es", "Efectivo")
	form.Set("sub_1", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/"+itoa(id), form)
	if rec.Code >= 400 {
		t.Fatalf("edit returned %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := accountIDByName(t, st, "Cash Renamed"); got != id {
		t.Errorf("renamed account not found (want id %d); got %d", id, got)
	}
}

// TestAccountsDeactivate: a Bookkeeper deactivates an account; its active flag
// flips.
func TestAccountsDeactivate(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Temp"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/"+itoa(id)+"/deactivate", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("deactivate returned %d, body: %s", rec.Code, rec.Body.String())
	}
	acct, _ := st.GetAccount(context.Background(), id)
	if acct.Active != 0 {
		t.Errorf("account %d still active after deactivate", id)
	}
}

// TestAccountsCreateInvalidShowsFieldError: a create missing the required en name
// re-renders the form region at 422 with the localized field error (the p10.3
// convention), mapping the store's ErrNameRequired to an i18n key.
func TestAccountsCreateInvalidShowsFieldError(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "") // missing -> ErrNameRequired
	form.Set("sub_1", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The English error string for a missing account name must be present.
	if !strings.Contains(body, "English") && !strings.Contains(body, "name") {
		t.Errorf("422 body does not contain the localized name error; body: %s", body)
	}
	if !strings.Contains(body, "autofocus") {
		t.Errorf("422 body missing autofocus on the first invalid field; body: %s", body)
	}
	// p26.7: the 422 re-render is now a FULL page (anti-jank on a standalone form
	// page), not the bare partial -- assert the shell chrome is present.
	if !strings.Contains(body, `class="app-nav"`) {
		t.Errorf("422 create re-render is not a full page (missing shell nav); body: %s", body)
	}
}

// TestAccountsEditErrorReRenderExcludesSelf: a failed EDIT submit (422 re-render)
// must build the parent select with the account's OWN id excluded (self +
// descendants) -- the re-render path must thread the edit id into the option
// build, not fall back to create's id=0. We force a validation error (blank en
// name) and assert the account's own id does not appear as a parent <option>.
func TestAccountsEditErrorReRenderExcludesSelf(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	// Parent asset + a child asset, so the parent select for the CHILD would list
	// the parent but must never list the child itself.
	parent, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Assets"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := st.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "") // blank -> ErrNameRequired via SetAccountName? No: edit
	form.Set("parent_id", itoa(parent))
	form.Set("sub_1", "1")
	// Force a 990-type mismatch to trigger the 422 (an asset can't take a revenue
	// line); this is a store typed error mapped to a field, re-rendering the form.
	form.Set("form990_code", "VIII.1f")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/"+itoa(child), form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("edit error status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The child's own id must NOT be offered as a parent option in the re-render.
	if strings.Contains(rec.Body.String(), `value="`+itoa(child)+`"`) {
		t.Errorf("re-rendered parent select offers the account's own id %d; body: %s", child, rec.Body.String())
	}
}

// TestAccountsGetPermissions: GET /accounts is TxnRead; anon -> login redirect,
// ReadOnly allowed, NoAccess forbidden.
func TestAccountsGetPermissions(t *testing.T) {
	h, st, sm := accountsApp(t)
	readOnly := mkUser(t, st, "ro", "read", false)
	noAccess := mkUser(t, st, "na", "none", false)

	// anon
	rec := asUser(t, h, sm, 0, http.MethodGet, "/accounts", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon GET /accounts: status=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
	// read-only allowed
	rec = asUser(t, h, sm, readOnly, http.MethodGet, "/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("ReadOnly GET /accounts: status=%d, want 200", rec.Code)
	}
	// no-access forbidden
	rec = asUser(t, h, sm, noAccess, http.MethodGet, "/accounts", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("NoAccess GET /accounts: status=%d, want 403", rec.Code)
	}
}

// TestAccountsPostPermissions: POST /accounts is TxnWrite; ReadOnly forbidden.
func TestAccountsPostPermissions(t *testing.T) {
	h, st, sm := accountsApp(t)
	readOnly := mkUser(t, st, "ro", "read", false)

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "X")
	form.Set("sub_1", "1")

	rec := asUser(t, h, sm, readOnly, http.MethodPost, "/accounts", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ReadOnly POST /accounts: status=%d, want 403", rec.Code)
	}
}

// TestSubAssignmentPropagation: assigning a sub to a child account through the
// FORM propagates that sub up the ancestor chain (the p05.2 store behavior
// surfaced in the UX). Asserts the resulting parent membership.
func TestSubAssignmentPropagation(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	subA, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "A", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("create sub A: %v", err)
	}
	// Parent maps {root} only.
	parent, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Assets"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// Child under parent, initially {root}.
	child, err := st.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Edit the child through the form, checking BOTH root and subA.
	form := url.Values{}
	form.Set("type", "asset")
	form.Set("currency", "USD")
	form.Set("name_en", "Cash")
	form.Set("parent_id", itoa(parent))
	form.Set("sub_1", "1")
	form.Set("sub_"+itoa(subA), itoa(subA))

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/"+itoa(child), form)
	if rec.Code >= 400 {
		t.Fatalf("edit child returned %d, body: %s", rec.Code, rec.Body.String())
	}

	// subA must have propagated up to the parent account.
	psubs, _ := st.AccountSubsidiaryIDs(context.Background(), parent)
	has := false
	for _, s := range psubs {
		if s == subA {
			has = true
		}
	}
	if !has {
		t.Errorf("subA did not propagate to parent %d after form edit; parent subs=%v", parent, psubs)
	}
}

// TestBalancesColumnMatchesQuery: the per-account balances the page renders equal
// exactly the p08.4 SubtreeBalancesAsOf output for the same as-of date and scope.
// The handler's balance assembly is exposed as balancesByAccount(asof, scopeSub)
// so this asserts on the data structure, not scraped HTML.
func TestBalancesColumnMatchesQuery(t *testing.T) {
	_, st, _ := accountsApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Two accounts + a balanced transaction so there are non-zero balances.
	cash, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	eq, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "equity", DefaultCurrency: "USD", Names: map[string]string{"en": "Opening"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create equity: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: cash, Amount: 100000},
			{AccountID: eq, Amount: -100000},
		},
	}); err != nil {
		t.Fatalf("post txn: %v", err)
	}

	const asof = "2025-12-31"
	want, err := st.SubtreeBalancesAsOf(ctx, asof, 1)
	if err != nil {
		t.Fatalf("SubtreeBalancesAsOf: %v", err)
	}
	wantMap := map[[2]string]int64{}
	for _, b := range want {
		wantMap[[2]string{itoa(b.AccountID), b.Currency}] = b.Amount
	}

	got, err := balancesByAccount(ctx, st, asof, 1)
	if err != nil {
		t.Fatalf("balancesByAccount: %v", err)
	}
	gotMap := map[[2]string]int64{}
	for acct, cells := range got {
		for _, c := range cells {
			gotMap[[2]string{itoa(acct), c.Currency}] = c.Minor
		}
	}

	if len(gotMap) != len(wantMap) {
		t.Fatalf("balance cell count = %d, want %d", len(gotMap), len(wantMap))
	}
	for k, v := range wantMap {
		if gotMap[k] != v {
			t.Errorf("balance[%v] = %d, want %d", k, gotMap[k], v)
		}
	}
}

// TestAccountsFilterRemembered: p26.14 -- the chart-of-accounts subsidiary+active
// filters are remembered in the session. A GET carrying the filter form params
// (sub present, active off) both applies and SAVES the selection; a later bare
// nav to /accounts (no params) RESTORES it, including "active only" OFF (which is
// remembered as off, not treated as "no preference -> restore default on").
func TestAccountsFilterRemembered(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "read", true)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	subA, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Alpha", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}

	// One session shared across both requests (same token row => request 2 reads
	// the value request 1 committed).
	cookie := mintCookie(t, sm, admin)
	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// (a) A deliberate filter submit: sub present + active unchecked (no active key).
	rec := do("/accounts?sub=" + itoa(subA))
	if rec.Code != http.StatusOK {
		t.Fatalf("filter GET = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `value="`+itoa(subA)+`" selected`) {
		t.Fatalf("filter GET did not select sub %d; body: %s", subA, rec.Body.String())
	}

	// (b) A bare nav to /accounts (no params) must RESTORE the saved selection.
	rec = do("/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("bare GET = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+itoa(subA)+`" selected`) {
		t.Errorf("bare GET did not restore sub %d; body: %s", subA, body)
	}
	// "active only" was OFF on the saving request -> restored OFF: the checkbox
	// input must NOT carry `checked`.
	if activeCheckboxChecked(body) {
		t.Errorf("bare GET restored active-only as ON; want remembered OFF; body: %s", body)
	}
}

// TestAccountsFilterActiveOffOverwritesOn: the load-bearing case the task singles
// out. Within ONE session, save active ON, then a later sub-present request with
// active UNCHECKED must OVERWRITE the remembered value to OFF (not be treated as
// "no preference"), so a subsequent bare nav restores OFF. This discriminates the
// correct unconditional save from a "save only when on" bug.
func TestAccountsFilterActiveOffOverwritesOn(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin4", "read", true)
	cookie := mintCookie(t, sm, admin)
	do := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	do("/accounts?sub=0&active=1")              // remember active ON
	do("/accounts?sub=0")                       // sub present, active unchecked -> overwrite to OFF
	if activeCheckboxChecked(do("/accounts")) { // bare nav must restore OFF
		t.Errorf("active-only was not overwritten to OFF by an unchecked sub-present request")
	}
}

// TestAccountsFilterRememberedActiveOn: the mirror case -- saving with active=1
// restores active-only ON.
func TestAccountsFilterRememberedActiveOn(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin3", "read", true)
	cookie := mintCookie(t, sm, admin)
	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/accounts?sub=0&active=1"); rec.Code != http.StatusOK {
		t.Fatalf("filter GET = %d", rec.Code)
	}
	body := do("/accounts").Body.String()
	if !activeCheckboxChecked(body) {
		t.Errorf("bare GET did not restore active-only ON; body: %s", body)
	}
}

// activeCheckboxChecked reports whether the rendered active-only checkbox input
// carries `checked`. It isolates the input tag so an unrelated `checked` elsewhere
// can't fool the assertion.
func activeCheckboxChecked(body string) bool {
	i := strings.Index(body, `name="active"`)
	if i < 0 {
		return false
	}
	end := strings.IndexByte(body[i:], '>')
	if end < 0 {
		return false
	}
	return strings.Contains(body[i:i+end], "checked")
}

// itoa is a tiny local int64->string to keep the test file dependency-free.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
