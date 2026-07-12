package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cuento/internal/store"
)

// p13.2 admin: currencies management (/admin/currencies, Perm Admin). A list of
// currencies (code, name, symbol, exponent, active) with an inline add form and a
// per-row enable/disable toggle. Currencies are STATIC reference data (D1), NOT a
// versioned business table -- so these writes are plain reference-data upserts in
// the store, OUTSIDE the write funnel (no actor, no changes/version row), exactly
// like org settings. They are used by FX (p14) later. Every string via {{t}} (rule
// 9); no inline script (rule 12); currency names/symbols are stored data.

// currencyRow is one rendered list row.
type currencyRow struct {
	Code     string
	Name     string
	Symbol   string
	Exponent int64
	Active   bool
}

// currenciesPageModel is the GET /admin/currencies model: the rows plus the add
// form's echoed values + field errors (an inline add form on the list).
type currenciesPageModel struct {
	Rows []currencyRow
	Form currencyAddForm
}

// currencyAddForm is the inline add-form model (echoed values + errors).
type currencyAddForm struct {
	Code     string
	Name     string
	Symbol   string
	Exponent string
	Errors   formErrors
}

// currenciesPage handles GET /admin/currencies (Admin): the list + inline add form.
func (s *server) currenciesPage(w http.ResponseWriter, r *http.Request) {
	model, err := s.buildCurrenciesPage(r, currencyAddForm{Exponent: "2"})
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "admin_currencies.tmpl", s.newShellPage(r, model))
}

// buildCurrenciesPage reads the currency rows into the model, carrying the given
// add-form state (for a 422 re-render).
func (s *server) buildCurrenciesPage(r *http.Request, form currencyAddForm) (currenciesPageModel, error) {
	curs, err := s.store.Currencies(r.Context())
	if err != nil {
		return currenciesPageModel{}, err
	}
	model := currenciesPageModel{Form: form}
	for _, c := range curs {
		model.Rows = append(model.Rows, currencyRow{
			Code: c.Code, Name: c.Name, Symbol: c.Symbol,
			Exponent: c.Exponent, Active: c.Active != 0,
		})
	}
	return model, nil
}

// currencyAdd handles POST /admin/currencies (Admin): add (or re-enable) a
// currency. The code is uppercased; the store validates shape (3 letters, exponent
// 0..4, non-empty symbol/name) and does an idempotent upsert. A bad shape maps to a
// field error + 422 re-render; success redirects to the list.
func (s *server) currencyAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	form := currencyAddForm{
		Code:     strings.ToUpper(strings.TrimSpace(r.PostFormValue("code"))),
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		Symbol:   strings.TrimSpace(r.PostFormValue("symbol")),
		Exponent: strings.TrimSpace(r.PostFormValue("exponent")),
	}

	exp, perr := strconv.ParseInt(form.Exponent, 10, 64)
	if perr != nil {
		form.Errors.add("exponent", "admin.currencies.error.bad_exponent")
		s.renderCurrencyFormError(w, r, form)
		return
	}

	if err := s.store.AddCurrency(r.Context(), store.AddCurrencyInput{
		Code: form.Code, Exponent: exp, Symbol: form.Symbol, Name: form.Name,
	}); err != nil {
		if errors.Is(err, store.ErrInvalidCurrency) {
			form.Errors.add("code", "admin.currencies.error.invalid")
			s.renderCurrencyFormError(w, r, form)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/admin/currencies")
}

// currencyToggle handles POST /admin/currencies/{code}/toggle (Admin): flip a
// currency's active flag. The desired state comes from an "active" form value
// ("1" to enable, else disable) so the toggle is idempotent and unambiguous.
func (s *server) currencyToggle(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.PathValue("code"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	active := r.PostFormValue("active") == "1"
	if err := s.store.SetCurrencyActive(r.Context(), code, active); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/admin/currencies", http.StatusSeeOther)
}

// renderCurrencyFormError re-renders the currencies list at 422 with the add-form
// errors shown (a full-page re-render; the add form lives on the list, no separate
// partial region to swap).
func (s *server) renderCurrencyFormError(w http.ResponseWriter, r *http.Request, form currencyAddForm) {
	model, err := s.buildCurrenciesPage(r, form)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, "admin_currencies.tmpl", s.newShellPage(r, model))
}
