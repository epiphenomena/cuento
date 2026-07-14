package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
)

// Payee autocomplete + template autofill (p12.3). The suggest ranking and the
// last-transaction lookup are READ-ONLY (rule 2 permits reads via sqlc). The web
// handler renders what these return; it never re-derives ranking or splits.

// PayeeSuggestion is one ranked autocomplete result: the payee id and name. The
// ordering is carried by the slice position (most-recent-first), so the handler
// renders it verbatim without re-sorting.
type PayeeSuggestion struct {
	ID   int64
	Name string
}

// SuggestPayees is RETAINED but no longer wired to the web (p26.3): the header payee is
// now a single client-side combobox filtering the full payee option list, so the
// /payees/suggest fragment was removed. The store method + its sqlc query + test are
// kept intact (a reusable prefix-ranked reader; no dead generated-SQL churn to retire).
//
// SuggestPayees returns active payees whose name PREFIX-matches q (case-insensitive;
// payees.name is COLLATE NOCASE), ranked MOST-RECENT-FIRST by the payee's latest
// non-deleted transaction date; payees never used (or with only deleted txns) rank
// last, then by name (D-choice recorded in DECISIONS p12.3). An empty/whitespace q
// returns nothing (autocomplete shows suggestions only once the user types). LIKE
// metacharacters in q are escaped so a literal % or _ is matched as itself, keeping
// the match a true prefix.
func (s *Store) SuggestPayees(ctx context.Context, q string) ([]PayeeSuggestion, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	rows, err := s.q.SuggestPayees(ctx, likePrefix(q))
	if err != nil {
		return nil, fmt.Errorf("store: suggest payees: %w", err)
	}
	out := make([]PayeeSuggestion, 0, len(rows))
	for _, r := range rows {
		out = append(out, PayeeSuggestion{ID: r.ID, Name: r.Name})
	}
	return out, nil
}

// likePrefix builds a LIKE pattern matching values that START WITH q. The default
// SQLite LIKE has no ESCAPE character (and sqlc rejects an explicit ESCAPE clause),
// so any %, _ or backslash in q is neutralized by mapping it to the single-char
// wildcard '_' -- this keeps the match a prefix (never a mid-string wildcard) for the
// rare payee name containing a metacharacter, without an escape clause. Autocomplete
// tolerates that a literal '_' also matches any one char; a false wildcard would not.
func likePrefix(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 1)
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

// LookupPayeeByName returns the id of the payee whose name matches (case-insensitive
// -- payees.name is UNIQUE COLLATE NOCASE), or 0 with no error when the name is new
// or blank. READ-ONLY (never creates): the p17.3 edit&post prefill matches a parsed
// payee to a known payee at GET without minting a payee (creation happens on save via
// EnsurePayee).
func (s *Store) LookupPayeeByName(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	p, err := s.q.GetPayeeByName(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: lookup payee %q: %w", name, err)
	}
	return p.ID, nil
}

// EnsurePayee find-or-creates a payee by name (p12.3 create-on-save): it returns the
// id of the existing payee whose name matches (case-insensitively -- payees.name is
// UNIQUE COLLATE NOCASE), or creates one and returns the new id. Creation is its OWN
// versioned change (reusing CreatePayee), kept SEPARATE from the transaction write so a
// later txn-validation failure leaves the payee harmlessly present and reusable on
// retry. A blank name returns 0 (no payee) with no error -- an untagged transaction.
func (s *Store) EnsurePayee(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	existing, err := s.q.GetPayeeByName(ctx, name)
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: lookup payee %q: %w", name, err)
	}
	id, err := s.CreatePayee(ctx, name)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// PayeeTemplate is a payee's last non-deleted transaction, used to prefill the
// editor grid (p12.3): the transaction's currency and its live split set (in display
// order). Found is false when the payee has no non-deleted transaction (never used or
// only deleted) -- the handler then renders no template rows.
type PayeeTemplate struct {
	Found    bool
	Currency string
	Splits   []sqlc.Split
}

// PayeeLastTransactionTemplate returns the payee's LAST non-deleted transaction's
// currency + splits (p12.3 autofill). It finds the greatest (date, id) live
// transaction for the payee, then REUSES SplitsByTransaction (the existing splits
// reader) to read its splits in display order. No rows -> Found=false.
func (s *Store) PayeeLastTransactionTemplate(ctx context.Context, payeeID int64) (PayeeTemplate, error) {
	last, err := s.q.LastTransactionForPayee(ctx, sql.NullInt64{Int64: payeeID, Valid: true})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PayeeTemplate{Found: false}, nil
		}
		return PayeeTemplate{}, fmt.Errorf("store: last transaction for payee %d: %w", payeeID, err)
	}
	splits, err := s.q.SplitsByTransaction(ctx, last.ID)
	if err != nil {
		return PayeeTemplate{}, fmt.Errorf("store: template splits for payee %d: %w", payeeID, err)
	}
	return PayeeTemplate{Found: true, Currency: last.Currency, Splits: splits}, nil
}
