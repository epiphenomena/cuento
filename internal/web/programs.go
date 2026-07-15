package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cuento/internal/money"
	"cuento/internal/store"
)

// p11.5 programs management (/programs) -- programs are a DIMENSION (D24): a
// single-root tree ("General", the unallocated default). This page follows the
// p11.1 accounts / p11.3 subsidiaries CRUD-page template exactly, with two
// deltas: (1) perms are BOOKKEEPING, not org-structure -- GET is TxnRead, the
// mutations TxnWrite (program structure is managed like funds, D24), and (2) the
// list carries a period R/E ACTIVITY total per program (store.ProgramActivity,
// aggregated flat per program+currency -- the tree rollup is the report layer's
// job, p15). Money renders through the money formatters honoring the user's
// settings (rule 10); the handler NEVER re-validates -- the store owns validation
// (D24); it only TRANSLATES the store's TYPED errors to (field, key) pairs (the
// p10.3 form-error convention). Every string via {{t}} (rule 9); program names are
// stored data (proper nouns), rendered verbatim; no inline script (rule 12).

// ---- activity assembly ----------------------------------------------------

// programActivityTotals returns the per-program R/E activity for from..to in the
// subsidiary scope scopeSub (subsidiary + descendants, D18), aggregated FLAT per
// (program, currency) -- a program's OWN cells summed, NOT a tree rollup (the
// rollup is the report layer's job, p15; the store comment on ProgramActivity is
// explicit). It is exposed as a plain function (from, to, scope explicit) so it is
// testable directly against the p08.4 ProgramActivity query without scraping HTML
// or depending on time.Now. Numbers come STRAIGHT from ProgramActivity; this only
// sums per currency and attaches each currency's exponent for rendering.
func programActivityTotals(ctx context.Context, st *store.Store, from, to string, scopeSub int64) (map[int64][]balanceCell, error) {
	cells, err := st.ProgramActivity(ctx, from, to, scopeSub)
	if err != nil {
		return nil, err
	}
	exps, err := currencyExponents(ctx, st)
	if err != nil {
		return nil, err
	}
	// Sum per (program, currency); the ProgramActivity rows are per (program,
	// account, currency), so several rows can share a (program, currency) key.
	sums := make(map[int64]map[string]int64)
	for _, c := range cells {
		if sums[c.ProgramID] == nil {
			sums[c.ProgramID] = make(map[string]int64)
		}
		sums[c.ProgramID][c.Currency] += c.Amount
	}
	out := make(map[int64][]balanceCell, len(sums))
	for pid, byCcy := range sums {
		for ccy, minor := range byCcy {
			out[pid] = append(out[pid], balanceCell{
				Currency: ccy,
				Minor:    minor,
				Exponent: exps[ccy],
			})
		}
	}
	return out, nil
}

// ---- page model -----------------------------------------------------------

// progRow is one rendered tree row: the program plus its indent depth, active
// state, and formatted per-currency period activity totals.
type progRow struct {
	ID       int64
	Name     string
	Active   bool
	Depth    int
	Activity []string // pre-formatted "CCY 1,234.56" strings (rule 10)
}

// programsPageModel is the GET /programs model: the tree rows, the formatted
// activity period bounds, and -- when a deactivate guard fired -- a page-level
// localized error key to surface.
type programsPageModel struct {
	Rows []progRow
	From string // formatted period start (rule 10)
	To   string // formatted period end (rule 10)
	// ErrorKey is a page-level i18n error key set only when a deactivate was
	// blocked (ErrProgramHasActiveChildren); "" otherwise. The list re-renders at
	// 422 with this message so the guard is visible with no execution (D24).
	ErrorKey string
}

// programsPage handles GET /programs (TxnRead): the depth-indented tree list with
// per-program period R/E activity totals plus the hidden inline form region. The
// period defaults to the current year to date (off s.now()); a from/to query param
// overrides it. The scope is the root subsidiary (full consolidation, D18).
func (s *server) programsPage(w http.ResponseWriter, r *http.Request) {
	from, to := s.programPeriod(r)
	model, err := s.buildProgramsPage(r.Context(), from, to)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "programs.tmpl", s.newShellPageControls(r, model, "programs"))
}

// programPeriod resolves the activity period: a from/to query param (ISO
// YYYY-MM-DD) when both present, else the current year to date (Jan 1 .. today)
// off s.now(). ISO is always accepted on input regardless of the user's date
// setting (D16); the bounds are formatted for display in buildProgramsPage.
func (s *server) programPeriod(r *http.Request) (from, to string) {
	if q := r.URL.Query(); q.Get("from") != "" && q.Get("to") != "" {
		return q.Get("from"), q.Get("to")
	}
	now := s.now()
	return now.Format("2006") + "-01-01", now.Format("2006-01-02")
}

