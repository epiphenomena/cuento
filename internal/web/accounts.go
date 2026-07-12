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

// p11.1 chart of accounts (/accounts) -- the first big feature page and the
// template every later CRUD page follows. GET renders a tree table of accounts
// (store.Tree, name-fallback) with per-currency balances (store.SubtreeBalancesAsOf,
// matching the p08.4 numbers), a subsidiary filter, and an active filter. The
// inline htmx create/edit form (TxnWrite) reuses the p10.3 form-error convention:
// on a store TYPED error the handler maps it to an i18n KEY and re-renders the
// form region at 422 with autofocus -- it never duplicates the store's validation
// (the store is the source of truth). All money renders through the money
// formatters honoring the user's settings (rule 10); every string via {{t}}.

// ---- balances assembly ---------------------------------------------------

// balanceCell is one account's balance in one currency, kept as raw minor units
// plus the currency exponent so the template renders it through the money
// formatter (rule 10). It mirrors store.AccountCurrencyAmount but carries the
// exponent (from the currencies table) the formatter needs.
type balanceCell struct {
	Currency string
	Minor    int64
	Exponent int
}

// balancesByAccount returns the per-account, per-currency balances as of `asof`
// for the subsidiary scope `scopeSub` (subsidiary + descendants, D18). It is the
// page's balance assembly, exposed as a plain function (asof, scope explicit) so
// it is testable directly against the p08.4 query without scraping HTML or
// depending on time.Now. The numbers come STRAIGHT from SubtreeBalancesAsOf; this
// only attaches each currency's exponent for rendering.
func balancesByAccount(ctx context.Context, st *store.Store, asof string, scopeSub int64) (map[int64][]balanceCell, error) {
	rows, err := st.SubtreeBalancesAsOf(ctx, asof, scopeSub)
	if err != nil {
		return nil, err
	}
	exps, err := currencyExponents(ctx, st)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]balanceCell)
	for _, r := range rows {
		out[r.AccountID] = append(out[r.AccountID], balanceCell{
			Currency: r.Currency,
			Minor:    r.Amount,
			Exponent: exps[r.Currency],
		})
	}
	return out, nil
}

// currencyExponents maps each currency code to its minor-unit exponent (D1), so
// the money formatter can split minor units correctly per currency.
func currencyExponents(ctx context.Context, st *store.Store) (map[string]int, error) {
	curs, err := st.Currencies(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]int, len(curs))
	for _, c := range curs {
		m[c.Code] = int(c.Exponent)
	}
	return m, nil
}

// ---- page model ----------------------------------------------------------

// acctRow is one rendered tree row: the account plus its formatted per-currency
// balances and an indent depth for the tree table.
type acctRow struct {
	ID       int64
	Name     string
	Type     string
	Active   bool
	Depth    int
	Balances []string // pre-formatted "CCY 1,234.56" strings (rule 10)
}

// subOption is a subsidiary offered in the filter and the checklist.
type subOption struct {
	ID   int64
	Name string
}

// accountsPageModel is the GET /accounts model: the tree rows, the filter state,
// and the option lists the (initially hidden) inline form needs.
type accountsPageModel struct {
	Rows       []acctRow
	Subs       []subOption
	SubFilter  int64 // 0 = all
	ActiveOnly bool
	AsOf       string // formatted as-of date (rule 10)
}

