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

// mergeCandidatesCmd emits a REVIEWABLE candidate-pairing sheet (Deliverable 2 of
// the bilingual-merge work, D p26.116): from the reviewed account-mapping CSV it
// finds US-only and UPH-only leaves that share a leading GL NUMBER and proposes
// which ones the human should MERGE into one bilingual account. It NEVER edits the
// mapping -- it only writes a candidate CSV the human reviews, then hand-copies the
// confirmed merge_into links back into mapping-accounts.csv. Reproducible: re-run it
// whenever the mapping changes to regenerate the sheet.
func mergeCandidatesCmd(args []string) error {
	fs := flag.NewFlagSet("merge-candidates", flag.ContinueOnError)
	mapPath := fs.String("map", "", "path to the reviewed account-mapping CSV")
	outPath := fs.String("o", "", "output candidate-pairing CSV path (default: stdout)")
	usSub := fs.String("us-sub", "US", "subsidiary code marking the ENGLISH/US-only side")
	uphSub := fs.String("uph-sub", "UPH", "subsidiary code marking the SPANISH/UPH-only side")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mapPath == "" {
		return fmt.Errorf("-map is required")
	}

	accMap, err := readAccountMapFile(*mapPath)
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
	n, err := writeMergeCandidates(out, accMap, *usSub, *uphSub)
	if err != nil {
		return err
	}
	if *outPath != "" {
		fmt.Fprintf(os.Stderr, "merge-candidates: wrote %d candidate pair(s) to %s\n", n, *outPath)
	}
	return nil
}

// mergeCandidateCols is the header of the candidate-pairing CSV, in order.
var mergeCandidateCols = []string{
	"gl_number",
	"us_source_acct",
	"us_name_en",
	"uph_source_acct",
	"uph_name_es",
	"same_parent",
	"cuento_type",
	"cuento_parent",
	"suggested_verdict",
}

// mergeCandidate is one proposed US<->UPH pairing row.
type mergeCandidate struct {
	gl         string
	usAcct     string
	usNameEN   string
	uphAcct    string
	uphNameES  string
	sameParent bool
	cuentoType string // from the US (anchor) row
	parent     string // cuento_parent from the US (anchor) row
	verdict    string // MERGE? | FALSE-FRIEND? | AMBIGUOUS? | REVIEW-DIFFERENT-PARENT?
}

// The prior go-live investigation flagged two RISKY same-GL patterns the sheet must
// NOT rubber-stamp as MERGE?:
//
//   - the Cash/bank BLOCK GL 1110-1130 (a CONTIGUOUS RANGE): these numbers name
//     DIFFERENT PHYSICAL bank/petty-cash accounts across the two books (e.g. a US
//     "Petty Cash" vs a UPH bank savings account at the same number) even though they
//     share the ::typ:asset:Cash parent -- a same-number coincidence, not one account.
//   - typeDivergentFalseFriends: a DISCRETE set of numbers whose US and UPH leaves
//     sit under different parent tiers (a same-number coincidence at the account level).
//
// Both are forced to FALSE-FRIEND? so the human verifies before merging.
const (
	cashBlockLo = 1110
	cashBlockHi = 1130
)

var typeDivergentFalseFriends = map[string]bool{"2500": true, "7240": true, "7560": true, "8220": true, "8560": true}

// inCashBlock reports whether a GL number falls in the risky Cash/bank block range.
func inCashBlock(gl string) bool {
	n, err := strconv.Atoi(gl)
	return err == nil && n >= cashBlockLo && n <= cashBlockHi
}

// glNumber returns the leading integer prefix of a source_acct (e.g. "1050 Cash:BOA"
// -> "1050"), or "" if the account has no numeric prefix (the ::typ: tiers).
func glNumber(sourceAcct string) string {
	head := sourceAcct
	if i := strings.IndexByte(sourceAcct, ' '); i >= 0 {
		head = sourceAcct[:i]
	}
	if head == "" {
		return ""
	}
	if _, err := strconv.Atoi(head); err != nil {
		return ""
	}
	return head
}

// leafSide reports whether a mapping row is a leaf siloed to EXACTLY the given
// subsidiary code (US-only or UPH-only). A row scoped to both books (or already
// merged) is not a candidate.
func leafSide(m AccountMap, code string) bool {
	return len(m.Subsidiaries) == 1 && m.Subsidiaries[0] == code
}

