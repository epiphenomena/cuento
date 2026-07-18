package web

import (
	"errors"
	"net/http"
	"strconv"

	"cuento/internal/store"
)

// p13.2 admin: per-user permissions detail (/admin/users/{id}, Perm Admin). One
// page gathers the txn_perm select and the report-group grant checkboxes for a
// single user -- the two VERSIONED perm changes (each a change naming the acting
// admin, rule 5). The txn_perm change (POST .../txn-perm) and the grants diff
// (POST .../grants) are separate small forms so each is its own audited change.
//
// The grants form submits the FULL desired set (checked boxes); the handler diffs
// it against the current grants and issues one grant per newly-checked group and
// one revoke per newly-unchecked group -- each a versioned change. Unchanged
// groups produce no change (the store's Has-guard also no-ops a redundant grant).
// The system user is unreachable here (the store's AdminUserByID refuses id 1 ->
// 404). Every string via {{t}} (rule 9); no inline script (rule 12).

// grantCheckbox is one report-group row on the detail page: the group name, whether
// the user currently holds it (drives the checkbox's checked state), and -- for a
// PROGRAM-DIMENSIONED group (one containing at least one program-dimensioned report,
// p27.4c) -- an optional program-subtree scope. ScopeSelectable is true only for such
// groups: a group with NO program-dimensioned report (e.g. "funds" after the p27.4b
// demotions) is never offered a picker, because scoping it would grant effectively
// nothing (the empty-coverage trap the p27.4b note flagged). ProgramID/ScopeName
// carry the CURRENT scope of a held grant so the admin can see it ("org-wide" when nil).
type grantCheckbox struct {
	Name            string
	Granted         bool
	ScopeSelectable bool
	ProgramID       *int64 // current scope of a held grant (nil = org-wide)
	ScopeName       string // program name for ProgramID (empty = org-wide)
}

// ScopeID returns the current scope's program id, or 0 when the grant is org-wide
// (nil ProgramID). It exists so the template can compare a program option's id to the
// held scope with a plain `eq` (a *int64 is not directly `eq`-comparable to an int64).
func (g grantCheckbox) ScopeID() int64 {
	if g.ProgramID == nil {
		return 0
	}
	return *g.ProgramID
}

// programScopeOption is one selectable program in a grant's program-scope picker
// (p27.4c): the program id and its display name (a stored proper noun, D5). The set
// is the whole program tree in pre-order, mirroring the report page's program
// selector (report.tmpl) -- a flat <select>, the sanctioned program-picker pattern.
type programScopeOption struct {
	ID   int64
	Name string
}

// userDetailModel is the GET /admin/users/{id} model: the subject user, the
// txn_perm options (with the current one selectable), the grant checkboxes, and an
// optional saved notice.
type userDetailModel struct {
	ID          int64
	Username    string
	DisplayName string
	IsAdmin     bool
	Disabled    bool
	TxnPerm     string
	TxnPerms    []settingOption
	Grants      []grantCheckbox
	// Programs is the program-scope option set every program-dimensioned grant row
	// shares (the app's program selector, reused). Empty when no programs exist.
	Programs []programScopeOption
	// CanSubmitExpenses drives the p20.2 admin toggle for the standalone expense-
	// submit capability (p20.1). Admin-gated; a versioned change on save.
	CanSubmitExpenses bool
	Saved             bool

	// errorKey/errorField carry the single crafted-bad-perm validation message and
	// the field it applies to (autofocus). Lowercase (template-invisible directly);
	// exported accessors below let the template read them.
	errorKey   string
	errorField string
}

// ErrorKey / ErrorField expose the (optional) validation message + field to the
// template (the detail page uses one page-level error, not the formErrors embed).
func (m userDetailModel) ErrorKey() string   { return m.errorKey }
func (m userDetailModel) ErrorField() string { return m.errorField }