// accountsPage handles GET /accounts (TxnRead): the tree table with balances and
// the subsidiary + active filters. Balances are as of today; the scope is the
// selected subsidiary (or the root for "all", full consolidation, D18).
func (s *server) accountsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	subFilter := parseID(r.URL.Query().Get("sub"))
	activeOnly := r.URL.Query().Get("active") == "1"

	var subPtr *int64
	if subFilter > 0 {
		subPtr = &subFilter
	}
	rows, err := s.store.Tree(ctx, lang, subPtr)
	if err != nil {
		s.serverError(w)
		return
	}

	// Scope for balances: the selected subsidiary, else the root (full
	// consolidation, D18). SubtreeBalancesAsOf scopes to a sub + descendants.
	scope := subFilter
	if scope <= 0 {
		root, err := s.rootSubsidiary(ctx)
		if err != nil {
			s.serverError(w)
			return
		}
		scope = root
	}

	asofTime := s.now()
	asof := asofTime.Format("2006-01-02")
	bals, err := balancesByAccount(ctx, s.store, asof, scope)
	if err != nil {
		s.serverError(w)
		return
	}

	opts := formatOptsFor(u)
	// Depth from the tree's parent chain (rows are pre-order).
	depth := treeDepths(rows)

	model := accountsPageModel{
		SubFilter:  subFilter,
		ActiveOnly: activeOnly,
		AsOf:       money.FormatDate(asofTime, dateFormatFor(u)),
	}
	for _, row := range rows {
		if activeOnly && row.Active == 0 {
			continue
		}
		ar := acctRow{
			ID:     row.ID,
			Name:   row.Name,
			Type:   row.Type,
			Active: row.Active != 0,
			Depth:  depth[row.ID],
		}
		for _, c := range bals[row.ID] {
			ar.Balances = append(ar.Balances, c.Currency+" "+money.Format(c.Minor, c.Exponent, opts))
		}
		model.Rows = append(model.Rows, ar)
	}

	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	for _, sub := range subs {
		model.Subs = append(model.Subs, subOption{ID: sub.ID, Name: sub.Name})
	}

	s.render(w, r, http.StatusOK, "accounts.tmpl", s.newShellPage(r, model))
}

// treeDepths computes each account's indent depth from the pre-ordered tree rows
// (root accounts depth 0). It walks parent ids, which are all present earlier in
// pre-order.
func treeDepths(rows []store.TreeRow) map[int64]int {
	depth := make(map[int64]int, len(rows))
	for _, r := range rows {
		if r.ParentID.Valid {
			depth[r.ID] = depth[r.ParentID.Int64] + 1
		} else {
			depth[r.ID] = 0
		}
	}
	return depth
}

// rootSubsidiary returns the id of the single root subsidiary (NULL parent), the
// full-consolidation scope for an unfiltered balances view (D18).
func (s *server) rootSubsidiary(ctx context.Context) (int64, error) {
	subs, err := s.store.SubTree(ctx)
	if err != nil {
		return 0, err
	}
	for _, sub := range subs {
		if !sub.ParentID.Valid {
			return sub.ID, nil
		}
	}
	return 0, errors.New("web: no root subsidiary")
}

// ---- form model ----------------------------------------------------------

// accountForm is the create/edit form model (the demoFormModel shape: value
// fields + an embedded Errors). It carries the option lists (parents/currencies/
// 990 lines/subs/programs) the selects render, the account's current sub set (for
// checklist pre-check), and the edit target id (0 = create).
type accountForm struct {
	ID               int64 // 0 = create
	Type             string
	Currency         string
	ParentID         int64
	Names            []nameInput // one per enabled language (p11.4, D14); en first + required
	Reconcilable     bool
	Intercompany     bool
	FunctionalClass  string
	DefaultProgram   int64
	Form990Code      string
	Form990Effective string // inherited effective code shown as placeholder when unset (D25)
	CheckedSubs      map[int64]bool

	Parents    []store.ParentOption
	Currencies []currencyOption
	Lines      []store.Form990Option
	Subs       []subOption
	Programs   []programOption
	IsExpense  bool // type == expense (functional class shown)
	IsRE       bool // revenue/expense (default program shown)

	Errors formErrors
}

