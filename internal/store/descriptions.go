package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Per-split description autocomplete + per-row prefill (p26.18). Step 4a of the
// payee->per-split-description migration: these READ-ONLY store methods (rule 2
// permits reads via sqlc) feed the entry-grid description field. The web handlers
// render what these return; they never re-derive ranking or fields. This REPLACES
// the payee autofill (SuggestPayees / PayeeLastTransactionTemplate) at the split
// level -- the whole-grid payee template becomes per-ROW description prefill.

// SuggestDescriptions returns up to 10 DISTINCT non-empty split descriptions whose
// text SUBSTRING-matches q (case-insensitive), across non-deleted transactions,
// ranked most-recently-used first. p26.38: SCOPED to subsidiary sub (filter, not
// prefer) so subsidiary A never surfaces B's descriptions; sub == 0 means "no
// subsidiary chosen yet" -> unscoped (all subs). A blank q returns nothing
// (autocomplete shows suggestions only once the user types). LIKE metacharacters in q
// are neutralized so a literal % or _ is matched as itself.
func (s *Store) SuggestDescriptions(ctx context.Context, q string, sub ids.SubsidiaryID) ([]string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	rows, err := s.q.SuggestDescriptions(ctx, sqlc.SuggestDescriptionsParams{
		Description:  likeContains(q),
		SubsidiaryID: sub,
	})
	if err != nil {
		return nil, fmt.Errorf("store: suggest descriptions: %w", err)
	}
	return rows, nil
}

// DescriptionPrefill is the most-recent split carrying an exact description, used to
// prefill one editor ROW (p26.18). Found is false when no non-deleted split has that
// exact description. Fund/Program are 0 when the split carried none (unrestricted /
// no program); Class is "" when none (non-expense). Currency is the matched split's
// TRANSACTION currency -- the true scale of Amount's minor units -- so the handler
// formats Amount consistently with how the editor renders split amounts.
type DescriptionPrefill struct {
	Found     bool
	AccountID int64
	Amount    int64         // signed minor units (net-debit sign, D1/D2)
	FundID    ids.FundID    // 0 == unrestricted
	ProgramID ids.ProgramID // 0 == none
	Class     string
	Memo      string
	Currency  string
}

// PrefillDescription returns the MOST-RECENT non-deleted split whose description
// EQUALS q exactly. p26.38: SCOPED to subsidiary sub (filter, not prefer) so a prefill
// in subsidiary A never pulls B's account/fund/program; sub == 0 (no subsidiary chosen
// yet) is unscoped. A blank q, or no exact match, returns Found=false. The returned
// account/fund/program may now be inactive within sub -- the method returns it
// regardless (the editor's p26.10 option injection + save-time guard handle
// display/validation); the caller notes this contract.
func (s *Store) PrefillDescription(ctx context.Context, q string, sub ids.SubsidiaryID) (DescriptionPrefill, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return DescriptionPrefill{Found: false}, nil
	}
	row, err := s.q.PrefillDescription(ctx, sqlc.PrefillDescriptionParams{
		Description:  q,
		SubsidiaryID: sub,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DescriptionPrefill{Found: false}, nil
		}
		return DescriptionPrefill{}, fmt.Errorf("store: prefill description: %w", err)
	}
	out := DescriptionPrefill{
		Found:     true,
		AccountID: row.AccountID,
		Amount:    row.Amount,
		Class:     nullStr(row.FunctionalClass),
		Memo:      row.Memo,
		Currency:  row.Currency,
	}
	if row.FundID.Valid {
		out.FundID = ids.FundID(row.FundID.Int64)
	}
	if row.ProgramID.Valid {
		out.ProgramID = ids.ProgramID(row.ProgramID.Int64)
	}
	return out, nil
}

// nullStr unwraps a NullString to its value or "" when NULL.
func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// likeContains builds a LIKE pattern matching values that CONTAIN q as a substring
// ('%' + q + '%'). It neutralizes LIKE metacharacters
// (%, _, backslash) in q by mapping each to the single-char wildcard '_' -- the
// default SQLite LIKE has no ESCAPE character and sqlc rejects an explicit ESCAPE
// clause, so a literal % or _ in the query is kept from acting as a wildcard without
// an escape clause. Autocomplete tolerates that a literal '_' also matches any one
// char; a false wildcard would not.
func likeContains(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 2)
	b.WriteByte('%')
	for _, r := range q {
		switch r {
		case '%', '_', '\\':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('%')
	return b.String()
}
