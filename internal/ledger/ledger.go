// Package ledger holds cuento's integrity suite: the named checks that verify,
// against a whole database, the invariants the store enforces on write (AGENTS
// rule 7 -- "enforced twice"). Check runs every rule and returns the violations
// it finds; `cuento check` (cmd/cuento) prints them and sets its exit code.
//
// The check SQL are const strings, the ONE sanctioned exception to the sqlc-only
// rule (AGENTS rule 6): they are static, reviewed as a set here, and never
// string-concatenated. Each check is a `{Rule, Severity, SQL}` row whose query
// returns ONE column -- a text detail naming the offending id(s) -- with one row
// per violation. A rule that returns no rows passed. Structuring every check as
// "select the offending rows" keeps the set uniform and each rule's negative
// test trivial (corrupt a copy, assert exactly that rule's Rule appears).
//
// Severity is Error or Warning. Errors mean the books are inconsistent (a bug or
// tampering); Warnings are conditions a human should see but that do not by
// themselves make the ledger wrong (a restricted fund overspent, D23; an
// unmapped 990 leaf, D25). `cuento check` exits non-zero on any Error, and also
// on any Warning under --strict.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// Severity classifies a violation. Errors fail `cuento check` always; Warnings
// fail only under --strict.
type Severity string

const (
	// Error means the ledger is internally inconsistent.
	Error Severity = "error"
	// Warning means a condition a human should review (D23, D25), not a defect.
	Warning Severity = "warning"
)

// Violation is one failing row from one check: the rule that flagged it, its
// severity, and a Detail naming the offending id(s) so an operator can find it.
type Violation struct {
	Rule     string
	Severity Severity
	Detail   string
}

// check is one registered integrity rule. SQL must return exactly one TEXT
// column (the detail) and one row per violation; no rows == the rule passed.
// Structuring every rule this way is what makes the set reviewable and each
// negative test a single-rule assertion.
type check struct {
	Rule     string
	Severity Severity
	SQL      string
}

// checks is THE reviewed registry (AGENTS rule 6): the whole integrity suite in
// one place, in rule-number order. Z8/Z9 are the reconciliation checks, active
// since p16.1 (reconciliations + splits.reconciliation_id landed there, D13).
var checks = []check{
	{Rule: "Z1", Severity: Error, SQL: sqlZ1},
	{Rule: "Z2", Severity: Error, SQL: sqlZ2},
	{Rule: "Z3", Severity: Error, SQL: sqlZ3},
	{Rule: "Z4", Severity: Error, SQL: sqlZ4},
	{Rule: "Z5", Severity: Error, SQL: sqlZ5},
	{Rule: "Z6", Severity: Error, SQL: sqlZ6},
	{Rule: "Z7", Severity: Error, SQL: sqlZ7},
	// Z8/Z9 -- reconciliation checks (D13), active since p16.1: Z8 verifies every
	// cleared split matches its recon's account and currency; Z9 verifies each
	// finalized recon's opening + cleared splits equal its statement balance. On a
	// db with no reconciliation rows both find nothing (check stays clean).
	{Rule: "Z8", Severity: Error, SQL: sqlZ8},
	{Rule: "Z9", Severity: Error, SQL: sqlZ9},
	{Rule: "Z10", Severity: Error, SQL: sqlZ10},
	{Rule: "Z11", Severity: Error, SQL: sqlZ11},
	{Rule: "Z12", Severity: Error, SQL: sqlZ12},
	{Rule: "Z13", Severity: Error, SQL: sqlZ13},
	{Rule: "Z14", Severity: Error, SQL: sqlZ14},
	{Rule: "Z15", Severity: Error, SQL: sqlZ15},
	{Rule: "Z16", Severity: Error, SQL: sqlZ16},
	{Rule: "Z17", Severity: Warning, SQL: sqlZ17},
	{Rule: "Z18", Severity: Warning, SQL: sqlZ18},
	{Rule: "Z19", Severity: Warning, SQL: sqlZ19},
}

// Check runs every registered integrity rule against db and returns the
// violations, in registry (rule-number) order. It uses only reads, opens no
// transaction, and never mutates. A rule whose SQL errors (a genuinely broken
// query or an I/O failure) aborts Check with that error -- Check reports ledger
// violations, not query bugs.
func Check(ctx context.Context, db *sql.DB) ([]Violation, error) {
	var out []Violation
	for _, c := range checks {
		vs, err := runCheck(ctx, db, c)
		if err != nil {
			return nil, fmt.Errorf("ledger check %s: %w", c.Rule, err)
		}
		out = append(out, vs...)
	}
	return out, nil
}

// runCheck executes one rule's SQL and turns each returned row into a Violation.
func runCheck(ctx context.Context, db *sql.DB, c check) ([]Violation, error) {
	rows, err := db.QueryContext(ctx, c.SQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Violation
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, err
		}
		out = append(out, Violation{Rule: c.Rule, Severity: c.Severity, Detail: detail})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// HasErrors reports whether any violation is Error-severity. `cuento check` exits
// non-zero when this is true (regardless of --strict).
func HasErrors(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity == Error {
			return true
		}
	}
	return false
}
