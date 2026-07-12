package main

import (
	"strings"
	"testing"

	"cuento/internal/ledger"
)

// The exit decision is the CLI's whole contract (the task requires it be
// testable without a database): an Error fails ALWAYS; a Warning fails only under
// --strict; a clean run always exits 0.
func TestCheckExitCode(t *testing.T) {
	errV := ledger.Violation{Rule: "Z1", Severity: ledger.Error, Detail: "x"}
	warnV := ledger.Violation{Rule: "Z18", Severity: ledger.Warning, Detail: "y"}

	tests := []struct {
		name   string
		vs     []ledger.Violation
		strict bool
		want   int
	}{
		{"clean not strict", nil, false, 0},
		{"clean strict", nil, true, 0},
		{"warning not strict -> 0", []ledger.Violation{warnV}, false, 0},
		{"warning strict -> 1", []ledger.Violation{warnV}, true, 1},
		{"error not strict -> 1", []ledger.Violation{errV}, false, 1},
		{"error strict -> 1", []ledger.Violation{errV}, true, 1},
		{"error+warning not strict -> 1", []ledger.Violation{warnV, errV}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkExitCode(tt.vs, tt.strict); got != tt.want {
				t.Errorf("checkExitCode(%v, strict=%v) = %d, want %d", tt.vs, tt.strict, got, tt.want)
			}
		})
	}
}

// printViolations output is deterministic (errors before warnings, then by rule)
// and ends with a summary; a clean set prints the clean line.
func TestPrintViolations(t *testing.T) {
	var b strings.Builder
	printViolations(&b, []ledger.Violation{
		{Rule: "Z18", Severity: ledger.Warning, Detail: "fund 2"},
		{Rule: "Z1", Severity: ledger.Error, Detail: "txn 5"},
	})
	out := b.String()
	if !strings.Contains(out, "error Z1: txn 5") || !strings.Contains(out, "warning Z18: fund 2") {
		t.Errorf("missing expected lines:\n%s", out)
	}
	// error line must precede warning line.
	if strings.Index(out, "error Z1") > strings.Index(out, "warning Z18") {
		t.Errorf("errors should sort before warnings:\n%s", out)
	}
	if !strings.Contains(out, "1 error(s), 1 warning(s)") {
		t.Errorf("missing summary:\n%s", out)
	}

	var c strings.Builder
	printViolations(&c, nil)
	if !strings.Contains(c.String(), "clean") {
		t.Errorf("clean output missing 'clean': %s", c.String())
	}
}
