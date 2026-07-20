package store

import (
	"context"
	"database/sql"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
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
	AccountID ids.AccountID
	Currency  string
	Amount    int64 // signed minor units (net-debit, D2)
}

// FundCurrencyAmount is one (fund, currency) balance cell. FundID 0 is the
// unrestricted group (D20).
type FundCurrencyAmount struct {
	FundID   ids.FundID // 0 = unrestricted
	Currency string
	Amount   int64
}

// FunctionalCell is one (expense account, functional class, currency) activity
// cell (D21). Only expense splits carry a class.
type FunctionalCell struct {
	AccountID       ids.AccountID
	FunctionalClass string
	Currency        string
	Amount          int64
}

// ProgramCell is one (program, account, currency) activity cell (D24). Only
// revenue/expense splits carry a program. Rows are raw per (program, account) --
// the tree rollup is the report layer's job.
type ProgramCell struct {
	ProgramID ids.ProgramID
	AccountID ids.AccountID
	Currency  string
	Amount    int64
}

// IntercompanyAccountIDs returns the ids of every account flagged intercompany
// (D19), id-ordered. The report toolkit's IntercompanyNet sums these accounts'
// balances per currency across a consolidated scope to assert they net to zero;
// a nonzero residual renders as a warning row.
func (s *Store) IntercompanyAccountIDs(ctx context.Context) ([]ids.AccountID, error) {
	ids, err := s.q.IntercompanyAccountIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: intercompany account ids: %w", err)
	}
	return ids, nil
}

