package bankimport

import (
	"errors"
	"reflect"
	"testing"
)

// p26.64 horizontal-column-mapping tests: reading a file's columns for display, and
// the two-way conversion between per-column roles and the index-based Config.

// TestColumnsWithHeader: the header row supplies the names and the second row the
// sample.
func TestColumnsWithHeader(t *testing.T) {
	raw := []byte("date,amount,desc,memo\n2025-01-15,100.00,Acme,Invoice\n2025-01-16,-5,Bob,x\n")
	cols, err := Columns(raw, DelimiterComma, true)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	want := []ColumnInfo{
		{Index: 0, Name: "date", Sample: "2025-01-15"},
		{Index: 1, Name: "amount", Sample: "100.00"},
		{Index: 2, Name: "desc", Sample: "Acme"},
		{Index: 3, Name: "memo", Sample: "Invoice"},
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("cols = %+v, want %+v", cols, want)
	}
}

// TestColumnsNoHeader: names are synthesized "Column N" and the FIRST row is the sample.
func TestColumnsNoHeader(t *testing.T) {
	raw := []byte("2025-01-15,100.00,Acme\n2025-01-16,-5,Bob\n")
	cols, err := Columns(raw, DelimiterComma, false)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	want := []ColumnInfo{
		{Index: 0, Name: "Column 1", Sample: "2025-01-15"},
		{Index: 1, Name: "Column 2", Sample: "100.00"},
		{Index: 2, Name: "Column 3", Sample: "Acme"},
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("cols = %+v, want %+v", cols, want)
	}
}

// TestColumnsAutoDelimiter: the delimiter is sniffed when not pinned.
func TestColumnsAutoDelimiter(t *testing.T) {
	raw := []byte("date;amount;desc\n2025-01-15;100.00;Acme\n")
	cols, err := Columns(raw, DelimiterAuto, true)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(cols) != 3 || cols[1].Name != "amount" {
		t.Fatalf("auto-delimiter cols = %+v, want 3 semicolon-split columns", cols)
	}
}

// TestColumnsEmpty: a file with no rows is ErrNoRows.
func TestColumnsEmpty(t *testing.T) {
	if _, err := Columns([]byte(""), DelimiterComma, true); !errors.Is(err, ErrNoRows) {
		t.Fatalf("empty Columns err = %v, want ErrNoRows", err)
	}
}

// TestConfigFromRolesSingleMode: date/amount/desc roles map to the right indices; the
// unclaimed debit/credit land as -1.
func TestConfigFromRolesSingleMode(t *testing.T) {
	roles := []Role{RoleDate, RoleAmount, RoleDescription, RoleIgnore}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountSingle, false, DateISO)
	if cfg.DateCol != 0 || cfg.AmountCol != 1 || cfg.DescCol != 2 {
		t.Fatalf("cfg = %+v, want date=0 amount=1 desc=2", cfg)
	}
	if cfg.DebitCol != -1 || cfg.CreditCol != -1 || cfg.MemoCol != -1 {
		t.Fatalf("cfg unclaimed cols = %+v, want all -1", cfg)
	}
	if cfg.Delimiter != DelimiterComma || !cfg.HasHeader || cfg.Amount != AmountSingle || cfg.DateFmt != DateISO {
		t.Fatalf("cfg settings not carried: %+v", cfg)
	}
}

// TestConfigFromRolesDebitCredit: debit and credit map to their columns.
func TestConfigFromRolesDebitCredit(t *testing.T) {
	roles := []Role{RoleDate, RoleDebit, RoleCredit, RoleDescription}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountDebitCredit, true, DateUS)
	if cfg.DateCol != 0 || cfg.DebitCol != 1 || cfg.CreditCol != 2 || cfg.DescCol != 3 {
		t.Fatalf("cfg = %+v, want date=0 debit=1 credit=2 desc=3", cfg)
	}
	if cfg.AmountCol != -1 {
		t.Fatalf("cfg.AmountCol = %d, want -1 (unclaimed in debit/credit)", cfg.AmountCol)
	}
	if !cfg.SignFlip || cfg.Amount != AmountDebitCredit {
		t.Fatalf("cfg mode/signflip not carried: %+v", cfg)
	}
}

// TestConfigFromRolesLastWins: two columns claiming the same role -> the last wins.
func TestConfigFromRolesLastWins(t *testing.T) {
	roles := []Role{RoleAmount, RoleAmount}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountSingle, false, DateISO)
	if cfg.AmountCol != 1 {
		t.Fatalf("cfg.AmountCol = %d, want 1 (last claimant wins)", cfg.AmountCol)
	}
}

// TestRolesFromConfigRoundTrip: RolesFromConfig is the inverse of ConfigFromRoles for
// the mapping-UI roles (date/desc/amount/debit/credit).
func TestRolesFromConfigRoundTrip(t *testing.T) {
	roles := []Role{RoleDate, RoleAmount, RoleDescription, RoleIgnore}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountSingle, false, DateISO)
	got := RolesFromConfig(cfg, 4)
	if !reflect.DeepEqual(got, roles) {
		t.Fatalf("round trip roles = %+v, want %+v", got, roles)
	}
}

// TestConfigFromRolesMemoOptional: a Memo role maps to MemoCol; with no Memo role the
// MemoCol stays unmapped (-1) -- memo is optional, an import with no memo column is fine
// (p26.65).
func TestConfigFromRolesMemoOptional(t *testing.T) {
	// WITH a memo column mapped.
	roles := []Role{RoleDate, RoleAmount, RoleDescription, RoleMemo}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountSingle, false, DateISO)
	if cfg.MemoCol != 3 {
		t.Fatalf("cfg.MemoCol = %d, want 3 (memo column mapped)", cfg.MemoCol)
	}

	// WITHOUT a memo column: MemoCol is unmapped (-1), not an error.
	noMemo := []Role{RoleDate, RoleAmount, RoleDescription, RoleIgnore}
	cfgNo := ConfigFromRoles(noMemo, DelimiterComma, true, AmountSingle, false, DateISO)
	if cfgNo.MemoCol != -1 {
		t.Fatalf("cfgNo.MemoCol = %d, want -1 (no memo column mapped)", cfgNo.MemoCol)
	}
}

// TestRolesFromConfigMemoRoundTrip: a mapped memo column round-trips through
// ConfigFromRoles -> RolesFromConfig, so a saved profile PRE-SELECTS the memo dropdown
// (p26.65 restored memo as an optional role).
func TestRolesFromConfigMemoRoundTrip(t *testing.T) {
	roles := []Role{RoleDate, RoleAmount, RoleDescription, RoleMemo}
	cfg := ConfigFromRoles(roles, DelimiterComma, true, AmountSingle, false, DateISO)
	got := RolesFromConfig(cfg, 4)
	if !reflect.DeepEqual(got, roles) {
		t.Fatalf("memo round trip roles = %+v, want %+v", got, roles)
	}
}

// TestRolesFromConfigOutOfRange: a profile column index >= this file's column count
// (a profile built for a WIDER file) lands no role and never panics.
func TestRolesFromConfigOutOfRange(t *testing.T) {
	cfg := Config{DateCol: 0, AmountCol: 5, DescCol: -1, DebitCol: -1, CreditCol: -1, MemoCol: -1}
	got := RolesFromConfig(cfg, 3) // file has only 3 columns; AmountCol=5 is out of range
	want := []Role{RoleDate, RoleIgnore, RoleIgnore}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("out-of-range roles = %+v, want %+v (amount dropped)", got, want)
	}
}
