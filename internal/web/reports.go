package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"cuento/internal/i18n"
	"cuento/internal/money"
	"cuento/internal/reports"
	"cuento/internal/store"
)

// p15.1 reports framework -- the web side of internal/reports. The reports package
// owns the report SHAPE (Report/Table/Toolkit) and the CSV renderer but no HTTP and
// no html/template (so it never imports web); THIS file is the generic glue:
//
//   - reportPage renders ANY report's Table into the app shell via ONE generic
//     template (report.tmpl), formatting typed cells per the user's settings
//     (rule 10) and localizing every label (rule 9). p15.3–p15.11 add reports
//     WITHOUT touching this handler or that template.
//   - reportCSV streams the same Table as text/csv via reports.WriteCSV.
//   - the shared params form (rendered on EVERY report page) carries the subsidiary
//     SCOPE selector (always, D18), plus as-of / period / granularity / target
//     currency controls gated by the report's ParamsSpec.
//
// Routes are auto-mounted per report in routes.go (GET /reports/{id} and
// /reports/{id}.csv), each gated by ReportGroup(report.Group); this handler derives
// WHICH report from the request path (the routes are concrete literals, so there is
// no {id} PathValue). An unknown id never matches a route (mux 404s); a defensive
// lookup here 404s too.

// reportFromPath resolves the Report the concrete route serves from the request
// path. The mounted patterns are literal "/reports/<id>" and "/reports/<id>.csv",
// so the id is the last path segment (minus a ".csv" suffix). Returns ok=false for
// an id not in the registry (defensive: the route only exists for a registered
// report, so this is unreachable in normal serving, but keeps the handler total).
func (s *server) reportFromPath(r *http.Request) (reports.Report, bool) {
	id := strings.TrimPrefix(r.URL.Path, "/reports/")
	id = strings.TrimSuffix(id, ".csv")
	return s.reports.Get(id)
}

// resolveParams builds the validated Params for a report run from the query string,
// applying the shared defaults (D18): scope defaults to the user's default
// subsidiary, else the root (full consolidation); as-of/period default to a sensible
// window; target currency defaults to the scope subsidiary's base currency. Only the
// params the report's spec declares are parsed from the query; the rest keep their
// zero value. It also returns the params-form model so the page can re-render the
// controls with the resolved (defaulted) values.
func (s *server) resolveParams(
	ctx context.Context, u *store.CurrentUser, rep reports.Report, q map[string][]string,
) (reports.Params, paramsForm, error) {
	rows, err := s.store.SubTree(ctx)
	if err != nil {
		return reports.Params{}, paramsForm{}, err
	}
	// Reduce the sqlc rows to a local, web-owned shape so the rest of this file
	// (and the form model) never names the store's generated type. SubTree is
	// pre-order, so subs[0] is the single root (D18).
	subs := make([]subInfo, len(rows))
	for i, r := range rows {
		subs[i] = subInfo{ID: r.ID, Name: r.Name, Base: r.BaseCurrency}
	}

	// Scope: query override -> user default -> root.
	scope := int64(0)
	if len(subs) > 0 {
		scope = subs[0].ID
	}
	if u != nil && u.DefaultSubsidiaryID != nil {
		scope = *u.DefaultSubsidiaryID
	}
	if v := first(q, "scope"); v != "" {
		if id := parseID(v); id != 0 && subExists(subs, id) {
			scope = id
		}
	}

	// Base currency of the resolved scope subsidiary (the target-currency default).
	base := ""
	for _, sub := range subs {
		if sub.ID == scope {
			base = sub.Base
			break
		}
	}

	p := reports.Params{Scope: scope, Lang: langOf(ctx)}
	df := dateFormatFor(u)
	today := s.now()

	if rep.ParamsSpec.AsOf {
		p.AsOf = resolveDate(first(q, "asof"), df, today)
	}
	if rep.ParamsSpec.Period {
		// Default period: year-to-date (Jan 1 of the current year .. today).
		yearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		p.From = resolveDate(first(q, "from"), df, yearStart)
		p.To = resolveDate(first(q, "to"), df, today)
	}
	if rep.ParamsSpec.Granularity {
		p.Granularity = reports.ParseGranularity(first(q, "granularity"))
	}
	if rep.ParamsSpec.Currency {
		p.TargetCurrency = base
		if v := first(q, "currency"); v != "" {
			p.TargetCurrency = v
		}
	}
	if rep.ParamsSpec.Detail {
		// Only "currency" turns detail on; any other value (incl. empty) is the
		// default converted-only view.
		if first(q, "detail") == "currency" {
			p.Detail = "currency"
		}
	}

	form, err := s.buildParamsForm(ctx, u, rep, p, subs)
	if err != nil {
		return reports.Params{}, paramsForm{}, err
	}
	return p, form, nil
}

