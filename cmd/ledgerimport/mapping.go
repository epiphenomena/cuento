package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"cuento/internal/store"
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
//
// The optional trailing mergeIntoCol is NOT part of this base list: an empty
// merge column carries no information and would needlessly widen every historical
// file, so the reader accepts BOTH an 11-column file (no merges) and a
// 12-column file (with merge_into), and the writer only emits the 12th column
// when at least one row declares a merge. That keeps today's merge-free output
// byte-identical (D p26.116).
var accountMapCols = []string{
	"source_acct",
	"cuento_type",
	"cuento_parent",
	"subsidiaries",
	"functional_class_default",
	"default_program",
	"form990_code",
	"intercompany",
	"active",
	"name_en",
	"name_es",
}

// mergeIntoCol is the OPTIONAL 12th column. When set on a row it names the
// source_acct of the CANONICAL partner row this row merges INTO: the two source
// accounts collapse to ONE cuento account (a bilingual EN/ES account spanning
// both books). Empty (or column absent) = no merge, the historical behavior.
const mergeIntoCol = "merge_into"

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
	Intercompany    bool // D19: an intra-group account eliminated at consolidation
	Active          bool // false = inactivated in the source (QuickBooks "(deleted)")
	NameEN          string
	NameES          string

	// MergeInto, when non-empty, is the source_acct of the CANONICAL partner row
	// this row merges INTO (the two source accounts become ONE bilingual cuento
	// account). Empty = no merge (the common case / historical behavior). See
	// accounts() and reloadAccounts() for the two-pass wiring, and the merge rule:
	// the canonical (merge_into TARGET) row supplies name_en, this (merge_into
	// SOURCE) row supplies name_es; subsidiaries are unioned; the account is active
	// if EITHER row is active.
	MergeInto string
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
	// Emit the optional merge_into column ONLY when at least one row declares a
	// merge, so a merge-free file stays byte-identical to the historical 11-column
	// format (D p26.116).
	withMerge := false
	for _, r := range rows {
		if r.MergeInto != "" {
			withMerge = true
			break
		}
	}

	cw := csv.NewWriter(w)
	header := accountMapCols
	if withMerge {
		header = append(append([]string{}, accountMapCols...), mergeIntoCol)
	}
	if err := cw.Write(header); err != nil {
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
			strconv.FormatBool(r.Intercompany),
			strconv.FormatBool(r.Active),
			r.NameEN,
			r.NameES,
		}
		if withMerge {
			rec = append(rec, r.MergeInto)
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
	// Accept BOTH the historical 11-column file and a 12-column file whose extra
	// trailing column is merge_into. FieldsPerRecord=-1 lets us decide the width
	// from the header, then we enforce a consistent width ourselves below.
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read account map: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("read account map: empty (missing header)")
	}
	for i, h := range accountMapCols {
		if len(rows[0]) <= i || rows[0][i] != h {
			return nil, fmt.Errorf("read account map: header col %d = %q, want %q", i, colAt(rows[0], i), h)
		}
	}
	// The optional 12th column, if present, must be merge_into (a scrambled/extra
	// column fails loudly rather than being silently read as a merge target).
	hasMerge := false
	switch len(rows[0]) {
	case len(accountMapCols):
		// historical width, no merge column
	case len(accountMapCols) + 1:
		if rows[0][len(accountMapCols)] != mergeIntoCol {
			return nil, fmt.Errorf("read account map: header col %d = %q, want %q",
				len(accountMapCols), rows[0][len(accountMapCols)], mergeIntoCol)
		}
		hasMerge = true
	default:
		return nil, fmt.Errorf("read account map: header has %d columns, want %d or %d",
			len(rows[0]), len(accountMapCols), len(accountMapCols)+1)
	}
	wantCols := len(rows[0])

	out := make([]AccountMap, 0, len(rows)-1)
	for i, f := range rows[1:] {
		if len(f) != wantCols {
			return nil, fmt.Errorf("read account map: row %d has %d columns, want %d", i+2, len(f), wantCols)
		}
		ic, err := parseBoolCell(f[7], false)
		if err != nil {
			return nil, fmt.Errorf("read account map: row %d intercompany %q: %w", i+2, f[7], err)
		}
		active, err := parseBoolCell(f[8], true) // blank = active (the common case)
		if err != nil {
			return nil, fmt.Errorf("read account map: row %d active %q: %w", i+2, f[8], err)
		}
		m := AccountMap{
			SourceAcct:      f[0],
			CuentoType:      f[1],
			CuentoParent:    f[2],
			Subsidiaries:    splitList(f[3]),
			FunctionalClass: f[4],
			DefaultProgram:  f[5],
			Form990Code:     f[6],
			Intercompany:    ic,
			Active:          active,
			NameEN:          f[9],
			NameES:          f[10],
		}
		if hasMerge {
			m.MergeInto = strings.TrimSpace(f[11])
		}
		out = append(out, m)
	}
	return out, nil
}

