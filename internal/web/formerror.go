package web

import "net/http"

// p10.3 establishes the form-error convention every later form (accounts p11,
// transactions p12, reconciliation p16, import p17) REUSES. The shape:
//
//   - A form's markup is a SINGLE-SOURCED named partial (e.g. "demo-form"). The
//     full page renders it inside the shell; a failed POST re-renders JUST that
//     partial. Because both paths render the same template, input ids stay stable
//     across the swap (anti-jank, Appendix C) with no duplication.
//   - On a failed validation the handler returns HTTP 422 (Unprocessable Content)
//     and writes only the partial. htmx swaps it into the form's target (htmx 2.x
//     is configured to swap 422 responses; see base.tmpl's htmx-config meta).
//   - Field errors are i18n KEYS (rule 9: Go returns keys for display), rendered
//     via {{t}} in the request language. FormErrors is ORDERED so "first invalid
//     field" is deterministic; the template marks exactly that field `autofocus`.
//
// renderFormError is deliberately THIN — a 422 + partial name + model over the
// existing render (which already sets no-store and rebinds {{t}}/{{asset}} for the
// request language). That thinness is the point: p11/p12 call it, pass their own
// partial name and model, and inherit the 422/partial/i18n/autofocus behavior.

// fieldError pairs a form field's stable name with the i18n KEY of its validation
// message (rule 9). The name matches the field's model key so the template can ask
// "is this field the first invalid one?" for autofocus placement.
type fieldError struct {
	Field string // stable field name, e.g. "name"
	Key   string // i18n error key, e.g. "error.required"
}

// formErrors is the ordered set of a form's field errors. Order is submission /
// field order, so FirstInvalid() is the top-most invalid field — the one the
// convention gives `autofocus` after the swap (Appendix C). A form model embeds one
// of these and the partial consults it; the zero value means "no errors" (a valid
// render).
type formErrors struct {
	fields []fieldError
}

// add appends a field's error KEY, keeping insertion (field) order so FirstInvalid
// is deterministic. It ignores an empty key so callers can `add(name, validate(v))`
// where validate returns "" when the value is fine.
func (fe *formErrors) add(field, key string) {
	if key == "" {
		return
	}
	fe.fields = append(fe.fields, fieldError{Field: field, Key: key})
}

// any reports whether the form has at least one field error (i.e. the POST is
// invalid and must re-render the partial at 422).
func (fe formErrors) any() bool { return len(fe.fields) > 0 }

// ErrorKey returns the i18n error key for a field, or "" if that field is valid.
// Exported (capitalized) so templates can call it: {{if .Errors.ErrorKey "name"}}.
func (fe formErrors) ErrorKey(field string) string {
	for _, f := range fe.fields {
		if f.Field == field {
			return f.Key
		}
	}
	return ""
}

// FirstInvalid returns the field name of the first (top-most) invalid field, or ""
// when the form is valid. The template stamps `autofocus` on exactly this field so
// focus lands on the first problem after the swap (anti-jank, Appendix C). Exported
// for template use: {{if eq .Errors.FirstInvalid "name"}}autofocus{{end}}.
func (fe formErrors) FirstInvalid() string {
	if len(fe.fields) == 0 {
		return ""
	}
	return fe.fields[0].Field
}

// renderFormError writes a failed form POST: HTTP 422 plus ONLY the named form-region
// partial (single-sourced with the full page), rendered in the request language via
// render. htmx swaps the 422 body into the form's target. This is the one call every
// later form makes on invalid input — it never writes a full page, so the swap is
// jank-free and the input ids stay stable.
func (s *server) renderFormError(w http.ResponseWriter, r *http.Request, partial string, model any) {
	s.render(w, r, http.StatusUnprocessableEntity, partial, model)
}
