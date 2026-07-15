package web

import (
	"net/http"

	"cuento/internal/money"
)

// Per-split description autocomplete + per-row prefill (p26.18). Step 4a of the
// payee->per-split-description migration: the SERVER backend the entry grids consume
// in 4b. Both routes are TxnWrite (they feed the transaction/expense ENTRY flow,
// matching the editor's write-gating -- they exist only to author an entry) and are
// picked up by the permission-matrix test automatically (rule 8).
//
// These REPLACED the old payee autofill: the whole-grid payee template became per-ROW
// /descriptions/prefill, and /descriptions/suggest is the distinct-description
// analogue of the removed payee suggest. The payee entity + its routes were physically
// removed in p26.20; the grid is wired to these per-split description endpoints.

// descSuggestModel carries the ranked distinct descriptions the desc-suggest
// fragment renders as <li> options.
type descSuggestModel struct {
	Suggestions []string
}

// descPrefillModel carries the one matched split's fields the desc-prefill fragment
// renders as data-* attributes. Amount is preformatted (signed, user's number
// format, the matched split's currency exponent). Found=false renders an empty
// element the client detects as "nothing to prefill".
type descPrefillModel struct {
	Found       bool
	AccountID   int64
	AmountInput string
	FundID      int64
	ProgramID   int64
	Class       string
	Memo        string
}

// descriptionsSuggest handles GET /descriptions/suggest?q=<text>&sub=<id>: the
// desc-suggest fragment listing up to 10 DISTINCT non-empty split descriptions that
// substring-match q, ranked most-recently-used first, preferring subsidiary sub. A
// blank/empty q renders nothing (the store returns no rows -> empty <ul>).
func (s *server) descriptionsSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query().Get("q")
	sub := parseID(r.URL.Query().Get("sub"))

	sugg, err := s.store.SuggestDescriptions(ctx, q, sub)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "desc-suggest", descSuggestModel{Suggestions: sugg})
}

// descriptionsPrefill handles GET /descriptions/prefill?q=<exact description>&sub=<id>:
// the desc-prefill fragment carrying the MOST-RECENT split with that exact
// description (preferring subsidiary sub) as data-* attributes for the client to
// apply to an otherwise-empty row. No match -> an empty (data-less) element. The
// amount is formatted in the MATCHED split's own transaction currency (the true
// scale of the stored minor units -- the endpoint has no in-progress-txn currency),
// mirroring payeeTemplate's display-only formatting; the editor re-validates the
// parsed amount on save.
//
// CONTRACT NOTE: the returned account/fund/program ids are the split's raw ids and
// may now be inactive or out of subsidiary sub. They are returned regardless -- the
// editor's p26.10 option injection + save-time guard handle display/validation. The
// client applies them only when the row is otherwise empty (4b).
func (s *server) descriptionsPrefill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	q := r.URL.Query().Get("q")
	sub := parseID(r.URL.Query().Get("sub"))

	pf, err := s.store.PrefillDescription(ctx, q, sub)
	if err != nil {
		s.serverError(w)
		return
	}
	model := descPrefillModel{Found: pf.Found}
	if pf.Found {
		exp := s.currencyExponent(ctx, pf.Currency)
		model.AccountID = pf.AccountID
		model.AmountInput = money.Format(pf.Amount, exp, money.FormatOpts{Number: numberFormatFor(u)})
		model.FundID = pf.FundID
		model.ProgramID = pf.ProgramID
		model.Class = pf.Class
		model.Memo = pf.Memo
	}
	s.render(w, r, http.StatusOK, "desc-prefill", model)
}