// colAt returns row[i] or "" (used only for a friendly header-mismatch message on
// a too-short header, so we never index out of range).
func colAt(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

// parseBoolCell parses a boolean flag cell: blank = def, else a Go bool literal
// (true/false, also 1/0/t/f via strconv). A garbage value fails loudly rather than
// silently defaulting.
func parseBoolCell(s string, def bool) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return strconv.ParseBool(s)
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
	// root program ("General"). A `kat` with no entry falls back to the root. This
	// is the PARENT-level program dimension.
	Programs map[string]string `json:"programs"`

	// ProgramClasses maps a source `klass` (the raw, hierarchical QuickBooks class)
	// -> a program name, giving CHILD-level program detail that `kat` collapses. It
	// takes precedence over Programs on a split: klass is finer AND more correct
	// (e.g. a US-ledger split classed "UPH:Summer Camp" is a UPH program even though
	// its kat is "uplam"). A klass with no entry falls back to the kat program. Merges
	// bilingual (EN/US, ES/HNL) classes to one program by mapping both to one name.
	ProgramClasses map[string]string `json:"program_classes"`

	// ProgramParents maps a program NAME -> its parent program name, so ProgramClasses
	// children nest under their kat-level parent (which nests under root "General").
	// A program with no entry is created directly under "General".
	ProgramParents map[string]string `json:"program_parents"`

	// Funds maps a source `donor` value -> a fund definition. A donor with no
	// entry (or a blank donor) means unrestricted (NULL fund).
	Funds map[string]FundConfig `json:"funds"`

	// CampusFund, when set, is a single fund assigned to every split whose source
	// `kat` is "campus" -- a marker-driven fund (NOT donor-driven), so it lives in
	// its own field, not the donor-keyed Funds map. A campus split takes this fund
	// regardless of its donor (kat=campus precedence, D p26.40). Its Subsidiaries
	// (if given) are ignored: the fund is scoped programmatically to ALL configured
	// child subsidiaries (a superset of every subsidiary that posts a campus split),
	// so leave it empty in the config. nil = no campus fund (the campus->fund path is
	// off; kat still feeds program).
	CampusFund *FundConfig `json:"campus_fund"`

	// CampusAssetAccounts lists source_acct keys (matching the account-mapping CSV's
	// source_acct column) for FIXED-ASSET accounts whose splits belong to the campus
	// fund even though their source `kat` is not "campus" -- capital held BY the
	// campus project (land, buildings under construction, etc.). A split on one of
	// these accounts is treated like a campus split (assigned RtW, paired via the
	// p26.43 offset logic), bypassing the R/E-only guard that the pool-driven kat
	// path uses -- see resolveSplit. These are asset swaps within the fund, so they
	// do NOT enter the chronological drawdown pool (only campus revenue/expense
	// does). Empty/nil = no such accounts (the account-driven path is off; the
	// kat=campus path is unaffected).
	CampusAssetAccounts []string `json:"campus_asset_accounts"`

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

	// SkipCountries lists source `country` markers to drop entirely (the
	// consolidation-marker rows, docs hazard #3).
	SkipCountries []string `json:"skip_countries"`

	// OpeningBalanceTyps lists source `typ` values whose tid groups are treated as
	// opening balances (their imbalance is absorbed by OpeningBalanceAccount).
	OpeningBalanceTyps []string `json:"opening_balance_typs"`

	// Corrections is a list of fully-specified MANUAL ADJUSTMENT transactions applied
	// after the source import (D p26.72). Each is a balanced journal entry the owner's
	// consolidation worksheet requires but the mechanical CSV import cannot express --
	// e.g. a year-end in-transit CUTOFF correction whose two legs the source books
	// straddle across a fiscal boundary. They post through the store exactly like an
	// imported transaction (versioned, invariant-checked, per-fund zero-sum), so a
	// mis-specified adjustment fails the build loudly rather than corrupting the ledger.
	// Empty/nil = no adjustments (the common case). This is a GENERAL primitive: the
	// owner may add future legitimate consolidation/cutoff entries here without code
	// changes. The SPECIFIC values live only in the gitignored config (rule 11).
	Corrections []Correction `json:"corrections"`
}