// buildProgramsPage assembles the tree rows (depth-indented, pre-order) with each
// program's formatted per-currency activity totals for from..to. Depth comes from
// the parent chain, always present earlier in the pre-ordered ProgramTree rows.
func (s *server) buildProgramsPage(ctx context.Context, from, to string) (programsPageModel, error) {
	u := currentUser(ctx)

	rows, err := s.store.ProgramTree(ctx)
	if err != nil {
		return programsPageModel{}, err
	}

	scope, err := s.rootSubsidiary(ctx)
	if err != nil {
		return programsPageModel{}, err
	}
	totals, err := programActivityTotals(ctx, s.store, from, to, scope)
	if err != nil {
		return programsPageModel{}, err
	}

	opts := formatOptsFor(u)
	depth := make(map[int64]int, len(rows))
	model := programsPageModel{
		From: money.FormatDate(parseISOForDisplay(from), dateFormatFor(u)),
		To:   money.FormatDate(parseISOForDisplay(to), dateFormatFor(u)),
	}
	for _, row := range rows {
		d := 0
		if row.ParentID.Valid {
			d = depth[row.ParentID.Int64] + 1
		}
		depth[row.ID] = d
		pr := progRow{
			ID:     row.ID,
			Name:   row.Name,
			Active: row.Active != 0,
			Depth:  d,
		}
		for _, c := range totals[row.ID] {
			pr.Activity = append(pr.Activity, money.FormatMoney(c.Minor, c.Currency, c.Exponent, opts))
		}
		model.Rows = append(model.Rows, pr)
	}
	return model, nil
}

