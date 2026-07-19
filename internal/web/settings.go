package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p13.1 my settings (/settings, AnyUser). ONE personal-preferences page: the UI
// language, the money/date display columns (date format, number format, display
// mode signed vs DR/CR, negative style), the theme, and the (optional) default
// subsidiary for new transactions. A user edits THEIR OWN settings only (the admin
// edit-other-users surface is p13.2).
//
// The render paths already honor these columns per-user (format.go maps the raw
// column strings to money.FormatOpts / DateFormat; resolveLang reads locale;
// resolveTheme reads theme). This page's job is to PERSIST them under one versioned
// change (store.UpdateUserSettings), so the register/reports then render per-user
// automatically.
//
// This is a FULL-PAGE form (like /admin/org, not an inline htmx swap): on save it
// 303-redirects back to GET /settings (Post/Redirect/Get) so the new locale/theme
// render on the fresh GET; on an invalid POST (only reachable by a crafted request,
// since every input is a fixed <select>) it re-renders the WHOLE page at 422 with
// the field errors + autofocus (the p10.3 convention, adapted to a full page --
// there is no htmx target region to swap). The theme is ALSO written to the theme
// cookie on save (like setTheme) so resolveTheme -- which reads the cookie BEFORE
// the DB -- reflects the change on the very next render. Every string via {{t}}
// (rule 9); no inline script (rule 12).

// settingOption is one <option> of a settings select: the stored value and the
// i18n key of its localized label (rule 9 -- the template renders the key via
// {{t}}). Used for the closed-vocabulary selects (date/number/display/neg/theme).
type settingOption struct {
	Value    string
	LabelKey string
}

// settingsForm is the GET/POST /settings model: the current values (echoed into the
// selects so the swap/reload keeps the choice), the option lists, the Saved flag,
// and the ordered field errors (for a crafted-invalid POST). It follows the form
// model shape -- its own value fields plus an embedded formErrors named Errors.
type settingsForm struct {
	Locale         string
	DateFormat     string
	NumberFormat   string
	DisplayMode    string
	NegStyle       string
	Theme          string
	DefaultSub     int64 // 0 == unset (the "none" option)
	DefaultProgram int64 // 0 == unset (the "none" option) (p26.5)

	Langs        []langOption
	DateFormats  []settingOption
	NumberFmts   []settingOption
	DisplayModes []settingOption
	NegStyles    []settingOption
	Themes       []settingOption
	Subs         []subOption
	Programs     []programOption

	Saved  bool
	Errors formErrors
}

// The fixed option vocabularies (mirroring the store validators + format.go). Kept
// as functions so each render builds fresh slices; labels are i18n keys.
func dateFormatOptions() []settingOption {
	return []settingOption{
		{"ISO", "settings.date.iso"},
		{"US", "settings.date.us"},
		{"EU", "settings.date.eu"},
	}
}

func numberFormatOptions() []settingOption {
	return []settingOption{
		{"US", "settings.number.us"},
		{"EU", "settings.number.eu"},
		{"plain", "settings.number.plain"},
	}
}

func displayModeOptions() []settingOption {
	return []settingOption{
		{"signed", "settings.display.signed"},
		{"dr_cr", "settings.display.dr_cr"},
	}
}

func negStyleOptions() []settingOption {
	return []settingOption{
		{"minus", "settings.neg.minus"},
		{"parens", "settings.neg.parens"},
	}
}

func themeOptions() []settingOption {
	return []settingOption{
		{"auto", "theme.auto"},
		{"light", "theme.light"},
		{"dark", "theme.dark"},
	}
}

// settingsPage handles GET /settings (AnyUser): the preferences form prefilled from
// the current user's stored settings.
func (s *server) settingsPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	if u == nil {
		// enforce() (AnyUser) never lets an anon reach here, but stay total.
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	form, err := s.buildSettingsForm(r, settingsForm{
		Locale:         u.Locale,
		DateFormat:     u.DateFormat,
		NumberFormat:   u.NumberFormat,
		DisplayMode:    u.DisplayMode,
		NegStyle:       u.NegStyle,
		Theme:          u.Theme,
		DefaultSub:     derefID(u.DefaultSubsidiaryID),
		DefaultProgram: derefID(u.DefaultProgramID),
	})
	if err != nil {
		s.serverError(w)
		return
	}
	form.Saved = settingsSavedNotice(r)
	s.render(w, r, http.StatusOK, "settings.tmpl", s.newShellPage(r, form))
}

