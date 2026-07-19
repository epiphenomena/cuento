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

// p11.4 org settings & languages (web). The Admin org form (/admin/org) edits the
// org name + enabled languages; enabled languages drive the account form's
// per-language name inputs. Driven through the REAL mounted router (httptest) over
// a real migrated db, reusing accountsApp/asUser/mkUser (p11.1 helpers).

// TestOrgPageRenders: GET /admin/org (Admin) renders the form prefilled from the
// seeded settings (enabled_languages = en,es). The org display name is DERIVED from
// the root subsidiary (p30.14) and shown read-only -- the editable org_name input
// is gone.
func TestOrgPageRenders(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/admin/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/org as admin: status=%d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="org_name"`) {
		t.Errorf("org form still has the retired org_name input; body: %s", body)
	}
	if !strings.Contains(body, `name="enabled_languages"`) {
		t.Errorf("org form missing enabled_languages input; body: %s", body)
	}
	if !strings.Contains(body, "en,es") {
		t.Errorf("org form does not prefill seeded enabled_languages; body: %s", body)
	}
	// The derived org display name (the root subsidiary's name) is shown, pointing
	// the admin at /admin/subsidiaries to change it.
	rootName, err := st.RootSubsidiaryName(context.Background())
	if err != nil {
		t.Fatalf("RootSubsidiaryName: %v", err)
	}
	if !strings.Contains(body, rootName) {
		t.Errorf("org page does not show the derived org name %q; body: %s", rootName, body)
	}
	if !strings.Contains(body, `href="/admin/subsidiaries"`) {
		t.Errorf("org page does not link to /admin/subsidiaries; body: %s", body)
	}
}

// TestOrgSettingsPersist: POSTing the org form stores enabled_languages; it reads
// back through the store. (org_name was retired, p30.14.)
func TestOrgSettingsPersist(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "admin2", "none", true)
	ctx := context.Background()

	form := url.Values{}
	form.Set("enabled_languages", "en,es,fr")

	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/org", form)
	if rec.Code >= 400 {
		t.Fatalf("POST /admin/org returned %d, body: %s", rec.Code, rec.Body.String())
	}

	langs, err := st.EnabledLanguages(ctx)
	if err != nil {
		t.Fatalf("EnabledLanguages: %v", err)
	}
	if got := join(langs); got != "en,es,fr" {
		t.Errorf("enabled_languages = %q, want en,es,fr", got)
	}
}

// join concatenates langs (mirrors the store test helper; the web package needs
// its own since it can't import the store test helper).
func join(langs []string) string {
	return strings.Join(langs, ",")
}

// TestOrgFormAdminOnly: a non-admin (Bookkeeper) is forbidden from the org form.
// (The permission matrix also covers this automatically; this is the explicit
// key-persona check the step asks for.)
func TestOrgFormAdminOnly(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	rec := asUser(t, h, sm, book, http.MethodGet, "/admin/org", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /admin/org as bookkeeper: status=%d, want 403", rec.Code)
	}
	rec = asUser(t, h, sm, book, http.MethodPost, "/admin/org",
		url.Values{"enabled_languages": {"en"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /admin/org as bookkeeper: status=%d, want 403", rec.Code)
	}
}

// TestAccountFormLanguageColumns: with enabled_languages = en,es the account
// create form renders name_en + name_es inputs but NOT name_fr; after adding fr to
// the org setting a name_fr input appears. This is the p11.4 payoff: adding a
// language exposes a name column in account forms.
func TestAccountFormLanguageColumns(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	// Default seed: en, es -> name_en and name_es, no name_fr.
	rec := asUser(t, h, sm, book, http.MethodGet, "/accounts/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts/new: status=%d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="name_en"`) {
		t.Errorf("account form missing name_en input; body: %s", body)
	}
	if !strings.Contains(body, `name="name_es"`) {
		t.Errorf("account form missing name_es input; body: %s", body)
	}
	if strings.Contains(body, `name="name_fr"`) {
		t.Errorf("account form unexpectedly has name_fr with only en,es enabled; body: %s", body)
	}
	// The stable en input id the e2e specs rely on must still exist.
	if !strings.Contains(body, `id="af-name-en"`) {
		t.Errorf("account form missing stable af-name-en id; body: %s", body)
	}

	// Enable a third language.
	ctx := context.Background()
	if err := st.SetOrgSetting(ctx, store.SettingEnabledLanguages, "en,es,fr"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	rec = asUser(t, h, sm, book, http.MethodGet, "/accounts/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts/new after adding fr: status=%d, body: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, `name="name_fr"`) {
		t.Errorf("account form missing name_fr after enabling fr; body: %s", body)
	}
}

// TestAccountCreateWritesEnabledLanguageNames: creating an account with fr enabled
// writes a name for each enabled language via account_names; the fr name reads
// back through the fr-lang tree.
func TestAccountCreateWritesEnabledLanguageNames(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := context.Background()

	if err := st.SetOrgSetting(ctx, store.SettingEnabledLanguages, "en,es,fr"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}

	form := url.Values{}
	form.Set("type", "asset")
	form.Set("name_en", "Cash")
	form.Set("name_es", "Efectivo")
	form.Set("name_fr", "Espèces")
	form.Set("currency", "USD")
	form.Set("sub_1", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts", form)
	if rec.Code >= 400 {
		t.Fatalf("POST /accounts returned %d, body: %s", rec.Code, rec.Body.String())
	}

	id := accountIDByName(t, st, "Cash") // en tree
	if id == 0 {
		t.Fatalf("created account not found by en name")
	}
	// The fr name resolves through the fr-language tree.
	frName := accountNameInLang(t, st, id, "fr")
	if frName != "Espèces" {
		t.Errorf("fr name = %q, want Espèces", frName)
	}
}

// TestAccountEditWritesEnabledLanguageNames: editing an account with fr enabled
// updates the fr name.
func TestAccountEditWritesEnabledLanguageNames(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := context.Background()

	// Create with en+es only first.
	actorCtx := store.WithActor(ctx, store.Actor{ID: book})
	id, err := st.CreateAccount(actorCtx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Bank", "es": "Banco"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Enable fr, then edit supplying a fr name.
	if err := st.SetOrgSetting(ctx, store.SettingEnabledLanguages, "en,es,fr"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	form := url.Values{}
	form.Set("type", "asset")
	form.Set("name_en", "Bank")
	form.Set("name_es", "Banco")
	form.Set("name_fr", "Banque")
	form.Set("currency", "USD")
	form.Set("sub_1", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/"+itoa(int64(id)), form)
	if rec.Code >= 400 {
		t.Fatalf("POST /accounts/%d returned %d, body: %s", id, rec.Code, rec.Body.String())
	}
	if got := accountNameInLang(t, st, id, "fr"); got != "Banque" {
		t.Errorf("fr name after edit = %q, want Banque", got)
	}
}

// accountNameInLang reads an account's name in a given language via the store Tree
// (name fallback: the exact-lang name when present).
func accountNameInLang(t *testing.T, st *store.Store, id ids.AccountID, lang string) string {
	t.Helper()
	rows, err := st.Tree(context.Background(), lang, nil)
	if err != nil {
		t.Fatalf("Tree(%s): %v", lang, err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}
