package web

import (
	"context"
	"errors"
	"net/http"

	"cuento/internal/store"
)

// p11.3 subsidiaries admin (/admin/subsidiaries, Perm Admin -- Appendix B/F). A
// tree list of subsidiaries (store.SubTree, depth-indented) showing name, base
// currency, and active state, with an inline htmx create/edit form (name, parent,
// base currency) and a deactivate action. It follows the p11.1 accounts pattern
// (accounts.go): the GET renders the page + hidden inline form; GET .../new and
// .../{id}/edit swap the "subsidiary-form" partial in; a bad POST maps the store's
// TYPED error to an i18n KEY and re-renders at 422 (the p10.3 convention). The
// handler NEVER re-validates -- the store owns validation (D18); this only
// TRANSLATES its typed errors to (field, key) pairs. Every string via {{t}}
// (rule 9); no inline script (rule 12); subsidiaries are proper nouns (stored
// data, not catalog entries, rule 9).
//
// Unlike the accounts page, DEACTIVATE here can be BLOCKED (ErrHasActiveChildren):
// the store's no-trace discipline leaves the subsidiary active, and the handler
// re-renders the LIST at 422 with the localized blocked message rather than 500ing
// (task requirement: show the guard, no execution).

// ---- page model ----------------------------------------------------------

// subRow is one rendered tree row: the subsidiary plus its indent depth, base
// currency, and active state.
type subRow struct {
	ID           int64
	Name         string
	BaseCurrency string
	Active       bool
	Depth        int
}

// subsidiariesPageModel is the GET /admin/subsidiaries model: the tree rows and,
// when a deactivate guard fired, a page-level localized error key to surface.
type subsidiariesPageModel struct {
	Rows []subRow
	// ErrorKey is a page-level i18n error key set only when a deactivate was
	// blocked (ErrHasActiveChildren); "" otherwise. The list re-renders at 422 with
	// this message so the guard is visible with no execution (D18).
	ErrorKey string
}

// subsidiariesPage handles GET /admin/subsidiaries (Admin): the depth-indented
// tree list plus the hidden inline form region.
func (s *server) subsidiariesPage(w http.ResponseWriter, r *http.Request) {
	model, err := s.buildSubsidiariesPage(r.Context())
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "subsidiaries.tmpl", s.newShellPageControls(r, model, "subsidiaries"))
}

// buildSubsidiariesPage assembles the tree rows (depth-indented, pre-order) for
// the list. Depth comes from the parent chain, which is always present earlier in
// the pre-ordered SubTree rows.
func (s *server) buildSubsidiariesPage(ctx context.Context) (subsidiariesPageModel, error) {
	rows, err := s.store.SubTree(ctx)
	if err != nil {
		return subsidiariesPageModel{}, err
	}
	depth := make(map[int64]int, len(rows))
	var model subsidiariesPageModel
	for _, row := range rows {
		d := 0
		if row.ParentID.Valid {
			d = depth[row.ParentID.Int64] + 1
		}
		depth[row.ID] = d
		model.Rows = append(model.Rows, subRow{
			ID:           row.ID,
			Name:         row.Name,
			BaseCurrency: row.BaseCurrency,
			Active:       row.Active != 0,
			Depth:        d,
		})
	}
	return model, nil
}

// ---- form model ----------------------------------------------------------

// subsidiaryForm is the create/edit form model (the demoFormModel shape: value
// fields + an embedded formErrors). It carries the parent + currency option lists
// the selects render and the edit target id (0 = create). IsRoot marks the root
// (edit only) so the template omits the parent select -- the root is immovable
// (D18); the store still enforces it regardless.
type subsidiaryForm struct {
	ID           int64 // 0 = create
	Name         string
	ParentID     int64
	BaseCurrency string
	IsRoot       bool

	Parents    []subOption
	Currencies []currencyOption

	Errors formErrors
}

// subsidiaryNewForm handles GET /admin/subsidiaries/new (Admin): the empty create
// form, rendered as the "subsidiary-form" partial for htmx to swap in. A new
// subsidiary defaults to a child of the root (id 1).
func (s *server) subsidiaryNewForm(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildSubsidiaryForm(r.Context(), 0)
	if err != nil {
		s.serverError(w)
		return
	}
	form.ParentID = 1 // default: child of the root subsidiary
	s.render(w, r, http.StatusOK, "subsidiary-form", form)
}

