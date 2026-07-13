package bankimport

import (
	"errors"
	"testing"
)

// Parser table tests (p17.2). Every case the step lists is covered: delimiter
// sniffing (comma/semicolon/tab), header detection, the single-signed-amount mode
// vs the debit/credit column PAIR, the sign-flip option, each date format, and a
// malformed row surfacing a per-row error (not a panic, not a file abort). Amounts
// are 2-decimal (exponent 2) unless a case says otherwise.

const exp2 = 2

// baseSingle is a comma, header, single-signed-amount, ISO-date config with
// columns date=0, amount=1, payee=2, memo=3.
func baseSingle() Config {
	return Config{
		Delimiter: DelimiterComma,
		HasHeader: true,
		Amount:    AmountSingle,
		DateFmt:   DateISO,
		DateCol:   0,
		AmountCol: 1,
		PayeeCol:  2,
		MemoCol:   3,
	}
}

func TestParseSingleSignedAmount(t *testing.T) {
	raw := []byte("date,amount,payee,memo\n2025-01-15,100.00,Acme,Invoice 5\n2025-01-16,-42.50,Bob,Refund\n")
	rows, err := Parse(raw, baseSingle(), exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Err != nil {
		t.Fatalf("row 0 err: %v", rows[0].Err)
	}
	if rows[0].Date != "2025-01-15" || rows[0].AmountMinor != 10000 || rows[0].Payee != "Acme" || rows[0].Memo != "Invoice 5" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].AmountMinor != -4250 {
		t.Errorf("row 1 amount = %d, want -4250", rows[1].AmountMinor)
	}
}