// subInfo is the web-owned reduction of a subsidiary row the report params form
// needs: its id, display name, and base currency. Reducing the store's generated
// SubTree row to this keeps the generated type out of this file's signatures.
type subInfo struct {
	ID   int64
	Name string
	Base string
}

// resolveDate parses a query date value per the user's date format (ISO always
// accepted), falling back to fallback (a time.Time) rendered ISO when the value is
// empty or unparseable. The stored/param form is always ISO (YYYY-MM-DD), the
// canonical internal date form the store queries expect.
func resolveDate(v string, df money.DateFormat, fallback time.Time) string {
	if v != "" {
		if t, err := money.ParseDate(v, df); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return fallback.Format("2006-01-02")
}

// ---- params form model ----------------------------------------------------

// paramsForm is the shared report params form's render model. Scope is ALWAYS
// present (every report is scoped, D18); the other controls render only when the
// report's ParamsSpec declares them. Values are the RESOLVED (defaulted) params so
// the form shows what will run, and dates are formatted per the user's setting.
type paramsForm struct {
	// ReportID is the report slug, for the form's action and the CSV link.
	ReportID string

	// Scopes are the subsidiary options (id, name, selected) for the scope selector.
	Scopes []scopeOption

	// The gate flags mirror ParamsSpec so the template shows only relevant controls.
	ShowAsOf        bool
	ShowPeriod      bool
	ShowGranularity bool
	ShowCurrency    bool
	ShowDetail      bool

	// Resolved control values (formatted for display where dated).
	AsOf        string // user-formatted date
	From        string // user-formatted date
	To          string // user-formatted date
	Granularity string // token: none|month|quarter
	Currency    string // selected target currency code
	Detail      string // token: ""|currency (per-currency detail toggle)

	// Options for the selects.
	Currencies []ccyChoice
}

// scopeOption is one subsidiary choice in the scope selector.
type scopeOption struct {
	ID       int64
	Name     string
	Selected bool
}

// ccyChoice is one target-currency choice in the report currency selector (the
// existing currencyOption in accounts.go carries a name; the report select shows
// codes only, and marks the selected one).
type ccyChoice struct {
	Code     string
	Selected bool
}

// buildParamsForm assembles the params-form model for rep with the resolved params
// p. It lists every subsidiary as a scope option (marking the resolved scope), and
// -- when the report converts -- every ACTIVE currency as a target-currency option
// (marking the resolved target, defaulted to the scope base). Dates are formatted
// per the user's setting for the inputs (which are plain text, never
// input[type=date] -- rule 10 / rule 12).
func (s *server) buildParamsForm(
	ctx context.Context, u *store.CurrentUser, rep reports.Report,
	p reports.Params, subs []subInfo,
) (paramsForm, error) {
	df := dateFormatFor(u)

	f := paramsForm{
		ReportID:        rep.ID,
		ShowAsOf:        rep.ParamsSpec.AsOf,
		ShowPeriod:      rep.ParamsSpec.Period,
		ShowGranularity: rep.ParamsSpec.Granularity,
		ShowCurrency:    rep.ParamsSpec.Currency,
		ShowDetail:      rep.ParamsSpec.Detail,
		Granularity:     p.Granularity.String(),
		Currency:        p.TargetCurrency,
		Detail:          p.Detail,
	}
	for _, sub := range subs {
		f.Scopes = append(f.Scopes, scopeOption{
			ID: sub.ID, Name: sub.Name, Selected: sub.ID == p.Scope,
		})
	}
	if p.AsOf != "" {
		f.AsOf = money.FormatDate(parseISOForDisplay(p.AsOf), df)
	}
	if p.From != "" {
		f.From = money.FormatDate(parseISOForDisplay(p.From), df)
	}
	if p.To != "" {
		f.To = money.FormatDate(parseISOForDisplay(p.To), df)
	}
	if rep.ParamsSpec.Currency {
		curs, err := s.store.Currencies(ctx)
		if err != nil {
			return paramsForm{}, err
		}
		for _, c := range curs {
			if c.Active == 0 {
				continue
			}
			f.Currencies = append(f.Currencies, ccyChoice{
				Code: c.Code, Selected: c.Code == p.TargetCurrency,
			})
		}
	}
	return f, nil
}

// ---- rendered table model -------------------------------------------------

// reportPageModel is the report page's template model: the localized title, the
// params form, and the rendered table (localized headers + per-user-formatted
// cells). It also carries the CSV link so the page can offer the export.
type reportPageModel struct {
	Title   string
	Params  paramsForm
	Table   renderedTable
	CSVHref string
}

// renderedTable is a Table prepared for the HTML template: localized column headers
// with alignment, and rows whose cells are already display strings (money/date per
// the user's settings, text/labels localized). Keeping the render here (not in
// reports) means the reports package stays i18n/format-free and the same generic
// template serves every report.
type renderedTable struct {
	Columns []renderedColumn
	Rows    []renderedRow
}

type renderedColumn struct {
	Header string
	Right  bool // right-align hint (money columns)
}

type renderedRow struct {
	Cells    []renderedCell
	Indent   int
	Subtotal bool
	Total    bool
	Warning  bool
}

type renderedCell struct {
	Text  string
	Right bool // right-align (money)
	// Href, when non-empty, makes the cell a DRILL link (p15.3d): the HTML template
	// renders <a href="{Href}">{Text}</a> (a plain link, strict CSP -- no inline
	// handler). It is set for a drillable cell (Cell.Drill != nil), pointing at the
	// report's /reports/{id}/drill route with the encoded filter.
	Href string
}

// renderTable turns a reports.Table into the display-ready renderedTable for lang,
// formatting money/date cells per the user's settings (rule 10) and localizing
// column headers and LABEL cells (CellLabel carries an i18n key), while TEXT cells
// (proper nouns) render verbatim. The label/text distinction is EXPLICIT in the
// cell kind (not guessed from the string), so a report can never render a raw key
// or wrongly translate a stored name. Money cells use the per-user opts with the
// currency code prefixed (e.g. "USD 1,234.56") so mixed-currency tables stay
// unambiguous.
func renderTable(t reports.Table, reportID, lang string, opts money.FormatOpts, df money.DateFormat, exps map[string]int) renderedTable {
	var rt renderedTable
	for _, c := range t.Columns {
		rt.Columns = append(rt.Columns, renderedColumn{
			Header: i18n.T(lang, c.HeaderKey),
			Right:  c.Align == reports.AlignRight,
		})
	}
	for _, row := range t.Rows {
		rr := renderedRow{
			Indent:   row.Indent,
			Subtotal: row.Kind == reports.RowSubtotal,
			Total:    row.Kind == reports.RowTotal,
			Warning:  row.Kind == reports.RowWarning,
		}
		for _, cell := range row.Cells {
			rr.Cells = append(rr.Cells, renderCell(cell, reportID, lang, opts, df, exps))
		}
		rt.Rows = append(rt.Rows, rr)
	}
	return rt
}

// renderCell formats one typed cell to a display string. A cell carrying a Drill
// (p15.3d) also gets an Href to the report's /reports/{id}/drill route with the
// encoded filter, so the HTML renders the value as a plain link (strict CSP).
func renderCell(c reports.Cell, reportID, lang string, opts money.FormatOpts, df money.DateFormat, exps map[string]int) renderedCell {
	href := ""
	if c.Drill != nil {
		href = "/reports/" + reportID + "/drill?" + c.Drill.Encode()
	}
	switch c.Kind {
	case reports.CellMoney:
		if c.Blank {
			return renderedCell{Text: "", Right: true}
		}
		return renderedCell{
			Text:  c.Currency + " " + money.Format(c.Minor, exps[c.Currency], opts),
			Right: true,
			Href:  href,
		}
	case reports.CellDate:
		if c.Text == "" {
			return renderedCell{}
		}
		return renderedCell{Text: money.FormatDate(parseISOForDisplay(c.Text), df)}
	case reports.CellLabel:
		return renderedCell{Text: i18n.T(lang, c.Text)}
	default: // CellText -- a stored proper noun, verbatim
		return renderedCell{Text: c.Text}
	}
}

// ---- handlers -------------------------------------------------------------

// reportPage handles GET /reports/{id} (ReportGroup(group)): it resolves the params
// from the query (with shared defaults), runs the report over a fresh Toolkit, and
// renders the Table into the app shell via the generic report.tmpl. The params form
// (incl. the always-present scope selector) is re-rendered with the resolved values.
func (s *server) reportPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	rep, ok := s.reportFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	params, form, err := s.resolveParams(ctx, u, rep, r.Form)
	if err != nil {
		s.serverError(w)
		return
	}

	table, err := rep.Run(ctx, reports.NewToolkit(s.store, params), params)
	if err != nil {
		s.serverError(w)
		return
	}

	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	model := reportPageModel{
		Title:   i18n.T(lang, rep.TitleKey),
		Params:  form,
		Table:   renderTable(table, rep.ID, lang, formatOptsFor(u), dateFormatFor(u), exps),
		CSVHref: "/reports/" + rep.ID + ".csv?" + r.Form.Encode(),
	}
	s.render(w, r, http.StatusOK, "report.tmpl", s.newShellPage(r, model))
}

