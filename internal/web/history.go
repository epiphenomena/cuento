package web

import (
	"context"
	"database/sql"
	"net/http"

	"cuento/internal/i18n"
	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p12.4 transaction history panel (GET /transactions/{id}/history, TxnRead). The
// timeline is reconstructed from the append-only version twins by the store
// (store.TransactionHistory), which returns STRUCTURED diffs (typed old/new values).
// This handler is the render layer: it resolves entity ids to names (rule 9, in the
// request language), formats amounts through the money formatter honoring the user's
// settings (rule 10), and maps each HistoryField to an i18n label key. Every string
// is a catalog key or a stored proper noun; no inline script (rule 12).
//
// CRITICAL: unlike edit/duplicate, history must render for a VOIDED transaction --
// store.TransactionHistory loads from the version rows (present regardless of the
// live deleted flag) and only 404s when the txn never existed. So this handler must
// NOT pre-guard with GetTransaction (which hides a soft-deleted txn).

// The history page is a vertical timeline of SAVED-STATE cards (first -> current).
// Each card shows the FULL transaction state at that version -- the header fields and
// the split rows as they stood -- with the CHANGES from the prior version highlighted:
// added (green, "+"), removed (red, struck, "-"), changed (amber, "~"). Color is never
// the only signal: every changed row/field carries a +/-/~ glyph and a status label.

// histFieldView is one header field row in a state card: its label, current value, and
// -- when it changed this version -- the marker and the prior value (shown struck).
type histFieldView struct {
	Label   string // i18n label KEY (rendered via {{t}})
	Value   string // display string of the value at THIS version
	Changed bool   // true when this field changed vs the prior version
	Old     string // prior value (display string), shown only when Changed
}

// histSplitRowView is one split line in a state card's split table, as it stood at this
// version, with its change status overlaid so the row can be highlighted.
//   - Status: "" unchanged | "create" added | "update" changed | "delete" removed(ghost)
//   - MarkerKey / StatusKey: i18n keys for the +/-/~ glyph label and the status word.
//   - Each cell (Account/Amount/Fund/Program/Class/Memo/Description) is a display
//     string; a Changed* flag + Old* prior value drives the per-cell emphasis on an
//     update so the eye lands on exactly the cell that moved.
type histSplitRowView struct {
	Line      int
	Status    string // "" | "create" | "update" | "delete"
	MarkerKey string // history.marker.added|.removed|.changed|.unchanged (accessible label)
	StatusKey string // history.split.added|.removed|.changed (visible status word; "" if unchanged)

	Account      string
	Amount       string
	Fund         string
	Program      string
	Class        string
	Memo         string
	Description  string
	ChangedCells map[string]*histCellDelta // field name -> prior value, for changed cells (update only)
}

// histCellDelta carries the prior display value of one changed cell (for the struck old
// value shown inline next to the new value on an updated row).
type histCellDelta struct {
	Old string
}

// histSplitCell is the template-side pairing of a split-table cell's value with whether
// that field changed on the row (and its prior value). makeHistSplitCell builds it from
// the row's ChangedCells map so the template needn't distinguish absent from empty.
type histSplitCell struct {
	Val     string
	Changed bool
	Old     string
}

// makeHistSplitCell is the `splitCell` template func: pair a cell value with the row's
// delta for that field (a nil *histCellDelta means unchanged this version -- the `index`
// on the ChangedCells map yields nil for an absent key).
func makeHistSplitCell(val string, delta *histCellDelta) histSplitCell {
	c := histSplitCell{Val: val}
	if delta != nil {
		c.Changed = true
		c.Old = delta.Old
	}
	return c
}

// histVersionView is one SAVED-STATE card: the version heading (op, actor, date, and a
// sequence label -- Initial / Current / #N), the full header field set, and the full
// split row set at this version.
type histVersionView struct {
	OpKey  string // i18n key: history.op.created | .updated | .voided
	Actor  string
	Date   string // formatted per the user's date format (rule 10)
	SeqKey string // history.seq.initial | .current | "" (a middle version)
	SeqNum int    // 1-based version number (shown for middle versions)
	Voided bool   // header is in the deleted state (void card)
	Fields []histFieldView
	Splits []histSplitRowView
}

// historyPageModel is the GET model for the history page.
type historyPageModel struct {
	TxnID    int64
	Versions []histVersionView
}

// txnHistory handles GET /transactions/{id}/history (TxnRead). It renders the change
// timeline reconstructed from versions; a nonexistent txn (no version rows) 404s.
func (s *server) txnHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := ids.TransactionID(parseID(r.PathValue("id")))

	entries, err := s.store.TransactionHistory(ctx, id)
	if err != nil {
		http.NotFound(w, r) // ErrTransactionNotFound -> 404; other errors -> also 404 (no txn to show)
		return
	}

	view, err := s.renderHistory(ctx, u, lang, entries)
	if err != nil {
		s.serverError(w)
		return
	}
	model := historyPageModel{TxnID: int64(id), Versions: view}
	s.render(w, r, http.StatusOK, "history.tmpl", s.newShellPage(r, model))
}

