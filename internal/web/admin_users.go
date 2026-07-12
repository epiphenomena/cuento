package web

import (
	"errors"
	"net/http"
	"strings"

	"cuento/internal/auth"
	"cuento/internal/store"
)

// p13.2 admin: users management (/admin/users, Perm Admin -- Appendix B/F). The
// list shows every manageable operator (the store's ListUsers excludes the seeded
// system user) with their admin flag, txn_perm, enabled/disabled state, and report
// grants; an inline htmx create form (username + password + admin? + txn_perm)
// hangs off the list. Per-user actions: disable (never DELETE -- D-conventions),
// reset password (a fresh argon2id hash via internal/auth -- the SAME hashing path
// login uses; never hand-rolled, rule 13), and the per-user detail page carrying
// the txn_perm select + report-group grant checkboxes.
//
// Guards (store-owned, honored here as page-level 422s like subsidiaries'
// ErrHasActiveChildren): the system user can never be managed (ErrSystemUser) and
// the LAST enabled admin cannot be disabled (ErrLastAdmin) -- else the org locks
// itself out of /admin/**. Every string via {{t}} (rule 9); no inline script (rule
// 12); usernames/display names are stored data (proper-noun-like), rendered
// verbatim.

// ---- list page -----------------------------------------------------------

// adminUserRow is one rendered list row: the user plus a joined, comma-free view
// of its report grants (for a compact list cell). Grants are the group names the
// user holds; the per-user page manages them.
type adminUserRow struct {
	ID          int64
	Username    string
	DisplayName string
	IsAdmin     bool
	TxnPerm     string
	Disabled    bool
	Grants      []string
}

// usersPageModel is the GET /admin/users model: the rows, the create-form option
// data (txn_perm choices), and an optional page-level error key (a blocked
// disable/reset guard surfaces here, no execution).
type usersPageModel struct {
	Rows      []adminUserRow
	TxnPerms  []settingOption
	ErrorKey  string
	CreateNew bool // render the create form expanded (after a failed create re-render)
}

// txnPermOptions is the fixed none/read/write vocabulary (mirroring the migration
// 00006 CHECK + store.validTxnPerm), labelled with i18n keys.
func txnPermOptions() []settingOption {
	return []settingOption{
		{"none", "admin.users.perm.none"},
		{"read", "admin.users.perm.read"},
		{"write", "admin.users.perm.write"},
	}
}

// usersPage handles GET /admin/users (Admin): the operator list + inline create
// form region.
func (s *server) usersPage(w http.ResponseWriter, r *http.Request) {
	model, err := s.buildUsersPage(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "admin_users.tmpl", s.newShellPage(r, model))
}

// buildUsersPage assembles the list rows (with each user's grants) and the create
// form's option data.
func (s *server) buildUsersPage(r *http.Request) (usersPageModel, error) {
	ctx := r.Context()
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return usersPageModel{}, err
	}
	model := usersPageModel{TxnPerms: txnPermOptions()}
	for _, u := range users {
		grants, err := s.store.ReportGrants(ctx, u.ID)
		if err != nil {
			return usersPageModel{}, err
		}
		model.Rows = append(model.Rows, adminUserRow{
			ID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
			IsAdmin: u.IsAdmin, TxnPerm: u.TxnPerm, Disabled: u.Disabled, Grants: grants,
		})
	}
	return model, nil
}

// ---- create --------------------------------------------------------------

// userCreateForm is the inline create-form model: echoed values (so a 422 keeps
// what was typed -- except the password, never echoed) + the txn_perm options +
// field errors.
type userCreateForm struct {
	Username    string
	DisplayName string
	IsAdmin     bool
	TxnPerm     string
	TxnPerms    []settingOption
	Errors      formErrors
}

// userNewForm handles GET /admin/users/new (Admin): the empty create form partial
// for an htmx swap into #user-create-form.
func (s *server) userNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "user-create-form", userCreateForm{TxnPerm: "none", TxnPerms: txnPermOptions()})
}

