package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
// Scope boundary (this step only): the editor grid + account/payee combobox +
// create/edit round-trip. NOT payee suggest/autofill (p12.3), NOT void/duplicate/
// history (p12.4), NOT the keyboard-only QA pass (p12.6).

// txnRowModel is one split row in the editor. It carries the row's current values
// (echoed on a re-render) plus its stable key and any per-row error key (trap 5).
type txnRowModel struct {
	Index    int    // 0-based row position -> the stable id/name suffix (trap 4)
	SplitID  string // existing split id (hidden field, trap 1); "" for a new row
	Account  int64  // chosen account id (0 = none)
	AmountDR string // debit magnitude (DR/CR mode display)
	AmountCR string // credit magnitude (DR/CR mode display)
	Amount   string // the SIGNED value echoed into the hidden signed field (trap 3)
	Fund     int64  // chosen fund id (0 = unrestricted)
	Program  int64  // chosen program id (R/E rows)
	Class    string // functional class (expense rows)
	Memo     string
	ErrorKey string // i18n key of this row's error (trap 5); "" = ok
}

// txnAccountOption is one account offered in a row's account combobox. It carries the
// metadata the client's row logic needs: Type (program/class gating), the account
// defaults (prefill), and the subsidiary set (the client re-filter on a header
// subsidiary change, Appendix C).
type txnAccountOption struct {
	ID             int64
	Name           string
	Type           string
	DefaultProgram int64  // 0 = none
	DefaultClass   string // "" = none
	SubsCSV        string // comma-separated subsidiary ids (data-subsidiaries)
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
	Payee      int64
	PayeeName  string // the typed/picked payee name (create-on-save + 422 echo)
	Memo       string
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
	Payees       []txnOption
	Classes      []string // program|management|fundraising

	RootProgram int64 // the program-defaulting fallback (D24)

	// AutofillNotice is the i18n key shown when a payee template (p12.3) had splits
	// dropped because their account is outside the selected subsidiary; "" = none.
	AutofillNotice string

	// Errors (trap 5). TotalsError is the overall/fund-imbalance key rendered in the
	// sticky totals bar; row errors live on each row's ErrorKey.
	TotalsError string
	// FirstErrorRow is the index of the first row carrying an error (autofocus target
	// after a 422 swap); -1 = none.
	FirstErrorRow int
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
		model.Payee = parseID(r.URL.Query().Get("payee"))
	} else {
		// First load: today's date (Appendix C `t` = today) and two empty rows.
		model.Date = money.FormatDate(time.Now(), dateFormatFor(u))
		model.Rows = []txnRowModel{{Index: 0}, {Index: 1}}
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
		n = 2
	}
	rows := make([]txnRowModel, 0, n)
	for i := 0; i < n; i++ {
		si := strconv.Itoa(i)
		q := r.URL.Query()
		acct := parseID(q.Get("account_" + si))
		row := txnRowModel{
			Index:    i,
			SplitID:  q.Get("split_id_" + si),
			Account:  acct,
			Amount:   q.Get("amount_" + si),
			AmountDR: q.Get("dr_" + si),
			AmountCR: q.Get("cr_" + si),
			Fund:     parseID(q.Get("fund_" + si)),
			Program:  parseID(q.Get("program_" + si)),
			Class:    q.Get("class_" + si),
			Memo:     q.Get("memo_" + si),
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
	if hdr.PayeeID.Valid {
		model.Payee = hdr.PayeeID.Int64
		// Echo the payee's name into the autocomplete input (the option list already
		// loaded it; no extra query).
		for _, p := range model.Payees {
			if p.ID == model.Payee {
				model.PayeeName = p.Name
				break
			}
		}
	}
	model.Memo = hdr.Memo

	exp := s.currencyExponent(ctx, hdr.Currency)
	// The prefilled amount MUST use the user's NUMBER format (rule 10) so that a
	// save-without-touching round-trips: parseSplitForms parses with the same format,
	// and a mismatch (e.g. EU number, dot=grouping) would corrupt the amount. Display
	// stays Signed (the hidden signed field is always net-debit); NegStyle is irrelevant
	// to the magnitudes/DR-CR columns.
	fmtOpts := money.FormatOpts{Number: numberFormatFor(u)}
	for i, sp := range splits {
		row := txnRowModel{
			Index:   i,
			SplitID: strconv.FormatInt(sp.ID, 10),
			Account: sp.AccountID,
			Amount:  money.Format(sp.Amount, exp, fmtOpts),
			Memo:    sp.Memo,
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
		// DR/CR display columns (client can also derive; prefilled for no-JS parity),
		// same number format as the signed field.
		if sp.Amount >= 0 {
			row.AmountDR = money.Format(sp.Amount, exp, fmtOpts)
		} else {
			row.AmountCR = money.Format(-sp.Amount, exp, fmtOpts)
		}
		model.Rows = append(model.Rows, row)
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
	model.Payee = parseID(r.FormValue("payee"))
	model.PayeeName = strings.TrimSpace(r.FormValue("payee_name"))
	model.Memo = r.FormValue("memo")
	model.FirstErrorRow = -1

	// Resolve the payee (p12.3 create-on-save): prefer a picked existing id; else, if a
	// name was typed, find-or-create it (its OWN change, before the txn write, so a
	// later txn-validation failure leaves the payee reusable on retry). A create failure
	// (e.g. a name collision race) surfaces as a server error, not a lost entry.
	payeeID := model.Payee
	if payeeID == 0 && model.PayeeName != "" {
		payeeID, err = s.store.EnsurePayee(s.actorCtx(ctx), model.PayeeName)
		if err != nil {
			s.serverError(w)
			return
		}
		model.Payee = payeeID
	}

	// Parse the date input honoring the user's format (ISO always accepted, D16). A
	// malformed date is a form error surfaced on the totals bar (a header field).
	dateISO, dateOK := parseEditorDate(r.FormValue("date"), dateFormatFor(u))
	model.Date = r.FormValue("date")

	rows, splits := s.parseSplitForms(r, s.currencyExponent(ctx, currency))
	model.Rows = rows

	if !dateOK {
		model.TotalsError = "error.txn.bad_date"
		s.renderFormError(w, r, "transaction-form", s.newShellPage(r, model))
		return
	}

	in := store.PostTransactionInput{
		Date:         dateISO,
		SubsidiaryID: subID,
		Memo:         model.Memo,
		Currency:     currency,
		Splits:       splits,
	}
	if payeeID != 0 {
		p := payeeID
		in.PayeeID = &p
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
		s.routeTxnError(&model, postErr, splits)
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
func (s *server) routeTxnError(model *txnEditorModel, err error, splits []store.SplitInput) {
	key := txnErrorKey(err)

	// Totals-bar errors: overall or per-fund imbalance, and the header date.
	if errors.Is(err, store.ErrUnbalanced) || errors.Is(err, store.ErrFundUnbalanced) {
		model.TotalsError = key
		return
	}

	// Row-scoped errors. Attribute to the first row that STRUCTURALLY matches the
	// error, from data already loaded -- no validation is re-derived.
	row := s.attributeRowError(model, err, splits)
	if row < 0 {
		// Unattributable typed error (should not happen for the mapped set): fall
		// back to the totals bar so it is never silently dropped.
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
		inSub[a.ID] = true
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
		memo := r.FormValue("memo_" + si)
		splitID := r.FormValue("split_id_" + si)

		row := txnRowModel{
			Index:    i,
			SplitID:  splitID,
			Account:  acct,
			Amount:   amountStr,
			Fund:     fund,
			Program:  prog,
			Class:    class,
			Memo:     memo,
			AmountDR: r.FormValue("dr_" + si),
			AmountCR: r.FormValue("cr_" + si),
		}
		rows = append(rows, row)

		// Skip fully-empty rows (no account and blank amount): a trailing scaffold row.
		if acct == 0 && amountStr == "" {
			continue
		}

		amount, perr := money.Parse(amountStr, exp, numberFormatFor(currentUser(r.Context())))
		if perr != nil {
			// Leave the amount as 0 so the store's balance check rejects it; the echo
			// keeps the raw text. (money.Parse being liberal, this is rare.)
			amount = 0
		}

		sp := store.SplitInput{
			AccountID: acct,
			Amount:    amount,
			Memo:      memo,
			Position:  pos,
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

// parseEditorDate parses the editor's date input honoring df (ISO always accepted,
// D16), returning the ISO string and whether it parsed. An empty input is invalid
// (a transaction needs a date).
func parseEditorDate(s string, df money.DateFormat) (string, bool) {
	if s == "" {
		return "", false
	}
	t, err := money.ParseDate(s, df)
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
	for _, a := range accts {
		opt := txnAccountOption{ID: a.ID, Name: a.Name, Type: a.Type, DefaultClass: a.DefaultClass, SubsCSV: idsCSV(a.SubsidiaryIDs)}
		if a.DefaultProgram != nil {
			opt.DefaultProgram = *a.DefaultProgram
		}
		model.Accounts = append(model.Accounts, opt)
	}

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

	// Payee combobox options (the suggestion endpoint is p12.3; here it is a combobox
	// over the existing payees list).
	payees, err := s.store.ListPayees(ctx)
	if err != nil {
		return model, err
	}
	for _, p := range payees {
		if p.Active == 0 {
			continue
		}
		model.Payees = append(model.Payees, txnOption{ID: p.ID, Name: p.Name})
	}

	return model, nil
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
