package store

import (
	"context"
	"testing"

	"cuento/internal/testutil"
)

// p11.4 org_settings. A simple key/value CONFIG table (non-versioned, like
// currencies): the store exposes plain reads and an idempotent upsert outside the
// write funnel. These tests drive the store directly over a migrated temp db.

// TestOrgSettingsSeeded: migration 00012 seeds enabled_languages en,es; the store
// reads it back. (org_name was retired in p30.14 -- the org display name is derived
// from the root subsidiary; the harmless seed row is left in place but unread.)
func TestOrgSettingsSeeded(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

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
// Exercised via the generic upsert on a bespoke key (org_name was retired, p30.14).
func TestSetOrgSettingPersists(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	const key = "test_key"
	if err := s.SetOrgSetting(ctx, key, "first"); err != nil {
		t.Fatalf("SetOrgSetting: %v", err)
	}
	got, err := s.OrgSetting(ctx, key, "")
	if err != nil {
		t.Fatalf("OrgSetting: %v", err)
	}
	if got != "first" {
		t.Errorf("%s = %q, want first", key, got)
	}

	if err := s.SetOrgSetting(ctx, key, "second"); err != nil {
		t.Fatalf("SetOrgSetting (update): %v", err)
	}
	got, err = s.OrgSetting(ctx, key, "")
	if err != nil {
		t.Fatalf("OrgSetting: %v", err)
	}
	if got != "second" {
		t.Errorf("%s after update = %q, want %q", key, got, "second")
	}
}

// TestRootSubsidiaryName: the org display name is derived from the root subsidiary
// (p30.14). The seeded root is renamed and RootSubsidiaryName returns the new name;
// adding a child does not change which name is returned (the root stays first).
func TestRootSubsidiaryName(t *testing.T) {
	s := New(testutil.NewDB(t))
	ctx := context.Background()

	name, err := s.RootSubsidiaryName(ctx)
	if err != nil {
		t.Fatalf("RootSubsidiaryName: %v", err)
	}
	if name == "" {
		t.Fatalf("RootSubsidiaryName returned empty for the seeded root")
	}

	// Rename the root; the derived name follows it.
	newName := "FitSupply"
	if err := s.UpdateSubsidiary(mutCtx(), 1, UpdateSubsidiaryInput{Name: &newName}); err != nil {
		t.Fatalf("UpdateSubsidiary(root): %v", err)
	}
	// A child must not shadow the root as the display name.
	if _, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: 1, Name: "Child", BaseCurrency: "USD"}); err != nil {
		t.Fatalf("CreateSubsidiary(child): %v", err)
	}
	got, err := s.RootSubsidiaryName(ctx)
	if err != nil {
		t.Fatalf("RootSubsidiaryName after rename: %v", err)
	}
	if got != newName {
		t.Errorf("RootSubsidiaryName = %q, want %q", got, newName)
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
