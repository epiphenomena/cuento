package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cuento/internal/db/sqlc"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p12.2 transaction editor -- the daily-use data-entry grid (Appendix C). Four
// routes (all TxnWrite): GET /transactions/new (blank grid), GET
// /transactions/{id}/edit (prefilled), POST /transactions (create), POST
// /transactions/{id} (update). This is the app's heart, so the FIVE TRAPS are baked
// in here:
//
//  1. EDIT ROUND-TRIPS SPLIT IDS. The edit form emits each existing split's id as a
//     hidden field; parseSplitForms maps it into SplitInput.ID, so UpdateTransaction
//     diffs by id (one op=update for a changed split, none for the untouched ones)
//     instead of delete-all + recreate-all.
//  3. ONE DR/CR<->SIGN SITE (client). The amount column posts ONE signed field
//     (amount_i); the DR/CR twin columns are a JS-only convenience that writes the
//     computed signed value into that hidden field before submit. So the SERVER is
//     mode-agnostic: it always parses a signed net-debit amount (money.Parse is used
//     but the client already normalized), the store contract is unchanged, and
//     signed-mode entry works with no JS.
//  4. STABLE INPUT IDS. Ids are keyed to split identity/position (txn-account-<i>,
//     etc.), NOT a counter that reinitializes -- so a 422 re-render keeps the same
//     ids, focus retention and htmx swaps work, and the id key is the SAME one that
//     carries the split-id round-trip (trap 1).
//  5. SERVER IS THE SOLE VALIDATOR. The handler calls Post/UpdateTransaction and maps
//     the store's typed errors to slots WITHOUT re-deriving validation: ErrUnbalanced
//     / ErrFundUnbalanced -> the totals bar; the row-scoped errors -> the offending
//     row (attributed from the option data the editor ALREADY loaded, never by
//     re-running balance/scope logic). Client chips are display-only.
//
// Scope boundary (this step only): the editor grid + account combobox +
// create/edit round-trip. NOT void/duplicate/history (p12.4), NOT the
// keyboard-only QA pass (p12.6). (The header payee field + its autofill were
// retired in p26.19/p26.20; per-split descriptions replace them.)

// txnRowModel is one split row in the editor. It carries the row's current values
// (echoed on a re-render) plus its stable key and any per-row error key (trap 5).
type txnRowModel struct {
	Index       int    // 0-based row position -> the stable id/name suffix (trap 4)
	SplitID     string // existing split id (hidden field, trap 1); "" for a new row
	Account     int64  // chosen account id (0 = none)
	AmountDR    string // debit magnitude (DR/CR mode display)
	AmountCR    string // credit magnitude (DR/CR mode display)
	Amount      string // the SIGNED value echoed into the hidden signed field (trap 3)
	Fund        int64  // chosen fund id (0 = unrestricted)
	Program     int64  // chosen program id (R/E rows)
	Class       string // functional class (expense rows)
	Description string // per-split free-text (p26.19; autocomplete + prefill source)
	Memo        string
	ErrorKey    string // i18n key of this row's error (trap 5); "" = ok
}

// txnAccountOption is one account offered in a row's account combobox. It carries the
// metadata the client's row logic needs: Type (program/class gating), the account
// defaults (prefill), and the subsidiary set (the client re-filter on a header
// subsidiary change, Appendix C).
type txnAccountOption struct {
	ID             int64
	Name           string
	Path           string // dotted ancestor chain ending in Name (combobox label, p26.1)
	Type           string
	DefaultProgram int64  // 0 = none
	DefaultClass   string // "" = none
	SubsCSV        string // comma-separated subsidiary ids (data-subsidiaries)
	// Unavailable marks an account force-included because an existing split references
	// it though it is inactive / a placeholder / out-of-subsidiary (p26.10). The
	// template appends a marker suffix and a data-unavailable attribute so the user
	// sees why the row's account is special; the value stays the real id, SELECTED.
	Unavailable bool
}

// txnOption is a plain id/name option (funds, programs, payees, subsidiaries).
type txnOption struct {
	ID   int64
	Name string
}

