// Package ids defines a DISTINCT Go type for every entity id in cuento so an
// int64 can never silently stand in for an id and one id type can never be
// passed where another is expected.
//
// Convention (see AGENTS.md):
//
//   - Every entity id is a distinct defined type over int64 (e.g. AccountID).
//     A function that expects an account id takes ids.AccountID, not int64.
//   - Bare int64 appears for an id ONLY at the two conversion boundaries: the
//     SQL scan boundary (the database hands back int64) and the HTTP
//     string-parse boundary. Convert immediately at those boundaries and keep
//     the value typed everywhere else.
//   - Non-id int64 — money minor units, counts, positions, exponents, sort
//     orders — stays int64.
//
// This package is a leaf: it imports nothing from internal/, so internal/db/sqlc,
// internal/store, internal/reports, and internal/web can all import it without a
// cycle. Only the standard library (database/sql, for the nullable-FK helpers) is
// imported.
//
// A defined int type formats, marshals, compares, and scans exactly like int64,
// so this change is purely about compile-time safety and has no behavior change.
package ids

import "database/sql"

// Entity ids. Each is a distinct type over int64.
type (
	// AccountID identifies a row in accounts.
	AccountID int64
	// SplitID identifies a row in splits.
	SplitID int64
	// TransactionID identifies a row in transactions.
	TransactionID int64
	// FundID identifies a row in funds.
	FundID int64
	// ProgramID identifies a row in programs.
	ProgramID int64
	// SubsidiaryID identifies a row in subsidiaries.
	SubsidiaryID int64
	// UserID identifies a row in users.
	UserID int64
	// ReconciliationID identifies a row in reconciliations.
	ReconciliationID int64
	// BudgetPlanID identifies a row in budget_plans.
	BudgetPlanID int64
	// BudgetSplitID identifies a row in budget_splits.
	BudgetSplitID int64
	// ExpenseReportID identifies a row in expense_reports.
	ExpenseReportID int64
	// ExpenseReportLineID identifies a row in expense_report_lines.
	ExpenseReportLineID int64
	// ChangeID identifies a row in changes (the audit change that groups a
	// versioned mutation).
	ChangeID int64
	// ImportBatchID identifies a row in import_batches.
	ImportBatchID int64
	// ImportRowID identifies a row in import_rows.
	ImportRowID int64
	// MappingProfileID identifies a row in mapping_profiles.
	MappingProfileID int64
)

// Note on audit-row PKs: the auto-increment PK of a *_versions table
// (AccountsVersion.ID, SplitsVersion.ID, …) is never passed around as an entity
// id — it identifies a snapshot row, not a business entity — so those stay int64.
// A *_versions.EntityID column, by contrast, IS the typed entity id (e.g.
// AccountsVersion.EntityID is an AccountID) and gets typed when that entity is
// converted.

// Ptr converts a nullable-int64 FK read from the SQL scan boundary into a typed
// pointer id: nil when the column was NULL, otherwise a pointer to the typed id.
// Use it at the store boundary to turn a sqlc model's sql.NullInt64 FK into
// *ids.XID.
func Ptr[T ~int64](v sql.NullInt64) *T {
	if !v.Valid {
		return nil
	}
	id := T(v.Int64)
	return &id
}

// Null converts a typed pointer id back into a sql.NullInt64 for the SQL boundary:
// an invalid NullInt64 when nil, otherwise a valid one carrying the underlying
// int64. It is the inverse of Ptr.
func Null[T ~int64](p *T) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