// SubtreeBalancesAsOf returns, per (account, currency), the cumulative signed
// balance of non-deleted splits whose transaction date <= asof and whose
// subsidiary is in scopeSub's descendant closure (D18). Balances are cumulative
// to the date.
func (s *Store) SubtreeBalancesAsOf(ctx context.Context, asof string, scopeSub ids.SubsidiaryID) ([]AccountCurrencyAmount, error) {
	rows, err := s.q.SubtreeBalancesAsOf(ctx, sqlc.SubtreeBalancesAsOfParams{
		ID:   int64(scopeSub),
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

// SubDatedActivity is one (subsidiary, account, currency, date) signed activity cell
// (p31.1) -- the per-holding-subsidiary, per-transaction-date grain the FX toolkit
// needs to remeasure foreign-currency monetary balances (ASC 830-20): the balance is
// Σ Amount to the as-of date valued at the closing rate, while the historical basis is
// Σ (Amount at Date) each valued at that date's transaction rate.
type SubDatedActivity struct {
	SubsidiaryID ids.SubsidiaryID
	AccountID    ids.AccountID
	Currency     string
	Date         string
	Amount       int64 // signed minor units (net-debit, D2)
}

// SubDatedBalancesAsOf returns, per (subsidiary, account, currency, date), the signed
// net-debit activity on that date for every non-deleted transaction dated <= asof whose
// subsidiary is in scopeSub's descendant closure (D18). It preserves the HOLDING
// subsidiary (so the caller knows each balance's functional currency = base_currency)
// and the transaction DATE (so the caller can value each dated flow at its own
// transaction-date rate). Rows are ordered (sub, account, currency, date). p31.1.
func (s *Store) SubDatedBalancesAsOf(ctx context.Context, asof string, scopeSub ids.SubsidiaryID) ([]SubDatedActivity, error) {
	rows, err := s.q.SubDatedBalancesAsOf(ctx, sqlc.SubDatedBalancesAsOfParams{
		ID:   int64(scopeSub),
		Date: asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: sub dated balances as of %s (scope %d): %w", asof, scopeSub, err)
	}
	out := make([]SubDatedActivity, len(rows))
	for i, r := range rows {
		out[i] = SubDatedActivity{
			SubsidiaryID: r.SubsidiaryID,
			AccountID:    r.AccountID,
			Currency:     r.Currency,
			Date:         r.Date,
			Amount:       r.Activity,
		}
	}
	return out, nil
}

// PeriodActivity returns, per (account, currency), the signed activity over the
// closed interval from <= date <= to in scopeSub's descendant closure.
func (s *Store) PeriodActivity(ctx context.Context, from, to string, scopeSub ids.SubsidiaryID) ([]AccountCurrencyAmount, error) {
	rows, err := s.q.PeriodActivity(ctx, sqlc.PeriodActivityParams{
		ID:     int64(scopeSub),
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

// FundSubtreeBalancesAsOf returns, per (account, currency), the cumulative signed
// balance of ONE fund's non-deleted splits whose transaction date <= asof and whose
// subsidiary is in scopeSub's descendant closure (D18). It is the fund-FILTERED
// variant of SubtreeBalancesAsOf -- the source for the Statement of Position's FUND
// selector (p15.4): a single fund's OWN assets/liabilities/net-assets. Because every
// transaction nets to zero within a fund group (D20/Z10), the fund's A - L equals its
// net-asset balance, so the balance-sheet identity holds for the fund.
func (s *Store) FundSubtreeBalancesAsOf(ctx context.Context, fundID ids.FundID, asof string, scopeSub ids.SubsidiaryID) ([]AccountCurrencyAmount, error) {
	rows, err := s.q.FundSubtreeBalancesAsOf(ctx, sqlc.FundSubtreeBalancesAsOfParams{
		ID:     int64(scopeSub),
		FundID: sql.NullInt64{Int64: int64(fundID), Valid: true},
		Date:   asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fund subtree balances (fund %d) as of %s (scope %d): %w", fundID, asof, scopeSub, err)
	}
	out := make([]AccountCurrencyAmount, len(rows))
	for i, r := range rows {
		out[i] = AccountCurrencyAmount{AccountID: r.AccountID, Currency: r.Currency, Amount: r.Balance}
	}
	return out, nil
}

// FundPeriodActivity returns, per (program, account, currency), the signed activity
// over the closed interval from <= date <= to in scopeSub's descendant closure (D18)
// restricted to ONE fund. It is the fund-FILTERED variant of PeriodActivity -- the
// source for the Statement of Activities' FUND selector (p15.5). It keeps the program
// dimension (nullable, ProgramID 0 = untagged) so the SAME rows can ALSO be narrowed
// to a program subtree in the report layer when a user picks both a fund and a
// program. Rows are raw per (program, account); the report layer keeps only R/E
// accounts, exactly as it does over PeriodActivity.
func (s *Store) FundPeriodActivity(ctx context.Context, fundID ids.FundID, from, to string, scopeSub ids.SubsidiaryID) ([]ProgramCell, error) {
	rows, err := s.q.FundPeriodActivity(ctx, sqlc.FundPeriodActivityParams{
		ID:     int64(scopeSub),
		FundID: sql.NullInt64{Int64: int64(fundID), Valid: true},
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fund period activity (fund %d) %s..%s (scope %d): %w", fundID, from, to, scopeSub, err)
	}
	out := make([]ProgramCell, len(rows))
	for i, r := range rows {
		// program_id is nullable here (a fund's asset/liability legs carry none); an
		// untagged split maps to ProgramID 0, which the report layer's InProgramScope
		// treats as out-of-any-subtree, so only R/E (program-tagged) rows survive a
		// program filter.
		out[i] = ProgramCell{
			ProgramID: ids.ProgramID(r.ProgramID.Int64),
			AccountID: r.AccountID,
			Currency:  r.Currency,
			Amount:    r.Activity,
		}
	}
	return out, nil
}

// FundBalancesAsOf returns, per (fund, currency), the fund's cumulative
// unexpended balance to asof in scopeSub's descendant closure, INCLUDING the
// unrestricted group (fund id 0, D20). It is the ASSET-side sum: a whole-fund sum
// is identically zero (D20/Z10 fund conservation), so the balance is the fund's
// cash/asset position = unexpended restricted resources (Z18 precedent, recorded
// as p08.4 in docs/DECISIONS.md).
func (s *Store) FundBalancesAsOf(ctx context.Context, asof string, scopeSub ids.SubsidiaryID) ([]FundCurrencyAmount, error) {
	rows, err := s.q.FundBalancesAsOf(ctx, sqlc.FundBalancesAsOfParams{
		ID:   int64(scopeSub),
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

// CurrentCashFundBalancesAsOf returns, per (fund, currency), the fund's cumulative
// CASH-AVAILABLE balance to asof in scopeSub's descendant closure -- the sum over
// accounts flagged current_cash (p27.1), INCLUDING the unrestricted group (fund id
// 0, D20). It is the cash-flow projection's PER-FUND opening base (p27.3): unlike
// FundBalancesAsOf (the whole asset-side position, which includes receivables and
// capitalized non-cash assets), this is strictly the spendable cash the org can
// project forward.
func (s *Store) CurrentCashFundBalancesAsOf(ctx context.Context, asof string, scopeSub ids.SubsidiaryID) ([]FundCurrencyAmount, error) {
	rows, err := s.q.CurrentCashFundBalancesAsOf(ctx, sqlc.CurrentCashFundBalancesAsOfParams{
		ID:   int64(scopeSub),
		Date: asof,
	})
	if err != nil {
		return nil, fmt.Errorf("store: current-cash fund balances as of %s (scope %d): %w", asof, scopeSub, err)
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
func (s *Store) FunctionalActivity(ctx context.Context, from, to string, scopeSub ids.SubsidiaryID) ([]FunctionalCell, error) {
	rows, err := s.q.FunctionalActivity(ctx, sqlc.FunctionalActivityParams{
		ID:     int64(scopeSub),
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

// FunctionalCellProgram is one (expense account, functional class, program,
// currency) activity cell -- FunctionalCell plus the program dimension (D24). Used
// only on the p27.4 program-scoped functional-expenses path.
type FunctionalCellProgram struct {
	AccountID       ids.AccountID
	FunctionalClass string
	ProgramID       ids.ProgramID
	Currency        string
	Amount          int64
}

// FunctionalActivityByProgram is the p27.4 SCOPED variant of FunctionalActivity: it
// additionally keys each (expense account, class, currency) cell by program_id, so a
// program-scoped report grant can filter the functional-expense matrix to the granted
// subtree BEFORE rolling classes up. Only expense splits carry BOTH a class (D21) and
// a program (D24). The unscoped path keeps FunctionalActivity (no program column), so
// the goldens do not move.
func (s *Store) FunctionalActivityByProgram(ctx context.Context, from, to string, scopeSub ids.SubsidiaryID) ([]FunctionalCellProgram, error) {
	rows, err := s.q.FunctionalActivityByProgram(ctx, sqlc.FunctionalActivityByProgramParams{
		ID:     int64(scopeSub),
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: functional activity by program %s..%s (scope %d): %w", from, to, scopeSub, err)
	}
	out := make([]FunctionalCellProgram, len(rows))
	for i, r := range rows {
		// functional_class and program_id are NOT NULL-filtered in SQL, so Valid holds.
		out[i] = FunctionalCellProgram{
			AccountID:       r.AccountID,
			FunctionalClass: r.FunctionalClass.String,
			ProgramID:       ids.ProgramID(r.ProgramID.Int64),
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
func (s *Store) ProgramActivity(ctx context.Context, from, to string, scopeSub ids.SubsidiaryID) ([]ProgramCell, error) {
	rows, err := s.q.ProgramActivity(ctx, sqlc.ProgramActivityParams{
		ID:     int64(scopeSub),
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
			ProgramID: ids.ProgramID(r.ProgramID.Int64),
			AccountID: r.AccountID,
			Currency:  r.Currency,
			Amount:    r.Activity,
		}
	}
	return out, nil
}

// BudgetKeyCell is one (subsidiary, account, fund, program, currency, date)
// net-debit activity cell -- the ACTUALS grain the p19.2 budget toolkit compares
// against a budget line's (sub, account, fund, program) key. FundID 0 is the
// unrestricted group (D20). Date is preserved so the caller buckets each cell by
// its own date (discrete, no pro-rata) exactly as it buckets budget occurrences.
type BudgetKeyCell struct {
	SubsidiaryID ids.SubsidiaryID
	AccountID    ids.AccountID
	FundID       ids.FundID // 0 = unrestricted
	ProgramID    ids.ProgramID
	Currency     string
	Date         string
	Amount       int64 // signed minor units (net-debit, D2)
}

// BudgetKeyActivity returns, per (subsidiary, account, fund, program, currency,
// date), the signed net-debit activity of the revenue/expense splits over the
// closed interval from <= date <= to in scopeSub's descendant closure (D18). It is
// the actuals read the budget toolkit's ActualsVsBudget uses: the SAME per-key
// grain a budget line carries (unrestricted = fund 0, COALESCE), with the date kept
// so the caller buckets by occurrence date. Only R/E splits (those carrying a
// program, D24) are returned.
func (s *Store) BudgetKeyActivity(ctx context.Context, from, to string, scopeSub ids.SubsidiaryID) ([]BudgetKeyCell, error) {
	rows, err := s.q.BudgetKeyActivity(ctx, sqlc.BudgetKeyActivityParams{
		ID:     int64(scopeSub),
		Date:   from,
		Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("store: budget key activity %s..%s (scope %d): %w", from, to, scopeSub, err)
	}
	out := make([]BudgetKeyCell, len(rows))
	for i, r := range rows {
		// program_id is NOT NULL-filtered in SQL, so Valid is always true.
		out[i] = BudgetKeyCell{
			SubsidiaryID: r.SubsidiaryID,
			AccountID:    r.AccountID,
			FundID:       r.FundID,
			ProgramID:    ids.ProgramID(r.ProgramID.Int64),
			Currency:     r.Currency,
			Date:         r.Date,
			Amount:       r.Activity,
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
	From       string         // "" = no lower bound (YYYY-MM-DD)
	To         string         // "" = no upper bound (YYYY-MM-DD)
	Text       string         // "" = no text filter
	FundID     *ids.FundID    // nil = any fund; a fund id filters to that fund's splits
	Subsidiary *int64         // nil = any subsidiary
	ProgramID  *ids.ProgramID // nil = any program
}

// RegisterCursor is the keyset position: the (Date, SplitID) of the LAST row of
// the previous page. The zero value (Date == "") is the first page.
type RegisterCursor struct {
	Date    string
	SplitID ids.SplitID
}

// RegisterRow is one register line with its per-currency running balance.
type RegisterRow struct {
	SplitID         ids.SplitID
	TxnID           ids.TransactionID
	Date            string
	SubsidiaryID    ids.SubsidiaryID
	Currency        string
	AccountID       ids.AccountID // the split's OWN account (a descendant leaf for a parent rollup)
	Amount          int64         // signed minor units (net-debit, D2)
	FundID          *ids.FundID
	ProgramID       *ids.ProgramID
	FunctionalClass *string
	SplitMemo       string
	TxnMemo         string
	Description     string // per-split free-text (p26.15); the register Description column
	RunningBalance  int64  // cumulative to this row within its currency
}

// RegisterPage returns one page of the register for accountID in REVERSE
// chronological order -- descending by the total order (Date, SplitID), NEWEST on top
// (p26.9) -- with a per-currency running balance computed by an ASCENDING window over
// the WHOLE filtered set (so each row's running balance is the cumulative total from
// the oldest split up to that row, continuing correctly across pages, never
// restarting; the top/newest row shows the latest balance). When accountID is a PLACEHOLDER
// (parent) account the query rolls up the splits of ALL its descendant leaf
// accounts (p26.6): the SQL closes a recursive descendant set over the account tree
// (base = accountID itself, so a LEAF sees only its own splits -- unchanged) and the
// window then accumulates ONE combined running balance across the merged, date-
// ordered descendant sequence. Each row carries its own AccountID so the caller
// resolves the counter-account against the actual leaf, not the parent. Keyset
// paging is applied here in Go
// (see balances.sql RegisterPage for why it cannot live in SQL over the windowed
// CTE): the query returns the full filtered/windowed/ordered set; this method
// seeks past cursor and returns up to limit rows plus the next cursor.
//
// next is the cursor to pass for the following page; hasMore reports whether more
// rows exist beyond this page (next is meaningful only when hasMore). limit <= 0
// is treated as "no limit" (single page of everything after the cursor).
func (s *Store) RegisterPage(
	ctx context.Context,
	accountID ids.AccountID,
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

	// Neutralize LIKE metacharacters so a literal % or _ in the register text filter
	// matches itself rather than acting as a wildcard (same helper descriptions.go uses).
	like := likeContains(filters.Text)

	rows, err := s.q.RegisterPage(ctx, sqlc.RegisterPageParams{
		AccountID:    accountID,
		Column2:      fromActive,
		Date:         filters.From,
		Column4:      toActive,
		Date_2:       filters.To,
		Column6:      textActive,
		Memo:         like,
		Memo_2:       like,
		Description:  like,
		Column10:     fundActive,
		FundID:       ids.Null(filters.FundID),
		Column12:     subActive,
		SubsidiaryID: ids.SubsidiaryID(derefOr0(filters.Subsidiary)),
		Column14:     progActive,
		ProgramID:    ids.Null(filters.ProgramID),
	})
	if err != nil {
		return nil, RegisterCursor{}, false, fmt.Errorf("store: register page (account %d): %w", accountID, err)
	}

	// Keyset seek in Go, matching the SQL's DESCENDING display order (date DESC,
	// split_id DESC, p26.9): rows arrive newest-first. The cursor is the LAST row of
	// the previous page -- the OLDEST row already shown -- so the next page wants rows
	// STRICTLY OLDER than it, which sit further down the descending array. Skip rows up
	// to and including the cursor (those NEWER-or-equal), then break at the first
	// strictly-older row. The zero cursor (empty Date) starts at the top (newest).
	start := 0
	if cursor.Date != "" || cursor.SplitID != 0 {
		for start < len(rows) {
			r := rows[start]
			if r.Date < cursor.Date || (r.Date == cursor.Date && r.SplitID < cursor.SplitID) {
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
			AccountID:       r.AccountID,
			Amount:          r.Amount,
			FundID:          ids.Ptr[ids.FundID](r.FundID),
			ProgramID:       ids.Ptr[ids.ProgramID](r.ProgramID),
			FunctionalClass: nullStringToPtr(r.FunctionalClass),
			SplitMemo:       r.SplitMemo,
			TxnMemo:         r.TxnMemo,
			Description:     r.Description,
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
	SplitID         ids.SplitID
	TxnID           ids.TransactionID
	Date            string
	SubsidiaryID    ids.SubsidiaryID
	Currency        string
	Amount          int64 // signed minor units (net-debit, D2)
	AccountID       ids.AccountID
	IsAsset         bool
	ProgramID       *ids.ProgramID
	FunctionalClass *string
	SplitMemo       string
	TxnMemo         string
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
func (s *Store) FundLedger(ctx context.Context, fundID ids.FundID, asof string) ([]FundLedgerRow, error) {
	rows, err := s.q.FundLedger(ctx, sqlc.FundLedgerParams{
		FundID: sql.NullInt64{Int64: int64(fundID), Valid: true},
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
			ProgramID:       ids.Ptr[ids.ProgramID](r.ProgramID),
			FunctionalClass: nullStringToPtr(r.FunctionalClass),
			SplitMemo:       r.SplitMemo,
			TxnMemo:         r.TxnMemo,
			RunningBalance:  r.RunningBalance,
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// DrillSplits: the report drill-down (p15.3d) -- the individual splits behind ONE
// report figure, so their signed sum reconciles to that figure.
// ---------------------------------------------------------------------------

// DrillFilter selects the splits contributing to one report figure. It MIRRORS the
// toolkit's balance/activity queries (scope descendant closure D18, per-currency,
// date bound) so the drill list reconciles to the cell it drills. A leaf-account
// trial-balance cell sets AccountID + Currency + AsOf; a period report sets From/To
// instead; Fund/Program/Class narrow further (nil = no filter on that dimension).
type DrillFilter struct {
	Scope     ids.SubsidiaryID // subsidiary; consolidated with ALL descendants (D18)
	AccountID ids.AccountID    // the leaf account the figure sums (0 = none => empty result)
	Currency  string           // native currency of the cell (per-currency reconciliation)

	// Date bound. When AsOf != "" the figure is cumulative (t.date <= AsOf); else
	// From/To bound a period (From <= t.date <= To, either side optional).
	AsOf string
	From string
	To   string

	FundID    *ids.FundID    // nil = any fund
	ProgramID *ids.ProgramID // nil = any program
	Class     *string        // nil = any functional class
}

// DrillRow is one split behind a report figure: its raw signed amount (net-debit,
// D2 -- summed by the caller to reconcile) plus the txn/display fields the drill
// list renders (reusing the register row rendering) and the txn id each row links to
// (the p12.4 editor/history).
type DrillRow struct {
	SplitID         ids.SplitID
	TxnID           ids.TransactionID
	Date            string
	SubsidiaryID    ids.SubsidiaryID
	Currency        string
	Amount          int64 // signed minor units (net-debit, D2)
	FundID          *ids.FundID
	ProgramID       *ids.ProgramID
	FunctionalClass *string
	SplitMemo       string
	TxnMemo         string
	Description     string // per-split free-text (p26.15); the ledger Description cell
}

// DrillSplits returns every non-deleted split matching f, ordered by (date,
// split_id). The signed sum of the returned amounts EQUALS the report figure f
// drills (the RECONCILIATION invariant, p15.3d): the query uses the SAME scope
// descendant closure and per-currency/date filtering the toolkit's BalancesAsOf /
// Activity used to produce the cell, so the two agree by construction. A zero
// AccountID (an empty/degenerate filter, e.g. the permission matrix's bare drill
// hit) returns no rows without erroring.
func (s *Store) DrillSplits(ctx context.Context, f DrillFilter) ([]DrillRow, error) {
	if f.AccountID == 0 {
		return nil, nil
	}

	asofActive := b2i(f.AsOf != "")
	fromActive := b2i(f.From != "")
	toActive := b2i(f.To != "")
	fundActive := b2i(f.FundID != nil)
	progActive := b2i(f.ProgramID != nil)
	classActive := b2i(f.Class != nil)

	rows, err := s.q.DrillSplits(ctx, sqlc.DrillSplitsParams{
		ID:              int64(f.Scope),
		AccountID:       f.AccountID,
		Currency:        f.Currency,
		Column4:         asofActive,
		Date:            f.AsOf,
		Column6:         fromActive,
		Date_2:          f.From,
		Column8:         toActive,
		Date_3:          f.To,
		Column10:        fundActive,
		FundID:          ids.Null(f.FundID),
		Column12:        progActive,
		ProgramID:       ids.Null(f.ProgramID),
		Column14:        classActive,
		FunctionalClass: nullStringPtr(f.Class),
	})
	if err != nil {
		return nil, fmt.Errorf("store: drill splits (account %d, ccy %s, scope %d): %w",
			f.AccountID, f.Currency, f.Scope, err)
	}
	out := make([]DrillRow, len(rows))
	for i, r := range rows {
		out[i] = DrillRow{
			SplitID:         r.SplitID,
			TxnID:           r.TxnID,
			Date:            r.Date,
			SubsidiaryID:    r.SubsidiaryID,
			Currency:        r.Currency,
			Amount:          r.Amount,
			FundID:          ids.Ptr[ids.FundID](r.FundID),
			ProgramID:       ids.Ptr[ids.ProgramID](r.ProgramID),
			FunctionalClass: nullStringToPtr(r.FunctionalClass),
			SplitMemo:       r.SplitMemo,
			TxnMemo:         r.TxnMemo,
			Description:     r.Description,
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

// nullInt64ToPtr maps a nullable column to *int64 (invalid -> nil). The string
// inverse (nullStringToPtr) already exists.
func nullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
