package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"cuento/internal/money"
	"cuento/internal/store"
)

// p12.5 funds workspace (/funds) -- funds are the restricted-fund SPLIT DIMENSION
// (D20). Fund GRANTS are BOOKKEEPING data (like programs, D24/p11.5), so GET (list
// + statement) is TxnRead and the create/edit/close/reopen mutations are TxnWrite
// (subsidiaries/users stay Admin -- unchanged). This file follows the p11.5
// programs / p11.1 accounts CRUD-page template: a list page, an inline htmx
// create/edit form reusing the p10.3 form-error convention, and close/reopen as
// inline POST actions (mirroring programDeactivate -- close is an ACTION, not a
// form). The store owns validation (D20); the handler only TRANSLATES the store's
// typed errors to (field, key) pairs.
//
// The list shows, per fund: per-currency asset-side balance (store.FundBalancesAsOf
// at s.now(), the SAME quantity Z18 warns on), a WARNING BADGE when ANY currency is
// negative (Z18: overspent restricted resources -- net-debit sign, D2, so
// balance < 0 = negative), the funder, and the scope (subsidiaries + optional
// program). An active/closed toggle (?closed=1) splits active from closed funds
// (CloseFund sets active=0, p07.3). The unrestricted group (fund 0) is NOT a real
// fund and never appears.
//
// The DETAIL page (/funds/{id}) is the fund's STATEMENT: store.FundLedger -- every
// split tagged the fund across ALL accounts with an opening (0) / running / closing
// per-currency ASSET-side balance that reconciles to the list balance by
// construction. Every string via {{t}} (rule 9); money/dates via the formatters
// honoring the user's settings (rule 10); fund/account names are stored proper
// nouns rendered verbatim; no inline script (rule 12).

// ---- list -----------------------------------------------------------------

// fundListRow is one rendered fund on the list: its name/funder, formatted
// per-currency balances, the negative-badge gate, the scope (subsidiary names +
// optional program), and its active state.
type fundListRow struct {
	ID       int64
	Name     string
	Funder   string
	Balances []string // pre-formatted "CCY 1,234.56" strings (rule 10)
	Negative bool     // Z18: any currency's asset-side balance < 0 (overspent)
	Subs     []string // subsidiary names in this fund's scope (D20)
	Program  string   // "" = no program scope; else the program name
}

// fundsPageModel is the GET /funds model: the fund rows for the selected toggle
// (active vs closed) and the toggle flag.
type fundsPageModel struct {
	Rows       []fundListRow
	ShowClosed bool // the active/closed toggle state (?closed=1)
}

// fundsPage handles GET /funds (TxnRead): the fund list for the selected toggle,
// each with per-currency balances (as of s.now(), root scope = full consolidation,
// D18), a negative badge (Z18), funder, and scope. ?closed=1 shows CLOSED funds;
// the default shows ACTIVE ones.
func (s *server) fundsPage(w http.ResponseWriter, r *http.Request) {
	showClosed := r.URL.Query().Get("closed") == "1"
	model, err := s.buildFundsPage(r.Context(), showClosed)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "funds.tmpl", s.newShellPageControls(r, model, "funds"))
}

// buildFundsPage assembles the list rows for the toggle state. Balances come from
// FundBalancesAsOf (as of today, root scope); scope names from FundSubsidiaries +
// the fund's program. Exposed with the toggle explicit so it is testable directly.
func (s *server) buildFundsPage(ctx context.Context, showClosed bool) (fundsPageModel, error) {
	u := currentUser(ctx)

	funds, err := s.store.ListFunds(ctx)
	if err != nil {
		return fundsPageModel{}, err
	}

	scope, err := s.rootSubsidiary(ctx)
	if err != nil {
		return fundsPageModel{}, err
	}
	asof := s.now().Format("2006-01-02")
	balCells, err := s.store.FundBalancesAsOf(ctx, asof, scope)
	if err != nil {
		return fundsPageModel{}, err
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		return fundsPageModel{}, err
	}
	subNames, err := subNameMap(ctx, s.store)
	if err != nil {
		return fundsPageModel{}, err
	}
	progNames, err := s.programNameMap(ctx)
	if err != nil {
		return fundsPageModel{}, err
	}

	// Group the balance cells per fund (fund 0 = unrestricted, dropped below).
	byFund := make(map[int64][]store.FundCurrencyAmount, len(balCells))
	for _, c := range balCells {
		byFund[c.FundID] = append(byFund[c.FundID], c)
	}

	opts := formatOptsFor(u)
	model := fundsPageModel{ShowClosed: showClosed}
	for _, f := range funds {
		active := f.Active != 0
		if active == showClosed {
			continue // the toggle selects exactly one of active / closed
		}
		row := fundListRow{ID: f.ID, Name: f.Name, Funder: f.Funder}

		cells := byFund[f.ID]
		// Deterministic currency order for a stable render.
		sort.Slice(cells, func(i, j int) bool { return cells[i].Currency < cells[j].Currency })
		for _, c := range cells {
			row.Balances = append(row.Balances, money.FormatMoney(c.Amount, c.Currency, exps[c.Currency], opts))
			if c.Amount < 0 { // Z18: overspent restricted resources (net-debit, D2)
				row.Negative = true
			}
		}

		subs, err := s.store.FundSubsidiaryIDs(ctx, f.ID)
		if err != nil {
			return fundsPageModel{}, err
		}
		for _, sid := range subs {
			row.Subs = append(row.Subs, subNames[sid])
		}
		if f.ProgramID.Valid {
			row.Program = progNames[f.ProgramID.Int64]
		}
		model.Rows = append(model.Rows, row)
	}
	return model, nil
}

