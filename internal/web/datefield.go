package web

// p28.18 ONE reusable date-input component (owner directive: "several of these,
// like the date inputs, need to be components created once and reused, rather than
// recreated each use"). The MARKUP for a `input.js-datefield` was hand-written and
// drifting at eight sites; this struct + the `datefield` template func let every
// site emit the canonical markup through the shared {{template "datefield"}} partial
// (templates/datefield.tmpl). The client enhancer (static/datefield.js) is unchanged
// — it keys on `input.js-datefield`, so behavior is identical.
//
// Empty string fields are OMITTED from the emitted markup (not rendered as name=""),
// so a site with no name/value/placeholder/format reproduces exactly what it wrote
// by hand. Autofocus is a bool the CALLER computes inline from its own error state
// (three sites do), since it is an attribute the partial owns and can't inject from
// outside.
type dateFieldModel struct {
	ID          string // the input's id (handlers/JS query these — must match the site's old id)
	Name        string // form field name; "" omits the attribute (the cadence-start input has none)
	Value       string // current display value; "" omits the attribute
	Placeholder string // placeholder text; "" omits the attribute (the report from/to have none)
	Class       string // extra classes appended after the canonical `js-datefield` (e.g. "txn-date")
	Format      string // per-input data-date-format override; "" omits the attribute
	Autofocus   bool   // caller-computed error-focus flag
}

// dateField is the `datefield` template func: it builds the partial's model from
// positional args so a site calls
//
//	{{template "datefield" (datefield "id" "name" "value" "placeholder" "class" "format" false)}}
//
// mirroring the makeRegRowCtx/makeReconRowCtx constructor-func convention.
func dateField(id, name, value, placeholder, class, format string, autofocus bool) dateFieldModel {
	return dateFieldModel{
		ID:          id,
		Name:        name,
		Value:       value,
		Placeholder: placeholder,
		Class:       class,
		Format:      format,
		Autofocus:   autofocus,
	}
}
