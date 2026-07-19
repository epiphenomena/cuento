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

// p13.1 my-settings tests. They exercise the REAL mounted router (httptest) against
// a real migrated db (AGENTS testing conventions): the same rendered register/chrome
// is served to users whose stored settings differ, and the output MUST differ per
// the p03 formatters + {{t}} (rules 9/10) with no second code path.

// setUserSettings persists a user's preferences through the store (the same funnel
// the /settings POST uses), so a subsequent request renders per those settings. It
// keeps the test focused on the RENDER, not the form plumbing.
func setUserSettings(t *testing.T, st *store.Store, userID ids.UserID, in store.UserSettingsInput) {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: userID})
	if err := st.UpdateUserSettings(ctx, userID, in, known); err != nil {
		t.Fatalf("UpdateUserSettings(%d): %v", userID, err)
	}
}

// TestFormatsFollowUserSettings: two users with DIFFERENT date/number/display
// settings request the SAME account register and see it rendered differently, end
// to end (real router, real db). User A (US number / ISO date / signed) sees
// "1,234.50" and the ISO date; User B (EU number / EU date / DR-CR display) sees
// "1.234,50", the EU date "15/03/2025", and a DR/CR marker -- proving the register
// honors per-user formatting through format.go with no separate render path.
func TestFormatsFollowUserSettings(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// One checking-account debit of 1,234.50 on 2025-03-15 (cents + grouping + a
	// discriminating date make every format difference visible in one row).
	e.post2(t, ctx, "2025-03-15", 123450, e.checking, e.expense, nil, "")

	// Two read users with contrasting settings.
	userA := mkUser(t, e.st, "fmt_a", "read", false)
	userB := mkUser(t, e.st, "fmt_b", "read", false)
	setUserSettings(t, e.st, userA, store.UserSettingsInput{
		Locale: "en", DateFormat: "ISO", NumberFormat: "US",
		DisplayMode: "signed", NegStyle: "minus", Theme: "auto",
	})
	setUserSettings(t, e.st, userB, store.UserSettingsInput{
		Locale: "en", DateFormat: "EU", NumberFormat: "EU",
		DisplayMode: "dr_cr", NegStyle: "minus", Theme: "auto",
	})

	regURL := "/accounts/" + itoa(e.checking) + "/register"
	bodyA := asUser(t, e.h, e.sm, userA, http.MethodGet, regURL, nil).Body.String()
	bodyB := asUser(t, e.h, e.sm, userB, http.MethodGet, regURL, nil).Body.String()

	// The SAME register renders DIFFERENTLY.
	if bodyA == bodyB {
		t.Fatal("the two users' registers are byte-identical; per-user formatting is not applied")
	}

	// User A: US grouping, ISO date, signed (no DR/CR marker).
	if !strings.Contains(bodyA, "1,234.50") {
		t.Errorf("user A register missing US-grouped amount 1,234.50")
	}
	if !strings.Contains(bodyA, "2025-03-15") {
		t.Errorf("user A register missing ISO date 2025-03-15")
	}
	if strings.Contains(bodyA, " DR") || strings.Contains(bodyA, " CR") {
		t.Errorf("user A (signed) register unexpectedly shows a DR/CR marker")
	}

	// User B: EU grouping, EU date, DR/CR display.
	if !strings.Contains(bodyB, "1.234,50") {
		t.Errorf("user B register missing EU-grouped amount 1.234,50")
	}
	if !strings.Contains(bodyB, "15/03/2025") {
		t.Errorf("user B register missing EU date 15/03/2025")
	}
	if !strings.Contains(bodyB, " DR") && !strings.Contains(bodyB, " CR") {
		t.Errorf("user B (dr_cr) register missing a DR/CR marker")
	}
	// And user B must NOT show user A's US-grouped amount.
	if strings.Contains(bodyB, "1,234.50") {
		t.Errorf("user B register shows US grouping 1,234.50; EU format not applied")
	}
}

