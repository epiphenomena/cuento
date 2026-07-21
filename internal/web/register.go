package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p12.1 account register (/accounts/{id}/register, TxnRead). The register is a
// table of one account's splits (via store.RegisterPage, p08.4) with a per-currency
// running balance and keyset (seek) paging. Each row shows: date, subsidiary badge
// (only when the account maps to >1 sub, D18), payee, memo, counter-account (the
// other split's account for a 2-split txn, or "Split" for >2), fund chip (only on a
// non-NULL/restricted fund, D20), amount (per the user's display mode / neg style,
// D2/rule 10), and the running balance per currency.
//
// Anti-jank (Appendix C): paging APPENDS the next rows at the end of the tbody via
// a self-replacing sentinel row (htmx hx-get to THIS route with the next cursor +
// active filters, hx-swap outerHTML on itself), so nothing above shifts and scroll
// is preserved; every row carries a stable id (reg-row-<split_id>); the filter
// inputs live in the full page and are never re-rendered on a page fetch, so their
// ids stay stable across swaps. ONE route serves both: a normal GET renders the
// full page; a GET carrying a cursor param renders just the rows fragment (keeping
// the single route Appendix B lists).
//
// The row-assembly (registerRows) is a plain function so paging/ordering/running-
// balance/filter behavior is testable directly against RegisterPage without HTML
// scraping; the running balance is passed straight through (never recomputed).

// registerPageSize is how many rows one register page holds. Keyset paging fetches
// the next page on demand (the sentinel row), so this bounds the initial payload
// and each append.
const registerPageSize = 50

// regRow is one rendered register line. It carries BOTH the raw values (for direct
// test assertions and the keyset cursor) and the pre-formatted display strings the
// template renders (rule 10: money/date through the formatters).
type regRow struct {
	SplitID int64
	TxnID   int64
	DateISO string // raw YYYY-MM-DD (cursor + ordering)

	Amount         int64 // raw signed minor units (net-debit, D2)
	RunningBalance int64 // raw cumulative-per-currency minor units
	Currency       string

	// Rendered strings (rule 9/10).
	Date              string // formatted per the user's date setting
	SubName           string // subsidiary name (rendered only when the page shows the badge)
	Description       string // split description (p26.17 Description column; "" = none)
	Memo              string // split memo, else txn memo
	CounterAccount    string // the other account's name (2-split); "" when IsSplit
	IsSplit           bool   // true for a >2-split txn -> template shows the "Split" word
	FundName          string // "" = unrestricted (no chip); else the fund name (chip)
	AmountFmt         string
	RunningBalanceFmt string
}

// registerRows assembles one page of the register for accountID as of the keyset
// cursor, attaching the display names (subsidiary, payee, fund, counter-account)
// the store's RegisterRow does not carry. Numbers (amount, running balance) come
// STRAIGHT from RegisterPage; this only formats them and resolves names. It returns
// the same (next cursor, hasMore) RegisterPage does, so the caller drives paging.
func registerRows(
	ctx context.Context,
	st *store.Store,
	accountID ids.AccountID,
	cursor store.RegisterCursor,
	filters store.RegisterFilters,
	limit int,
	lang string,
	opts money.FormatOpts,
) (rows []regRow, next store.RegisterCursor, hasMore bool, err error) {
	page, next, hasMore, err := st.RegisterPage(ctx, accountID, cursor, filters, limit)
	if err != nil {
		return nil, store.RegisterCursor{}, false, err
	}

	names, err := accountNameMap(ctx, st, lang)
	if err != nil {
		return nil, store.RegisterCursor{}, false, err
	}
	funds, err := fundNameMap(ctx, st)
	if err != nil {
		return nil, store.RegisterCursor{}, false, err
	}
	exps, err := currencyExponents(ctx, st)
	if err != nil {
		return nil, store.RegisterCursor{}, false, err
	}
	subs, err := subNameMap(ctx, st)
	if err != nil {
		return nil, store.RegisterCursor{}, false, err
	}
	df := dateFormatForLang(ctx)

	// Counter-accounts: resolved per (txn, row-account) on the page (bounded by page
	// size). For a parent-account rollup (p26.6) one txn can appear as several rows --
	// e.g. an intra-parent transfer between two descendant leaves -- whose counter-
	// accounts differ (each names the OTHER leaf), so the cache key is the row's OWN
	// account, not the txn alone. For a leaf register the row account is constant, so
	// this degrades to the previous per-txn behavior.
	type counterKey struct {
		txnID ids.TransactionID
		acct  ids.AccountID
	}
	counters := make(map[counterKey]counterAccount, len(page))

	rows = make([]regRow, 0, len(page))
	for _, r := range page {
		ck := counterKey{txnID: r.TxnID, acct: r.AccountID}
		ca, ok := counters[ck]
		if !ok {
			ca, err = resolveCounterAccount(ctx, st, r.TxnID, r.AccountID, names)
			if err != nil {
				return nil, store.RegisterCursor{}, false, err
			}
			counters[ck] = ca
		}

		memo := r.SplitMemo
		if memo == "" {
			memo = r.TxnMemo
		}
		fund := ""
		if r.FundID != nil {
			fund = funds[*r.FundID]
		}
		exp := exps[r.Currency]

		rows = append(rows, regRow{
			SplitID:           int64(r.SplitID),
			TxnID:             int64(r.TxnID),
			DateISO:           r.Date,
			Amount:            r.Amount,
			RunningBalance:    r.RunningBalance,
			Currency:          r.Currency,
			Date:              money.FormatDate(parseISOForDisplay(r.Date), df),
			SubName:           subs[int64(r.SubsidiaryID)],
			Description:       r.Description,
			Memo:              memo,
			CounterAccount:    ca.name,
			IsSplit:           ca.isSplit,
			FundName:          fund,
			AmountFmt:         money.FormatMoney(r.Amount, r.Currency, exp, opts),
			RunningBalanceFmt: money.FormatMoney(r.RunningBalance, r.Currency, exp, opts),
		})
	}
	return rows, next, hasMore, nil
}