// renderHistory turns the store's structured diffs into display view models: entity
// ids -> names (rule 9), amounts -> formatted strings (rule 10), fields -> i18n label
// keys. The name maps are loaded ONCE per render (tiny sets), reusing the register's
// helpers.
func (s *server) renderHistory(ctx context.Context, u *store.CurrentUser, lang string, entries []store.HistoryEntry) ([]histVersionView, error) {
	accounts, err := accountNameMap(ctx, s.store, lang)
	if err != nil {
		return nil, err
	}
	funds, err := fundNameMap(ctx, s.store)
	if err != nil {
		return nil, err
	}
	subs, err := subNameMap(ctx, s.store)
	if err != nil {
		return nil, err
	}
	programs, err := programNameMap(ctx, s.store)
	if err != nil {
		return nil, err
	}

	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	// r groups the per-value display helpers for one render, keyed by the version's own
	// currency so amounts format with the right exponent (the txn is single-currency,
	// D3, but currency can itself be edited across versions).
	r := histResolver{lang: lang, df: df, accounts: accounts, funds: funds, subs: subs, programs: programs, opts: opts}

	views := make([]histVersionView, 0, len(entries))
	for i, e := range entries {
		st := e.State
		cur := st.Header.Currency
		if cur == "" {
			cur = "USD"
		}
		exp := s.currencyExponent(ctx, cur)

		vv := histVersionView{
			OpKey:  histOpKey(e.Op),
			Actor:  e.ActorName,
			Date:   money.FormatDate(parseISOForDisplay(dateOnly(e.At)), df),
			Voided: st.Header.Deleted,
		}
		switch {
		case i == 0:
			vv.SeqKey = "history.seq.initial"
		case i == len(entries)-1:
			vv.SeqKey = "history.seq.current"
		default:
			vv.SeqNum = i + 1
		}

		// Header field rows: the FULL header at this version, each flagged changed when
		// it moved this version (from the header diffs, keyed by field).
		changedHdr := headerDiffByField(e.HeaderDiffs, i == 0)
		vv.Fields = r.headerFields(st.Header, cur, exp, changedHdr)

		// Split rows: the FULL live split set at this version, each with its change
		// status overlaid. On the FIRST version the rows are the initial baseline, not
		// "added" -- render them neutral (DECISIONS p29.16).
		for _, sp := range st.Splits {
			vv.Splits = append(vv.Splits, r.splitRow(sp, cur, exp, i == 0))
		}
		views = append(views, vv)
	}
	return views, nil
}

// histResolver holds the per-render name maps + format options so the field/split
// builders can turn structured state into display strings (rules 9/10).
type histResolver struct {
	lang     string
	df       money.DateFormat
	accounts map[ids.AccountID]string
	funds    map[ids.FundID]string
	subs     map[int64]string
	programs map[int64]string
	opts     money.FormatOpts
}