// TestLocaleSwitchSwapsChrome: an es-locale user sees the es catalog EVERYWHERE --
// both the chrome (nav) and the page content -- with NO raw i18n keys leaking. It
// loads the settings page itself (chrome + a page whose every string is catalogued)
// as an es user and asserts es strings render in both the nav and <main>, and that a
// representative raw key is absent.
func TestLocaleSwitchSwapsChrome(t *testing.T) {
	h, st, sm := accountsApp(t)

	esUser := mkUser(t, st, "es_user", "read", false)
	setUserSettings(t, st, esUser, store.UserSettingsInput{
		Locale: "es", DateFormat: "ISO", NumberFormat: "US",
		DisplayMode: "signed", NegStyle: "minus", Theme: "auto",
	})

	body := asUser(t, h, sm, esUser, http.MethodGet, "/settings", nil).Body.String()

	// Chrome (top nav): the es labels render. p23.9 moved Settings into the hub (now the
	// p26.77 "All" landing), so the top-nav es labels are "Cuentas" (accounts) and "Todo"
	// (all); the Settings es label "Ajustes" now lives in the section bar (asserted below).
	navStart := strings.Index(body, `<nav class="app-nav"`)
	if navStart < 0 {
		t.Fatal("no app-nav in the rendered shell")
	}
	navEnd := strings.Index(body[navStart:], "</nav>")
	nav := body[navStart : navStart+navEnd]
	if !strings.Contains(nav, "Cuentas") {
		t.Errorf("nav chrome not in es: missing 'Cuentas' (accounts)")
	}
	if !strings.Contains(nav, "Todo") {
		t.Errorf("nav chrome not in es: missing 'Todo' (all)\nnav=%s", nav)
	}
	// The section bar (More area) carries the localized Settings link ("Ajustes").
	if !strings.Contains(body, "Ajustes") {
		t.Errorf("section bar not in es: missing 'Ajustes' (settings)")
	}

	// Page content (<main>): the es settings labels render (page body localized too).
	mainStart := strings.Index(body, `<main`)
	if mainStart < 0 {
		t.Fatal("no <main> in the rendered page")
	}
	mainHTML := body[mainStart:]
	for _, want := range []string{"Idioma", "Formato de fecha", "Tema"} {
		if !strings.Contains(mainHTML, want) {
			t.Errorf("page content not in es: missing %q", want)
		}
	}

	// The html lang attribute is es.
	if !strings.Contains(body, `lang="es"`) {
		t.Errorf("html lang is not es")
	}

	// No raw i18n keys leaked (unresolved {{t}} would print the dotted key).
	for _, key := range []string{"nav.settings", "settings.language", "settings.title", "settings.theme"} {
		if strings.Contains(body, key) {
			t.Errorf("raw i18n key %q leaked into the rendered page", key)
		}
	}
}

// TestSettingsSavePersistsAndTakesEffect: POST /settings validates + persists the
// user's own preferences (versioned), sets the theme cookie for immediate SSR, and
// 303-redirects (PRG) to GET /settings?saved. The next GET reflects the new locale
// (es chrome) and the theme cookie renders the chosen theme SSR.
func TestSettingsSavePersistsAndTakesEffect(t *testing.T) {
	h, st, sm := accountsApp(t)
	uid := mkUser(t, st, "saver", "read", false)

	form := url.Values{
		"locale":             {"es"},
		"date_format":        {"EU"},
		"number_format":      {"EU"},
		"display_mode":       {"dr_cr"},
		"neg_style":          {"parens"},
		"theme":              {"dark"},
		"default_subsidiary": {"1"}, // seeded root subsidiary
		"default_program":    {"1"}, // seeded root program
	}
	rec := asUser(t, h, sm, uid, http.MethodPost, "/settings", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/settings") {
		t.Errorf("redirect Location = %q, want /settings...", loc)
	}
	// The theme cookie is set so resolveTheme (cookie before DB) reflects it at once.
	if !strings.Contains(rec.Header().Get("Set-Cookie"), themeCookieName+"=dark") {
		t.Errorf("theme cookie not set to dark: %q", rec.Header().Get("Set-Cookie"))
	}

	// Persisted: the store's live row now carries the new settings.
	cu, err := st.UserByID(context.Background(), uid)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if cu.Locale != "es" || cu.DateFormat != "EU" || cu.DisplayMode != "dr_cr" || cu.Theme != "dark" {
		t.Fatalf("persisted settings = %+v, want es/EU/dr_cr/dark", cu)
	}
	if cu.DefaultSubsidiaryID == nil || *cu.DefaultSubsidiaryID != 1 {
		t.Errorf("default subsidiary not persisted: %v", cu.DefaultSubsidiaryID)
	}
	if cu.DefaultProgramID == nil || *cu.DefaultProgramID != 1 {
		t.Errorf("default program not persisted: %v", cu.DefaultProgramID)
	}

	// The subsequent GET renders es chrome (locale took effect).
	body := asUser(t, h, sm, uid, http.MethodGet, "/settings", nil).Body.String()
	if !strings.Contains(body, `lang="es"`) {
		t.Errorf("subsequent GET not in es after save")
	}
}

