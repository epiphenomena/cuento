package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
)

// org_settings is a simple key/value CONFIG table (p11.4) -- NOT a versioned
// business table (it is absent from Appendix A's versions list, like currencies
// and report_groups). Admin writes here are configuration, not audited business
// mutations, so these helpers are plain sqlc reads and an idempotent upsert
// OUTSIDE the write funnel: rule 2 permits reads and reference/config upserts via
// sqlc without an actor or a changes row (exactly like SyncReportGroups and the
// currency reads).
//
// Report base currency is INTENTIONALLY NOT a setting here: it follows the scoped
// subsidiary's base_currency (D18), so it lives on subsidiaries, not org_settings.

// Org-setting keys. Kept as named constants so callers reference them
// symbolically and the two seeded keys are discoverable from Go.
const (
	// SettingOrgName is the organization's display name (seeded empty; no chrome
	// consumer yet -- p13.x wires it in. p11.4 only stores it).
	SettingOrgName = "org_name"
	// SettingEnabledLanguages is a CSV of the languages account NAMES may be
	// entered in (seeded 'en,es', D14). It drives the account form's per-language
	// name inputs ONLY -- the UI chrome stays en/es via i18n.T fallback.
	SettingEnabledLanguages = "enabled_languages"
)

// baseNameLang is the required base language for account names: en is always an
// enabled name language and the name-fallback base (p05.3, D14), so
// EnabledLanguages guarantees it is present and first even if an admin drops it
// from the stored CSV -- a create/edit form must always offer the required en
// name input.
const baseNameLang = "en"

// OrgSetting returns the value for key, or def when the key is unset (never
// written / seeded away). A read, so it bypasses the write funnel (rule 2).
func (s *Store) OrgSetting(ctx context.Context, key, def string) (string, error) {
	v, err := s.q.GetOrgSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get org setting %q: %w", key, err)
	}
	return v, nil
}

// SetOrgSetting upserts one config key idempotently. It is a config write, not an
// audited business mutation, so it is a plain sqlc upsert outside the write funnel
// (like SyncReportGroups) -- no actor, no changes row.
func (s *Store) SetOrgSetting(ctx context.Context, key, value string) error {
	if err := s.q.UpsertOrgSetting(ctx, sqlc.UpsertOrgSettingParams{Key: key, Value: value}); err != nil {
		return fmt.Errorf("store: set org setting %q: %w", key, err)
	}
	return nil
}

// EnabledLanguages returns the languages account names may be entered in, parsed
// from the enabled_languages CSV (D14). It always includes the required base
// language en FIRST (p05.3 name fallback), even if an admin dropped it from the
// stored value; the rest follow in stored order, trimmed, with blanks and
// duplicates dropped. This is the option source the account form's per-language
// name inputs render from -- adding a language to the setting makes a new name
// column appear.
func (s *Store) EnabledLanguages(ctx context.Context) ([]string, error) {
	raw, err := s.OrgSetting(ctx, SettingEnabledLanguages, baseNameLang+",es")
	if err != nil {
		return nil, err
	}
	return parseEnabledLanguages(raw), nil
}

// parseEnabledLanguages splits a CSV of language codes into a clean, ordered,
// deduped slice with the base language (en) guaranteed present and first. Trimmed
// per element; empties dropped; later duplicates dropped (first occurrence wins).
func parseEnabledLanguages(raw string) []string {
	seen := map[string]bool{baseNameLang: true}
	out := []string{baseNameLang}
	for _, part := range strings.Split(raw, ",") {
		lang := strings.TrimSpace(part)
		if lang == "" || seen[lang] {
			continue
		}
		seen[lang] = true
		out = append(out, lang)
	}
	return out
}