// nameInput is one per-language account-name field in the form (p11.4). The set is
// driven by the org's enabled_languages (D14): adding a language makes a new name
// column appear. Lang is the code ("en"); Field/ID are the stable form-field name
// and element id ("name_en"/"af-name-en") the template stamps and the handler
// reads; Required marks the base language (en) whose name is required (p05.3);
// LabelArg is the uppercased code shown in the interpolated "Name (%s)" label so
// any language gets a label without a per-language chrome catalog key.
type nameInput struct {
	Lang     string
	Field    string // form field name, e.g. "name_en"
	ID       string // element id, e.g. "af-name-en"
	Value    string
	Required bool
	LabelArg string // e.g. "EN"
}

// nameFieldFor returns the form-field name for a language's account-name input.
// Stable and used on both render (template via the struct) and parse (handler),
// so the en/es fields keep their historical names ("name_en"/"name_es") that
// existing tests and e2e specs select.
func nameFieldFor(lang string) string { return "name_" + lang }

// nameInputsFor builds the per-language name-input descriptors from the enabled
// languages, carrying the current value for each from names (lang->value). en is
// always first and marked required (EnabledLanguages guarantees en first).
func nameInputsFor(langs []string, names map[string]string) []nameInput {
	out := make([]nameInput, 0, len(langs))
	for _, lang := range langs {
		out = append(out, nameInput{
			Lang:     lang,
			Field:    nameFieldFor(lang),
			ID:       "af-name-" + lang, // hyphenated, stable (e2e specs select #af-name-en)
			Value:    names[lang],
			Required: lang == "en",
			LabelArg: strings.ToUpper(lang),
		})
	}
	return out
}

type currencyOption struct {
	Code string
	Name string
}

type programOption struct {
	ID   int64
	Name string
}

// accountNewForm handles GET /accounts/new (TxnWrite): the empty create form,
// rendered as the "account-form" partial for htmx to swap in. The form's option
// lists depend on the chosen type; new forms default to "asset" and the client
// re-fetches on type change (progressive: a full submit still validates server-
// side). Defaults to root subsidiary checked.
func (s *server) accountNewForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	typ := r.URL.Query().Get("type")
	if typ == "" {
		typ = "asset"
	}
	form, err := s.buildAccountForm(ctx, 0, typ)
	if err != nil {
		s.serverError(w)
		return
	}
	// A type-change re-fetch carries the current field values (htmx hx-include
	// sends the form as query params on the GET); overlay them so switching type
	// does not wipe what the user typed.
	overlayFormValues(&form, r)
	s.render(w, r, http.StatusOK, "account-form", form)
}

// overlayFormValues copies any submitted (query or form) field values onto a form
// so a type-change re-fetch preserves the user's entries. Only non-empty values
// override; the subsidiary checklist is rebuilt from the sub_<id> params when any
// are present (so an in-progress selection survives a type switch).
func overlayFormValues(form *accountForm, r *http.Request) {
	q := r.URL.Query()
	get := func(k string) string {
		if v := r.FormValue(k); v != "" {
			return v
		}
		return q.Get(k)
	}
	// Overlay each enabled-language name field so a type-change re-fetch preserves
	// what the user typed (p11.4: the set of fields is the enabled languages).
	for i := range form.Names {
		if v := get(form.Names[i].Field); v != "" {
			form.Names[i].Value = v
		}
	}
	if v := get("currency"); v != "" {
		form.Currency = v
	}
	if v := parseID(get("parent_id")); v > 0 {
		form.ParentID = v
	}
	// Checkboxes only appear in the params when checked; if ANY sub_* param is
	// present, take the submitted set as authoritative (preserving an in-progress
	// selection). Otherwise keep the default/prefilled set.
	submitted := map[int64]bool{}
	any := false
	for k, vals := range q {
		if len(k) > 4 && k[:4] == "sub_" && len(vals) > 0 && vals[0] != "" {
			if sid := parseID(k[4:]); sid > 0 {
				submitted[sid] = true
				any = true
			}
		}
	}
	if any {
		form.CheckedSubs = submitted
	}
}