// programNameMap returns id->name for every program (for the fund scope column and
// the form's program select).
func (s *server) programNameMap(ctx context.Context) (map[int64]string, error) {
	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(progs))
	for _, p := range progs {
		m[p.ID] = p.Name
	}
	return m, nil
}

// ---- statement (detail) ---------------------------------------------------

// fundStmtRow is one rendered statement line: the split's date/account/memo/amount
// plus the per-currency running (asset-side) balance and whether this row MOVED the
// balance (an asset split).
type fundStmtRow struct {
	Date              string
	SubName           string
	AccountName       string
	Memo              string
	IsAsset           bool
	Currency          string
	AmountFmt         string
	RunningBalanceFmt string
}

// fundOpenClose is the opening/closing balance for one currency of the statement.
type fundOpenClose struct {
	Currency   string
	OpeningFmt string
	ClosingFmt string
}

// fundStatementModel is the GET /funds/{id} model: the fund header, its rows, and
// the per-currency opening/closing balances (opening is 0; closing is the last
// running balance per currency -- reconciling to the list balance).
type fundStatementModel struct {
	FundID    int64
	FundName  string
	Funder    string
	Active    bool
	Rows      []fundStmtRow
	OpenClose []fundOpenClose
}

// fundStatement handles GET /funds/{id} (TxnRead): the fund's statement -- all its
// splits across all accounts to today (store.FundLedger, same as-of as the list's
// balances) with a per-currency opening (0) / running / closing asset-side balance.
// The closing balance reconciles to the list balance and to FundBalancesAsOf by
// construction (both sum the asset splits to the SAME as-of).
func (s *server) fundStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	id := parseID(r.PathValue("id"))
	fund, err := s.store.GetFund(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	asof := s.now().Format("2006-01-02")
	ledger, err := s.store.FundLedger(ctx, id, asof)
	if err != nil {
		s.serverError(w)
		return
	}

	names, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		s.serverError(w)
		return
	}
	subs, err := subNameMap(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	model := fundStatementModel{
		FundID:   id,
		FundName: fund.Name,
		Funder:   fund.Funder,
		Active:   fund.Active != 0,
	}

	// Closing balance per currency = the LAST running balance seen for it (the
	// ledger is ordered, so the final row per currency carries the closing). Opening
	// is 0 (the whole set is shown). Track first-seen order for a stable render.
	closing := make(map[string]int64)
	var ccyOrder []string
	seen := make(map[string]bool)
	for _, lr := range ledger {
		memo := lr.SplitMemo
		if memo == "" {
			memo = lr.TxnMemo
		}
		model.Rows = append(model.Rows, fundStmtRow{
			Date:              money.FormatDate(parseISOForDisplay(lr.Date), df),
			SubName:           subs[lr.SubsidiaryID],
			AccountName:       names[lr.AccountID],
			Memo:              memo,
			IsAsset:           lr.IsAsset,
			Currency:          lr.Currency,
			AmountFmt:         money.FormatMoney(lr.Amount, lr.Currency, exps[lr.Currency], opts),
			RunningBalanceFmt: money.FormatMoney(lr.RunningBalance, lr.Currency, exps[lr.Currency], opts),
		})
		closing[lr.Currency] = lr.RunningBalance
		if !seen[lr.Currency] {
			seen[lr.Currency] = true
			ccyOrder = append(ccyOrder, lr.Currency)
		}
	}
	sort.Strings(ccyOrder)
	for _, ccy := range ccyOrder {
		model.OpenClose = append(model.OpenClose, fundOpenClose{
			Currency:   ccy,
			OpeningFmt: money.FormatMoney(0, ccy, exps[ccy], opts),
			ClosingFmt: money.FormatMoney(closing[ccy], ccy, exps[ccy], opts),
		})
	}

	s.render(w, r, http.StatusOK, "fund_statement.tmpl", s.newShellPage(r, model))
}