// Correction is one manual adjustment transaction (D p26.72). It is a
// fully-specified, self-balancing journal entry addressed by human-readable keys
// (subsidiary NAME, source_acct, donor, program name) matching the rest of the
// mapping, resolved to ids at apply time. A single currency and subsidiary per
// entry (rule 7: one currency + one subsidiary per transaction).
type Correction struct {
	Date        string            `json:"date"`        // YYYY-MM-DD, the posting date
	Subsidiary  string            `json:"subsidiary"`  // subsidiary NAME (must be configured)
	Currency    string            `json:"currency"`    // ISO code; every split posts in it
	Memo        string            `json:"memo"`        // optional transaction memo (audit trail)
	Description string            `json:"description"` // optional default per-split description
	Splits      []CorrectionSplit `json:"splits"`
}

// CorrectionSplit is one leg of a Correction. Amount is the EXACT signed net-debit
// in the currency's minor units (rule 3: int64, never a float / lossy source `v`);
// debits positive, credits negative (D2). Account is a source_acct key in the
// account-mapping CSV. Fund (a donor key), Program (a program name) and
// FunctionalClass are optional and validated against the same maps the import uses.
type CorrectionSplit struct {
	Account         string `json:"account"`          // source_acct in the account-mapping CSV
	Amount          int64  `json:"amount"`           // exact net-debit, minor units (Dr + / Cr -)
	Fund            string `json:"fund"`             // donor key -> fund (optional; "" = unrestricted)
	Program         string `json:"program"`          // program NAME (optional; R/E only)
	FunctionalClass string `json:"functional_class"` // program|management|fundraising (optional; expense only)
	Description     string `json:"description"`      // per-split description (optional; falls back to the entry's)
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

// rateCols is the header of the optional historical-rates CSV, in order. The file
// is a flat list of report-time FX facts (D12) the go-live build loads via
// store.PutRates so the shipped db can convert HNL->USD (etc.) at report time --
// the importer stores native minor units only, so without these no consolidated-
// USD report can be produced. Kept as one list so any future writer and this
// reader agree by construction.
var rateCols = []string{"rate_date", "base", "quote", "rate", "source"}

// ReadRates parses the historical-rates CSV from r into store.Rate rows for a
// single PutRates batch. The header is required and validated against rateCols so
// a scrambled/short file fails loudly rather than mis-loading columns. rate is a
// float (D12 / rule 3 -- the one place a non-integer amount is allowed). Blank
// source defaults to "import" for the audit trail.
func ReadRates(r io.Reader) ([]store.Rate, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(rateCols)
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read rates: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("read rates: empty (missing header)")
	}
	for i, h := range rateCols {
		if rows[0][i] != h {
			return nil, fmt.Errorf("read rates: header col %d = %q, want %q", i, rows[0][i], h)
		}
	}

	out := make([]store.Rate, 0, len(rows)-1)
	for i, f := range rows[1:] {
		val, err := strconv.ParseFloat(strings.TrimSpace(f[3]), 64)
		if err != nil {
			return nil, fmt.Errorf("read rates: row %d rate %q: %w", i+2, f[3], err)
		}
		src := strings.TrimSpace(f[4])
		if src == "" {
			src = "import"
		}
		out = append(out, store.Rate{
			RateDate: strings.TrimSpace(f[0]),
			Base:     strings.TrimSpace(f[1]),
			Quote:    strings.TrimSpace(f[2]),
			Value:    val,
			Source:   src,
		})
	}
	return out, nil
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
