package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// classCandidatesCmd emits a REVIEWABLE class-classification sheet (Deliverable 2 of
// the class-driven-fund work): from the source export + the reviewed mapping config it
// lists one row per distinct source `klass` (the QuickBooks CLASS column) with its row
// count, current program mapping, a few sample accounts, and a HEURISTIC is_fund hint
// for obvious grant/restricted-fund names. The human reviews the sheet, marks is_fund=Y
// for the classes that are really restricted funds, keeps is_program=Y where a class is
// also a program, and fills fund_key + funder + restriction + dates. The confirmed rows
// are then hand-copied into mapping.json (fund_classes + fund_defs). This command NEVER
// edits the mapping; it only writes the candidate sheet. Reproducible: re-run whenever
// the source or mapping changes to regenerate the sheet.
func classCandidatesCmd(args []string) error {
	fs := flag.NewFlagSet("class-candidates", flag.ContinueOnError)
	source := fs.String("source", "", "path to the source ledger CSV export")
	configPath := fs.String("config", "", "path to the global mapping config JSON")
	outPath := fs.String("o", "", "output class-classification CSV path (default: stdout)")
	samples := fs.Int("samples", 3, "max distinct sample accounts per class")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *configPath == "" {
		return fmt.Errorf("-source and -config are both required")
	}

	cfg, err := readConfigFile(*configPath)
	if err != nil {
		return err
	}
	src, err := os.Open(*source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()
	recs, err := ParseRecords(src)
	if err != nil {
		return err
	}

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	n, hinted, err := writeClassCandidates(out, recs, cfg, *samples)
	if err != nil {
		return err
	}
	if *outPath != "" {
		fmt.Fprintf(os.Stderr, "class-candidates: wrote %d class(es) (%d hinted as funds) to %s\n", n, hinted, *outPath)
	}
	return nil
}

// classCandidateCols is the header of the class-classification CSV, in order. The
// pre-filled columns (row_count, current_program_mapping, sample_accounts, is_program,
// is_fund hint) inform the human; the DECISION columns (is_fund, fund_key, funder,
// restriction, start_date, end_date) are for the human to fill.
var classCandidateCols = []string{
	"class",
	"row_count",
	"current_program_mapping",
	"sample_accounts",
	"is_fund",
	"is_program",
	"fund_key",
	"funder",
	"restriction",
	"start_date",
	"end_date",
}

// classCandidate is one distinct-klass row.
type classCandidate struct {
	class      string
	rowCount   int
	curProgram string // from cfg.ProgramClasses (blank = not a mapped program)
	samples    []string
	isFundHint bool // heuristic: the name looks like a grant/restricted fund
	isProgram  bool // the class is already a mapped program (ProgramClasses)
}

// fundNameHints are case-insensitive substrings that mark a class name as an OBVIOUS
// grant/restricted-fund candidate (a HINT the human confirms, never an auto-decision).
// The Spanish needles are written as \u escapes so the source file stays pure ASCII
// while the compiled string still holds the UTF-8 bytes of the accented class names in
// the export: o-acute (\u00f3) is in "Subvencion"/"Donacion", and enye (\u00f1) is in
// "Campana". Matching those exact byte sequences is what makes the hint fire on the
// real accented klass values.
var fundNameHints = []string{
	"grant",
	"fund",
	"subvenci\u00f3n", // Subvencion (grant)
	"campa\u00f1a",    // Campana (campaign)
	"donaci\u00f3n",   // Donacion (donation)
	"beca",            // scholarship
	"embajada",        // embassy
}

// looksLikeFund reports whether a class name contains any fund hint (case-insensitive).
// The needles are already lowercase; the class is lowercased for the compare. Both
// sides are UTF-8, so ToLower on the ASCII range plus exact-byte accented needles is
// sufficient here (the accented needles are matched as-is, not case-folded).
func looksLikeFund(class string) bool {
	lc := strings.ToLower(class)
	for _, h := range fundNameHints {
		if strings.Contains(lc, h) {
			return true
		}
	}
	return false
}

// buildClassCandidates aggregates the export into one candidate per distinct non-empty
// klass: its row count, the current ProgramClasses mapping (if any), up to maxSamples
// distinct sample accounts, and the two hints (is_program from the mapping, is_fund from
// the name heuristic). Deterministic order (class name).
func buildClassCandidates(recs []Record, cfg Config, maxSamples int) []classCandidate {
	type agg struct {
		rowCount int
		acctSet  map[string]bool
		accts    []string // insertion order, capped at maxSamples
	}
	byClass := map[string]*agg{}
	for _, r := range recs {
		if r.Klass == "" {
			continue
		}
		a := byClass[r.Klass]
		if a == nil {
			a = &agg{acctSet: map[string]bool{}}
			byClass[r.Klass] = a
		}
		a.rowCount++
		if r.Acct != "" && !a.acctSet[r.Acct] && len(a.accts) < maxSamples {
			a.acctSet[r.Acct] = true
			a.accts = append(a.accts, r.Acct)
		}
	}

	out := make([]classCandidate, 0, len(byClass))
	for class, a := range byClass {
		prog, isProg := cfg.ProgramClasses[class]
		out = append(out, classCandidate{
			class:      class,
			rowCount:   a.rowCount,
			curProgram: prog,
			samples:    a.accts,
			isFundHint: looksLikeFund(class),
			isProgram:  isProg,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].class < out[j].class })
	return out
}

// writeClassCandidates emits the class-classification CSV (a '#'-comment human-guidance
// preamble, the header, then one row per distinct class) and returns the class count and
// the number hinted as funds. The is_fund cell is pre-filled with the heuristic hint
// ("Y" or ""); is_program with "Y" where the class is already a program; the human edits
// the rest. Sample accounts are joined with " | " into one cell (no comma, so the CSV
// stays single-column). The '#' lines are written raw (NOT CSV-quoted) so the sheet reads
// as guidance; a re-import of this file is not a supported path (the human hand-copies
// confirmed funds into mapping.json).
func writeClassCandidates(w io.Writer, recs []Record, cfg Config, maxSamples int) (int, int, error) {
	cands := buildClassCandidates(recs, cfg, maxSamples)

	preamble := "" +
		"# CLASS CLASSIFICATION -- REVIEW, do NOT auto-apply.\n" +
		"# One row per distinct source class (QuickBooks CLASS column). Mark up the\n" +
		"# DECISION columns, then hand-copy the confirmed funds into mapping.json.\n" +
		"# is_fund      : set Y for a class that is a GRANT or RESTRICTED FUND (money the\n" +
		"#                org must track and spend only for a purpose/period). The is_fund\n" +
		"#                cell is PRE-FILLED with a heuristic hint (Y) for obvious grant\n" +
		"#                names (Grant/Fund/Subvencion/Campana/Donacion/Beca/Embajada);\n" +
		"#                the hint is a suggestion only -- confirm or clear it.\n" +
		"# is_program   : PRE-FILLED Y where the class is already a mapped program. Keep Y\n" +
		"#                when the class is BOTH a fund and a program (a grant funding a\n" +
		"#                program) -- fund_id and program_id are independent on a split.\n" +
		"# fund_key     : for each is_fund=Y row, a short opaque key (e.g. edu_grant_2025).\n" +
		"#                Two classes may share ONE key to fund one grant from both.\n" +
		"# funder       : the grantor / donor organization name.\n" +
		"# restriction  : one of purpose | time | perpetual.\n" +
		"# start_date /\n" +
		"# end_date     : optional YYYY-MM-DD grant period bounds ('' = none).\n" +
		"# In mapping.json set fund_classes[class] = fund_key and fund_defs[fund_key] =\n" +
		"# {name, name_es, funder, purpose, restriction, start_date, end_date,\n" +
		"# subsidiaries:[...], program:optional}. A class marked is_fund must NOT also ride\n" +
		"# a campus (kat=campus) row or a donor fund on the same transaction.\n"
	if _, err := io.WriteString(w, preamble); err != nil {
		return 0, 0, fmt.Errorf("write preamble: %w", err)
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(classCandidateCols); err != nil {
		return 0, 0, fmt.Errorf("write header: %w", err)
	}
	hinted := 0
	for _, c := range cands {
		isFund := ""
		if c.isFundHint {
			isFund = "Y"
			hinted++
		}
		isProgram := ""
		if c.isProgram {
			isProgram = "Y"
		}
		rec := []string{
			c.class,
			strconv.Itoa(c.rowCount),
			c.curProgram,
			strings.Join(c.samples, " | "),
			isFund,
			isProgram,
			"", // fund_key -- human fills
			"", // funder -- human fills
			"", // restriction -- human fills
			"", // start_date -- human fills
			"", // end_date -- human fills
		}
		if err := cw.Write(rec); err != nil {
			return 0, 0, fmt.Errorf("write class %q: %w", c.class, err)
		}
	}
	cw.Flush()
	return len(cands), hinted, cw.Error()
}