// ---- create / edit form ---------------------------------------------------

// fundForm is the create/edit form model. It carries the value fields, the
// subsidiary checklist (Subs + CheckedSubs, the sub_<id> pattern), the optional
// program-scope select options, and an embedded formErrors. ID 0 = create.
type fundForm struct {
	ID          int64
	Name        string
	Funder      string
	Purpose     string
	Restriction string
	ProgramID   int64
	Notes       string

	Subs        []subOption
	CheckedSubs map[int64]bool
	Programs    []programOption

	Errors formErrors
}

// fundNewForm handles GET /funds/new (TxnWrite): the empty create form, rendered as
// the "fund-form" partial for htmx to swap in.
func (s *server) fundNewForm(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildFundForm(r.Context(), 0)
	if err != nil {
		s.serverError(w)
		return
	}
	form.Restriction = "purpose" // sensible default
	s.render(w, r, http.StatusOK, "fund-form", form)
}

// fundEditForm handles GET /funds/{id}/edit (TxnWrite): the form prefilled from the
// fund's current state (fields + subsidiary set + program scope), for an inline
// htmx swap.
func (s *server) fundEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	fund, err := s.store.GetFund(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	form, err := s.buildFundForm(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	form.Name = fund.Name
	form.Funder = fund.Funder
	form.Purpose = fund.Purpose
	form.Restriction = fund.Restriction
	form.Notes = fund.Notes
	if fund.ProgramID.Valid {
		form.ProgramID = fund.ProgramID.Int64
	}
	sids, err := s.store.FundSubsidiaryIDs(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	for _, sid := range sids {
		form.CheckedSubs[sid] = true
	}
	s.render(w, r, http.StatusOK, "fund-form", form)
}

// buildFundForm assembles the option lists a fund form needs: the subsidiary
// checklist (ALL subsidiaries -- a fund scopes to a FLAT set, >=1, D20) and the
// program-scope select (every program; the store validates existence). The checked
// set starts empty (create); the edit path fills it from FundSubsidiaries.
func (s *server) buildFundForm(ctx context.Context, id int64) (fundForm, error) {
	form := fundForm{ID: id, CheckedSubs: map[int64]bool{}}

	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		return form, err
	}
	for _, sub := range subs {
		form.Subs = append(form.Subs, subOption{ID: sub.ID, Name: sub.Name})
	}

	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return form, err
	}
	// p29.13: dotted hierarchy path per program for the fund form's program combobox.
	progPaths, err := s.store.ProgramPaths(ctx)
	if err != nil {
		return form, err
	}
	for _, p := range progs {
		form.Programs = append(form.Programs, programOption{ID: p.ID, Name: p.Name, Path: progPaths[p.ID]})
	}
	return form, nil
}

// parsedFundForm is the validated-shape of a submitted fund form (raw strings turned
// into typed fields); the store does the real validation.
type parsedFundForm struct {
	name        string
	funder      string
	purpose     string
	restriction string
	programID   int64
	notes       string
	subs        []int64
}

// parseFundForm reads the POST form into a fundForm (for a 422 re-render) and a
// parsedFundForm (for the store call). id is the edit target (0 = create); it is
// threaded into buildFundForm so a 422 re-render rebuilds the same option lists. It
// does NOT validate business rules (the store owns that).
func (s *server) parseFundForm(r *http.Request, id int64) (fundForm, parsedFundForm, error) {
	if err := r.ParseForm(); err != nil {
		return fundForm{}, parsedFundForm{}, err
	}
	in := parsedFundForm{
		name:        r.PostFormValue("name"),
		funder:      r.PostFormValue("funder"),
		purpose:     r.PostFormValue("purpose"),
		restriction: r.PostFormValue("restriction"),
		programID:   parseID(r.PostFormValue("program_id")),
		notes:       r.PostFormValue("notes"),
	}
	// Subsidiary checklist: fields named sub_<id> that are set (the accounts pattern).
	checked := map[int64]bool{}
	for key, vals := range r.PostForm {
		if len(key) > 4 && key[:4] == "sub_" && len(vals) > 0 && vals[0] != "" {
			if sid := parseID(key[4:]); sid > 0 {
				in.subs = append(in.subs, sid)
				checked[sid] = true
			}
		}
	}

	form, err := s.buildFundForm(r.Context(), id)
	if err != nil {
		return fundForm{}, parsedFundForm{}, err
	}
	// Echo submitted values back so a 422 re-render keeps what the user entered.
	form.Name = in.name
	form.Funder = in.funder
	form.Purpose = in.purpose
	form.Restriction = in.restriction
	form.ProgramID = in.programID
	form.Notes = in.notes
	form.CheckedSubs = checked
	return form, in, nil
}