// txnEditorModel is the whole editor page/partial model.
type txnEditorModel struct {
	// Header.
	TxnID      int64 // 0 = create (POST /transactions); else edit (POST /transactions/{id})
	IsEdit     bool
	Subsidiary int64
	Date       string // echoed in the user's date format
	Memo       string
	Notes      string // longer multiline explanation (p24.2)
	Currency   string

	DisplayDRCR bool // the user's display_mode == dr_cr -> render twin columns
	DateFormat  string

	// ImportRowID != 0 puts the editor in p17.3 "edit & post" mode: the form posts to
	// /import/rows/{id}/post (LINK the staged row to the created txn), the subsidiary is
	// LOCKED to the batch's subsidiary (disabled select + a hidden carrier, since a
	// disabled control does not POST; the sub-change re-filter hx-get is suppressed).
	ImportRowID   int64
	ImportBatchID int64 // the batch to return to on cancel / after post (import mode)

	// ExpenseReportID != 0 puts the editor in p20.3 "review & post" mode: the form
	// posts to /expenses/review/{id}/post (create the balanced txn + CONVERT the report
	// atomically), the subsidiary is LOCKED to the report's subsidiary (same disabled
	// select + hidden carrier as import mode), and cancel returns to the review queue.
	ExpenseReportID int64

	Rows []txnRowModel

	// Options.
	Subsidiaries []txnOption
	Accounts     []txnAccountOption // filtered to Subsidiary (leaf+active)
	Funds        []txnOption        // ActiveFunds(Subsidiary)
	Programs     []txnOption        // active programs
	Classes      []string           // program|management|fundraising

	RootProgram int64 // the program-defaulting fallback (D24)
	UserProgram int64 // the user's default_program (p26.5); 0 = unset. Prefill tier BELOW an account's own default_program and ABOVE RootProgram.

	// --- p26.34 main-split header ---------------------------------------------
	// The position-0 split is presented as a transaction-level HEADER (desc + account)
	// whose amount is the auto-balanced per-fund residual of the body splits (computed
	// SERVER-SIDE). MainPresent gates the whole feature: it is true for a normal txn
	// editor; false for import / expense-review (their grids stay flat) and for the
	// multi-fund reload FALLBACK (FlatFallback), which renders every split as a body row.
	MainPresent bool
	// FlatFallback is set when a LOADED txn has >=2 distinct fund groups: the header
	// decomposition/fan-out is skipped and every split renders as a flat body row (the
	// pre-p26.34 grid), saved as-is. Multi-fund reload is a zero-instance case today; this
	// guard keeps the fragile reconstruction out of the load path (see decomposeMain).
	FlatFallback    bool
	MainAccount     int64  // the main (position-0) split's account
	MainDescription string // header description (fuels descfield autocomplete for all splits)
	MainMemo        string
	MainProgram     int64  // round-tripped when the main account is R/E
	MainClass       string // round-tripped when the main account is expense
	MainFund        int64  // display-only gating; the SAVED main fund is DERIVED from the body
	MainSplitID     string // existing split0 id (round-tripped so UpdateTransaction diffs by id)
	MainAmount      string // the auto-balanced residual, formatted for DISPLAY (server recomputes on save)

	// Origin is where Cancel returns for a NORMAL (non-import / non-expense) txn: the
	// account register the user came from (p26.33), threaded as a `from` query param
	// from the register's new/edit links and round-tripped on a 422 re-render. Empty
	// (or an off-site value) falls back to /accounts. Import/expense modes have their
	// own dedicated cancel destinations and ignore this.
	Origin string

	// Errors (trap 5). TotalsError is the overall/fund-imbalance key rendered in the
	// sticky totals bar; row errors live on each row's ErrorKey.
	TotalsError string
	// FirstErrorRow is the index of the first row carrying an error (autofocus target
	// after a 422 swap); -1 = none.
	FirstErrorRow int

	// RejectError is the i18n KEY of the reject-reason error shown next to the reject
	// form on the review PAGE (p26.27: a blank reject reason re-renders the review page
	// at 422 with this key rendered via {{t}}); "" = none. Only meaningful in the
	// ExpenseReportID (review & post) mode.
	RejectError string
}

// txnNewForm handles GET /transactions/new: a blank grid defaulted to the user's
// subsidiary (else the sole/root), with two empty rows to start. It ALSO serves the
// subsidiary-change re-filter: the header select's hx-get re-fetches this route with
// the chosen `subsidiary` and the current rows (hx-include), so the server re-filters
// the account comboboxes AND fund options to the new sub (Appendix C: never
// silent-clear -- a row referencing a now-invalid account keeps its value and is
// flagged). This is the no-JS fallback too; the client mirrors it for snappiness.
func (s *server) txnNewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)

	// A `subsidiary` query param means this is a re-filter (the select's hx-get);
	// absent means a first load -> the user's default subsidiary.
	sub := s.defaultSubsidiary(ctx, u)
	reFilter := r.URL.Query().Get("subsidiary") != ""
	if reFilter {
		sub = parseID(r.URL.Query().Get("subsidiary"))
	}

	model, err := s.newEditorModel(ctx, u, sub)
	if err != nil {
		s.serverError(w)
		return
	}
	// p26.33: Cancel returns to the register the user came from. On the re-filter the
	// origin rides in via hx-include (the hidden #txn-origin field), so read the query
	// either way.
	model.Origin = sanitizeOrigin(r.URL.Query().Get("from"))
	// p26.34: a NEW normal txn uses the header-main mode (the position-0 split is the
	// header desc + account, its amount the auto-balanced residual of the body).
	model.MainPresent = true

	if reFilter {
		// Echo the included rows (the user's typed entries survive the re-filter) and
		// flag any row whose account is not in the new sub (never silent-clear).
		rows := s.echoRowsFromQuery(r, model)
		model.Rows = rows
		for i := range rows {
			if rows[i].ErrorKey != "" {
				model.FirstErrorRow = i
				break
			}
		}
		if d := r.URL.Query().Get("date"); d != "" {
			model.Date = d
		}
		model.Memo = r.URL.Query().Get("memo")
		model.Notes = r.URL.Query().Get("notes")
		// p26.34: echo the header main fields across the subsidiary re-filter (they ride in
		// via the hidden main_* inputs on hx-include). The header account keeps its value even
		// if it left the new sub (the store re-validates on save; the p26.10 inject shows it).
		if mh, ok := parseMainHeader(r); ok {
			model.MainAccount = mh.AccountID
			model.MainDescription = mh.Description
			model.MainMemo = mh.Memo
			model.MainProgram = mh.ProgramID
			model.MainClass = mh.Class
			model.MainSplitID = mh.SplitID
			s.injectMainAccount(ctx, &model)
		}
	} else {
		// First load: today's date (Appendix C `t` = today) and ONE empty body row. The
		// client auto-appends a fresh trailing row as soon as the last row is edited
		// (p25.2), so there is always exactly one empty row and no "Add row" button; the
		// server drops the trailing empty row on submit (parseSplitForms).
		model.Date = money.FormatDate(time.Now(), dateFormatFor(u))
		model.Rows = []txnRowModel{{Index: 0}}
	}
	s.renderEditor(w, r, model)
}