// accountEditForm handles GET /accounts/{id}/edit (TxnWrite): the form prefilled
// from the account's current state, for an inline htmx swap.
func (s *server) accountEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	acct, err := s.store.GetAccount(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	form, err := s.buildAccountForm(ctx, id, acct.Type)
	if err != nil {
		s.serverError(w)
		return
	}
	// Prefill from the live account + its names + sub set.
	form.Currency = acct.DefaultCurrency
	if acct.ParentID.Valid {
		form.ParentID = acct.ParentID.Int64
	}
	form.Reconcilable = acct.Reconcilable != 0
	form.Intercompany = acct.Intercompany != 0
	if acct.FunctionalClass.Valid {
		form.FunctionalClass = acct.FunctionalClass.String
	}
	if acct.DefaultProgramID.Valid {
		form.DefaultProgram = acct.DefaultProgramID.Int64
	}
	if acct.Form990Code.Valid {
		form.Form990Code = acct.Form990Code.String
	}
	// Prefill each enabled-language name from account_names (exact-lang, no
	// fallback -- an empty box means that language has no name yet, p11.4).
	for i := range form.Names {
		form.Names[i].Value = s.accountNameExact(ctx, id, form.Names[i].Lang)
	}
	if ids, err := s.store.AccountSubsidiaryIDs(ctx, id); err == nil {
		for _, sid := range ids {
			form.CheckedSubs[sid] = true
		}
	}
	// A type-change re-fetch on the edit form overlays in-progress entries over the
	// live values (same mechanism as the create form).
	overlayFormValues(&form, r)
	s.render(w, r, http.StatusOK, "account-form", form)
}

// accountName reads an account's name in a given language WITH the p05.3 fallback
// (en -> any) via Tree -- used where a display name is wanted (merge preview).
func (s *server) accountName(ctx context.Context, id int64, lang string) string {
	rows, err := s.store.Tree(ctx, lang, nil)
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}

// accountNameExact reads an account's name in EXACTLY the given language (no
// fallback), for prefilling the per-language edit inputs (p11.4). "" on any error
// or when that language has no name yet.
func (s *server) accountNameExact(ctx context.Context, id int64, lang string) string {
	name, err := s.store.AccountName(ctx, id, lang)
	if err != nil {
		return ""
	}
	return name
}

// buildAccountForm assembles the option lists for a form of a given account type.
// The 990 lines are filtered to the type (D25), the parent options exclude the
// account's own descendants + wrong-class targets (D11), and the effective 990
// code (inherited) is resolved for the placeholder (D25).
func (s *server) buildAccountForm(ctx context.Context, id int64, typ string) (accountForm, error) {
	form := accountForm{
		ID:          id,
		Type:        typ,
		CheckedSubs: map[int64]bool{},
		IsExpense:   typ == "expense",
		IsRE:        typ == "revenue" || typ == "expense",
	}
	if id == 0 {
		form.CheckedSubs[1] = true // default: root subsidiary checked on a new account
	}

	// Per-language name inputs, driven by the org's enabled languages (p11.4, D14):
	// adding a language here makes a new name column appear. en is always first and
	// required. Values are filled by the caller (prefill on edit, echo on 422).
	langs, err := s.store.EnabledLanguages(ctx)
	if err != nil {
		return form, err
	}
	form.Names = nameInputsFor(langs, nil)

	parents, err := s.store.ParentOptions(ctx, langOf(ctx), typ, id)
	if err != nil {
		return form, err
	}
	form.Parents = parents

	curs, err := s.store.Currencies(ctx)
	if err != nil {
		return form, err
	}
	for _, c := range curs {
		if c.Active != 0 {
			form.Currencies = append(form.Currencies, currencyOption{Code: c.Code, Name: c.Name})
		}
	}

	lines, err := s.store.Form990LinesForType(ctx, typ)
	if err != nil {
		return form, err
	}
	form.Lines = lines

	subs, err := s.store.AllSubsidiaries(ctx)
	if err != nil {
		return form, err
	}
	for _, sub := range subs {
		form.Subs = append(form.Subs, subOption{ID: sub.ID, Name: sub.Name})
	}

	if form.IsRE {
		progs, err := s.store.ProgramTree(ctx)
		if err != nil {
			return form, err
		}
		for _, p := range progs {
			form.Programs = append(form.Programs, programOption{ID: p.ID, Name: p.Name})
		}
	}

	// Inherited effective 990 code (D25): shown as a placeholder when the account
	// has no own code, so the user sees what it would resolve to.
	if id != 0 {
		if eff, err := s.store.Effective990Codes(ctx); err == nil {
			form.Form990Effective = eff[id]
		}
	}
	return form, nil
}