// counterAccount is the resolved "other side" of a register row's transaction: for
// a 2-split txn, the other split's account name; for >2, the "Split" marker (name
// left empty, isSplit true).
type counterAccount struct {
	name    string
	isSplit bool
}

// resolveCounterAccount reads a transaction's live splits and returns the counter-
// account for the register row on selfAccount: for a 2-split transaction, the OTHER
// split's account; for a transaction with MORE THAN 2 splits, the "Split" marker
// (name empty, isSplit true). The gate is the transaction's TOTAL split count (the
// plain spec reading), not the count of distinct other accounts -- a >2-split entry
// whose non-self splits happen to share one account (common under D20 mixed-fund
// splitting) still reads as "Split". A 1-split txn cannot exist (the store rejects
// it).
func resolveCounterAccount(ctx context.Context, st *store.Store, txnID ids.TransactionID, selfAccount ids.AccountID, names map[ids.AccountID]string) (counterAccount, error) {
	splits, err := st.TransactionSplits(ctx, txnID)
	if err != nil {
		return counterAccount{}, err
	}
	if len(splits) == 2 {
		for _, sp := range splits {
			if sp.AccountID != selfAccount {
				return counterAccount{name: names[sp.AccountID]}, nil
			}
		}
	}
	return counterAccount{isSplit: true}, nil
}

// accountNameMap returns id->resolved-name for every account (name fallback for
// lang, p05.3), the counter-account and (unused here) label lookups the register
// needs. Built from AccountTree so the same fallback the tree uses applies.
func accountNameMap(ctx context.Context, st *store.Store, lang string) (map[ids.AccountID]string, error) {
	rows, err := st.Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	m := make(map[ids.AccountID]string, len(rows))
	for _, r := range rows {
		m[r.ID] = r.Name
	}
	return m, nil
}

// fundNameMap returns id->name for every fund (active AND closed; a chip may name a
// now-closed fund).
func fundNameMap(ctx context.Context, st *store.Store) (map[ids.FundID]string, error) {
	fs, err := st.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[ids.FundID]string, len(fs))
	for _, f := range fs {
		m[f.ID] = f.Name
	}
	return m, nil
}

// subNameMap returns id->name for every subsidiary (for the row sub badge).
func subNameMap(ctx context.Context, st *store.Store) (map[int64]string, error) {
	subs, err := st.AllSubsidiaries(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(subs))
	for _, s := range subs {
		m[int64(s.ID)] = s.Name
	}
	return m, nil
}

// dateFormatForLang resolves the current user's date format from the request
// context (nil user -> ISO), so registerRows can format row dates.
func dateFormatForLang(ctx context.Context) money.DateFormat {
	return dateFormatFor(currentUser(ctx))
}