// echoRowsFromQuery rebuilds the editor rows from the GET query (the subsidiary
// re-filter's hx-include), preserving every typed value and flagging any row whose
// chosen account is not in the newly selected subsidiary's option set (Appendix C:
// per-row error, never silent-clear). It reuses the model's already-loaded account
// options (no re-derivation) to decide membership.
func (s *server) echoRowsFromQuery(r *http.Request, model txnEditorModel) []txnRowModel {
	inSub := make(map[int64]bool, len(model.Accounts))
	for _, a := range model.Accounts {
		inSub[a.ID] = true
	}
	n := int(parseID(r.URL.Query().Get("rows")))
	if n <= 0 {
		n = 1
	}
	rows := make([]txnRowModel, 0, n)
	for i := 0; i < n; i++ {
		si := strconv.Itoa(i)
		q := r.URL.Query()
		acct := parseID(q.Get("account_" + si))
		row := txnRowModel{
			Index:       i,
			SplitID:     q.Get("split_id_" + si),
			Account:     acct,
			Amount:      q.Get("amount_" + si),
			AmountDR:    q.Get("dr_" + si),
			AmountCR:    q.Get("cr_" + si),
			Fund:        parseID(q.Get("fund_" + si)),
			Program:     parseID(q.Get("program_" + si)),
			Class:       q.Get("class_" + si),
			Description: q.Get("description_" + si),
			Memo:        q.Get("memo_" + si),
		}
		// A chosen account that left the new sub's set is flagged (its fund options may
		// also have changed; the store re-validates authoritatively on save).
		if acct != 0 && !inSub[acct] {
			row.ErrorKey = "error.txn.account_not_in_sub"
		}
		rows = append(rows, row)
	}
	return rows
}

// txnEditForm handles GET /transactions/{id}/edit: prefill the grid from the live
// transaction (header + splits, each split's id round-tripped as a hidden field --
// trap 1).
func (s *server) txnEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	id := parseID(r.PathValue("id"))

	hdr, err := s.store.GetTransaction(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	splits, err := s.store.TransactionSplits(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}

	model, err := s.newEditorModel(ctx, u, hdr.SubsidiaryID)
	if err != nil {
		s.serverError(w)
		return
	}
	model.TxnID = id
	model.IsEdit = true
	model.Currency = hdr.Currency
	model.Date = money.FormatDate(parseISOForDisplay(hdr.Date), dateFormatFor(u))
	model.Memo = hdr.Memo
	model.Notes = hdr.Notes
	// p26.33: Cancel returns to the register the edit link came from (`from` query param).
	model.Origin = sanitizeOrigin(r.URL.Query().Get("from"))

	exp := s.currencyExponent(ctx, hdr.Currency)
	// The prefilled amount MUST use the user's NUMBER format (rule 10) so that a
	// save-without-touching round-trips: parseSplitForms parses with the same format,
	// and a mismatch (e.g. EU number, dot=grouping) would corrupt the amount. Display
	// stays Signed (the hidden signed field is always net-debit); NegStyle is irrelevant
	// to the magnitudes/DR-CR columns.
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}

	// p26.34: DECOMPOSE the loaded splits into the header main (position 0) + body (the
	// rest). Splits arrive in position order (SplitsByTransaction ORDER BY position, id).
	// A MULTI-FUND stored txn (>=2 distinct fund groups) is a zero-instance case today; to
	// keep the fragile fan-out reconstruction OUT of the load path, we FALL BACK to the flat
	// grid (every split a body row, no header) -- so a re-save posts the splits as-is via the
	// pre-p26.34 path (main_present=0), never re-fanning-out and never double-counting.
	distinctFunds := make(map[int64]bool)
	for _, sp := range splits {
		key := int64(0)
		if sp.FundID.Valid {
			key = sp.FundID.Int64
		}
		distinctFunds[key] = true
	}
	// log-comment (task cap-guard rule): multi-fund reload takes the flat fallback.
	flat := len(distinctFunds) >= 2
	model.MainPresent = !flat
	model.FlatFallback = flat

	bodyRow := func(idx int, sp sqlc.Split) txnRowModel {
		row := txnRowModel{
			Index:       idx,
			SplitID:     strconv.FormatInt(sp.ID, 10),
			Account:     sp.AccountID,
			Amount:      money.Format(sp.Amount, exp, fmtOpts),
			Description: sp.Description,
			Memo:        sp.Memo,
		}
		if sp.FundID.Valid {
			row.Fund = sp.FundID.Int64
		}
		if sp.ProgramID.Valid {
			row.Program = sp.ProgramID.Int64
		}
		if sp.FunctionalClass.Valid {
			row.Class = sp.FunctionalClass.String
		}
		if sp.Amount >= 0 {
			row.AmountDR = money.Format(sp.Amount, exp, fmtOpts)
		} else {
			row.AmountCR = money.Format(-sp.Amount, exp, fmtOpts)
		}
		return row
	}

	if model.MainPresent && len(splits) > 0 {
		// Header = split0; body = split1..n (re-indexed 0-based body rows).
		m := splits[0]
		model.MainAccount = m.AccountID
		model.MainDescription = m.Description
		model.MainMemo = m.Memo
		model.MainSplitID = strconv.FormatInt(m.ID, 10)
		model.MainAmount = money.Format(m.Amount, exp, fmtOpts)
		if m.FundID.Valid {
			model.MainFund = m.FundID.Int64
		}
		if m.ProgramID.Valid {
			model.MainProgram = m.ProgramID.Int64
		}
		if m.FunctionalClass.Valid {
			model.MainClass = m.FunctionalClass.String
		}
		s.injectMainAccount(ctx, &model)
		for i, sp := range splits[1:] {
			model.Rows = append(model.Rows, bodyRow(i, sp))
		}
	} else {
		// Flat fallback (multi-fund) or an empty txn: every split is a body row.
		for i, sp := range splits {
			model.Rows = append(model.Rows, bodyRow(i, sp))
		}
	}

	// Ensure every split's account (even a now-inactive / out-of-sub one) renders as a
	// SELECTED option rather than a blank select (p26.10).
	if err := s.injectRowAccounts(ctx, &model); err != nil {
		s.serverError(w)
		return
	}
	s.renderEditor(w, r, model)
}

// renderEditor renders the editor: the full shell page normally, or JUST the
// transaction-form partial for an htmx request (the subsidiary-change re-filter swaps
// #txn-form, so it must not nest a whole page inside it -- Appendix C anti-jank). Both
// paths render the SAME single-sourced partial, so input ids stay stable (trap 4).
func (s *server) renderEditor(w http.ResponseWriter, r *http.Request, model txnEditorModel) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, http.StatusOK, "transaction-form", s.newShellPage(r, model))
		return
	}
	s.render(w, r, http.StatusOK, "transaction_form.tmpl", s.newWideShellPage(r, model))
}

