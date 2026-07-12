package main

import (
	"strings"
	"testing"
)

// header is the 22-column export header row (docs/ledger-export.md order). Reused
// by the synthetic CSV builders below. ALL values in these tests are INVENTED --
// no real export value is ever read (AGENTS rule 11).
const header = "country,stmt,typ,acct,kat,dt,v,ndb,fndb,kls,klass,tid,desc,donor,currency,xrt,db,fdb,cr,fcr,clr,parent"

func TestParseRecordsShape(t *testing.T) {
	// A synthetic 3-line export exercising the doc's hazards: a quoted memo with
	// an embedded comma AND an embedded newline (record 1), plain dot-decimal
	// amounts, and the float-noisy net columns (v/ndb/fndb) filled with garbage
	// that MUST be ignored.
	csv := header + "\n" +
		`US,A,deposit,Checking,PROG,2025-01-05,999.999999999,111.111111111,0,mgmt,detail,1,"memo, with comma` + "\n" +
		`and a newline",DONOR1,USD,1.0,100.00,100.00,0,0,R,Assets` + "\n" +
		`US,I,deposit,Grant Revenue,PROG,2025-01-05,-0.00000001,-42.4242424242,0,,det,1,plain memo,DONOR1,USD,1.0,0,0,100.00,100.00,,Revenue` + "\n"

	recs, err := ParseRecords(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}

	r0 := recs[0]
	if r0.Country != "US" || r0.Stmt != "A" || r0.Acct != "Checking" || r0.Parent != "Assets" {
		t.Errorf("record 0 fields wrong: %+v", r0)
	}
	if !strings.Contains(r0.Desc, "memo, with comma") || !strings.Contains(r0.Desc, "newline") {
		t.Errorf("record 0 memo did not preserve comma+newline: %q", r0.Desc)
	}
	if r0.Db != "100.00" || r0.Cr != "0" {
		t.Errorf("record 0 amounts wrong: db=%q cr=%q", r0.Db, r0.Cr)
	}
	if r0.Currency != "USD" || r0.Tid != "1" || r0.Donor != "DONOR1" {
		t.Errorf("record 0 dims wrong: %+v", r0)
	}

	r1 := recs[1]
	if r1.Db != "0" || r1.Cr != "100.00" {
		t.Errorf("record 1 amounts wrong: db=%q cr=%q", r1.Db, r1.Cr)
	}
}

func TestParseRecordsRejectsWrongFieldCount(t *testing.T) {
	// A row with only 3 fields must be a hard error (FieldsPerRecord pinned to 22).
	bad := header + "\n" + "US,A,deposit\n"
	if _, err := ParseRecords(strings.NewReader(bad)); err == nil {
		t.Fatal("want error on short row, got nil")
	}
}

func TestNetDebitFromAuthoritativeColumns(t *testing.T) {
	// db/cr are authoritative; the sign is net-debit (debit +, credit -). Blank
	// sides map to 0. Exponent 2 (USD/MXN).
	cases := []struct {
		db, cr string
		want   int64
	}{
		{"100.00", "0", 10000},
		{"0", "100.00", -10000},
		{"", "42.50", -4250},
		{"42.50", "", 4250},
		{"1.2", "0", 120}, // one-decimal amounts (doc: <= 2 decimals)
		{"0", "0", 0},
	}
	for _, c := range cases {
		got, err := NetDebit(c.db, c.cr, 2)
		if err != nil {
			t.Fatalf("NetDebit(%q,%q): %v", c.db, c.cr, err)
		}
		if got != c.want {
			t.Errorf("NetDebit(%q,%q) = %d, want %d", c.db, c.cr, got, c.want)
		}
	}
}

func TestNetDebitIgnoresFloatNoisyColumns(t *testing.T) {
	// Prove the parser NEVER lets the float-noisy ndb into the amount: a record
	// whose ndb column is wildly wrong but whose authoritative db/cr are exact
	// must still yield the exact net-debit from db/cr.
	// Use the 22-field row() helper (build_test.go) which fills v/ndb/fndb with
	// obvious garbage; assert the amount still comes from the authoritative db/cr.
	csv := header + "\n" +
		row("US", "A", "x", "Checking", "", "2025-02-01", "", "7", "m", "", "USD", "1.0", "50.00", "50.00", "0", "0", "Assets") + "\n"
	recs, err := ParseRecords(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	got, err := NetDebit(recs[0].Db, recs[0].Cr, 2)
	if err != nil {
		t.Fatalf("NetDebit: %v", err)
	}
	if got != 5000 {
		t.Errorf("net-debit = %d, want 5000 (from db/cr, not ndb)", got)
	}
}