func TestParseSignFlip(t *testing.T) {
	cfg := baseSingle()
	cfg.SignFlip = true
	raw := []byte("date,amount,payee,memo\n2025-01-15,100.00,Acme,x\n")
	rows, err := Parse(raw, cfg, exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rows[0].AmountMinor != -10000 {
		t.Errorf("sign-flip amount = %d, want -10000", rows[0].AmountMinor)
	}
}

func TestParseDebitCreditPair(t *testing.T) {
	// date=0, debit=1, credit=2, payee=3. A debit adds (net-debit +), a credit
	// subtracts (net-debit -). Exactly one is filled per row.
	cfg := Config{
		Delimiter: DelimiterComma,
		HasHeader: true,
		Amount:    AmountDebitCredit,
		DateFmt:   DateISO,
		DateCol:   0,
		DebitCol:  1,
		CreditCol: 2,
		PayeeCol:  3,
		MemoCol:   -1,
	}
	raw := []byte("date,debit,credit,payee\n2025-01-15,100.00,,Acme\n2025-01-16,,42.50,Bob\n")
	rows, err := Parse(raw, cfg, exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rows[0].AmountMinor != 10000 {
		t.Errorf("debit row amount = %d, want 10000", rows[0].AmountMinor)
	}
	if rows[1].AmountMinor != -4250 {
		t.Errorf("credit row amount = %d, want -4250", rows[1].AmountMinor)
	}
	if rows[0].Memo != "" {
		t.Errorf("unmapped memo should be empty, got %q", rows[0].Memo)
	}
}

func TestParseDebitCreditSignFlip(t *testing.T) {
	cfg := Config{
		Delimiter: DelimiterComma, HasHeader: true, Amount: AmountDebitCredit,
		DateFmt: DateISO, DateCol: 0, DebitCol: 1, CreditCol: 2, PayeeCol: 3, MemoCol: -1,
		SignFlip: true,
	}
	raw := []byte("date,debit,credit,payee\n2025-01-15,100.00,,Acme\n")
	rows, err := Parse(raw, cfg, exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Debit normally +10000; with sign flip the whole result inverts.
	if rows[0].AmountMinor != -10000 {
		t.Errorf("flipped debit = %d, want -10000", rows[0].AmountMinor)
	}
}

func TestParseDebitCreditBothBlankOrBothFilled(t *testing.T) {
	cfg := Config{
		Delimiter: DelimiterComma, HasHeader: true, Amount: AmountDebitCredit,
		DateFmt: DateISO, DateCol: 0, DebitCol: 1, CreditCol: 2, PayeeCol: 3, MemoCol: -1,
	}
	raw := []byte("date,debit,credit,payee\n2025-01-15,,,Acme\n2025-01-16,10.00,5.00,Bob\n")
	rows, err := Parse(raw, cfg, exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rows[0].Err == nil {
		t.Error("both-blank row should be a per-row error")
	}
	if rows[1].Err == nil {
		t.Error("both-filled row should be a per-row error")
	}
}

func TestParseDelimiterSniffing(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"comma", "date,amount,payee,memo\n2025-01-15,100.00,Acme,x\n"},
		{"semicolon", "date;amount;payee;memo\n2025-01-15;100.00;Acme;x\n"},
		{"tab", "date\tamount\tpayee\tmemo\n2025-01-15\t100.00\tAcme\tx\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseSingle()
			cfg.Delimiter = DelimiterAuto // force sniffing
			rows, err := Parse([]byte(tt.raw), cfg, exp2)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(rows) != 1 || rows[0].Err != nil {
				t.Fatalf("rows=%+v", rows)
			}
			if rows[0].AmountMinor != 10000 || rows[0].Payee != "Acme" {
				t.Errorf("sniffed %s wrong: %+v", tt.name, rows[0])
			}
		})
	}
}

func TestParseHeaderDetection(t *testing.T) {
	raw := "2025-01-15,100.00,Acme,x\n2025-01-16,50.00,Bob,y\n"

	// HasHeader=false: both lines are data.
	cfg := baseSingle()
	cfg.HasHeader = false
	rows, err := Parse([]byte(raw), cfg, exp2)
	if err != nil {
		t.Fatalf("Parse (no header): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("no-header rows = %d, want 2", len(rows))
	}

	// HasHeader=true drops the first line, leaving one data row.
	cfg.HasHeader = true
	rows, err = Parse([]byte(raw), cfg, exp2)
	if err != nil {
		t.Fatalf("Parse (header): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("header rows = %d, want 1 (first line consumed as header)", len(rows))
	}
}

func TestParseDateFormats(t *testing.T) {
	// Disambiguating dates: 2025-03-04 is the 4th of March. In US (MM/DD/YYYY) it
	// is 03/04/2025; in EU (DD/MM/YYYY) it is 04/03/2025. Crucially the EU case
	// uses 13/01/2025 which is INVALID as ISO and as US (no month 13) -- so the EU
	// layout is the ONLY one that parses it, defeating money.ParseDate's always-on
	// ISO fallback masking a wrong layout.
	tests := []struct {
		name   string
		layout DateLayout
		cell   string
		want   string
	}{
		{"iso", DateISO, "2025-03-04", "2025-03-04"},
		{"us", DateUS, "03/04/2025", "2025-03-04"},
		{"eu", DateEU, "13/01/2025", "2025-01-13"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseSingle()
			cfg.DateFmt = tt.layout
			raw := []byte("date,amount,payee,memo\n" + tt.cell + ",100.00,Acme,x\n")
			rows, err := Parse(raw, cfg, exp2)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if rows[0].Err != nil {
				t.Fatalf("row err: %v", rows[0].Err)
			}
			if rows[0].Date != tt.want {
				t.Errorf("%s date = %q, want %q", tt.name, rows[0].Date, tt.want)
			}
		})
	}

	// A US-layout config fed a value that is NOT a valid US date and NOT ISO ->
	// per-row error (13 is not a month).
	cfg := baseSingle()
	cfg.DateFmt = DateUS
	rows, _ := Parse([]byte("date,amount,payee,memo\n13/01/2025,100.00,Acme,x\n"), cfg, exp2)
	if rows[0].Err == nil {
		t.Error("13/01/2025 under US layout should be a per-row date error")
	}
}

func TestParseCurrencySymbolsAndGrouping(t *testing.T) {
	raw := []byte("date,amount,payee,memo\n2025-01-15,\"$1,234.56\",Acme,x\n2025-01-16,(50.00),Bob,y\n")
	rows, err := Parse(raw, baseSingle(), exp2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rows[0].AmountMinor != 123456 {
		t.Errorf("grouped/symbol amount = %d, want 123456", rows[0].AmountMinor)
	}
	if rows[1].AmountMinor != -5000 {
		t.Errorf("parens amount = %d, want -5000", rows[1].AmountMinor)
	}
}

func TestParseMalformedRowIsPerRowError(t *testing.T) {
	// A bad amount and a short row each surface as ParsedRow.Err; a good row in the
	// same file still parses. The parser NEVER aborts the file or panics.
	raw := []byte("date,amount,payee,memo\n2025-01-15,notmoney,Acme,x\n2025-01-16\n2025-01-17,10.00,Good,z\n")
	rows, err := Parse(raw, baseSingle(), exp2)
	if err != nil {
		t.Fatalf("Parse should not file-abort on bad rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].Err == nil {
		t.Error("bad-amount row should have Err")
	}
	if rows[1].Err == nil {
		t.Error("short row (missing amount column) should have Err")
	}
	if rows[2].Err != nil {
		t.Errorf("good row should parse: %v", rows[2].Err)
	}
	if rows[2].AmountMinor != 1000 {
		t.Errorf("good row amount = %d, want 1000", rows[2].AmountMinor)
	}
}

func TestParseEmptyFile(t *testing.T) {
	if _, err := Parse([]byte(""), baseSingle(), exp2); !errors.Is(err, ErrNoRows) {
		t.Errorf("empty file err = %v, want ErrNoRows", err)
	}
	// Header only, no data.
	if _, err := Parse([]byte("date,amount,payee,memo\n"), baseSingle(), exp2); !errors.Is(err, ErrNoRows) {
		t.Errorf("header-only err = %v, want ErrNoRows", err)
	}
}