// fundCreate handles POST /funds (TxnWrite). It parses the form and calls
// store.CreateFund; an empty subsidiary checklist yields ErrFundNoSubsidiary (the
// handler does not re-validate). On a typed error it maps to a field-error key and
// re-renders the form at 422; success redirects (PRG).
func (s *server) fundCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in, err := s.parseFundForm(r, 0)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	create := store.CreateFundInput{
		Name:         in.name,
		Funder:       in.funder,
		Purpose:      in.purpose,
		Restriction:  in.restriction,
		Notes:        in.notes,
		Subsidiaries: in.subs,
	}
	if in.programID > 0 {
		create.ProgramID = &in.programID
	}
	if _, err := s.store.CreateFund(s.actorCtx(ctx), create); err != nil {
		s.renderFundFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/funds")
}

// fundUpdate handles POST /funds/{id} (TxnWrite): update fields, subsidiary set,
// and program scope via UpdateFund. The full desired subsidiary set is the checklist
// (nil-vs-empty: an empty checklist is a non-nil empty slice, so the store rejects
// it with ErrFundNoSubsidiary). program_id absent/0 CLEARS the scope (the store's
// non-nil-0-clears convention); a positive value sets it.
func (s *server) fundUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in, err := s.parseFundForm(r, id)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	upd := store.UpdateFundInput{
		Name:        &in.name,
		Funder:      &in.funder,
		Purpose:     &in.purpose,
		Restriction: &in.restriction,
		Notes:       &in.notes,
		// The checklist is the full desired set; the store diffs it. An empty set is
		// a non-nil empty slice here, so the store applies (and rejects) it rather
		// than leaving the set unchanged.
		Subsidiaries: nonNilSubs(in.subs),
	}
	// program_id: a positive value sets the scope; 0/absent CLEARS it (non-nil 0).
	prog := in.programID
	upd.ProgramID = &prog
	if err := s.store.UpdateFund(s.actorCtx(ctx), id, upd); err != nil {
		s.renderFundFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/funds")
}

// nonNilSubs guarantees a non-nil slice so UpdateFund treats an empty checklist as
// "desired = empty" (-> ErrFundNoSubsidiary) rather than "leave unchanged" (nil).
func nonNilSubs(subs []int64) []int64 {
	if subs == nil {
		return []int64{}
	}
	return subs
}

// fundClose handles POST /funds/{id}/close (TxnWrite): CloseFund sets active=0
// (op=update, audited); the fund then moves under the closed toggle. Close is an
// ACTION, not a form (mirroring programDeactivate). Success redirects to the list.
func (s *server) fundClose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.CloseFund(s.actorCtx(ctx), id); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/funds?closed=1", http.StatusSeeOther)
}

// fundReopen handles POST /funds/{id}/reopen (TxnWrite): ReopenFund sets active=1;
// the fund returns to the active list. Success redirects to the active list.
func (s *server) fundReopen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.ReopenFund(s.actorCtx(ctx), id); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/funds", http.StatusSeeOther)
}

// renderFundFormError maps a store TYPED error to an i18n field-error key and
// re-renders the "fund-form" partial at 422 (the p10.3 convention). It never
// re-validates -- the store is the source of truth; this only TRANSLATES its typed
// errors to (field, key) pairs. An unknown error is a real server fault.
func (s *server) renderFundFormError(w http.ResponseWriter, r *http.Request, form fundForm, err error) {
	field, key := fundErrorField(err)
	if key == "" {
		s.serverError(w)
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "fund-form", form)
}

// fundErrorField maps a store typed error to the (form field, i18n key) pair the
// form-error convention needs. The field name drives autofocus placement. An
// unrecognized error returns ("",""), treated as a 500.
func fundErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrFundNoSubsidiary):
		return "subs", "error.fund.no_subsidiary"
	case errors.Is(err, store.ErrFundProgramMissing):
		return "program_id", "error.fund.program_missing"
	case errors.Is(err, store.ErrFundSubInUseBySplit):
		return "subs", "error.fund.sub_in_use"
	case errors.Is(err, store.ErrFundNotFound):
		return "name", "error.fund.not_found"
	default:
		return "", ""
	}
}

// fundStatementURL is a tiny helper the list template uses for the per-row link.
func fundStatementURL(id int64) string { return "/funds/" + strconv.FormatInt(id, 10) }
