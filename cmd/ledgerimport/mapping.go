package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The mapping is two stdlib-parseable files (a deliberate within-allowlist
// substitution for PLAN's "YAML" -- no yaml dependency, rule 1 / D15; recorded in
// DECISIONS p09.3):
//
//   - an ACCOUNT-MAPPING CSV (encoding/csv): one reviewable row per distinct
//     source account, with best-guess columns the human edits. `accounts` emits
//     the skeleton; `build` reads the reviewed file.
//   - a GLOBAL CONFIG JSON (encoding/json): the subsidiary/program/fund trees,
//     functional-class map, currency/FX-clearing config, and row filters.

// accountMapCols is the header of the account-mapping CSV, in order. Kept as one
// list so the emitter and the reader agree by construction.
var accountMapCols = []string{
	"source_acct",
	"cuento_type",
	"cuento_parent",
	"subsidiaries",
	"functional_class_default",
	"default_program",
	"form990_code",
	"name_en",
	"name_es",
}

// AccountMap is one reviewable account-mapping row. Multi-valued cells
// (subsidiaries) are ";"-separated. Empty cells mean "unset" (the human left the
// best guess or blanked it deliberately).
type AccountMap struct {
	SourceAcct      string
	CuentoType      string // asset|liability|revenue|expense|equity
	CuentoParent    string // source_acct of the parent, or "" for top-level
	Subsidiaries    []string
	FunctionalClass string // program|management|fundraising (expense only)
	DefaultProgram  string // program name (R/E only)
	Form990Code     string
	NameEN          string
	NameES          string
}

// stmtToType maps the source `stmt` super-type char to a cuento account type
// (docs/ledger-export.md: A/L/I/E/O -> asset/liability/revenue/expense/equity).
func stmtToType(stmt string) string {
	switch strings.ToUpper(strings.TrimSpace(stmt)) {
	case "A":
		return "asset"
	case "L":
		return "liability"
	case "I":
		return "revenue"
	case "E":
		return "expense"
	case "O":
		return "equity"
	default:
		return ""
	}
}

// WriteAccountMap emits the account-mapping CSV (header + rows) to w. Row order
// is caller-controlled (accounts sorts by source account for a stable skeleton).
func WriteAccountMap(w io.Writer, rows []AccountMap) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(accountMapCols); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, r := range rows {
		rec := []string{
			r.SourceAcct,
			r.CuentoType,
			r.CuentoParent,
			strings.Join(r.Subsidiaries, ";"),
			r.FunctionalClass,
			r.DefaultProgram,
			r.Form990Code,
			r.NameEN,
			r.NameES,
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("write row %q: %w", r.SourceAcct, err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// ReadAccountMap parses the reviewed account-mapping CSV from r. The header is
// required and validated against accountMapCols (so a scrambled/short file fails
// loudly rather than mis-mapping columns).
func ReadAccountMap(r io.Reader) ([]AccountMap, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(accountMapCols)
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read account map: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("read account map: empty (missing header)")
	}
	for i, h := range accountMapCols {
		if rows[0][i] != h {
			return nil, fmt.Errorf("read account map: header col %d = %q, want %q", i, rows[0][i], h)
		}
	}

	out := make([]AccountMap, 0, len(rows)-1)
	for _, f := range rows[1:] {
		out = append(out, AccountMap{
			SourceAcct:      f[0],
			CuentoType:      f[1],
			CuentoParent:    f[2],
			Subsidiaries:    splitList(f[3]),
			FunctionalClass: f[4],
			DefaultProgram:  f[5],
			Form990Code:     f[6],
			NameEN:          f[7],
			NameES:          f[8],
		})
	}
	return out, nil
}

// splitList parses a ";"-separated cell into trimmed non-empty values.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Config is the global mapping JSON. It carries everything that is NOT
// per-account: the entity trees, dimension derivations, currency/FX config, and
// row filters. Every field is stdlib-JSON (encoding/json), no yaml (D15).
type Config struct {
	// RootSubsidiaryName renames the seeded root subsidiary (id 1); the seed is
	// renamed, never a second root (single-root trigger). Its base currency is set
	// by RootBaseCurrency.
	RootSubsidiaryName string `json:"root_subsidiary_name"`
	RootBaseCurrency   string `json:"root_base_currency"`

	// Subsidiaries maps a source `country` code -> the child subsidiary to create
	// under the root. The root itself is not listed here.
	Subsidiaries map[string]SubsidiaryConfig `json:"subsidiaries"`

	// Programs maps a source `kat` code -> a program name created under the seeded
	// root program ("General"). A `kat` with no entry falls back to the root.
	Programs map[string]string `json:"programs"`

	// Funds maps a source `donor` value -> a fund definition. A donor with no
	// entry (or a blank donor) means unrestricted (NULL fund).
	Funds map[string]FundConfig `json:"funds"`

	// FunctionalClasses maps a source `kls` code -> program|management|fundraising
	// (D21). An unmapped/blank kls leaves the split to the account default.
	FunctionalClasses map[string]string `json:"functional_classes"`

	// BaseCurrency is the org functional currency; FXClearingAccount is the
	// source_acct (in the account-mapping CSV) of the multicurrency FX Clearing
	// account (D3) used to decompose cross-currency tid groups.
	BaseCurrency      string `json:"base_currency"`
	FXClearingAccount string `json:"fx_clearing_account"`

	// OpeningBalanceAccount is the source_acct of the per-subsidiary
	// Equity:Opening Balances account (D22) that receives the counter-leg for
	// opening-balance / single-split source groups.
	OpeningBalanceAccount string `json:"opening_balance_account"`

	// PayeeColumn selects which source column supplies the payee name (the task's
	// "payee-ish source column as configured"). Values: "typ", "klass", "desc", or
	// "" (no payees). Default "" avoids minting thousands of junk payees from the
	// long/multi-line `desc` memo on real data (a p09.4 tuning knob); memos always
	// come from `desc` regardless.
	PayeeColumn string `json:"payee_column"`

	// SkipCountries lists source `country` markers to drop entirely (the
	// consolidation-marker rows, docs hazard #3).
	SkipCountries []string `json:"skip_countries"`

	// OpeningBalanceTyps lists source `typ` values whose tid groups are treated as
	// opening balances (their imbalance is absorbed by OpeningBalanceAccount).
	OpeningBalanceTyps []string `json:"opening_balance_typs"`
}

// SubsidiaryConfig is a child subsidiary derived from a source country code.
type SubsidiaryConfig struct {
	Name         string `json:"name"`
	BaseCurrency string `json:"base_currency"`
}

// FundConfig is a fund derived from a source donor value (D20).
type FundConfig struct {
	Name         string   `json:"name"`
	Funder       string   `json:"funder"`
	Purpose      string   `json:"purpose"`
	Restriction  string   `json:"restriction"`  // purpose|time|perpetual
	Subsidiaries []string `json:"subsidiaries"` // subsidiary NAMES (>=1)
	Program      string   `json:"program"`      // optional program-subtree scope
}

// ReadConfig decodes the global config JSON from r, rejecting unknown fields so a
// typo in the mapping is caught during review, not silently ignored.
func ReadConfig(r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return c, nil
}

// contains reports whether s is in list (small linear scan; lists are tiny).
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
