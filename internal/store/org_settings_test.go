package store

import (
	"context"
	"testing"

	"cuento/internal/testutil"
)

// p11.4 org_settings. A simple key/value CONFIG table (non-versioned, like
// currencies): the store exposes plain reads and an idempotent upsert outside the
// write funnel. These tests drive the store directly over a migrated temp db.

// TestOrgSettingsSeeded: migration 00012 seeds an empty org_name and
// enabled_languages en,es; the store reads them back.
func TestOrgSettingsSeeded(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	name, err := s.OrgSetting(ctx, SettingOrgName, "DEFAULT")
	if err != nil {
		t.Fatalf("OrgSetting org_name: %v", err)
	}
	if name != "" {
		t.Errorf("seeded org_name = %q, want empty", name)
	}

	langs, err := s.OrgSetting(ctx, SettingEnabledLanguages, "DEFAULT")
	if err != nil {
		t.Fatalf("OrgSetting enabled_languages: %v", err)
	}
	if langs != "en,es" {
		t.Errorf("seeded enabled_languages = %q, want %q", langs, "en,es")
	}
}

// TestOrgSettingDefaultWhenUnset: an unseeded key returns the caller's default,
// never an error.
func TestOrgSettingDefaultWhenUnset(t *testing.T) {
	s := New(testutil.NewDB(t))
	got, err := s.OrgSetting(context.Background(), "no_such_key", "fallback")
	if err != nil {
		t.Fatalf("OrgSetting: %v", err)
	}
	if got != "fallback" {
		t.Errorf("unset key = %q, want %q", got, "fallback")
	}
}

// TestSetOrgSettingPersists: SetOrgSetting stores a value that reads back, and a
// second write updates it (idempotent upsert). No actor needed (config, not audit).
func TestSetOrgSettingPersists(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	if err := s.SetOrgSetting(ctx, SettingOrgName, "FitSupply"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	got, err := s.OrgSetting(ctx, SettingOrgName, "")
	if err != nil {
		t.Fatalf("OrgSetting: %v", err)
	}
	if got != "FitSupply" {
		t.Errorf("org_name = %q, want FitSupply", got)
	}

	if err := s.SetOrgSetting(ctx, SettingOrgName, "FitSupply Inc"); err != nil {
		t.Fatalf("SetOrgSetting (update): %v", err)
	}
	got, err = s.OrgSetting(ctx, SettingOrgName, "")
	if err != nil {
		t.Fatalf("OrgSetting: %v", err)
	}
	if got != "FitSupply Inc" {
		t.Errorf("org_name after update = %q, want %q", got, "FitSupply Inc")
	}
}

// TestEnabledLanguagesParsesCSV: EnabledLanguages parses the stored CSV into a
// clean slice, always with en first, honoring an added third language.
func TestEnabledLanguagesParsesCSV(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	// Default seed: en, es.
	langs, err := s.EnabledLanguages(ctx)
	if err != nil {
		t.Fatalf("EnabledLanguages: %v", err)
	}
	if got, want := join(langs), "en,es"; got != want {
		t.Errorf("seed EnabledLanguages = %q, want %q", got, want)
	}

	// Add a third language.
	if err := s.SetOrgSetting(ctx, SettingEnabledLanguages, "en, es , fr"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	langs, err = s.EnabledLanguages(ctx)
	if err != nil {
		t.Fatalf("EnabledLanguages: %v", err)
	}
	if got, want := join(langs), "en,es,fr"; got != want {
		t.Errorf("EnabledLanguages = %q, want %q", got, want)
	}
}

// TestEnabledLanguagesAlwaysIncludesEn: even if an admin drops en from the CSV,
// EnabledLanguages guarantees en is present and first (the required base, p05.3).
func TestEnabledLanguagesAlwaysIncludesEn(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	if err := s.SetOrgSetting(ctx, SettingEnabledLanguages, "es,fr"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	langs, err := s.EnabledLanguages(ctx)
	if err != nil {
		t.Fatalf("EnabledLanguages: %v", err)
	}
	if len(langs) == 0 || langs[0] != "en" {
		t.Fatalf("EnabledLanguages = %v, want en first", langs)
	}
	if got, want := join(langs), "en,es,fr"; got != want {
		t.Errorf("EnabledLanguages = %q, want %q", got, want)
	}
}

// join concatenates a slice of langs for stable comparison.
func join(langs []string) string {
	out := ""
	for i, l := range langs {
		if i > 0 {
			out += ","
		}
		out += l
	}
	return out
}