// buildSettingsForm attaches the option lists (language switcher options, the fixed
// setting vocabularies, and the subsidiary list) to a form carrying the current
// values. Langs are labelled in their OWN tongue (langOptions) with the current one
// flagged; the select vocabularies carry i18n label KEYS the template localizes.
func (s *server) buildSettingsForm(r *http.Request, form settingsForm) (settingsForm, error) {
	subs, err := s.store.AllSubsidiaries(r.Context())
	if err != nil {
		return settingsForm{}, err
	}
	form.Subs = make([]subOption, 0, len(subs))
	for _, sub := range subs {
		form.Subs = append(form.Subs, subOption{ID: sub.ID, Name: sub.Name})
	}
	// Default-program options (p26.5): the ACTIVE programs in tree order, mirroring the
	// txn editor's program select (an inactive program is not offered as a new default).
	progs, err := s.store.ProgramTree(r.Context())
	if err != nil {
		return settingsForm{}, err
	}
	// p29.13: dotted hierarchy path per program for the default-program combobox.
	progPaths, err := s.store.ProgramPaths(r.Context())
	if err != nil {
		return settingsForm{}, err
	}
	form.Programs = make([]programOption, 0, len(progs))
	for _, p := range progs {
		if p.Active == 0 {
			continue
		}
		form.Programs = append(form.Programs, programOption{ID: int64(p.ID), Name: p.Name, Path: progPaths[p.ID]})
	}
	form.Langs = langOptions(form.Locale)
	form.DateFormats = dateFormatOptions()
	form.NumberFmts = numberFormatOptions()
	form.DisplayModes = displayModeOptions()
	form.NegStyles = negStyleOptions()
	form.Themes = themeOptions()
	return form, nil
}

// settingsUpdate handles POST /settings (AnyUser): validate + persist the user's own
// preferences under one versioned change, set the theme cookie for immediate SSR
// effect, then 303-redirect back to GET /settings (PRG) so the new locale/theme
// render on the fresh GET. On invalid input (crafted POST) it re-renders the whole
// page at 422 with the field errors + autofocus.
func (s *server) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	form := settingsForm{
		Locale:       r.PostFormValue("locale"),
		DateFormat:   r.PostFormValue("date_format"),
		NumberFormat: r.PostFormValue("number_format"),
		DisplayMode:  r.PostFormValue("display_mode"),
		NegStyle:     r.PostFormValue("neg_style"),
		Theme:        r.PostFormValue("theme"),
	}

	// default_subsidiary: "" (the "none" option) clears it; a non-empty value must
	// parse to a positive id (existence is checked in the store).
	var defaultSub *int64
	if v := r.PostFormValue("default_subsidiary"); v != "" {
		id, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || id <= 0 {
			s.renderSettingsError(w, r, form, "default_subsidiary")
			return
		}
		defaultSub = &id
		form.DefaultSub = id
	}

	// default_program: "" (the "none" option) clears it; a non-empty value must parse
	// to a positive id (existence is checked in the store). Mirrors default_subsidiary.
	var defaultProgram *ids.ProgramID
	if v := r.PostFormValue("default_program"); v != "" {
		id, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || id <= 0 {
			s.renderSettingsError(w, r, form, "default_program")
			return
		}
		pid := ids.ProgramID(id)
		defaultProgram = &pid
		form.DefaultProgram = id
	}

	err := s.store.UpdateUserSettings(
		store.WithActor(r.Context(), store.Actor{ID: u.ID}),
		u.ID,
		store.UserSettingsInput{
			Locale:              form.Locale,
			DateFormat:          form.DateFormat,
			NumberFormat:        form.NumberFormat,
			DisplayMode:         form.DisplayMode,
			NegStyle:            form.NegStyle,
			Theme:               form.Theme,
			DefaultSubsidiaryID: defaultSub,
			DefaultProgramID:    defaultProgram,
		},
		known, // i18n.Langs membership (middleware.go)
	)
	if err != nil {
		if errors.Is(err, store.ErrInvalidSetting) {
			// A crafted POST slipped a value past the fixed selects. Re-render the
			// whole page at 422 with a form-level error, autofocus on locale (the
			// first field). We cannot pinpoint the offending field (the store returns
			// one sentinel), so flag the first control.
			s.renderSettingsError(w, r, form, "locale")
			return
		}
		s.serverError(w)
		return
	}

	// Theme cookie, like setTheme: resolveTheme reads the cookie BEFORE the DB, so
	// without this the next render would show the stale cookie theme. Path "/" so
	// every page sees it; HttpOnly (the theme is resolved server-side, no JS reads
	// it); Secure outside -dev.
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookieName,
		Value:    form.Theme,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.Dev,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})

	// PRG: 303 back to GET /settings so the fresh render reflects the new locale/
	// theme and the browser reload lands on a GET. A "saved" marker is carried in the
	// query so the GET can show the notice without a session round-trip.
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// renderSettingsError re-renders the full settings page at 422 with the field's
// error flagged (autofocus lands on it via FirstInvalid). Used only for the
// crafted-invalid POST path; a normal user, offered fixed <select> options, never
// reaches it.
func (s *server) renderSettingsError(w http.ResponseWriter, r *http.Request, form settingsForm, field string) {
	form.Errors.add(field, "settings.error.invalid")
	built, err := s.buildSettingsForm(r, form)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, "settings.tmpl", s.newShellPage(r, built))
}

// settingsSavedNotice reports whether the GET carries the PRG "saved" marker, so the
// page shows the "settings saved" notice after a successful POST/redirect.
func settingsSavedNotice(r *http.Request) bool {
	return r.URL.Query().Get("saved") != ""
}