// ---- create / update / deactivate ---------------------------------------

// accountCreate handles POST /accounts (TxnWrite). It parses the form, calls
// store.CreateAccount, and on a store TYPED error maps it to an i18n field-error
// key and re-renders the form region at 422 (the p10.3 convention). Success
// redirects to /accounts (PRG).
func (s *server) accountCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in, err := s.parseAccountForm(r, 0)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// One name per enabled language that the user filled in (p11.4). en is required;
	// an empty en yields ErrNameRequired from the store, mapped to the en input.
	names := map[string]string{}
	for lang, name := range in.names {
		if name != "" {
			names[lang] = name
		}
	}
	create := store.CreateAccountInput{
		Type:            in.typ,
		DefaultCurrency: in.currency,
		Names:           names,
		Subsidiaries:    in.subs,
		Reconcilable:    in.reconcilable,
		Intercompany:    in.intercompany,
	}
	if in.parentID > 0 {
		create.ParentID = &in.parentID
	}
	// Only send functional class on expense, default program on R/E -- else the
	// store's early ErrFunctionalClassNotExpense / ErrDefaultProgramNotRE fire.
	if in.typ == "expense" && in.functionalClass != "" {
		create.FunctionalClass = &in.functionalClass
	}
	if (in.typ == "revenue" || in.typ == "expense") && in.defaultProgram > 0 {
		create.DefaultProgramID = &in.defaultProgram
	}
	if in.form990Code != "" {
		create.Form990Code = &in.form990Code
	}

	actorCtx := s.actorCtx(ctx)
	if _, err := s.store.CreateAccount(actorCtx, create); err != nil {
		s.renderAccountFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/accounts")
}

// accountUpdate handles POST /accounts/{id} (TxnWrite): move/flags via
// UpdateAccount, names via SetAccountName, and the subsidiary set via
// SetAccountSubsidiaries (which propagates to ancestors, D18). The calls are not
// atomic across each other (each is its own change); the most-likely-to-fail
// (UpdateAccount: move/990/program) runs first so its typed error maps cleanly.
func (s *server) accountUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in, err := s.parseAccountForm(r, id)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	actorCtx := s.actorCtx(ctx)

	upd := store.UpdateAccountInput{
		DefaultCurrency: &in.currency,
		Reconcilable:    &in.reconcilable,
		Intercompany:    &in.intercompany,
	}
	// Parent: UpdateAccount treats a non-nil ParentID as a MOVE target and has no
	// way to express "move to NULL/top-level" (a non-nil 0 would resolve to a
	// non-existent account id 0). So a positive selection moves; selecting
	// "top level" is a no-op reparent (an account already at top level stays;
	// re-homing a child to top level is not offered by the store today, D-note).
	if in.parentID > 0 {
		upd.ParentID = &in.parentID
	}
	if in.typ == "expense" {
		if in.functionalClass != "" {
			upd.FunctionalClass = &in.functionalClass
		}
	}
	if in.typ == "revenue" || in.typ == "expense" {
		dp := in.defaultProgram // 0 clears (per UpdateAccountInput semantics)
		upd.DefaultProgramID = &dp
	}
	// 990 code: "" clears, a value sets (validated against type).
	code := in.form990Code
	upd.Form990Code = &code

	if err := s.store.UpdateAccount(actorCtx, id, upd); err != nil {
		s.renderAccountFormError(w, r, form, err)
		return
	}
	// Names: write each enabled language the user filled in (p11.4). Iterate the
	// form's ordered Names (deterministic, en first) rather than the map so behavior
	// is stable. An empty box is skipped (no name for that language) rather than
	// erasing an existing one; a now-disabled language is simply not in the set, so
	// its stored name is left alone (fallback still uses it).
	for _, ni := range form.Names {
		name := in.names[ni.Lang]
		if name == "" {
			continue
		}
		if err := s.store.SetAccountName(actorCtx, id, ni.Lang, name); err != nil {
			s.renderAccountFormError(w, r, form, err)
			return
		}
	}
	// Subsidiary set (propagates to ancestors, D18).
	if err := s.store.SetAccountSubsidiaries(actorCtx, id, in.subs); err != nil {
		s.renderAccountFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/accounts")
}