// accountShowsSubBadge reports whether an account maps to MORE THAN ONE subsidiary,
// which is the gate for rendering the per-row subsidiary badge (D18): a single-sub
// account has no ambiguity, so the badge is noise.
func accountShowsSubBadge(ctx context.Context, st *store.Store, accountID ids.AccountID) (bool, error) {
	ids, err := st.AccountSubsidiaryIDs(ctx, accountID)
	if err != nil {
		return false, err
	}
	return len(ids) > 1, nil
}

// ---- page model ----------------------------------------------------------

// regFilterOption is one option in the fund / subsidiary / program filter selects.
type regFilterOption struct {
	ID   int64
	Name string
	// Path (p29.13) is a PROGRAM option's dotted ancestor chain, stamped on the register
	// program-filter select's data-path so the shared fuzzy combobox ranks by the
	// hierarchy. Empty for the subsidiary options that reuse this type.
	Path string
}

// registerPageModel is the GET model: the account header, gating flags, the current
// page of rows + the keyset "next" state, the filter state (echoed into the form),
// and the filter option lists.
type registerPageModel struct {
	AccountID   int64
	AccountName string

	ShowSubBadge bool // account maps to >1 subsidiary (D18)
	Reconcilable bool // account is reconcilable -> render the recon column (p16 wires the mark)
	CanCreateTxn bool // account is a LEAF -> offer the New-transaction action (p29.7); a parent holds no splits

	Rows []regRow

	// Keyset paging state (Appendix C anti-jank). HasMore drives the sentinel row;
	// NextDate/NextID are the cursor carried by its hx-get. Fragment is set when this
	// is a rows-only page fetch (the sentinel-swap response), so the template renders
	// the fragment define rather than the full page.
	HasMore  bool
	NextDate string
	NextID   int64
	Fragment bool

	// Filter state (echoed into the form inputs; carried through paging).
	FilterFrom string // formatted for the text input (user's date format)
	FilterTo   string
	FilterText string
	FilterFund int64
	FilterSub  int64
	FilterProg int64

	Funds    []regFilterOption
	Subs     []regFilterOption
	Programs []regFilterOption

	// p31 post-save notice: set when the transaction editor redirected here after a save
	// whose MAIN-header split carried a description (the ?main_desc=<N> PRG marker). The
	// register shows a non-blocking "heads up" banner reminding the user the header memo is
	// the usual place; MainDescCopies is how many blank body lines the copy-down filled.
	MainDescNotice bool
	MainDescCopies int
}