// reportCSV handles GET /reports/{id}.csv (same ReportGroup Perm as the HTML page):
// it runs the report with the same param resolution and streams the Table as
// text/csv via reports.WriteCSV, with headers localized to the request language and
// a download filename of the report id. The CSV values are machine-plain (no
// grouping, ISO dates) and correctly escaped by encoding/csv.
func (s *server) reportCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	rep, ok := s.reportFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	params, _, err := s.resolveParams(ctx, u, rep, r.Form)
	if err != nil {
		s.serverError(w)
		return
	}

	table, err := rep.Run(ctx, reports.NewToolkit(s.store, params), params)
	if err != nil {
		s.serverError(w)
		return
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}

	// The reports CSV writer is i18n-free (it emits a text cell's raw string), so
	// resolve LABEL cells to localized TEXT cells here before handing it off; proper-
	// noun TEXT cells and money/date cells are untouched. Headers are localized by
	// the localizer passed to WriteCSV.
	localized := localizeLabelCells(table, lang)

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", "attachment; filename=\""+rep.ID+".csv\"")
	h.Set("Cache-Control", "no-store")
	// Best-effort: a write error mid-stream has already sent a 200 header, so there
	// is no clean way to signal it; the export is a read with no side effects.
	_ = reports.WriteCSV(w, localized, func(key string) string { return i18n.T(lang, key) }, exps)
}