// subsidiaryEditForm handles GET /admin/subsidiaries/{id}/edit (Admin): the form
// prefilled from the subsidiary's current state, for an inline htmx swap.
func (s *server) subsidiaryEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	sub, err := s.store.GetSubsidiary(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	form, err := s.buildSubsidiaryForm(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	form.Name = sub.Name
	form.BaseCurrency = sub.BaseCurrency
	form.IsRoot = !sub.ParentID.Valid
	if sub.ParentID.Valid {
		form.ParentID = sub.ParentID.Int64
	}
	s.render(w, r, http.StatusOK, "subsidiary-form", form)
}

// buildSubsidiaryForm assembles the option lists for a form. The parent options
// are every subsidiary EXCEPT the subject and its descendants (so the select never
// OFFERS a cycle) -- presentation only; the store still enforces cycles and root
// immovability. The currency options are the active currencies.
func (s *server) buildSubsidiaryForm(ctx context.Context, id int64) (subsidiaryForm, error) {
	form := subsidiaryForm{ID: id}

	// Exclude the subject + its descendants from the parent options on edit (self +
	// transitive closure; Descendants includes self as its base case).
	excluded := map[int64]bool{}
	if id != 0 {
		desc, err := s.store.Descendants(ctx, id)
		if err != nil {
			return form, err
		}
		for _, d := range desc {
			excluded[d.ID] = true
		}
	}

	subs, err := s.store.SubTree(ctx)
	if err != nil {
		return form, err
	}
	for _, sub := range subs {
		if excluded[sub.ID] {
			continue
		}
		form.Parents = append(form.Parents, subOption{ID: sub.ID, Name: sub.Name})
	}

	curs, err := s.store.Currencies(ctx)
	if err != nil {
		return form, err
	}
	for _, c := range curs {
		if c.Active != 0 {
			form.Currencies = append(form.Currencies, currencyOption{Code: c.Code, Name: c.Name})
		}
	}
	return form, nil
}

// ---- create / update / deactivate ---------------------------------------

// subsidiaryCreate handles POST /admin/subsidiaries (Admin). It parses the form
// and calls store.CreateSubsidiary; a no-parent submit yields ErrSecondRoot from
// the store (the handler does not re-validate). On a typed error it maps to a
// field-error key and re-renders the form at 422; success redirects (PRG).
func (s *server) subsidiaryCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in, err := s.parseSubsidiaryForm(r, 0)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	create := store.CreateSubsidiaryInput{
		ParentID:     in.parentID,
		Name:         in.name,
		BaseCurrency: in.baseCurrency,
	}
	if _, err := s.store.CreateSubsidiary(s.actorCtx(ctx), create); err != nil {
		s.renderSubsidiaryFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/admin/subsidiaries")
}

// subsidiaryUpdate handles POST /admin/subsidiaries/{id} (Admin): rename / move /
// base-currency change via UpdateSubsidiary. A non-positive parent id sends no
// ParentID (no move), so an accidental "0" never reparents; giving the ROOT a
// parent yields ErrRootImmovable and a cycle yields ErrCycle -- both mapped to
// keys and re-rendered at 422.
func (s *server) subsidiaryUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in, err := s.parseSubsidiaryForm(r, id)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	upd := store.UpdateSubsidiaryInput{
		Name:         &in.name,
		BaseCurrency: &in.baseCurrency,
	}
	// A positive parent selection is a MOVE target; the store validates it against
	// root-immovability and cycles. Selecting nothing (0) leaves the parent as-is.
	// Reparenting the ROOT is still attempted (the form for the root omits the
	// select, but a raw submit reaches the store, which returns ErrRootImmovable).
	if in.parentID > 0 {
		upd.ParentID = &in.parentID
	}
	if err := s.store.UpdateSubsidiary(s.actorCtx(ctx), id, upd); err != nil {
		s.renderSubsidiaryFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/admin/subsidiaries")
}

// subsidiaryDeactivate handles POST /admin/subsidiaries/{id}/deactivate (Admin).
// DeactivateSubsidiary is BLOCKED while the subsidiary has active children
// (ErrHasActiveChildren); the store's no-trace discipline leaves it active. On the
// guard the handler re-renders the LIST at 422 with the localized blocked message
// (the guard is shown; nothing executed). Success redirects to the list.
func (s *server) subsidiaryDeactivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.DeactivateSubsidiary(s.actorCtx(ctx), id); err != nil {
		if key := subsidiaryPageErrorKey(err); key != "" {
			model, berr := s.buildSubsidiariesPage(ctx)
			if berr != nil {
				s.serverError(w)
				return
			}
			model.ErrorKey = key
			s.render(w, r, http.StatusUnprocessableEntity, "subsidiaries.tmpl", s.newShellPageControls(r, model, "subsidiaries"))
			return
		}
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/admin/subsidiaries", http.StatusSeeOther)
}

// parsedSubsidiaryForm is the validated-shape (raw strings turned into typed
// fields) of a submitted subsidiary form; the store does the real validation.
type parsedSubsidiaryForm struct {
	name         string
	parentID     int64
	baseCurrency string
}

// parseSubsidiaryForm reads the POST form into a subsidiaryForm (for a 422
// re-render) and a parsedSubsidiaryForm (for the store call). id is the edit
// target (0 = create); it is threaded into buildSubsidiaryForm so a 422 re-render
// of an EDIT excludes the subject + descendants from the parent select -- exactly
// as the initial edit GET does. It does NOT validate business rules (the store
// owns that).
func (s *server) parseSubsidiaryForm(r *http.Request, id int64) (subsidiaryForm, parsedSubsidiaryForm, error) {
	if err := r.ParseForm(); err != nil {
		return subsidiaryForm{}, parsedSubsidiaryForm{}, err
	}
	in := parsedSubsidiaryForm{
		name:         r.PostFormValue("name"),
		parentID:     parseID(r.PostFormValue("parent_id")),
		baseCurrency: r.PostFormValue("base_currency"),
	}
	form, err := s.buildSubsidiaryForm(r.Context(), id)
	if err != nil {
		return subsidiaryForm{}, parsedSubsidiaryForm{}, err
	}
	// Echo submitted values back so a 422 re-render keeps what the user entered.
	form.Name = in.name
	form.ParentID = in.parentID
	form.BaseCurrency = in.baseCurrency
	// On edit, mark the root so the template still omits the parent select.
	if id != 0 {
		if sub, gerr := s.store.GetSubsidiary(r.Context(), id); gerr == nil {
			form.IsRoot = !sub.ParentID.Valid
		}
	}
	return form, in, nil
}

// renderSubsidiaryFormError maps a store TYPED error to an i18n field-error key
// and re-renders the "subsidiary-form" partial at 422 (the p10.3 convention). It
// never re-validates -- the store is the source of truth; this only TRANSLATES its
// typed errors to (field, key) pairs.
func (s *server) renderSubsidiaryFormError(w http.ResponseWriter, r *http.Request, form subsidiaryForm, err error) {
	field, key := subsidiaryErrorField(err)
	if key == "" {
		s.serverError(w) // an unknown error is a real server fault, not a validation failure
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "subsidiary-form", form)
}

// subsidiaryErrorField maps a store typed error to the (form field, i18n key) pair
// the form-error convention needs. The field name drives autofocus placement. Note
// store.ErrCycle is SHARED with account moves; it maps to a subsidiary-specific
// key here (same physical error, different page). An unrecognized error returns
// ("",""), which the caller treats as a 500.
func subsidiaryErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrSecondRoot):
		return "parent_id", "error.subsidiary.second_root"
	case errors.Is(err, store.ErrParentMissing):
		return "parent_id", "error.subsidiary.parent_missing"
	case errors.Is(err, store.ErrRootImmovable):
		return "parent_id", "error.subsidiary.root_immovable"
	case errors.Is(err, store.ErrCycle):
		return "parent_id", "error.subsidiary.cycle"
	default:
		return "", ""
	}
}

// subsidiaryPageErrorKey maps a DEACTIVATE guard error to a page-level i18n key
// (the deactivate action has no form to target, so its error surfaces on the list).
// Only ErrHasActiveChildren is a user-facing guard here; anything else is a server
// fault ("").
func subsidiaryPageErrorKey(err error) string {
	if errors.Is(err, store.ErrHasActiveChildren) {
		return "error.subsidiary.has_active_children"
	}
	return ""
}