// registerPage handles GET /accounts/{id}/register (TxnRead). A normal GET renders
// the full page; a GET carrying a cursor param (the sentinel's hx-get) renders just
// the rows fragment so paging appends without a full reload (anti-jank). Both share
// registerRows and the same filter parsing, so page 1 and page N are consistent.
func (s *server) registerPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	id := ids.AccountID(parseID(r.PathValue("id")))
	acct, err := s.store.GetAccount(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	filters, echo := s.parseRegisterFilters(r, u)
	cursor := parseRegisterCursor(r)

	opts := formatOptsFor(u)
	rows, next, hasMore, err := registerRows(ctx, s.store, id, cursor, filters, registerPageSize, lang, opts)
	if err != nil {
		s.serverError(w)
		return
	}

	badge, err := accountShowsSubBadge(ctx, s.store, id)
	if err != nil {
		s.serverError(w)
		return
	}

	// A parent account holds no splits (D11), so its register offers no New-transaction
	// action; only a leaf does. Active matters too: a deactivated account accepts no new
	// entry even if it is a leaf.
	leaf, err := s.store.AccountIsLeaf(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}

	model := registerPageModel{
		AccountID:    int64(id),
		AccountName:  s.accountName(ctx, id, lang),
		ShowSubBadge: badge,
		Reconcilable: acct.Reconcilable != 0,
		CanCreateTxn: leaf && acct.Active != 0,
		Rows:         rows,
		HasMore:      hasMore,
		NextDate:     next.Date,
		NextID:       int64(next.SplitID),
		FilterFrom:   echo.from,
		FilterTo:     echo.to,
		FilterText:   filters.Text,
		FilterFund:   derefID(filters.FundID),
		FilterSub:    derefID(filters.Subsidiary),
		FilterProg:   derefID(filters.ProgramID),
	}

	// p31: a ?main_desc=<N> marker means the txn editor redirected here after saving a
	// transaction whose main-header split carried a description (PRG, the settings-notice
	// pattern). Surface a non-blocking banner on the full page (the fragment/results swaps
	// below never render it, so it only shows on the post-save navigation).
	if v := r.URL.Query().Get("main_desc"); v != "" {
		model.MainDescNotice = true
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			model.MainDescCopies = n
		}
	}

	// A cursor-carrying request is the sentinel's page fetch: render ONLY the rows
	// fragment (the next rows + a fresh sentinel), so nothing above shifts. Detect it
	// by the presence of the cursor param (a normal page-1 GET has no cursor). Checked
	// FIRST so a paging fetch never falls into the filter-swap branch below.
	if isRegisterFragment(r) {
		model.Fragment = true
		s.render(w, r, http.StatusOK, "register-rows", model)
		return
	}

	// p23.12: a filter change is the section-bar form's hx-get targeting
	// #register-results (HX-Target header), so swap ONLY the results table (page 1 for
	// the new filters); the form lives in the section bar and is not re-rendered, so
	// its option lists are not needed here. A full load or a boosted nav (HX-Target
	// absent / "body") renders the whole page.
	if r.Header.Get("HX-Target") == "register-results" {
		s.render(w, r, http.StatusOK, "register-results", model)
		return
	}

	// The full page needs the filter option lists (the section-bar selects).
	if err := s.attachRegisterFilterOptions(ctx, &model); err != nil {
		s.serverError(w)
		return
	}
	// The register is a data-dense, many-column table (date/description/memo/counter/
	// fund/amount/running/actions...); after p26.8 pinned columns to their content
	// (no wrap), it overflows the centered 60rem reading column and spills right. Opt
	// into the wide <main> like the transaction editor so it fits and stays centered.
	page := s.newShellPageControls(r, model, "register")
	page.Shell.Wide = true
	s.render(w, r, http.StatusOK, "register.tmpl", page)
}

// registerFilterEcho carries the filter dates AS THE USER TYPED THEM (their date
// format) so a 200 re-render of the form keeps them, while RegisterFilters carries
// the ISO the store wants.
type registerFilterEcho struct {
	from string
	to   string
}

// parseRegisterFilters reads the register filters from the query. Date fields are
// TEXT inputs honoring the user's date format (ISO always accepted, D16); a
// malformed date is dropped (no filter) rather than erroring the page. Fund /
// subsidiary / program are ids; 0/absent means no filter. The echo carries the
// dates re-formatted in the user's format for the form inputs.
func (s *server) parseRegisterFilters(r *http.Request, u *store.CurrentUser) (store.RegisterFilters, registerFilterEcho) {
	q := r.URL.Query()
	df := dateFormatFor(u)

	var f store.RegisterFilters
	var echo registerFilterEcho

	if v := q.Get("from"); v != "" {
		if t, err := money.ParseDate(v, df, s.now()); err == nil {
			iso := t.Format("2006-01-02")
			f.From = iso
			echo.from = money.FormatDate(t, df)
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := money.ParseDate(v, df, s.now()); err == nil {
			iso := t.Format("2006-01-02")
			f.To = iso
			echo.to = money.FormatDate(t, df)
		}
	}
	f.Text = q.Get("text")
	if id := parseID(q.Get("fund")); id > 0 {
		fid := ids.FundID(id)
		f.FundID = &fid
	}
	if id := parseID(q.Get("sub")); id > 0 {
		f.Subsidiary = &id
	}
	if id := parseID(q.Get("prog")); id > 0 {
		pid := ids.ProgramID(id)
		f.ProgramID = &pid
	}
	return f, echo
}

// parseRegisterCursor reads the keyset cursor from the query (the sentinel's
// hx-get). Absent -> the zero cursor (page 1).
func parseRegisterCursor(r *http.Request) store.RegisterCursor {
	q := r.URL.Query()
	return store.RegisterCursor{
		Date:    q.Get("cursor_date"),
		SplitID: ids.SplitID(parseID(q.Get("cursor_id"))),
	}
}

// isRegisterFragment reports whether this request is a paging fetch (carries a
// cursor), which should render the rows fragment rather than the full page. Keyed
// on the cursor param (present only on the sentinel's hx-get), so a JS-off user
// still gets the full page-1 view.
func isRegisterFragment(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("cursor_date") != "" || q.Get("cursor_id") != ""
}

// attachRegisterFilterOptions fills the filter selects: funds (ACTIVE only -- the
// filter is a fund CHOICE, so a closed fund is not offered as a new selection; a
// historical split that references a closed fund still renders its NAME via
// fundNameMap. NOTE the store's fund filter cannot select the unrestricted/NULL
// group, so no "unrestricted" option is offered), subsidiaries, and programs. Names
// are stored proper nouns (verbatim).
func (s *server) attachRegisterFilterOptions(ctx context.Context, m *registerPageModel) error {
	funds, err := s.store.ListFunds(ctx)
	if err != nil {
		return err
	}
	for _, f := range funds {
		if f.Active == 0 {
			continue // a closed fund is not an offered filter CHOICE (D20)
		}
		m.Funds = append(m.Funds, regFilterOption{ID: int64(f.ID), Name: f.Name})
	}
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		m.Subs = append(m.Subs, regFilterOption{ID: int64(sub.ID), Name: sub.Name})
	}
	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return err
	}
	// p29.13: dotted hierarchy path per program for the register program-filter combobox.
	progPaths, err := s.store.ProgramPaths(ctx)
	if err != nil {
		return err
	}
	for _, p := range progs {
		m.Programs = append(m.Programs, regFilterOption{ID: int64(p.ID), Name: p.Name, Path: progPaths[p.ID]})
	}
	return nil
}

