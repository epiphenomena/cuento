package store

import (
	"context"
	"database/sql"
	"fmt"

	"cuento/internal/db/sqlc"
)

// Balance queries (p08.4) -- the READ-ONLY backbone of registers, fund pages,
// program pages, and the report toolkit (Appendix E over these, p15.2). Every
// method is a pure read via sqlc (rule 6); amounts stay int64 minor units
// (rule 3, D2 net-debit signs) -- the SQL CASTs each SUM to INTEGER so sqlc never
// types money as float. Scope (D18) is a subsidiary consolidated with ALL its
// descendants: each query closes the descendant set with a recursive CTE over
// subsidiaries and joins on subsidiary_id IN (closure). Only NON-DELETED
// transactions count (t.deleted = 0) in every query. The unrestricted fund group
// (NULL fund_id, D20) is represented as fund id 0 (COALESCE), matching Appendix
// E's zero-FundID convention.

// AccountCurrencyAmount is one (account, currency) balance or activity cell.
type AccountCurrencyAmount struct {
	AccountID int64
	Currency  string
	Amount    int64 // signed minor units (net-debit, D2)
}

// FundCurrencyAmount is one (fund, currency) balance cell. FundID 0 is the
// unrestricted group (D20).
type FundCurrencyAmount struct {
	FundID   int64 // 0 = unrestricted
	Currency string
	Amount   int64
}

// FunctionalCell is one (expense account, functional class, currency) activity
// cell (D21). Only expense splits carry a class.
type FunctionalCell struct {
	AccountID       int64
	FunctionalClass string
	Currency        string
	Amount          int64
}

// ProgramCell is one (program, account, currency) activity cell (D24). Only
// revenue/expense splits carry a program. Rows are raw per (program, account) --
// the tree rollup is the report layer's job.
type ProgramCell struct {
	ProgramID int64
	AccountID int64
	Currency  string
	Amount    int64
}