// parseISOForDisplay parses a YYYY-MM-DD date for DISPLAY formatting. The bounds are
// produced internally (s.now()) or already ISO from the query; an unparseable
// value falls back to the zero time so a bad manual param never 500s the page (the
// activity query still ran with the raw string).
func parseISOForDisplay(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---- form model -----------------------------------------------------------

// programForm is the create/edit form model (the demoFormModel shape: value fields
// + an embedded formErrors). It carries the parent option list the select renders
// and the edit target id (0 = create). IsRoot marks the root (edit only) so the
// template omits the parent select -- the root is immovable (D24); the store still
// enforces it regardless.
type programForm struct {
	ID       int64 // 0 = create
	Name     string
	ParentID int64
	IsRoot   bool

	Parents []programOption

	Errors formErrors
}

// programNewForm handles GET /programs/new (TxnWrite): the empty create form,
// rendered as the "program-form" partial for htmx to swap in. A new program
// defaults to a child of the root (id 1).
func (s *server) programNewForm(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildProgramForm(r.Context(), 0)
	if err != nil {
		s.serverError(w)
		return
	}
	form.ParentID = 1 // default: child of the root program ("General")
	s.render(w, r, http.StatusOK, "program-form", form)
}

// programEditForm handles GET /programs/{id}/edit (TxnWrite): the form prefilled
// from the program's current state, for an inline htmx swap.
func (s *server) programEditForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	prog, err := s.store.GetProgram(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	form, err := s.buildProgramForm(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	form.Name = prog.Name
	form.IsRoot = !prog.ParentID.Valid
	if prog.ParentID.Valid {
		form.ParentID = prog.ParentID.Int64
	}
	s.render(w, r, http.StatusOK, "program-form", form)
}

// buildProgramForm assembles the parent option list for a form. The options are
// every program EXCEPT the subject and its descendants (so the select never OFFERS
// a cycle) -- presentation only; the store still enforces cycles and root
// immovability (like buildSubsidiaryForm).
func (s *server) buildProgramForm(ctx context.Context, id int64) (programForm, error) {
	form := programForm{ID: id}

	// Exclude the subject + its descendants from the parent options on edit (self +
	// transitive closure; ProgramDescendants includes self as its base case).
	excluded := map[int64]bool{}
	if id != 0 {
		desc, err := s.store.ProgramDescendants(ctx, id)
		if err != nil {
			return form, err
		}
		for _, d := range desc {
			excluded[d.ID] = true
		}
	}

	progs, err := s.store.ProgramTree(ctx)
	if err != nil {
		return form, err
	}
	for _, p := range progs {
		if excluded[p.ID] {
			continue
		}
		form.Parents = append(form.Parents, programOption{ID: p.ID, Name: p.Name})
	}
	return form, nil
}

// ---- create / update / deactivate -----------------------------------------

// programCreate handles POST /programs (TxnWrite). It parses the form and calls
// store.CreateProgram; a no-parent submit yields ErrProgramSecondRoot from the
// store (the handler does not re-validate). On a typed error it maps to a
// field-error key and re-renders the form at 422; success redirects (PRG).
func (s *server) programCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, in, err := s.parseProgramForm(r, 0)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if _, err := s.store.CreateProgram(s.actorCtx(ctx), store.CreateProgramInput{
		ParentID: in.parentID,
		Name:     in.name,
	}); err != nil {
		s.renderProgramFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/programs")
}

// programUpdate handles POST /programs/{id} (TxnWrite): rename / move via
// UpdateProgram. A non-positive parent id sends no ParentID (no move), so an
// accidental "0" never reparents; giving the ROOT a parent yields
// ErrProgramRootImmovable and a cycle yields ErrCycle -- both mapped to keys and
// re-rendered at 422.
func (s *server) programUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	form, in, err := s.parseProgramForm(r, id)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	upd := store.UpdateProgramInput{Name: &in.name}
	// A positive parent selection is a MOVE target; the store validates it against
	// root-immovability and cycles. Selecting nothing (0) leaves the parent as-is.
	// Reparenting the ROOT is still attempted (the form for the root omits the
	// select, but a raw submit reaches the store, which returns ErrProgramRootImmovable).
	if in.parentID > 0 {
		upd.ParentID = &in.parentID
	}
	if err := s.store.UpdateProgram(s.actorCtx(ctx), id, upd); err != nil {
		s.renderProgramFormError(w, r, form, err)
		return
	}
	redirectAfterForm(w, r, "/programs")
}

// programDeactivate handles POST /programs/{id}/deactivate (TxnWrite).
// DeactivateProgram is BLOCKED while the program has active children
// (ErrProgramHasActiveChildren); the store's no-trace discipline leaves it active.
// On the guard the handler re-renders the LIST at 422 with the localized blocked
// message (the guard is shown; nothing executed). Deactivation blocks NEW use but
// keeps history intact (op=update, not delete, D24). Success redirects to the list.
func (s *server) programDeactivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.DeactivateProgram(s.actorCtx(ctx), id); err != nil {
		if key := programPageErrorKey(err); key != "" {
			from, to := s.programPeriod(r)
			model, berr := s.buildProgramsPage(ctx, from, to)
			if berr != nil {
				s.serverError(w)
				return
			}
			model.ErrorKey = key
			s.render(w, r, http.StatusUnprocessableEntity, "programs.tmpl", s.newShellPageControls(r, model, "programs"))
			return
		}
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/programs", http.StatusSeeOther)
}

// parsedProgramForm is the validated-shape (raw strings turned into typed fields)
// of a submitted program form; the store does the real validation.
type parsedProgramForm struct {
	name     string
	parentID int64
}

// parseProgramForm reads the POST form into a programForm (for a 422 re-render) and
// a parsedProgramForm (for the store call). id is the edit target (0 = create); it
// is threaded into buildProgramForm so a 422 re-render of an EDIT excludes the
// subject + descendants from the parent select -- exactly as the initial edit GET
// does. It does NOT validate business rules (the store owns that).
func (s *server) parseProgramForm(r *http.Request, id int64) (programForm, parsedProgramForm, error) {
	if err := r.ParseForm(); err != nil {
		return programForm{}, parsedProgramForm{}, err
	}
	in := parsedProgramForm{
		name:     r.PostFormValue("name"),
		parentID: parseID(r.PostFormValue("parent_id")),
	}
	form, err := s.buildProgramForm(r.Context(), id)
	if err != nil {
		return programForm{}, parsedProgramForm{}, err
	}
	// Echo submitted values back so a 422 re-render keeps what the user entered.
	form.Name = in.name
	form.ParentID = in.parentID
	// On edit, mark the root so the template still omits the parent select.
	if id != 0 {
		if prog, gerr := s.store.GetProgram(r.Context(), id); gerr == nil {
			form.IsRoot = !prog.ParentID.Valid
		}
	}
	return form, in, nil
}

// renderProgramFormError maps a store TYPED error to an i18n field-error key and
// re-renders the "program-form" partial at 422 (the p10.3 convention). It never
// re-validates -- the store is the source of truth; this only TRANSLATES its typed
// errors to (field, key) pairs.
func (s *server) renderProgramFormError(w http.ResponseWriter, r *http.Request, form programForm, err error) {
	field, key := programErrorField(err)
	if key == "" {
		s.serverError(w) // an unknown error is a real server fault, not a validation failure
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "program-form", form)
}

// programErrorField maps a store typed error to the (form field, i18n key) pair
// the form-error convention needs. The field name drives autofocus placement. Note
// store.ErrCycle is SHARED with account/subsidiary moves; it maps to a
// program-specific key here (same physical error, different page). An unrecognized
// error returns ("",""), which the caller treats as a 500.
func programErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrProgramSecondRoot):
		return "parent_id", "error.program.second_root"
	case errors.Is(err, store.ErrProgramParentMissing):
		return "parent_id", "error.program.parent_missing"
	case errors.Is(err, store.ErrProgramRootImmovable):
		return "parent_id", "error.program.root_immovable"
	case errors.Is(err, store.ErrCycle):
		return "parent_id", "error.program.cycle"
	default:
		return "", ""
	}
}

// programPageErrorKey maps a DEACTIVATE guard error to a page-level i18n key (the
// deactivate action has no form to target, so its error surfaces on the list).
// Only ErrProgramHasActiveChildren is a user-facing guard here; anything else is a
// server fault ("").
func programPageErrorKey(err error) string {
	if errors.Is(err, store.ErrProgramHasActiveChildren) {
		return "error.program.has_active_children"
	}
	return ""
}