// redirectAfterForm sends the browser to `to` after a successful form POST. For an
// htmx request (HX-Request header) it sets HX-Redirect so htmx does a FULL-page
// navigation (reloading the tree) rather than swapping the redirect target into
// the small #account-form region; for a plain (JS-off) POST it uses a 303 (PRG).
// This keeps the inline form working with htmx while degrading gracefully.
func redirectAfterForm(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// accountDeactivate handles POST /accounts/{id}/deactivate (TxnWrite): a soft
// deactivate (active=0, history intact). Redirects back to the list.
func (s *server) accountDeactivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.DeactivateAccount(s.actorCtx(ctx), id); err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// parsedAccountForm is the validated-shape (raw strings turned into typed fields)
// of a submitted account form; the store does the real validation.
type parsedAccountForm struct {
	typ             string
	currency        string
	parentID        int64
	names           map[string]string // lang -> submitted name, for each enabled language (p11.4)
	reconcilable    bool
	intercompany    bool
	functionalClass string
	defaultProgram  int64
	form990Code     string
	subs            []int64
}

// parseAccountForm reads the POST form into an accountForm (for re-render) and a
// parsedAccountForm (for the store call). id is the edit target (0 = create); it
// is threaded into buildAccountForm so a 422 re-render of an EDIT excludes the
// account's own descendants from the parent select AND shows its inherited 990
// code -- exactly as the initial edit GET does. It rebuilds the option lists so a
// 422 re-render shows the same selects, and does NOT validate business rules --
// the store owns that (rule: don't duplicate validation).
func (s *server) parseAccountForm(r *http.Request, id int64) (accountForm, parsedAccountForm, error) {
	if err := r.ParseForm(); err != nil {
		return accountForm{}, parsedAccountForm{}, err
	}
	typ := r.PostFormValue("type")
	in := parsedAccountForm{
		typ:             typ,
		currency:        r.PostFormValue("currency"),
		parentID:        parseID(r.PostFormValue("parent_id")),
		names:           map[string]string{},
		reconcilable:    r.PostFormValue("reconcilable") != "",
		intercompany:    r.PostFormValue("intercompany") != "",
		functionalClass: r.PostFormValue("functional_class"),
		defaultProgram:  parseID(r.PostFormValue("default_program")),
		form990Code:     r.PostFormValue("form990_code"),
	}
	// Subsidiary checklist: fields named sub_<id> that are set.
	checked := map[int64]bool{}
	for key, vals := range r.PostForm {
		if len(key) > 4 && key[:4] == "sub_" && len(vals) > 0 && vals[0] != "" {
			if sid := parseID(key[4:]); sid > 0 {
				in.subs = append(in.subs, sid)
				checked[sid] = true
			}
		}
	}

	form, err := s.buildAccountForm(r.Context(), id, typ)
	if err != nil {
		return accountForm{}, parsedAccountForm{}, err
	}
	// Per-language names: read one submitted value per ENABLED language (the fields
	// buildAccountForm rendered), echo it into the form for a 422 re-render, and
	// carry it in the parsed input for the store call (p11.4). A field the browser
	// did not send reads as "".
	for i := range form.Names {
		v := r.PostFormValue(form.Names[i].Field)
		in.names[form.Names[i].Lang] = v
		form.Names[i].Value = v
	}
	// Echo submitted values back so a 422 re-render keeps what the user entered.
	form.Currency = in.currency
	form.ParentID = in.parentID
	form.Reconcilable = in.reconcilable
	form.Intercompany = in.intercompany
	form.FunctionalClass = in.functionalClass
	form.DefaultProgram = in.defaultProgram
	form.Form990Code = in.form990Code
	form.CheckedSubs = checked
	return form, in, nil
}

// renderAccountFormError maps a store TYPED error to an i18n field-error key and
// re-renders the "account-form" partial at 422 (the p10.3 convention). It never
// re-validates -- the store is the source of truth; this only TRANSLATES its
// typed errors to (field, key) pairs and lets renderFormError do the swap.
func (s *server) renderAccountFormError(w http.ResponseWriter, r *http.Request, form accountForm, err error) {
	field, key := accountErrorField(err)
	if key == "" {
		// Not a known validation error -> a real server fault.
		s.serverError(w)
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "account-form", form)
}

// accountErrorField maps a store typed error to the (form field, i18n key) pair
// the form-error convention needs. The field name drives autofocus placement
// (Appendix C). An unrecognized error returns ("",""), which the caller treats as
// a 500 (not a form validation failure).
func accountErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrNameRequired):
		return "name_en", "error.account.name_required"
	case errors.Is(err, store.ErrNoSubsidiary):
		return "subs", "error.account.no_subsidiary"
	case errors.Is(err, store.ErrCrossTypeClass):
		return "parent_id", "error.account.cross_type_class"
	case errors.Is(err, store.ErrCycle):
		return "parent_id", "error.account.cycle"
	case errors.Is(err, store.ErrSubMismatch):
		return "parent_id", "error.account.sub_mismatch"
	case errors.Is(err, store.ErrSubInUseByChild):
		return "subs", "error.account.sub_in_use_by_child"
	case errors.Is(err, store.Err990TypeMismatch):
		return "form990_code", "error.account.form990_type_mismatch"
	case errors.Is(err, store.ErrFunctionalClassNotExpense):
		return "functional_class", "error.account.functional_not_expense"
	case errors.Is(err, store.ErrDefaultProgramNotRE):
		return "default_program", "error.account.default_program_not_re"
	case errors.Is(err, store.ErrDefaultProgramMissing):
		return "default_program", "error.account.default_program_missing"
	case errors.Is(err, store.ErrDefaultProgramInactive):
		return "default_program", "error.account.default_program_inactive"
	case errors.Is(err, store.ErrAccountNotFound):
		return "parent_id", "error.account.parent_not_found"
	default:
		return "", ""
	}
}

// ---- small helpers -------------------------------------------------------

// actorCtx attaches the current user as the write actor (rule 2/5). Every store
// write in this file goes through it.
func (s *server) actorCtx(ctx context.Context) context.Context {
	if u := currentUser(ctx); u != nil {
		return store.WithActor(ctx, store.Actor{ID: u.ID})
	}
	return ctx
}

// serverError writes a 500 (the common server-fault path in these handlers).
func (s *server) serverError(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// parseID parses a positive int64 from a string, returning 0 on any problem (an
// absent/blank/invalid id -> 0, treated as "none").
func parseID(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// now returns the server's current time. A method so tests could override it
// later; today it is time.Now (the store owns clock injection for writes).
func (s *server) now() time.Time { return time.Now() }