// SubtreeBalancesAsOf returns, per (account, currency), the cumulative signed
// balance of non-deleted splits whose transaction date <= asof and whose
// subsidiary is in scopeSub's descendant closure (D18). Balances are cumulative
// to the date.
func (s *Store) SubtreeBalancesAsOf(ctx context.Context, asof string, scopeSub int64) ([]AccountCurrencyAmount, error) {
	rows, err := s.q.SubtreeBalancesAsOf(ctx, sqlc.SubtreeBalancesAsOfParams{
		ID:   scopeSub,
		Date: asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: subtree balances as of %s (scope %d): %w", asof, scopeSub, err)
	}
	out := make([]AccountCurrencyAmount, len(rows))
	for i, r := range rows {
		out[i] = AccountCurrencyAmount{AccountID: r.AccountID, Currency: r.Currency, Amount: r.Balance}
	}
	return out, nil
}

// PeriodActivity returns, per (account, currency), the signed activity over the
// closed interval from <= date <= to in scopeSub's descendant closure.
func (s *Store) PeriodActivity(ctx context.Context, from, to string, scopeSub int64) ([]AccountCurrencyAmount, error) {
	rows, err := s.q.PeriodActivity(ctx, sqlc.PeriodActivityParams{
		ID:     scopeSub,
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: period activity %s..%s (scope %d): %w", from, to, scopeSub, err)
	}
	out := make([]AccountCurrencyAmount, len(rows))
	for i, r := range rows {
		out[i] = AccountCurrencyAmount{AccountID: r.AccountID, Currency: r.Currency, Amount: r.Activity}
	}
	return out, nil
}

// FundBalancesAsOf returns, per (fund, currency), the fund's cumulative
// unexpended balance to asof in scopeSub's descendant closure, INCLUDING the
// unrestricted group (fund id 0, D20). It is the ASSET-side sum: a whole-fund sum
// is identically zero (D20/Z10 fund conservation), so the balance is the fund's
// cash/asset position = unexpended restricted resources (Z18 precedent, recorded
// as p08.4 in docs/DECISIONS.md).
func (s *Store) FundBalancesAsOf(ctx context.Context, asof string, scopeSub int64) ([]FundCurrencyAmount, error) {
	rows, err := s.q.FundBalancesAsOf(ctx, sqlc.FundBalancesAsOfParams{
		ID:   scopeSub,
		Date: asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fund balances as of %s (scope %d): %w", asof, scopeSub, err)
	}
	out := make([]FundCurrencyAmount, len(rows))
	for i, r := range rows {
		out[i] = FundCurrencyAmount{FundID: r.FundID, Currency: r.Currency, Amount: r.Balance}
	}
	return out, nil
}

// FunctionalActivity returns, per (expense account, functional class, currency),
// the signed activity over from <= date <= to in scopeSub's descendant closure.
// Only expense splits carry a class (D21), so the result contains exactly the
// expense activity, keyed by class.
func (s *Store) FunctionalActivity(ctx context.Context, from, to string, scopeSub int64) ([]FunctionalCell, error) {
	rows, err := s.q.FunctionalActivity(ctx, sqlc.FunctionalActivityParams{
		ID:     scopeSub,
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: functional activity %s..%s (scope %d): %w", from, to, scopeSub, err)
	}
	out := make([]FunctionalCell, len(rows))
	for i, r := range rows {
		// functional_class is NOT NULL-filtered in SQL, so Valid is always true.
		out[i] = FunctionalCell{
			AccountID:       r.AccountID,
			FunctionalClass: r.FunctionalClass.String,
			Currency:        r.Currency,
			Amount:          r.Activity,
		}
	}
	return out, nil
}

// ProgramActivity returns, per (program, account, currency), the signed activity
// over from <= date <= to in scopeSub's descendant closure. Only revenue/expense
// splits carry a program (D24). Rows are raw per (program, account) -- the tree
// rollup is the report layer's job (rollup-ready).
func (s *Store) ProgramActivity(ctx context.Context, from, to string, scopeSub int64) ([]ProgramCell, error) {
	rows, err := s.q.ProgramActivity(ctx, sqlc.ProgramActivityParams{
		ID:     scopeSub,
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: program activity %s..%s (scope %d): %w", from, to, scopeSub, err)
	}
	out := make([]ProgramCell, len(rows))
	for i, r := range rows {
		// program_id is NOT NULL-filtered in SQL, so Valid is always true.
		out[i] = ProgramCell{
			ProgramID: r.ProgramID.Int64,
			AccountID: r.AccountID,
			Currency:  r.Currency,
			Amount:    r.Activity,
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// RegisterPage: the account register with a per-currency running balance and
// keyset (seek) pagination.
// ---------------------------------------------------------------------------

// RegisterFilters are the optional register filters. A zero/empty field means
// "no filter on this dimension". Text matches (case-insensitively, substring)
// against the transaction memo, the split memo, or the payee name.
type RegisterFilters struct {
	From       string // "" = no lower bound (YYYY-MM-DD)
	To         string // "" = no upper bound (YYYY-MM-DD)
	Text       string // "" = no text filter
	FundID     *int64 // nil = any fund; a fund id filters to that fund's splits
	Subsidiary *int64 // nil = any subsidiary
	ProgramID  *int64 // nil = any program
}

// RegisterCursor is the keyset position: the (Date, SplitID) of the LAST row of
// the previous page. The zero value (Date == "") is the first page.
type RegisterCursor struct {
	Date    string
	SplitID int64
}

// RegisterRow is one register line with its per-currency running balance.
type RegisterRow struct {
	SplitID         int64
	TxnID           int64
	Date            string
	SubsidiaryID    int64
	Currency        string
	Amount          int64 // signed minor units (net-debit, D2)
	FundID          *int64
	ProgramID       *int64
	FunctionalClass *string
	SplitMemo       string
	TxnMemo         string
	PayeeID         *int64
	RunningBalance  int64 // cumulative to this row within its currency
}

// RegisterPage returns one page of the register for accountID, ascending by the
// total order (Date, SplitID), with a per-currency running balance computed by a
// window function over the WHOLE filtered set (so the running balance continues
// correctly across pages, never restarting). Keyset paging is applied here in Go
// (see balances.sql RegisterPage for why it cannot live in SQL over the windowed
// CTE): the query returns the full filtered/windowed/ordered set; this method
// seeks past cursor and returns up to limit rows plus the next cursor.
//
// next is the cursor to pass for the following page; hasMore reports whether more
// rows exist beyond this page (next is meaningful only when hasMore). limit <= 0
// is treated as "no limit" (single page of everything after the cursor).
func (s *Store) RegisterPage(
	ctx context.Context,
	accountID int64,
	cursor RegisterCursor,
	filters RegisterFilters,
	limit int,
) (page []RegisterRow, next RegisterCursor, hasMore bool, err error) {
	// active gates each optional predicate: 1 = apply, 0 = ignore. The compared
	// value is bound only when active, but positional ? needs a value regardless;
	// a zero value paired with active=0 is inert (the "? = 0 OR ..." short-circuits).
	fromActive := b2i(filters.From != "")
	toActive := b2i(filters.To != "")
	textActive := b2i(filters.Text != "")
	fundActive := b2i(filters.FundID != nil)
	subActive := b2i(filters.Subsidiary != nil)
	progActive := b2i(filters.ProgramID != nil)

	like := "%" + filters.Text + "%"

	rows, err := s.q.RegisterPage(ctx, sqlc.RegisterPageParams{
		AccountID:    accountID,
		Column2:      fromActive,
		Date:         filters.From,
		Column4:      toActive,
		Date_2:       filters.To,
		Column6:      textActive,
		Memo:         like,
		Memo_2:       like,
		Name:         like,
		Column10:     fundActive,
		FundID:       nullInt64Ptr(filters.FundID),
		Column12:     subActive,
		SubsidiaryID: derefOr0(filters.Subsidiary),
		Column14:     progActive,
		ProgramID:    nullInt64Ptr(filters.ProgramID),
	})
	if err != nil {
		return nil, RegisterCursor{}, false, fmt.Errorf("store: register page (account %d): %w", accountID, err)
	}

	// Keyset seek in Go: skip rows up to and including the cursor, using the same
	// (Date, SplitID) total order the SQL orders by. The zero cursor (empty Date)
	// starts at the top.
	start := 0
	if cursor.Date != "" || cursor.SplitID != 0 {
		for start < len(rows) {
			r := rows[start]
			if r.Date > cursor.Date || (r.Date == cursor.Date && r.SplitID > cursor.SplitID) {
				break
			}
			start++
		}
	}
	rows = rows[start:]

	// Take up to limit; a (limit+1)th remaining row means there is a next page.
	if limit > 0 && len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	page = make([]RegisterRow, len(rows))
	for i, r := range rows {
		page[i] = RegisterRow{
			SplitID:         r.SplitID,
			TxnID:           r.TxnID,
			Date:            r.Date,
			SubsidiaryID:    r.SubsidiaryID,
			Currency:        r.Currency,
			Amount:          r.Amount,
			FundID:          nullInt64ToPtr(r.FundID),
			ProgramID:       nullInt64ToPtr(r.ProgramID),
			FunctionalClass: nullStringToPtr(r.FunctionalClass),
			SplitMemo:       r.SplitMemo,
			TxnMemo:         r.TxnMemo,
			PayeeID:         nullInt64ToPtr(r.PayeeID),
			RunningBalance:  r.RunningBalance,
		}
	}
	if len(page) > 0 {
		last := page[len(page)-1]
		next = RegisterCursor{Date: last.Date, SplitID: last.SplitID}
	}
	return page, next, hasMore, nil
}

// ---------------------------------------------------------------------------
// FundLedger: one fund's statement -- all its splits across all accounts with a
// per-currency running (asset-side/unexpended) balance (p12.5).
// ---------------------------------------------------------------------------

// FundLedgerRow is one line of a fund statement: a split tagged the fund (on any
// account), its raw values (for tests + display), and the per-currency running
// balance that tracks the fund's ASSET-side (unexpended) position -- the same
// quantity FundBalancesAsOf reports. IsAsset marks the rows that MOVE the balance.
type FundLedgerRow struct {
	SplitID         int64
	TxnID           int64
	Date            string
	SubsidiaryID    int64
	Currency        string
	Amount          int64 // signed minor units (net-debit, D2)
	AccountID       int64
	IsAsset         bool
	ProgramID       *int64
	FunctionalClass *string
	SplitMemo       string
	TxnMemo         string
	PayeeID         *int64
	RunningBalance  int64 // cumulative asset-side amount to this row, per currency
}

// FundLedger returns fundID's statement to asof: every non-deleted split tagged the
// fund whose txn date <= asof, across ALL accounts, ordered by (date, split_id),
// with a per-currency running balance that tracks the fund's ASSET-side unexpended
// position. The CLOSING running balance per currency EQUALS FundBalancesAsOf(asof)
// for that fund/currency by construction (both sum the asset splits to the SAME
// as-of), so the fund page and the fund list agree even under future-dated splits.
// There is no paging (a single fund's split set is bounded) and no scope filter (a
// fund's splits already live only in its subsidiaries).
func (s *Store) FundLedger(ctx context.Context, fundID int64, asof string) ([]FundLedgerRow, error) {
	rows, err := s.q.FundLedger(ctx, sqlc.FundLedgerParams{
		FundID: sql.NullInt64{Int64: fundID, Valid: true},
		Date:   asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fund ledger (fund %d) as of %s: %w", fundID, asof, err)
	}
	out := make([]FundLedgerRow, len(rows))
	for i, r := range rows {
		out[i] = FundLedgerRow{
			SplitID:         r.SplitID,
			TxnID:           r.TxnID,
			Date:            r.Date,
			SubsidiaryID:    r.SubsidiaryID,
			Currency:        r.Currency,
			Amount:          r.Amount,
			AccountID:       r.AccountID,
			IsAsset:         r.IsAsset != 0,
			ProgramID:       nullInt64ToPtr(r.ProgramID),
			FunctionalClass: nullStringToPtr(r.FunctionalClass),
			SplitMemo:       r.SplitMemo,
			TxnMemo:         r.TxnMemo,
			PayeeID:         nullInt64ToPtr(r.PayeeID),
			RunningBalance:  r.RunningBalance,
		}
	}
	return out, nil
}

// b2i maps a bool to the 1/0 SQL active-flag.
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// derefOr0 returns *p or 0 when p is nil (the value is inert when its active flag
// is 0).
func derefOr0(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// nullInt64ToPtr maps a nullable column to *int64 (invalid -> nil). Inverse of
// accounts.go's nullInt64Ptr; the string inverse (nullStringToPtr) already exists.
func nullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
