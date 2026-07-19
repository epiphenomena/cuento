package web

import (
	"net/http"

	"cuento/internal/store"
)

// p11.4 org settings & languages (/admin/org, Perm Admin). A small Admin form to
// edit the enabled languages (a CSV of language codes account NAMES may be entered
// in, D14). The enabled languages drive the account form's per-language name inputs
// (accounts.go) -- adding a language here makes a new name column appear there.
//
// The organization's display NAME is no longer a setting (p30.14): it is derived
// from the root subsidiary (the consolidating entity, D18/p09.1) and shown here
// read-only, pointing the admin at /admin/subsidiaries to change it. So for now the
// only editable org-level setting is enabled_languages.
//
// org_settings is a simple non-versioned CONFIG table (like currencies/
// report_groups): the store reads/writes it with plain sqlc upserts outside the
// write funnel (rule 2 permits config upserts via sqlc). So this handler does NOT
// go through actorCtx -- these are configuration writes, not audited business
// mutations. Report base currency is intentionally NOT here: it follows the scoped
// subsidiary (D18). Every string via {{t}} (rule 9); no inline script (rule 12).

// orgForm is the GET/POST /admin/org model: the enabled-languages CSV plus the
// derived (read-only) organization display name, and a Saved flag the page shows
// after a successful POST.
type orgForm struct {
	OrgDisplayName   string
	EnabledLanguages string
	Saved            bool
	Errors           formErrors
}

// orgPage handles GET /admin/org (Admin): the settings form prefilled from the
// stored values (seed default: enabled_languages en,es).
func (s *server) orgPage(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildOrgForm(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "org.tmpl", s.newShellPage(r, form))
}

// buildOrgForm reads the current org settings into the form model. The org display
// name is derived from the root subsidiary (p30.14), not stored.
func (s *server) buildOrgForm(r *http.Request) (orgForm, error) {
	ctx := r.Context()
	name, err := s.store.RootSubsidiaryName(ctx)
	if err != nil {
		return orgForm{}, err
	}
	langs, err := s.store.OrgSetting(ctx, store.SettingEnabledLanguages, "en,es")
	if err != nil {
		return orgForm{}, err
	}
	return orgForm{OrgDisplayName: name, EnabledLanguages: langs}, nil
}

// orgUpdate handles POST /admin/org (Admin): store the enabled languages (a config
// write, not audited -- a plain sqlc upsert in the store). The enabled-languages
// value is stored VERBATIM; the store's EnabledLanguages parses and normalizes it
// (trim, dedupe, en-first) at read time, so a stray space or a dropped en never
// breaks the account form. On success the page re-renders with a Saved notice
// (settings pages have no list to redirect to).
func (s *server) orgUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	langs := r.PostFormValue("enabled_languages")

	if err := s.store.SetOrgSetting(ctx, store.SettingEnabledLanguages, langs); err != nil {
		s.serverError(w)
		return
	}

	// Re-render prefilled from the stored (normalized-on-read) values with a Saved
	// notice. For an htmx submit this swaps the form region back in place.
	form, err := s.buildOrgForm(r)
	if err != nil {
		s.serverError(w)
		return
	}
	form.Saved = true
	s.render(w, r, http.StatusOK, "org.tmpl", s.newShellPage(r, form))
}