// userCreate handles POST /admin/users (Admin): create an operator with a fresh
// argon2id password hash (via internal/auth, the shared credential path). It
// validates the required fields (username, password) HERE (username uniqueness is
// enforced by the DB UNIQUE via the store's error), maps failures to field-error
// keys, and re-renders the create form at 422; success redirects to the list.
func (s *server) userCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	form := userCreateForm{
		Username:    strings.TrimSpace(r.PostFormValue("username")),
		DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
		IsAdmin:     r.PostFormValue("is_admin") != "",
		TxnPerm:     r.PostFormValue("txn_perm"),
		TxnPerms:    txnPermOptions(),
	}
	password := r.PostFormValue("password")

	// Field-level validation (the create form has no store-side typed errors for
	// these). Display name defaults to the username when blank.
	if form.Username == "" {
		form.Errors.add("username", "admin.users.error.username_required")
	}
	if password == "" {
		form.Errors.add("password", "admin.users.error.password_required")
	}
	if !store.ValidTxnPermPublic(form.TxnPerm) {
		form.Errors.add("txn_perm", "admin.users.error.bad_perm")
	}
	if form.Errors.any() {
		s.renderFormError(w, r, "user-create-form", form)
		return
	}
	displayName := form.DisplayName
	if displayName == "" {
		displayName = form.Username
	}

	hash, err := auth.Hash(password)
	if err != nil {
		s.serverError(w)
		return
	}
	if _, err := s.store.CreateUser(s.actorCtx(r.Context()), store.CreateUserInput{
		Username:     form.Username,
		DisplayName:  displayName,
		PasswordHash: &hash,
		IsAdmin:      form.IsAdmin,
		TxnPerm:      form.TxnPerm,
	}); err != nil {
		// A duplicate username surfaces as ErrUsernameTaken; show it on the username
		// field rather than 500ing.
		if errors.Is(err, store.ErrUsernameTaken) {
			form.Errors.add("username", "admin.users.error.username_taken")
			s.renderFormError(w, r, "user-create-form", form)
			return
		}
		s.serverError(w)
		return
	}
	redirectAfterForm(w, r, "/admin/users")
}

// ---- disable / reset -----------------------------------------------------

// userDisable handles POST /admin/users/{id}/disable (Admin). It never deletes --
// disable only (D-conventions). The store guards refuse the system user and the
// last enabled admin; on a guard the list re-renders at 422 with the localized
// message (no execution), mirroring the subsidiaries deactivate path.
func (s *server) userDisable(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	if err := s.store.DisableUser(s.actorCtx(r.Context()), id); err != nil {
		s.renderUsersGuard(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// userResetPassword handles POST /admin/users/{id}/reset-password (Admin): set a
// new argon2id hash via the shared auth path. The system user is refused
// (ErrSystemUser -> page guard). A blank password is a 422 on the list.
func (s *server) userResetPassword(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if id == systemUserWebID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	password := r.PostFormValue("password")
	if password == "" {
		s.renderUsersPageError(w, r, "admin.users.error.password_required")
		return
	}
	hash, err := auth.Hash(password)
	if err != nil {
		s.serverError(w)
		return
	}
	if err := s.store.SetUserPassword(s.actorCtx(r.Context()), id, hash); err != nil {
		s.serverError(w)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// renderUsersGuard maps a store guard error to a response: the last-admin guard
// is a page-level 422 (a real blocked action the admin must see); the system user
// is off-limits -> a plain 303 back to the list (it never appears in the list, so
// this is only reachable by a crafted id); an unknown id is a 404. Anything else is
// a real 500.
func (s *server) renderUsersGuard(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		s.renderUsersPageError(w, r, "admin.users.error.last_admin")
	case errors.Is(err, store.ErrSystemUser):
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	case errors.Is(err, store.ErrUserNotFound):
		http.NotFound(w, r)
	default:
		s.serverError(w)
	}
}

// renderUsersPageError re-renders the list at 422 with a page-level error key.
func (s *server) renderUsersPageError(w http.ResponseWriter, r *http.Request, key string) {
	model, err := s.buildUsersPage(r)
	if err != nil {
		s.serverError(w)
		return
	}
	model.ErrorKey = key
	s.render(w, r, http.StatusUnprocessableEntity, "admin_users.tmpl", s.newShellPage(r, model))
}

// systemUserWebID mirrors the store's system-user id for the web-side reset guard
// (the store also refuses it; this avoids a needless hash + write).
const systemUserWebID int64 = 1