// localizeLabelCells returns a copy of t with each LABEL cell resolved to a
// localized TEXT cell (so the i18n-free CSV writer emits the localized string);
// proper-noun TEXT cells and money/date cells are unchanged. This is the CSV
// counterpart of renderCell's CellLabel branch -- one place each renderer localizes.
func localizeLabelCells(t reports.Table, lang string) reports.Table {
	out := reports.Table{Columns: t.Columns}
	out.Rows = make([]reports.Row, len(t.Rows))
	for i, row := range t.Rows {
		nr := reports.Row{Indent: row.Indent, Kind: row.Kind}
		nr.Cells = make([]reports.Cell, len(row.Cells))
		for j, c := range row.Cells {
			if c.Kind == reports.CellLabel {
				c = reports.TextCell(i18n.T(lang, c.Text))
			}
			nr.Cells[j] = c
		}
		out.Rows[i] = nr
	}
	return out
}

// ---- small helpers --------------------------------------------------------

// first returns the first value for key in a parsed form, or "".
func first(q map[string][]string, key string) string {
	if vs := q[key]; len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// subExists reports whether id is one of the subsidiaries (a query scope override
// must name a real subsidiary, else the default stands -- no arbitrary scope).
func subExists(subs []subInfo, id int64) bool {
	for _, s := range subs {
		if s.ID == id {
			return true
		}
	}
	return false
}