// userDetailPage handles GET /admin/users/{id} (Admin): the per-user perm editor.
// A missing / system user id is a 404 (the store refuses id 1 and unknown ids).
func (s *server) userDetailPage(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	model, err := s.buildUserDetail(r, id)
	if err != nil {
		// The system user is not manageable -> back to the list (not a 404: it exists,
		// it is just off-limits). An unknown id is a genuine 404.
		if errors.Is(err, store.ErrSystemUser) {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
		if errors.Is(err, store.ErrUserNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}
	model.Saved = r.URL.Query().Get("saved") != ""
	s.render(w, r, http.StatusOK, "admin_user_detail.tmpl", s.newShellPage(r, model))
}

// buildUserDetail loads the subject user, its current grants, and the full report-
// group set, marking which groups the user holds.
func (s *server) buildUserDetail(r *http.Request, id int64) (userDetailModel, error) {
	ctx := r.Context()
	u, err := s.store.AdminUserByID(ctx, id)
	if err != nil {
		return userDetailModel{}, err
	}
	held, err := s.store.ReportGrants(ctx, id)
	if err != nil {
		return userDetailModel{}, err
	}
	heldScope := make(map[string]*int64, len(held)) // group -> current scope (nil = org-wide)
	heldSet := make(map[string]bool, len(held))
	for _, g := range held {
		heldSet[g.Group] = true
		heldScope[g.Group] = g.ProgramID
	}
	groups, err := s.store.ReportGroupNames(ctx)
	if err != nil {
		return userDetailModel{}, err
	}
	// Program options (the app's program selector, reused) + an id->name lookup so a
	// held grant's current scope can show the program NAME, not a bare id.
	progs, err := s.programStatementOptions(ctx)
	if err != nil {
		return userDetailModel{}, err
	}
	progName := make(map[int64]string, len(progs))
	scopeOpts := make([]programScopeOption, 0, len(progs))
	for _, p := range progs {
		progName[p.ID] = p.Name
		scopeOpts = append(scopeOpts, programScopeOption{ID: p.ID, Name: p.Name})
	}
	// Which groups may carry a program scope: only those containing a program-
	// dimensioned report (p27.4c). Computed from the registry so it stays locked to
	// TestProgramDimensionedSet.
	pdGroups := s.reports.ProgramDimensionedGroups()

	model := userDetailModel{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
		IsAdmin: u.IsAdmin, Disabled: u.Disabled, TxnPerm: u.TxnPerm,
		TxnPerms:          txnPermOptions(),
		CanSubmitExpenses: u.CanSubmitExpenses,
		Programs:          scopeOpts,
	}
	for _, g := range groups {
		row := grantCheckbox{Name: g, Granted: heldSet[g], ScopeSelectable: pdGroups[g]}
		if scope := heldScope[g]; scope != nil {
			row.ProgramID = scope
			row.ScopeName = progName[*scope]
		}
		model.Grants = append(model.Grants, row)
	}
	return model, nil
}

// userSetTxnPerm handles POST /admin/users/{id}/txn-perm (Admin): a versioned
// txn_perm change naming the acting admin. A crafted bad value (the form is a fixed
// <select>) is a 422 re-render; the system user is unreachable (the store refuses
// id 1). Success 303-redirects back to the detail page with a saved notice (PRG).
func (s *server) userSetTxnPerm(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	perm := r.PostFormValue("txn_perm")
	if err := s.store.SetUserTxnPerm(s.actorCtx(r.Context()), id, perm); err != nil {
		switch {
		case errors.Is(err, store.ErrSystemUser):
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		case errors.Is(err, store.ErrUserNotFound):
			http.NotFound(w, r)
		case errors.Is(err, store.ErrInvalidTxnPerm):
			s.renderUserDetailError(w, r, id, "txn_perm", "admin.users.error.bad_perm")
		default:
			s.serverError(w)
		}
		return
	}
	http.Redirect(w, r, userDetailURL(id), http.StatusSeeOther)
}

// userSetGrants handles POST /admin/users/{id}/grants (Admin): the checkbox set is
// the DESIRED grants; the handler diffs against the current grants and issues one
// versioned grant per added group and one versioned revoke per removed group (each
// a change naming the acting admin). Unknown submitted groups are ignored (only the
// real report-group set is considered), so a crafted box cannot create a bogus
// grant. Success 303-redirects back with a saved notice.
func (s *server) userSetGrants(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// The subject must be a real, manageable user (refuses id 1 / unknown). The
	// system user is off-limits -> back to the list; an unknown id is a 404.
	if _, err := s.store.AdminUserByID(ctx, id); err != nil {
		if errors.Is(err, store.ErrSystemUser) {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
		if errors.Is(err, store.ErrUserNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}

	groups, err := s.store.ReportGroupNames(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	held, err := s.store.ReportGrants(ctx, id)
	if err != nil {
		s.serverError(w)
		return
	}
	heldSet := make(map[string]bool, len(held))
	heldScope := make(map[string]*int64, len(held))
	for _, g := range held {
		heldSet[g.Group] = true
		heldScope[g.Group] = g.ProgramID
	}

	// Which groups may carry a program scope (contain a program-dimensioned report,
	// p27.4c) and the set of real program ids -- both bound server-side so a crafted
	// "program_<group>" on a non-program-dim group (the empty-coverage trap) or a bogus
	// id cannot create a scope-to-nothing grant. Mirrors "unknown groups ignored".
	pdGroups := s.reports.ProgramDimensionedGroups()
	progs, err := s.programStatementOptions(ctx)
	if err != nil {
		s.serverError(w)
		return
	}

	// A group is desired iff its checkbox ("grant_<name>") is present; its desired scope
	// is the "program_<name>" value, HONORED only for a program-dimensioned group naming a
	// real program (else nil = org-wide).
	wanted := make(map[string]bool, len(groups))
	wantScope := make(map[string]*int64, len(groups))
	for _, g := range groups {
		wanted[g] = r.PostForm.Get("grant_"+g) != ""
		if wanted[g] && pdGroups[g] {
			if pid, ok := parseProgramScope(r.PostForm.Get("program_"+g), progs); ok {
				wantScope[g] = pid
			}
		}
	}

	actorCtx := s.actorCtx(ctx)
	for _, g := range groups {
		switch {
		case wanted[g] && !heldSet[g]:
			if err := s.store.GrantReportGroup(actorCtx, id, g, wantScope[g]); err != nil {
				s.serverError(w)
				return
			}
		case wanted[g] && heldSet[g] && !sameScope(heldScope[g], wantScope[g]):
			// Still held, still wanted, but the scope changed -> re-grant (GrantReportGroup
			// does the atomic revoke+grant, one change; a same-scope call is a no-op so the
			// sameScope guard keeps a plain resave from spamming an audit row).
			if err := s.store.GrantReportGroup(actorCtx, id, g, wantScope[g]); err != nil {
				s.serverError(w)
				return
			}
		case !wanted[g] && heldSet[g]:
			if err := s.store.RevokeReportGroup(actorCtx, id, g); err != nil {
				s.serverError(w)
				return
			}
		}
	}
	http.Redirect(w, r, userDetailURL(id), http.StatusSeeOther)
}

// parseProgramScope resolves a submitted "program_<group>" value to a program id,
// returning (nil,true) for an empty/absent value (org-wide) and (id,true) for a value
// naming a REAL program in progs. A non-empty value that is not a real program id
// yields (nil,false) -- the caller then leaves the grant org-wide rather than scoping
// to a bogus id (a crafted-input guard, mirroring programExists on the report page).
func parseProgramScope(raw string, progs []programOption) (*int64, bool) {
	if raw == "" {
		return nil, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !programExists(progs, id) {
		return nil, false
	}
	return &id, true
}

// sameScope reports whether two optional program scopes are equal (both nil, or both
// non-nil naming the same program) -- the no-op guard that keeps a resave with an
// unchanged scope from re-granting (and appending a spurious version row).
func sameScope(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// userSetCanSubmit handles POST /admin/users/{id}/can-submit (Admin): toggle the
// standalone can_submit_expenses capability (p20.1). The checkbox value ("1" when
// checked) is the DESIRED state; a versioned change naming the acting admin records
// it. The system user is unreachable (the store refuses id 1); an unknown id 404s.
// Success 303-redirects back with a saved notice (PRG). This is the p20.1-deferred
// admin UI for the ExpenseSubmit right.
func (s *server) userSetCanSubmit(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// Confirm the subject is a real, manageable user (refuses id 1 / unknown) before
	// the versioned write, mirroring userSetGrants -- so an unknown id 404s cleanly and
	// no stray version row is written.
	if _, err := s.store.AdminUserByID(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrSystemUser) {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
		if errors.Is(err, store.ErrUserNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}
	can := r.PostFormValue("can_submit_expenses") != ""
	if err := s.store.SetUserCanSubmitExpenses(s.actorCtx(r.Context()), id, can); err != nil {
		switch {
		case errors.Is(err, store.ErrSystemUser):
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		case errors.Is(err, store.ErrUserNotFound):
			http.NotFound(w, r)
		default:
			s.serverError(w)
		}
		return
	}
	http.Redirect(w, r, userDetailURL(id), http.StatusSeeOther)
}

// renderUserDetailError re-renders the detail page at 422 with a field error (the
// crafted-bad-perm path). It reloads the page model so the current state is shown.
func (s *server) renderUserDetailError(w http.ResponseWriter, r *http.Request, id int64, field, key string) {
	model, err := s.buildUserDetail(r, id)
	if err != nil {
		s.serverError(w)
		return
	}
	// The detail page has no formErrors embed; surface the message page-level via a
	// dedicated field. Keep it simple: a single error key drives an alert + autofocus
	// hint on the named field.
	model.errorField = field
	model.errorKey = key
	s.render(w, r, http.StatusUnprocessableEntity, "admin_user_detail.tmpl", s.newShellPage(r, model))
}

// userDetailURL builds the PRG target for the detail page with a saved marker.
func userDetailURL(id int64) string {
	return "/admin/users/" + strconv.FormatInt(id, 10) + "?saved=1"
}
