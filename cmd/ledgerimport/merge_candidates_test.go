package main

import (
	"strings"
	"testing"
)

// TestMergeCandidatesClassifier proves the candidate-pairing generator joins US-only
// and UPH-only leaves on their leading GL number ALONE (type/parent computed, not
// filtered) and classifies each pair with the documented precedence. Synthetic-only
// values that mirror the real risky-GL shapes (rule 11).
func TestMergeCandidatesClassifier(t *testing.T) {
	rows := []AccountMap{
		// Clean pair: same GL, same type, same parent -> MERGE?.
		{SourceAcct: "1200 AR:US AR", CuentoType: "asset", CuentoParent: "::typ:asset:Receivables", Subsidiaries: []string{"US"}, NameEN: "US AR"},
		{SourceAcct: "1200 CxC:HN CxC", CuentoType: "asset", CuentoParent: "::typ:asset:Receivables", Subsidiaries: []string{"UPH"}, NameES: "Cuentas por Cobrar HN"},
		// Cash block: same type+parent but a FALSE-FRIEND (different physical account).
		// The block is a CONTIGUOUS RANGE 1110-1130, so an INTERIOR number (1123) is
		// flagged too -- not just the endpoints (regression guard: a discrete {1110,
		// 1120, 1130} set would wrongly wave 1123 through as MERGE?).
		{SourceAcct: "1110 Cash:BofA", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"US"}, NameEN: "BofA"},
		{SourceAcct: "1110 Efectivo:Caja", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"UPH"}, NameES: "Caja"},
		{SourceAcct: "1123 Cash:Petty Cash", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"US"}, NameEN: "Petty Cash"},
		{SourceAcct: "1123 Efectivo:Banco Ahorros", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"UPH"}, NameES: "Banco Ahorros"},
		// GL 1131 is JUST outside the block -> a normal MERGE? (proves the range is closed).
		{SourceAcct: "1131 Cash:Money Market", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"US"}, NameEN: "Money Market"},
		{SourceAcct: "1131 Efectivo:Mercado", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"UPH"}, NameES: "Mercado Monetario"},
		// Same type, DIFFERENT parent -> REVIEW-DIFFERENT-PARENT?.
		{SourceAcct: "3000 A:US thing", CuentoType: "expense", CuentoParent: "::typ:expense:Office", Subsidiaries: []string{"US"}, NameEN: "US thing"},
		{SourceAcct: "3000 B:HN thing", CuentoType: "expense", CuentoParent: "::typ:expense:IT", Subsidiaries: []string{"UPH"}, NameES: "cosa HN"},
		// Different TYPE, same GL -> FALSE-FRIEND? (a coincidence, not one account).
		{SourceAcct: "4000 X:US asset", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"US"}, NameEN: "US asset"},
		{SourceAcct: "4000 Y:HN rev", CuentoType: "revenue", CuentoParent: "::typ:revenue:Donations", Subsidiaries: []string{"UPH"}, NameES: "ingreso HN"},
		// Two US leaves share a GL -> AMBIGUOUS? for every pair.
		{SourceAcct: "5000 P:US one", CuentoType: "expense", CuentoParent: "::typ:expense:Program", Subsidiaries: []string{"US"}, NameEN: "US one"},
		{SourceAcct: "5000 Q:US two", CuentoType: "expense", CuentoParent: "::typ:expense:Program", Subsidiaries: []string{"US"}, NameEN: "US two"},
		{SourceAcct: "5000 R:HN one", CuentoType: "expense", CuentoParent: "::typ:expense:Program", Subsidiaries: []string{"UPH"}, NameES: "HN uno"},
		// US-only number with NO UPH partner -> no candidate row.
		{SourceAcct: "9999 Z:US alone", CuentoType: "asset", CuentoParent: "::typ:asset:Cash", Subsidiaries: []string{"US"}, NameEN: "alone"},
		// A ::typ: tier (no numeric prefix) is skipped.
		{SourceAcct: "::typ:asset:Cash", CuentoType: "asset", Subsidiaries: []string{"US", "UPH"}, NameEN: "Cash"},
	}
	got := buildMergeCandidates(rows, "US", "UPH")

	want := map[string]string{ // us_source_acct -> expected verdict
		"1200 AR:US AR":          "MERGE?",
		"1110 Cash:BofA":         "FALSE-FRIEND?",
		"1123 Cash:Petty Cash":   "FALSE-FRIEND?", // interior of the 1110-1130 range
		"1131 Cash:Money Market": "MERGE?",        // just outside the range
		"3000 A:US thing":        "REVIEW-DIFFERENT-PARENT?",
		"4000 X:US asset":        "FALSE-FRIEND?",
	}
	byUS := map[string][]mergeCandidate{}
	for _, c := range got {
		byUS[c.usAcct] = append(byUS[c.usAcct], c)
	}
	for usAcct, verdict := range want {
		cs := byUS[usAcct]
		if len(cs) != 1 {
			t.Fatalf("%q produced %d candidate rows, want 1", usAcct, len(cs))
		}
		if cs[0].verdict != verdict {
			t.Errorf("%q verdict = %q, want %q", usAcct, cs[0].verdict, verdict)
		}
	}
	// Both 5000 US leaves pair with the one UPH leaf, both AMBIGUOUS?.
	amb := 0
	for _, c := range got {
		if c.gl == "5000" {
			amb++
			if c.verdict != "AMBIGUOUS?" {
				t.Errorf("GL 5000 pair verdict = %q, want AMBIGUOUS?", c.verdict)
			}
		}
	}
	if amb != 2 {
		t.Errorf("GL 5000 produced %d pairs, want 2 (both US leaves x the UPH leaf)", amb)
	}
	// The lone US GL 9999 and the ::typ: tier produced NO rows.
	for _, c := range got {
		if c.gl == "9999" || c.gl == "" {
			t.Errorf("unexpected candidate for gl %q (no UPH partner / not a leaf)", c.gl)
		}
	}

	// The written sheet carries the human-guidance preamble and the header.
	var b strings.Builder
	if _, err := writeMergeCandidates(&b, rows, "US", "UPH"); err != nil {
		t.Fatalf("writeMergeCandidates: %v", err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "# CANDIDATE") {
		t.Errorf("sheet missing guidance preamble; got %q...", out[:40])
	}
	if !strings.Contains(out, strings.Join(mergeCandidateCols, ",")) {
		t.Errorf("sheet missing the column header")
	}
}