// headerDiffByField indexes a version's header diffs by field, and returns nil for the
// first version (its "diffs" are just the initial values, not changes to highlight).
func headerDiffByField(diffs []store.FieldDiff, first bool) map[store.HistoryField]store.FieldDiff {
	if first {
		return nil
	}
	m := make(map[store.HistoryField]store.FieldDiff, len(diffs))
	for _, d := range diffs {
		m[d.Field] = d
	}
	return m
}

// headerFields builds the full header field rows for one version, marking each field
// changed (with its prior value) when the version's diffs touched it.
func (r histResolver) headerFields(h store.HistHeaderState, cur string, exp int, changed map[store.HistoryField]store.FieldDiff) []histFieldView {
	fields := []struct {
		field store.HistoryField
		value string
	}{
		{store.FieldDate, money.FormatDate(parseISOForDisplay(h.Date), r.df)},
		{store.FieldSubsidiary, r.subs[int64(h.SubsidiaryID)]},
		{store.FieldCurrency, h.Currency},
		{store.FieldMemo, h.Memo},
		{store.FieldNotes, h.Notes},
	}
	out := make([]histFieldView, 0, len(fields))
	for _, f := range fields {
		// Skip empty optional free-text (memo/notes) unless it changed this version.
		d, isChanged := changed[f.field]
		if f.value == "" && !isChanged && (f.field == store.FieldMemo || f.field == store.FieldNotes) {
			continue
		}
		fv := histFieldView{Label: histFieldLabel(f.field), Value: f.value, Changed: isChanged}
		if isChanged {
			fv.Old = r.resolveValue(f.field, d.Old, cur, exp)
		}
		out = append(out, fv)
	}
	return out
}

// splitRow builds one split-row view for a version, resolving its cells and (on an
// update) the prior value of each changed cell. first suppresses the "added" status on
// the initial-baseline version.
func (r histResolver) splitRow(sp store.HistSplitState, cur string, exp int, first bool) histSplitRowView {
	status := sp.Status
	if first && status == "create" {
		status = "" // initial baseline: neutral, not "added"
	}
	// Guard: an "update" with no changed fields would render as a spurious "changed"
	// (amber) row with nothing to point at. The store today only versions a split that
	// actually changed (splitUnchanged skips no-op splits), so this never fires; keep it
	// as cheap insurance so a future re-version-everything change can't paint every row
	// amber. Downgrade to neutral.
	if status == "update" && len(sp.ChangedFields) == 0 {
		status = ""
	}
	row := histSplitRowView{
		Line:        int(sp.Position) + 1,
		Status:      status,
		MarkerKey:   histMarkerKey(status),
		StatusKey:   histSplitStatusKey(status),
		Account:     r.accounts[sp.AccountID],
		Amount:      money.FormatMoney(sp.Amount, cur, exp, r.opts),
		Fund:        r.fundName(sp.FundID),
		Program:     r.programName(sp.ProgramID),
		Class:       r.classLabel(sp.FunctionalClass),
		Memo:        sp.Memo,
		Description: sp.Description,
	}
	if status == "update" && len(sp.ChangedFields) > 0 {
		row.ChangedCells = make(map[string]*histCellDelta, len(sp.ChangedFields))
		for _, d := range sp.ChangedFields {
			row.ChangedCells[string(d.Field)] = &histCellDelta{Old: r.resolveValue(d.Field, d.Old, cur, exp)}
		}
	}
	return row
}

// resolveValue turns one DiffValue into its display string given its field (used for the
// struck "old" value on changed header fields and split cells).
func (r histResolver) resolveValue(field store.HistoryField, v store.DiffValue, cur string, exp int) string {
	switch field {
	case store.FieldAmount:
		if v.Amount == 0 {
			return ""
		}
		return money.FormatMoney(v.Amount, cur, exp, r.opts)
	case store.FieldSubsidiary:
		return nameOr(r.subs, v.ID)
	case store.FieldAccount:
		return nameOr(r.accounts, v.ID)
	case store.FieldFund:
		return r.fundName(v.ID)
	case store.FieldProgram:
		return nameOr(r.programs, v.ID)
	case store.FieldFunctional:
		return r.classLabel(sql.NullString{String: v.Text, Valid: v.Text != ""})
	default: // date / memo / currency / notes / description -> stored text
		return v.Text
	}
}

