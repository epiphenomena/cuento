package main

import (
	"strings"
	"testing"
)

// TestClassCandidatesAggregatesAndHints proves the class-classification sheet builder:
// distinct classes are aggregated with row counts and sample accounts, the current
// program mapping and is_program flag come from cfg.ProgramClasses, and the is_fund hint
// fires on grant-like names -- INCLUDING the accented Spanish needles, whose \u-escaped
// literals must match the UTF-8 bytes of the accented class values. Synthetic values only.
func TestClassCandidatesAggregatesAndHints(t *testing.T) {
	cfg := Config{ProgramClasses: map[string]string{"Summer Camp": "Camp Program"}}
	recs := []Record{
		{Klass: "Summer Camp", Acct: "Revenue"},
		{Klass: "Summer Camp", Acct: "Cash"},
		{Klass: "Summer Camp", Acct: "Revenue"}, // dup account -> not re-sampled
		{Klass: "EDU Grant", Acct: "Grant Rev"},
		{Klass: "Campa\u00f1a Capital", Acct: "Bank"}, // accented -- must hint (Campana)
		{Klass: "Plain Class", Acct: "Misc"},
		{Klass: "", Acct: "Skip"}, // blank klass -> ignored
	}
	cands := buildClassCandidates(recs, cfg, 3)

	byClass := map[string]classCandidate{}
	for _, c := range cands {
		byClass[c.class] = c
	}
	if len(cands) != 4 {
		t.Fatalf("got %d classes, want 4 (blank klass dropped)", len(cands))
	}
	// Deterministic (sorted) order.
	if cands[0].class > cands[1].class || cands[1].class > cands[2].class {
		t.Errorf("candidates not sorted: %q %q %q", cands[0].class, cands[1].class, cands[2].class)
	}

	sc := byClass["Summer Camp"]
	if sc.rowCount != 3 {
		t.Errorf("Summer Camp rowCount = %d, want 3", sc.rowCount)
	}
	if len(sc.samples) != 2 { // Revenue, Cash (the dup Revenue is not re-added)
		t.Errorf("Summer Camp samples = %v, want 2 distinct", sc.samples)
	}
	if sc.curProgram != "Camp Program" || !sc.isProgram {
		t.Errorf("Summer Camp program = %q/%v, want Camp Program/true", sc.curProgram, sc.isProgram)
	}
	if sc.isFundHint {
		t.Errorf("Summer Camp should NOT be hinted as a fund")
	}

	if g := byClass["EDU Grant"]; !g.isFundHint {
		t.Errorf(`"EDU Grant" should be hinted (contains "grant")`)
	}
	if c := byClass["Campa\u00f1a Capital"]; !c.isFundHint {
		t.Errorf(`accented "Campana Capital" should be hinted (matches the \u00f1 needle)`)
	}
	if p := byClass["Plain Class"]; p.isFundHint || p.isProgram {
		t.Errorf("Plain Class should be neither fund nor program: %+v", p)
	}

	// The written sheet carries the header and a '#'-comment preamble.
	var b strings.Builder
	n, hinted, err := writeClassCandidates(&b, recs, cfg, 3)
	if err != nil {
		t.Fatalf("writeClassCandidates: %v", err)
	}
	if n != 4 || hinted != 2 {
		t.Errorf("wrote n=%d hinted=%d, want 4/2", n, hinted)
	}
	out := b.String()
	if !strings.HasPrefix(out, "# CLASS CLASSIFICATION") {
		t.Errorf("sheet missing '#' preamble: %q", out[:40])
	}
	if !strings.Contains(out, strings.Join(classCandidateCols, ",")) {
		t.Errorf("sheet missing header row")
	}
}