// buildMergeCandidates joins US-only and UPH-only leaves on their leading GL number
// (the join key is the NUMBER ALONE -- type/parent are computed and reported, NOT
// filtered, so same-number-different-type/parent false-friends still surface). Each
// US x UPH pair sharing a GL number is one candidate; the verdict is a heuristic hint
// the human confirms. Deterministic order (GL number, then US then UPH source_acct).
func buildMergeCandidates(accMap []AccountMap, usSub, uphSub string) []mergeCandidate {
	usByGL := map[string][]AccountMap{}
	uphByGL := map[string][]AccountMap{}
	for _, m := range accMap {
		gl := glNumber(m.SourceAcct)
		if gl == "" {
			continue
		}
		switch {
		case leafSide(m, usSub):
			usByGL[gl] = append(usByGL[gl], m)
		case leafSide(m, uphSub):
			uphByGL[gl] = append(uphByGL[gl], m)
		}
	}

	var out []mergeCandidate
	for gl, usRows := range usByGL {
		uphRows := uphByGL[gl]
		if len(uphRows) == 0 {
			continue // number present only on the US side -- no pair
		}
		// AMBIGUOUS when either side has >1 leaf for this number (the human must pick).
		ambiguous := len(usRows) > 1 || len(uphRows) > 1
		for _, u := range usRows {
			for _, h := range uphRows {
				c := mergeCandidate{
					gl:         gl,
					usAcct:     u.SourceAcct,
					usNameEN:   u.NameEN,
					uphAcct:    h.SourceAcct,
					uphNameES:  h.NameES,
					sameParent: u.CuentoParent == h.CuentoParent,
					cuentoType: u.CuentoType,
					parent:     u.CuentoParent,
				}
				c.verdict = candidateVerdict(gl, u, h, ambiguous)
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].gl != out[j].gl {
			// numeric-aware: GL numbers are equal-width integers, string order suffices,
			// but sort by integer value to be safe against varying widths.
			ii, _ := strconv.Atoi(out[i].gl)
			jj, _ := strconv.Atoi(out[j].gl)
			if ii != jj {
				return ii < jj
			}
		}
		if out[i].usAcct != out[j].usAcct {
			return out[i].usAcct < out[j].usAcct
		}
		return out[i].uphAcct < out[j].uphAcct
	})
	return out
}

// candidateVerdict is the heuristic hint, in precedence:
//   - AMBIGUOUS?               : >1 leaf on either side for this GL (human must choose)
//   - FALSE-FRIEND?            : a hardcoded risky GL (cash block / known divergent), OR
//     the two rows' cuento_type differs (a same-number coincidence, not one account)
//   - MERGE?                   : single leaf each side, same type AND same parent
//   - REVIEW-DIFFERENT-PARENT? : same type but different parent (likely-but-verify)
func candidateVerdict(gl string, us, uph AccountMap, ambiguous bool) string {
	if ambiguous {
		return "AMBIGUOUS?"
	}
	if inCashBlock(gl) || typeDivergentFalseFriends[gl] || us.CuentoType != uph.CuentoType {
		return "FALSE-FRIEND?"
	}
	if us.CuentoParent == uph.CuentoParent {
		return "MERGE?"
	}
	return "REVIEW-DIFFERENT-PARENT?"
}

// writeMergeCandidates emits the candidate-pairing CSV (a human-guidance comment
// row, the header, then one row per candidate) and returns the candidate count. The
// leading comment lines start with '#' so the human reading the sheet knows exactly
// what to do; they are NOT part of the machine header (a re-import of this file is not
// a supported path -- the human hand-copies confirmed merges into the mapping).
func writeMergeCandidates(w io.Writer, accMap []AccountMap, usSub, uphSub string) (int, error) {
	cands := buildMergeCandidates(accMap, usSub, uphSub)

	// Human-guidance preamble (comment lines). Written raw so the '#' is not CSV-quoted.
	preamble := "" +
		"# CANDIDATE US<->UPH ACCOUNT MERGES -- REVIEW, do NOT auto-apply.\n" +
		"# For each row: change suggested_verdict to MERGE (collapse the pair into one\n" +
		"# bilingual account) or KEEP (leave the two accounts separate). For a MERGE row,\n" +
		"# fix/fill us_name_en (the English name) and uph_name_es (the Spanish name) so the\n" +
		"# merged account reads correctly in both languages.\n" +
		"# Then, in mapping-accounts.csv, set the UPH row's merge_into column to the US\n" +
		"# row's source_acct (the US row is the canonical/name_en side; the UPH row supplies\n" +
		"# name_es), and put the reviewed names into the two rows' name_en/name_es cells.\n" +
		"# Verdict hints: MERGE?=clean same-type+same-parent pair; FALSE-FRIEND?=same GL#\n" +
		"# but a different physical account (cash block 1110-1130) or divergent type/parent;\n" +
		"# AMBIGUOUS?=more than one leaf shares this GL# on a side (pick which pairs);\n" +
		"# REVIEW-DIFFERENT-PARENT?=same type but different parent tier (verify before merge).\n"
	if _, err := io.WriteString(w, preamble); err != nil {
		return 0, fmt.Errorf("write preamble: %w", err)
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(mergeCandidateCols); err != nil {
		return 0, fmt.Errorf("write header: %w", err)
	}
	for _, c := range cands {
		rec := []string{
			c.gl,
			c.usAcct,
			c.usNameEN,
			c.uphAcct,
			c.uphNameES,
			strconv.FormatBool(c.sameParent),
			c.cuentoType,
			c.parent,
			c.verdict,
		}
		if err := cw.Write(rec); err != nil {
			return 0, fmt.Errorf("write candidate %q: %w", c.gl, err)
		}
	}
	cw.Flush()
	return len(cands), cw.Error()
}