// derefID returns *p or 0 when nil (the "no filter" sentinel the template echoes).
// Generic over any defined id type (ids.FundID, *int64 sub/program filters, …) so a
// typed nullable id renders through the same seam.
func derefID[T ~int64](p *T) int64 {
	if p == nil {
		return 0
	}
	return int64(*p)
}

// ---- template funcs ------------------------------------------------------

// regRowCtx pairs a register row with the page-level column gates so the shared
// "register-row" template (used by both the full page and the paging fragment) can
// reach .ShowSubBadge / .Reconcilable without the row model carrying them. It is
// the `regRowCtx` template func.
type regRowCtx struct {
	Row          regRow
	ShowSubBadge bool
	Reconcilable bool
	// AccountID is this register's account, so the per-row edit link can thread a
	// `from=/accounts/<id>/register` origin for the editor's Cancel (p26.33).
	AccountID int64
}

// makeRegRowCtx is the `regRowCtx` template func: {{regRowCtx $.Page .}}.
func makeRegRowCtx(m registerPageModel, row regRow) regRowCtx {
	return regRowCtx{Row: row, ShowSubBadge: m.ShowSubBadge, Reconcilable: m.Reconcilable, AccountID: m.AccountID}
}

// regColspan is the `regColspan` template func: the register table's column count,
// so the empty-row and sentinel cells span the full width. Base columns are 8 (date,
// payee, memo, counter, fund, amount, running, and the p12.4 actions column); +1 for
// the sub badge, +1 for the recon column when each is shown.
func regColspan(m registerPageModel) int {
	n := 8
	if m.ShowSubBadge {
		n++
	}
	if m.Reconcilable {
		n++
	}
	return n
}

// regMoreURL is the `regMoreURL` template func: the sentinel's hx-get target -- this
// route with the next keyset cursor AND the active filters, so paging carries the
// filter state forward. Built with url.Values so every value is escaped (no inline
// string concatenation of user input into a URL).
func regMoreURL(m registerPageModel) string {
	q := url.Values{}
	q.Set("cursor_date", m.NextDate)
	q.Set("cursor_id", strconv.FormatInt(m.NextID, 10))
	if m.FilterFrom != "" {
		q.Set("from", m.FilterFrom)
	}
	if m.FilterTo != "" {
		q.Set("to", m.FilterTo)
	}
	if m.FilterText != "" {
		q.Set("text", m.FilterText)
	}
	if m.FilterFund != 0 {
		q.Set("fund", strconv.FormatInt(m.FilterFund, 10))
	}
	if m.FilterSub != 0 {
		q.Set("sub", strconv.FormatInt(m.FilterSub, 10))
	}
	if m.FilterProg != 0 {
		q.Set("prog", strconv.FormatInt(m.FilterProg, 10))
	}
	return "/accounts/" + strconv.FormatInt(m.AccountID, 10) + "/register?" + q.Encode()
}