// TestSettingsRejectsCraftedInvalid: a POST slipping a value past the fixed selects
// is answered 422 (form error, whole page re-rendered) and NOTHING is persisted.
func TestSettingsRejectsCraftedInvalid(t *testing.T) {
	h, st, sm := accountsApp(t)
	uid := mkUser(t, st, "crafter", "read", false)

	form := url.Values{
		"locale":        {"en"},
		"date_format":   {"ISO"},
		"number_format": {"BOGUS"}, // not in the vocabulary
		"display_mode":  {"signed"},
		"neg_style":     {"minus"},
		"theme":         {"auto"},
	}
	rec := asUser(t, h, sm, uid, http.MethodPost, "/settings", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("crafted-invalid POST status = %d, want 422", rec.Code)
	}
	// Nothing persisted: the user's number_format stays the default US.
	cu, err := st.UserByID(context.Background(), uid)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if cu.NumberFormat != "US" {
		t.Errorf("number_format = %q, want unchanged US (invalid POST must not persist)", cu.NumberFormat)
	}
}

// TestSettingsRejectsInvalidProgram: a POST naming a non-existent default program is
// answered 422 (the store's existence check maps to ErrInvalidSetting) and NOTHING is
// persisted -- mirroring the default_subsidiary reject.
func TestSettingsRejectsInvalidProgram(t *testing.T) {
	h, st, sm := accountsApp(t)
	uid := mkUser(t, st, "progcrafter", "read", false)

	form := url.Values{
		"locale":          {"en"},
		"date_format":     {"ISO"},
		"number_format":   {"US"},
		"display_mode":    {"signed"},
		"neg_style":       {"minus"},
		"theme":           {"auto"},
		"default_program": {"999999"}, // no such program
	}
	rec := asUser(t, h, sm, uid, http.MethodPost, "/settings", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid-program POST status = %d, want 422", rec.Code)
	}
	cu, err := st.UserByID(context.Background(), uid)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if cu.DefaultProgramID != nil {
		t.Errorf("default program = %v, want nil (invalid POST must not persist)", cu.DefaultProgramID)
	}
}

// TestSettingsPermAnyUser: /settings is AnyUser -- a no-perm ("none") user reaches
// both GET and POST (they edit their OWN settings), while an anonymous request is
// bounced to login.
func TestSettingsPermAnyUser(t *testing.T) {
	h, st, sm := accountsApp(t)
	noPerm := mkUser(t, st, "noperm", "none", false)

	if rec := asUser(t, h, sm, noPerm, http.MethodGet, "/settings", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /settings as no-perm user = %d, want 200", rec.Code)
	}
	form := url.Values{
		"locale": {"en"}, "date_format": {"ISO"}, "number_format": {"US"},
		"display_mode": {"signed"}, "neg_style": {"minus"}, "theme": {"auto"},
	}
	if rec := asUser(t, h, sm, noPerm, http.MethodPost, "/settings", form); rec.Code != http.StatusSeeOther {
		t.Errorf("POST /settings as no-perm user = %d, want 303", rec.Code)
	}
	// Anonymous -> 302 to login.
	if rec := asUser(t, h, sm, 0, http.MethodGet, "/settings", nil); rec.Code != http.StatusFound {
		t.Errorf("anon GET /settings = %d, want 302 to login", rec.Code)
	}
}