// fundName resolves a nullable fund id to its name; an invalid id is the Unrestricted
// group (fund NULL) and renders "" (the template shows a dash).
func (r histResolver) fundName(id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return r.funds[ids.FundID(id.Int64)]
}

// programName resolves a nullable program id to its name ("" for none).
func (r histResolver) programName(id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return r.programs[id.Int64]
}

// classLabel renders a nullable functional class through its i18n label in the request
// language ("" for none). The class value ("program"/"management"/"fundraising") keys
// the functional.<class> catalog entry (rule 9).
func (r histResolver) classLabel(c sql.NullString) string {
	if !c.Valid || c.String == "" {
		return ""
	}
	return i18n.T(r.lang, "functional."+c.String)
}

// nameOr returns the mapped name for a valid id, else "" (an unset/invalid id --
// e.g. the Unrestricted fund, which callers handle before reaching here).
func nameOr[K ~int64](m map[K]string, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return m[K(id.Int64)]
}

// dateOnly trims an RFC3339Nano timestamp to its YYYY-MM-DD prefix so the money date
// formatter (which parses ISO dates) can render it in the user's format. The time-of-
// day is not shown (the timeline order disambiguates same-day edits; DECISIONS p12.4).
func dateOnly(at string) string {
	if len(at) >= 10 {
		return at[:10]
	}
	return at
}

// programNameMap returns id->name for every program (active and inactive; a diff may
// name a since-deactivated program).
func programNameMap(ctx context.Context, st *store.Store) (map[int64]string, error) {
	progs, err := st.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(progs))
	for _, p := range progs {
		m[int64(p.ID)] = p.Name
	}
	return m, nil
}

// --- i18n key maps --------------------------------------------------------

// histOpKey maps a header op to its timeline entry i18n label key.
func histOpKey(op string) string {
	switch op {
	case "create":
		return "history.op.created"
	case "delete":
		return "history.op.voided"
	default:
		return "history.op.updated"
	}
}

// histSplitStatusKey maps a split-row change status to its visible status-word i18n key
// (paired with the marker glyph). An unchanged row has no status word ("").
func histSplitStatusKey(status string) string {
	switch status {
	case "create":
		return "history.split.added"
	case "delete":
		return "history.split.removed"
	case "update":
		return "history.split.changed"
	default:
		return ""
	}
}

// histMarkerKey maps a change status to the i18n key for its +/-/~ marker's accessible
// label (the non-color signal, rule: color is never the only cue).
func histMarkerKey(status string) string {
	switch status {
	case "create":
		return "history.marker.added"
	case "delete":
		return "history.marker.removed"
	case "update":
		return "history.marker.changed"
	default:
		return "history.marker.unchanged"
	}
}

// histFieldLabel maps a diff field to its i18n label key.
func histFieldLabel(f store.HistoryField) string {
	switch f {
	case store.FieldDate:
		return "history.field.date"
	case store.FieldSubsidiary:
		return "history.field.subsidiary"
	case store.FieldCurrency:
		return "history.field.currency"
	case store.FieldAccount:
		return "history.field.account"
	case store.FieldAmount:
		return "history.field.amount"
	case store.FieldFund:
		return "history.field.fund"
	case store.FieldProgram:
		return "history.field.program"
	case store.FieldFunctional:
		return "history.field.functional_class"
	case store.FieldNotes:
		return "history.field.notes"
	case store.FieldSplitDescription:
		return "history.field.description"
	default: // FieldMemo / FieldSplitMemo
		return "history.field.memo"
	}
}
