package web

import (
	"context"
	"database/sql"
	"net/http"

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

// histDiffView is one rendered field change: the localized field label + the old/new
// display strings. Empty Old (a create/added field) or empty New (a removed field)
// renders as an addition/removal in the template.
type histDiffView struct {
	Label string // i18n label KEY (rendered via {{t}})
	Old   string // display string ("" = none/empty side)
	New   string
}

// histSplitView is one split's change within a timeline entry: its op (create/
// update/delete -> an i18n op label), a 1-based line number, and the per-field diffs.
type histSplitView struct {
	OpKey string // i18n key: history.split.added | .changed | .removed
	Line  int
	Diffs []histDiffView
}

// histEntryView is one change in the rendered timeline: actor, formatted date, the
// op label, header field diffs, and split diffs.
type histEntryView struct {
	Actor       string
	Date        string // formatted per the user's date format (rule 10)
	OpKey       string // i18n key: history.op.created | .updated | .voided
	HeaderDiffs []histDiffView
	SplitDiffs  []histSplitView
}

// historyPageModel is the GET model for the history page.
type historyPageModel struct {
	TxnID   int64
	Entries []histEntryView
}

// txnHistory handles GET /transactions/{id}/history (TxnRead). It renders the change
// timeline reconstructed from versions; a nonexistent txn (no version rows) 404s.
func (s *server) txnHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)
	id := parseID(r.PathValue("id"))

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
	model := historyPageModel{TxnID: id, Entries: view}
	s.render(w, r, http.StatusOK, "history.tmpl", s.newShellPage(r, model))
}

// renderHistory turns the store's structured diffs into display view models: entity
// ids -> names (rule 9), amounts -> formatted strings (rule 10), fields -> i18n label
// keys. The name maps are loaded ONCE per render (tiny sets), reusing the register's
// helpers.
func (s *server) renderHistory(ctx context.Context, u *store.CurrentUser, lang string, entries []store.HistoryEntry) ([]histEntryView, error) {
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
	payees, err := payeeNameMap(ctx, s.store)
	if err != nil {
		return nil, err
	}
	programs, err := programNameMap(ctx, s.store)
	if err != nil {
		return nil, err
	}

	// The txn is single-currency (D3); take the currency from the last header snapshot
	// carrying one so amounts format with the right exponent.
	currency := lastCurrency(entries)
	exp := s.currencyExponent(ctx, currency)
	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	// resolve turns one DiffValue into its display string given its field.
	resolve := func(field store.HistoryField, v store.DiffValue) string {
		switch field {
		case store.FieldAmount:
			// A zero-value DiffValue (empty side) renders as "" so a create/removal
			// shows one-sided; a real amount is always non-zero (schema CHECK).
			if v.Amount == 0 {
				return ""
			}
			return currency + " " + money.Format(v.Amount, exp, opts)
		case store.FieldSubsidiary:
			return nameOr(subs, v.ID)
		case store.FieldPayee:
			return nameOr(payees, v.ID)
		case store.FieldAccount:
			return nameOr(accounts, v.ID)
		case store.FieldFund:
			// An invalid id means the Unrestricted group (fund NULL); render its label.
			if !v.ID.Valid {
				return ""
			}
			return funds[v.ID.Int64]
		case store.FieldProgram:
			return nameOr(programs, v.ID)
		default: // date / memo / currency / functional_class -> the stored text
			return v.Text
		}
	}

	views := make([]histEntryView, 0, len(entries))
	for _, e := range entries {
		ev := histEntryView{
			Actor: e.ActorName,
			Date:  money.FormatDate(parseISOForDisplay(dateOnly(e.At)), df),
			OpKey: histOpKey(e.Op),
		}
		for _, d := range e.HeaderDiffs {
			ev.HeaderDiffs = append(ev.HeaderDiffs, histDiffView{
				Label: histFieldLabel(d.Field),
				Old:   resolve(d.Field, d.Old),
				New:   resolve(d.Field, d.New),
			})
		}
		for _, sd := range e.SplitDiffs {
			sv := histSplitView{OpKey: histSplitOpKey(sd.Op), Line: int(sd.Position) + 1}
			for _, d := range sd.Fields {
				sv.Diffs = append(sv.Diffs, histDiffView{
					Label: histFieldLabel(d.Field),
					Old:   resolve(d.Field, d.Old),
					New:   resolve(d.Field, d.New),
				})
			}
			ev.SplitDiffs = append(ev.SplitDiffs, sv)
		}
		views = append(views, ev)
	}
	return views, nil
}

// nameOr returns the mapped name for a valid id, else "" (an unset/invalid id --
// e.g. the Unrestricted fund, which callers handle before reaching here).
func nameOr(m map[int64]string, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return m[id.Int64]
}

// lastCurrency returns the currency of the latest header diff that set one, defaulting
// to USD when none is present (a splits-only change with no header currency).
func lastCurrency(entries []store.HistoryEntry) string {
	cur := "USD"
	for _, e := range entries {
		for _, d := range e.HeaderDiffs {
			if d.Field == store.FieldCurrency {
				if d.New.Text != "" {
					cur = d.New.Text
				} else if d.Old.Text != "" {
					cur = d.Old.Text
				}
			}
		}
	}
	return cur
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
		m[p.ID] = p.Name
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

// histSplitOpKey maps a split op to its i18n label key.
func histSplitOpKey(op string) string {
	switch op {
	case "create":
		return "history.split.added"
	case "delete":
		return "history.split.removed"
	default:
		return "history.split.changed"
	}
}

// histFieldLabel maps a diff field to its i18n label key.
func histFieldLabel(f store.HistoryField) string {
	switch f {
	case store.FieldDate:
		return "history.field.date"
	case store.FieldSubsidiary:
		return "history.field.subsidiary"
	case store.FieldPayee:
		return "history.field.payee"
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
	default: // FieldMemo / FieldSplitMemo
		return "history.field.memo"
	}
}