// txnCreate handles POST /transactions.
func (s *server) txnCreate(w http.ResponseWriter, r *http.Request) {
	s.txnSubmit(w, r, 0)
}

// txnUpdate handles POST /transactions/{id}.
func (s *server) txnUpdate(w http.ResponseWriter, r *http.Request) {
	s.txnSubmit(w, r, parseID(r.PathValue("id")))
}

// txnSubmit is the shared create/update path. It parses the form into a
// PostTransactionInput (client already normalized DR/CR into the signed amount_i
// field, trap 3), calls the store (the SOLE validator, trap 5), and on a typed error
// re-renders the form region at 422 with the error routed to its slot. txnID == 0 is
// a create; else an update.
func (s *server) txnSubmit(w http.ResponseWriter, r *http.Request, txnID int64) {
	ctx := r.Context()
	u := currentUser(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w)
		return
	}

	subID := parseID(r.FormValue("subsidiary"))
	currency := r.FormValue("currency")
	model, err := s.newEditorModel(ctx, u, subID)
	if err != nil {
		s.serverError(w)
		return
	}
	model.TxnID = txnID
	model.IsEdit = txnID != 0
	model.Currency = currency
	model.Memo = r.FormValue("memo")
	model.Notes = r.FormValue("notes")
	// p26.33: preserve the Cancel origin across a 422 re-render (the hidden #txn-origin
	// field posts with the form).
	model.Origin = sanitizeOrigin(r.FormValue("from"))
	model.FirstErrorRow = -1

	// Parse the date input honoring the user's format (ISO always accepted, D16). A
	// malformed date is a form error surfaced on the totals bar (a header field).
	dateISO, dateOK := parseEditorDate(r.FormValue("date"), dateFormatFor(u), s.now())
	model.Date = r.FormValue("date")

	rows, splits := s.parseSplitForms(r, s.currencyExponent(ctx, currency))
	model.Rows = rows
	// A submitted row may reference an inactive / out-of-sub account (e.g. editing a
	// pre-existing split whose account was deactivated); a 422 re-render must keep it
	// SELECTED, not re-blank it (p26.10). Best-effort: on a lookup failure the row keeps
	// its raw value and the store stays the authoritative validator.
	_ = s.injectRowAccounts(ctx, &model)

	// p26.34: when the header-main mode is active, the position-0 split arrives via the
	// header main_* fields and its amount is OMITTED -- the server computes the per-fund
	// residual and constructs the main split(s) (single fund → one; multi-fund → fan out).
	// The body `splits` become positions 1..m. Echo the header onto the model so a 422
	// re-render keeps it, and expose the residual for display.
	exp := s.currencyExponent(ctx, currency)
	numMains := 0 // p26.34: split-index offset for error attribution (mains are prepended)
	if mh, ok := parseMainHeader(r); ok {
		model.MainPresent = true
		model.MainAccount = mh.AccountID
		model.MainDescription = mh.Description
		model.MainMemo = mh.Memo
		model.MainProgram = mh.ProgramID
		model.MainClass = mh.Class
		model.MainSplitID = mh.SplitID
		s.injectMainAccount(ctx, &model)
		splits, numMains = autoBalanceMain(mh, splits)
		// The main residual for the header amount display (recomputed live client-side too).
		if len(splits) > 0 {
			model.MainAmount = money.Format(splits[0].Amount, exp, money.FormatOpts{Number: numberFormatFor(u)})
		}
	}

	if !dateOK {
		model.TotalsError = "error.txn.bad_date"
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	in := store.PostTransactionInput{
		Date:         dateISO,
		SubsidiaryID: subID,
		Memo:         model.Memo,
		Notes:        model.Notes,
		Currency:     currency,
		Splits:       splits,
	}

	// The write funnel needs the current user as the actor (rule 2/5).
	wctx := s.actorCtx(ctx)
	var postErr error
	if txnID == 0 {
		_, postErr = s.store.PostTransaction(wctx, in)
	} else {
		postErr = s.store.UpdateTransaction(wctx, txnID, in)
	}
	if postErr != nil {
		s.routeTxnError(&model, postErr, splits, numMains)
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	// Success: redirect to a deterministic destination -- the first split's account
	// register, so the entry is visible (p12.1). An htmx submit swaps the form region,
	// so a plain 303 would nest the register page inside #txn-form; instead send
	// HX-Redirect so htmx does a full-page client navigation. A non-htmx (no-JS)
	// submit gets the normal 303 (handler redirect, distinct from the enforcement 302).
	dest := "/accounts"
	if len(splits) > 0 {
		dest = "/accounts/" + strconv.FormatInt(splits[0].AccountID, 10) + "/register"
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// routeTxnError maps a store typed error to a slot on the model (trap 5). Totals-bar
// errors (overall / per-fund imbalance) go to TotalsError; row-scoped errors are
// attributed to a row using data the editor ALREADY has (the option sets, the split
// inputs) -- never by re-running balance/scope logic. The attribution is best-effort:
// the listed test requires the error to land on A ROW (not a page alert), which the
// first-matching-row heuristic satisfies deterministically.
// routeTxnError takes numMains = the count of MAIN (header) splits PREPENDED to `splits`
// by autoBalanceMain (0 in the flat / import / expense paths). A store error whose split
// index falls in the main range (< numMains) is a HEADER error and is routed to the totals
// bar (the header has no per-row error cell); a body index maps to model.Rows[idx-numMains].
func (s *server) routeTxnError(model *txnEditorModel, err error, splits []store.SplitInput, numMains int) {
	key := txnErrorKey(err)

	// Totals-bar errors: overall or per-fund imbalance, and the header date.
	if errors.Is(err, store.ErrUnbalanced) || errors.Is(err, store.ErrFundUnbalanced) {
		model.TotalsError = key
		return
	}

	// Row-scoped errors. Attribute to the first split that STRUCTURALLY matches the
	// error, from data already loaded -- no validation is re-derived. The returned index
	// is into the FULL split list (mains prepended).
	idx := s.attributeRowError(model, err, splits)
	if idx < 0 {
		// Unattributable typed error (should not happen for the mapped set): fall
		// back to the totals bar so it is never silently dropped.
		model.TotalsError = key
		return
	}
	// A MAIN (header) split error has no per-row cell -> surface on the totals bar (the
	// header region). This includes the accountless-header case (main account 0).
	if idx < numMains {
		model.TotalsError = key
		return
	}
	row := idx - numMains
	if row >= len(model.Rows) {
		// Defensive: an out-of-range body index (should not happen once numMains is
		// subtracted) falls back to the totals bar rather than panicking.
		model.TotalsError = key
		return
	}
	model.Rows[row].ErrorKey = key
	model.FirstErrorRow = row
}

// attributeRowError returns the index of the row a row-scoped store error belongs to,
// using ONLY data the editor already has (trap 5: no re-derivation). Returns -1 when
// no row matches.
func (s *server) attributeRowError(model *txnEditorModel, err error, splits []store.SplitInput) int {
	// Build a set of account ids valid for the chosen subsidiary from the loaded
	// option list (the SAME options the combobox offered).
	inSub := make(map[int64]bool, len(model.Accounts))
	acctType := make(map[int64]string, len(model.Accounts))
	for _, a := range model.Accounts {
		// An Unavailable option was force-included only so the row can DISPLAY its account
		// (p26.10); it is NOT a normally-offered (leaf+active+in-sub) account, so it must
		// NOT count as in-sub here -- otherwise an ErrInactiveAccount / not-in-sub error
		// would fail to attribute to the offending row.
		if !a.Unavailable {
			inSub[a.ID] = true
		}
		acctType[a.ID] = a.Type
	}

	switch {
	case errors.Is(err, store.ErrAccountNotInSubsidiary),
		errors.Is(err, store.ErrPlaceholderAccount),
		errors.Is(err, store.ErrInactiveAccount),
		errors.Is(err, store.ErrAccountMissing):
		// The offending row's account is NOT in the offered (leaf+active+in-sub) set.
		for i, sp := range splits {
			if !inSub[sp.AccountID] {
				return i
			}
		}
	case errors.Is(err, store.ErrFundProgramScope),
		errors.Is(err, store.ErrInactiveFund),
		errors.Is(err, store.ErrFundSubsidiaryScope),
		errors.Is(err, store.ErrFundMissing):
		// A fund-scope violation belongs to a row that carries a fund. Prefer an R/E
		// row (the program-scope case) but any funded row is a safe landing.
		for i, sp := range splits {
			if sp.FundID != nil && isRERow(acctType[sp.AccountID]) {
				return i
			}
		}
		for i, sp := range splits {
			if sp.FundID != nil {
				return i
			}
		}
	case errors.Is(err, store.ErrExpenseNeedsFunction),
		errors.Is(err, store.ErrNonExpenseFunction):
		// Structural: an expense row missing a class, or a non-expense row with one.
		for i, sp := range splits {
			isExp := acctType[sp.AccountID] == "expense"
			hasClass := sp.FunctionalClass != nil && *sp.FunctionalClass != ""
			if isExp && !hasClass {
				return i
			}
			if !isExp && hasClass {
				return i
			}
		}
	case errors.Is(err, store.ErrProgramOnBalanceSheet),
		errors.Is(err, store.ErrInactiveProgram),
		errors.Is(err, store.ErrProgramMissing):
		for i, sp := range splits {
			t := acctType[sp.AccountID]
			if !isRERow(t) && sp.ProgramID != nil {
				return i // A/L/E row carrying a program
			}
			if isRERow(t) {
				return i
			}
		}
	}
	return -1
}

func isRERow(t string) bool { return t == "revenue" || t == "expense" }

// txnErrorKey maps a store typed error to its i18n key (rule 9: Go returns keys).
func txnErrorKey(err error) string {
	switch {
	case errors.Is(err, store.ErrUnbalanced):
		return "error.txn.unbalanced"
	case errors.Is(err, store.ErrFundUnbalanced):
		return "error.txn.fund_unbalanced"
	case errors.Is(err, store.ErrTooFewSplits):
		return "error.txn.too_few_splits"
	case errors.Is(err, store.ErrBadDate):
		return "error.txn.bad_date"
	case errors.Is(err, store.ErrAccountNotInSubsidiary):
		return "error.txn.account_not_in_sub"
	case errors.Is(err, store.ErrPlaceholderAccount):
		return "error.txn.placeholder_account"
	case errors.Is(err, store.ErrInactiveAccount):
		return "error.txn.inactive_account"
	case errors.Is(err, store.ErrAccountMissing):
		return "error.txn.account_missing"
	case errors.Is(err, store.ErrInactiveFund):
		return "error.txn.inactive_fund"
	case errors.Is(err, store.ErrFundSubsidiaryScope):
		return "error.txn.fund_sub_scope"
	case errors.Is(err, store.ErrFundProgramScope):
		return "error.txn.fund_program_scope"
	case errors.Is(err, store.ErrFundMissing):
		return "error.txn.fund_missing"
	case errors.Is(err, store.ErrExpenseNeedsFunction):
		return "error.txn.expense_needs_class"
	case errors.Is(err, store.ErrNonExpenseFunction):
		return "error.txn.non_expense_class"
	case errors.Is(err, store.ErrProgramOnBalanceSheet):
		return "error.txn.program_on_bs"
	case errors.Is(err, store.ErrInactiveProgram):
		return "error.txn.inactive_program"
	case errors.Is(err, store.ErrProgramMissing):
		return "error.txn.program_missing"
	case errors.Is(err, store.ErrInactiveSubsidiary):
		return "error.txn.inactive_sub"
	case errors.Is(err, store.ErrInactiveCurrency):
		return "error.txn.inactive_currency"
	case errors.Is(err, store.ErrTransactionNotFound):
		return "error.txn.not_found"
	default:
		return "error.txn.generic"
	}
}

// --- parsing --------------------------------------------------------------

// parseSplitForms reads the per-row fields (account_i, amount_i, fund_i, program_i,
// class_i, memo_i, split_id_i) up to the `rows` count, returning both the echo model
// rows and the store SplitInputs. Row order == position. The SIGNED amount_i field is
// parsed with money.Parse (the client already normalized DR/CR into it, trap 3); an
// empty/blank row (no account AND no amount) is skipped so a trailing empty row is
// not submitted.
func (s *server) parseSplitForms(r *http.Request, exp int) ([]txnRowModel, []store.SplitInput) {
	n := int(parseID(r.FormValue("rows")))
	if n <= 0 {
		n = 0
	}
	var rows []txnRowModel
	var splits []store.SplitInput
	pos := int64(0)
	for i := 0; i < n; i++ {
		si := strconv.Itoa(i)
		acct := parseID(r.FormValue("account_" + si))
		amountStr := r.FormValue("amount_" + si)
		fund := parseID(r.FormValue("fund_" + si))
		prog := parseID(r.FormValue("program_" + si))
		class := r.FormValue("class_" + si)
		desc := r.FormValue("description_" + si)
		memo := r.FormValue("memo_" + si)
		splitID := r.FormValue("split_id_" + si)

		row := txnRowModel{
			Index:       i,
			SplitID:     splitID,
			Account:     acct,
			Amount:      amountStr,
			Fund:        fund,
			Program:     prog,
			Class:       class,
			Description: desc,
			Memo:        memo,
			AmountDR:    r.FormValue("dr_" + si),
			AmountCR:    r.FormValue("cr_" + si),
		}
		rows = append(rows, row)

		// Skip ONLY a fully-empty scaffold row (no account, no amount of any kind, no
		// memo). A row that carries content but no account is NOT dropped silently
		// (p26.10): it is submitted with account 0 so the store returns ErrAccountMissing,
		// which surfaces as a visible per-row error. This matches the client's isRowEmpty
		// predicate (memo / DR / CR count as content), so client and server agree on which
		// row is the droppable trailing empty.
		if acct == 0 && amountStr == "" && row.AmountDR == "" && row.AmountCR == "" && memo == "" {
			continue
		}

		amount, perr := money.Parse(amountStr, exp, numberFormatFor(currentUser(r.Context())))
		if perr != nil {
			// Leave the amount as 0 so the store's balance check rejects it; the echo
			// keeps the raw text. (money.Parse being liberal, this is rare.)
			amount = 0
		}

		sp := store.SplitInput{
			AccountID:   acct,
			Amount:      amount,
			Memo:        memo,
			Description: desc,
			Position:    pos,
		}
		pos++
		if id := parseID(splitID); id != 0 {
			sp.ID = &id
		}
		if fund != 0 {
			sp.FundID = &fund
		}
		if prog != 0 {
			sp.ProgramID = &prog
		}
		if class != "" {
			c := class
			sp.FunctionalClass = &c
		}
		splits = append(splits, sp)
	}
	return rows, splits
}

// mainHeaderInput is the parsed position-0 header (p26.34): the account/desc/memo the
// user sees in the header, plus the round-tripped program/class/split-id. The amount is
// NEVER submitted -- it is the auto-balanced residual computed by autoBalanceMain.
type mainHeaderInput struct {
	AccountID   int64
	Description string
	Memo        string
	ProgramID   int64
	Class       string
	SplitID     string
}

// parseMainHeader reads the header main_* fields (p26.34). Present reports whether the
// header-main mode is active (main_present=1); when false the caller uses the flat grid
// (import / expense / multi-fund fallback).
func parseMainHeader(r *http.Request) (mainHeaderInput, bool) {
	if r.FormValue("main_present") != "1" {
		return mainHeaderInput{}, false
	}
	return mainHeaderInput{
		AccountID:   parseID(r.FormValue("main_account")),
		Description: r.FormValue("main_description"),
		Memo:        r.FormValue("main_memo"),
		ProgramID:   parseID(r.FormValue("main_program")),
		Class:       r.FormValue("main_class"),
		SplitID:     r.FormValue("main_split_id"),
	}, true
}

// autoBalanceMain constructs the MAIN split(s) from the header + the parsed BODY splits
// (p26.34, rules 3+12: money math in Go). The body splits are the positions AFTER the
// main run; this function PREPENDS the main split(s) at positions 0..k-1 and returns the
// full split list (main first, body shifted after) ready for the store.
//
//   - Single fund among the body → ONE main split: amount = -(sum of body), fund = that
//     single fund, program/class/memo/description/split-id from the header. Idempotent:
//     loading a single-fund txn into the header then saving reproduces byte-identical
//     splits (the residual reconstructs split0's amount; the id round-trips).
//   - Multi-fund body → FAN OUT: one main split per fund with a NONZERO residual, each at
//     the header account, positions 0..k-1. A fund whose body already nets to zero gets NO
//     main (an amount=0 split is rejected by the store). The store's per-fund zero-sum then
//     validates the result. The single main split id is round-tripped only when there is
//     exactly ONE main (k==1); a fan-out's extra mains are new (no id to reuse).
//
// The header account being 0 (unset) while the body has content is NOT special-cased here:
// a main split with account 0 is emitted so the store returns ErrAccountMissing (never
// silently dropped -- do not regress "every split needs an account").
//
// Returns the full split list AND the number of MAIN splits (k) prepended, so the caller
// can map a store error's split index back to the right slot: index < k is a MAIN (header)
// error; index >= k maps to body row (index - k).
func autoBalanceMain(main mainHeaderInput, body []store.SplitInput) ([]store.SplitInput, int) {
	// Per-fund residual over the body (fund key 0 == unrestricted group), in first-seen
	// order so the fan-out is deterministic.
	residual := make(map[int64]int64)
	var order []int64
	seen := make(map[int64]bool)
	for _, b := range body {
		key := int64(0)
		if b.FundID != nil {
			key = *b.FundID
		}
		if !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
		residual[key] += b.Amount
	}

	// The main split(s), one per fund with a nonzero residual.
	var mains []store.SplitInput
	for _, key := range order {
		amt := -residual[key]
		if amt == 0 {
			continue // fund already balanced within the body: no main split needed
		}
		sp := store.SplitInput{
			AccountID:   main.AccountID,
			Amount:      amt,
			Memo:        main.Memo,
			Description: main.Description,
		}
		if key != 0 {
			k := key
			sp.FundID = &k
		}
		if main.ProgramID != 0 {
			p := main.ProgramID
			sp.ProgramID = &p
		}
		if main.Class != "" {
			c := main.Class
			sp.FunctionalClass = &c
		}
		mains = append(mains, sp)
	}
	// A body with ZERO nonzero-residual funds (already balanced, or empty) yields no main;
	// still emit ONE main carrying the header account so the store sees the header content
	// (and rejects an unbalanced / accountless / too-few-splits txn rather than silently
	// dropping the header). amount 0 -> the store's balance check rejects.
	if len(mains) == 0 {
		sp := store.SplitInput{AccountID: main.AccountID, Amount: 0, Memo: main.Memo, Description: main.Description}
		if main.ProgramID != 0 {
			p := main.ProgramID
			sp.ProgramID = &p
		}
		if main.Class != "" {
			c := main.Class
			sp.FunctionalClass = &c
		}
		mains = append(mains, sp)
	}
	// Round-trip the split id ONLY when there is exactly one main (single-fund / the common
	// idempotent case). A fan-out's extra mains are new inserts.
	if len(mains) == 1 {
		if id := parseID(main.SplitID); id != 0 {
			mains[0].ID = &id
		}
	}

	// Assemble: mains at positions 0..k-1, body shifted to k..k+m-1.
	out := make([]store.SplitInput, 0, len(mains)+len(body))
	pos := int64(0)
	for i := range mains {
		mains[i].Position = pos
		pos++
		out = append(out, mains[i])
	}
	for i := range body {
		body[i].Position = pos
		pos++
		out = append(out, body[i])
	}
	return out, len(mains)
}

// parseEditorDate parses the editor's date input honoring df (ISO always accepted,
// D16), returning the ISO string and whether it parsed. An empty input is invalid
// (a transaction needs a date).
func parseEditorDate(s string, df money.DateFormat, now time.Time) (string, bool) {
	if s == "" {
		return "", false
	}
	t, err := money.ParseDate(s, df, now)
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// --- model assembly -------------------------------------------------------

// newEditorModel builds the editor model's option lists for a subsidiary: the
// account combobox (leaf+active, in sub), the fund select (ActiveFunds(sub)), the
// active programs, the payees, the subsidiary select, and the functional classes. It
// is shared by the new/edit forms and the re-render path so the options always match
// the chosen subsidiary (Appendix C: changing the sub re-filters accounts + funds).
func (s *server) newEditorModel(ctx context.Context, u *store.CurrentUser, sub int64) (txnEditorModel, error) {
	lang := langOf(ctx)
	model := txnEditorModel{
		Subsidiary:    sub,
		Currency:      "USD",
		DisplayDRCR:   displayModeOf(userDisplayMode(u)) == money.DebitCredit,
		DateFormat:    dateFormatCode(u),
		Classes:       []string{"program", "management", "fundraising"},
		FirstErrorRow: -1,
		UserProgram:   derefID(userDefaultProgram(u)),
	}

	// Subsidiary select (all subs, tree order). The editor's currency defaults to the
	// SELECTED subsidiary's base currency (D18) so an MXN-subsidiary entry records in
	// MXN, not a hardcoded USD. (Cross-currency flows go through FX Clearing as two
	// transactions, D3; a single txn is single-currency, so the sub's base is the
	// right default. The edit form overrides this with the txn's stored currency.)
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		return model, err
	}
	for _, sb := range subs {
		model.Subsidiaries = append(model.Subsidiaries, txnOption{ID: sb.ID, Name: sb.Name})
		if sb.ID == sub && sb.BaseCurrency != "" {
			model.Currency = sb.BaseCurrency
		}
	}

	// Account combobox: leaf+active accounts mapped to `sub`, with metadata.
	accts, err := s.store.AccountEditorOptions(ctx, lang, sub)
	if err != nil {
		return model, err
	}
	model.Accounts = toTxnAccountOptions(accts)

	// Fund select: funds scoped to `sub` (D20/Q1).
	funds, err := s.store.ActiveFunds(ctx, sub)
	if err != nil {
		return model, err
	}
	for _, f := range funds {
		model.Funds = append(model.Funds, txnOption{ID: f.ID, Name: f.Name})
	}

	// Program select: active programs (tree order). The root is the default fallback.
	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return model, err
	}
	for _, p := range progs {
		if p.Active == 0 {
			continue
		}
		if !p.ParentID.Valid {
			model.RootProgram = p.ID
		}
		model.Programs = append(model.Programs, txnOption{ID: p.ID, Name: p.Name})
	}

	return model, nil
}

// toTxnAccountOptions maps store account options into the editor's option model,
// carrying the Unavailable flag (p26.10) through to the template.
func toTxnAccountOptions(accts []store.AccountEditorOption) []txnAccountOption {
	out := make([]txnAccountOption, 0, len(accts))
	for _, a := range accts {
		opt := txnAccountOption{
			ID: a.ID, Name: a.Name, Path: a.Path, Type: a.Type,
			DefaultClass: a.DefaultClass, SubsCSV: idsCSV(a.SubsidiaryIDs),
			Unavailable: a.Unavailable,
		}
		if a.DefaultProgram != nil {
			opt.DefaultProgram = *a.DefaultProgram
		}
		out = append(out, opt)
	}
	return out
}

// injectRowAccounts re-builds model.Accounts so EVERY account id referenced by a
// current row appears as a selectable option (p26.10). A prefilled/edited split may
// reference an account that is now inactive / a placeholder / out-of-subsidiary and so
// is not in the normal offered set; without this the row's select renders blank ("the
// missing accounts" bug) and a naive save would try to blank it. The referenced ids
// are force-included via AccountEditorOptionsWith (marked Unavailable). It is a no-op
// when no row references an off-list account (NEW transactions are unchanged), and MUST
// be called at every render site that prefills rows -- the edit GET, the create/update
// 422 re-render, and the import / expense-review prefill + their error re-renders --
// so the account never re-blanks on a save that fails validation.
func (s *server) injectRowAccounts(ctx context.Context, model *txnEditorModel) error {
	if len(model.Rows) == 0 {
		return nil
	}
	offered := make(map[int64]bool, len(model.Accounts))
	for _, a := range model.Accounts {
		offered[a.ID] = true
	}
	var include []int64
	seen := make(map[int64]bool)
	for _, row := range model.Rows {
		if row.Account == 0 || offered[row.Account] || seen[row.Account] {
			continue
		}
		seen[row.Account] = true
		include = append(include, row.Account)
	}
	if len(include) == 0 {
		return nil
	}
	accts, err := s.store.AccountEditorOptionsWith(ctx, langOf(ctx), model.Subsidiary, include)
	if err != nil {
		return err
	}
	model.Accounts = toTxnAccountOptions(accts)
	return nil
}

// injectMainAccount ensures the HEADER main account (p26.34) is a selectable option, the
// same p26.10 guard as injectRowAccounts but for the header select: an edited/loaded txn
// whose position-0 account is now inactive / a placeholder / out-of-sub must still render
// SELECTED in the header, not blank. Best-effort (no-op when the account is already
// offered or unset); the store stays the authoritative validator on save.
func (s *server) injectMainAccount(ctx context.Context, model *txnEditorModel) {
	if model.MainAccount == 0 {
		return
	}
	for _, a := range model.Accounts {
		if a.ID == model.MainAccount {
			return // already offered
		}
	}
	accts, err := s.store.AccountEditorOptionsWith(ctx, langOf(ctx), model.Subsidiary, []int64{model.MainAccount})
	if err != nil {
		return
	}
	model.Accounts = toTxnAccountOptions(accts)
}

// userDefaultProgram returns the user's default_program_id (p26.5), nil-safe: nil for
// an anonymous request or an unset preference. Threaded to the client as data-user-program
// so gateRow can use it as a program-prefill tier below the account's own default_program.
func userDefaultProgram(u *store.CurrentUser) *int64 {
	if u == nil {
		return nil
	}
	return u.DefaultProgramID
}

// sanitizeOrigin validates a `from`/origin value for the Cancel link (p26.33). To avoid
// an open redirect it accepts ONLY a same-site absolute PATH: it must begin with a single
// "/" (not "//..." which browsers treat as a protocol-relative cross-origin URL) and carry
// no scheme. Anything else returns "" (the caller falls back to /accounts). The value is
// rendered into an href attribute (html/template escapes it); this guard keeps it a
// local navigation, not an escape hatch off the app.
func sanitizeOrigin(v string) string {
	if v == "" || len(v) < 1 || v[0] != '/' {
		return ""
	}
	if strings.HasPrefix(v, "//") { // protocol-relative -> cross-origin
		return ""
	}
	if strings.ContainsAny(v, "\\\n\r") { // backslash / control chars
		return ""
	}
	return v
}

// defaultSubsidiary resolves the editor's default header subsidiary: the user's
// default_subsidiary_id if set, else the sole/root subsidiary (Appendix C).
func (s *server) defaultSubsidiary(ctx context.Context, u *store.CurrentUser) int64 {
	if u != nil && u.DefaultSubsidiaryID != nil {
		return *u.DefaultSubsidiaryID
	}
	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil || len(subs) == 0 {
		return 1 // the seeded root is id 1
	}
	// The root is the parent-less subsidiary; SubTree returns it first.
	return subs[0].ID
}

// currencyExponent returns the minor-unit exponent for a currency (for amount
// parse/format), defaulting to 2 on any lookup miss.
func (s *server) currencyExponent(ctx context.Context, code string) int {
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		return 2
	}
	if e, ok := exps[code]; ok {
		return e
	}
	return 2
}

// --- template funcs -------------------------------------------------------

// txnRowCtx pairs a split row with the page model so the shared "txn-row" template
// (used by the full page and the 422 re-render) can reach the option lists and the
// display mode without the row model carrying them. It is the `txnRowCtx` func.
type txnRowCtx struct {
	Page txnEditorModel
	Row  txnRowModel
}

// makeTxnRowCtx is the `txnRowCtx` template func: {{txnRowCtx $.Page .}}.
func makeTxnRowCtx(m txnEditorModel, row txnRowModel) txnRowCtx {
	return txnRowCtx{Page: m, Row: row}
}

// txnTitleKey is the `txnTitleKey` template func: the head/heading i18n key for the
// new vs edit editor.
func txnTitleKey(isEdit bool) string {
	if isEdit {
		return "txn.edit_title"
	}
	return "txn.new_title"
}

// --- small helpers --------------------------------------------------------

// idsCSV renders a slice of ids as a comma-separated string (the data-subsidiaries
// attribute the client re-filter reads).
func idsCSV(ids []int64) string {
	out := make([]byte, 0, len(ids)*3)
	for i, id := range ids {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendInt(out, id, 10)
	}
	return string(out)
}

// userDisplayMode / numberFormatFor / dateFormatCode read a (possibly nil) user's
// raw setting strings, defaulting sensibly.
func userDisplayMode(u *store.CurrentUser) string {
	if u == nil {
		return ""
	}
	return u.DisplayMode
}

func numberFormatFor(u *store.CurrentUser) money.NumberFormat {
	if u == nil {
		return money.NumberUS
	}
	return numberFormatOf(u.NumberFormat)
}

// dateFormatCode returns the user's date-format code string for the client's date
// shortcut logic (the JS honors ISO-always-accepted regardless).
func dateFormatCode(u *store.CurrentUser) string {
	if u == nil {
		return "ISO"
	}
	switch u.DateFormat {
	case "US":
		return "US"
	case "EU":
		return "EU"
	default:
		return "ISO"
	}
}
