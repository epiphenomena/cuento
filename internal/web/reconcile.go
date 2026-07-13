package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"cuento/internal/money"
	"cuento/internal/reports"
	"cuento/internal/store"
)

// p16.3 reconciliation workspace (D13/D20). The bank-statement reconciliation UI:
// a LIST of reconcilable accounts (prior-statement prefill) -> a WORKSPACE for one
// OPEN recon (the account's uncleared + this-recon splits, each with a cleared
// TOGGLE, and a sticky cleared/difference summary). The store (p16.2) owns the
// lifecycle + all validation; this file only renders and translates typed errors.
//
// URL scheme (documented in DECISIONS p16.3):
//   GET  /reconciliations                              (TxnRead)  list
//   POST /reconciliations                              (TxnWrite) start a recon
//   GET  /reconciliations/{id}                         (TxnRead)  workspace
//   POST /reconciliations/{id}/splits/{sid}/toggle     (TxnWrite) clear/unclear a split
//   POST /reconciliations/{id}/finalize                (TxnWrite) finalize (zero-diff)
//   POST /reconciliations/{id}/reopen                  (TxnWrite) reopen a finalized recon
//
// The TOGGLE is the p16.3 anti-jank requirement: it posts to the TxnWrite toggle
// action, flips SetSplitReconciled, and returns a PARTIAL -- the flipped <tr>
// (hx-target the row) PLUS an out-of-band swap of the sticky #recon-summary (the
// cleared total + difference chip + the enable/disable state of Finalize). NOTHING
// else on the page moves, so scroll + focus are preserved (Appendix C). The toggle
// is a NATIVE <button> with a stable id (recon-toggle-<splitID>): Space activates a
// focused button natively (the "Space toggles the focused row" affordance, CSP-safe,
// no ES module), and htmx restores focus to the same-id element after the swap so
// repeated Space keeps working.
//
// Finalize-disabled-until-zero is a SERVER-authoritative UI aid: the disabled state
// is computed from ReconciliationSummaryFor (which reuses Finalize's own opening +
// cleared queries), so the button is enabled iff Finalize would accept. A client may
// POST anyway; the store rejects a nonzero finalize with ErrReconciliationDifference,
// which the handler maps to a clean 422 (never a 500).

// ---- URL helpers (also used by the tests) --------------------------------

func reconWorkspacePath(id int64) string {
	return "/reconciliations/" + strconv.FormatInt(id, 10)
}

func reconTogglePath(reconID, splitID int64) string {
	return "/reconciliations/" + strconv.FormatInt(reconID, 10) +
		"/splits/" + strconv.FormatInt(splitID, 10) + "/toggle"
}

func reconFinalizePath(id int64) string {
	return "/reconciliations/" + strconv.FormatInt(id, 10) + "/finalize"
}

func reconReopenPath(id int64) string {
	return "/reconciliations/" + strconv.FormatInt(id, 10) + "/reopen"
}

// reconToggleID is the stable DOM id of a split's cleared toggle button (also the
// focus-restore anchor across the htmx swap).
func reconToggleID(splitID int64) string {
	return "recon-toggle-" + strconv.FormatInt(splitID, 10)
}

// reconRowID is the stable DOM id of a workspace split row (the toggle's hx-target).
func reconRowID(splitID int64) string {
	return "recon-row-" + strconv.FormatInt(splitID, 10)
}

// ---- LIST -----------------------------------------------------------------

// reconAccountRow is one reconcilable account on the list: its name + statement
// currency, the last finalized statement (date + balance, the opening prefill), and
// -- when one exists -- an OPEN recon to continue.
type reconAccountRow struct {
	AccountID      int64
	AccountName    string
	Currency       string
	LastDate       string // formatted last finalized statement date ("" = none yet)
	LastBalanceFmt string // formatted prior finalized statement balance (the opening prefill)
	OpenReconID    int64  // >0 => an open recon exists; link to its workspace

	// StartForm is the empty per-account start form (rendered only when no open
	// recon exists). It carries the account id + currency the POST needs.
	StartForm reconStartForm

	// History is the account's FINALIZED reconciliations (p16.4), newest first -- the
	// audit trail of completed statements, each linking to its statement report. Empty
	// for an account with none.
	History []reconHistoryRow
}

// reconHistoryRow is one FINALIZED reconciliation on the history list (p16.4): its
// statement date + balance, currency, the finalized-at date, and the link to its
// statement report.
type reconHistoryRow struct {
	ReconID       int64
	StatementDate string // formatted statement date
	BalanceFmt    string // formatted statement balance (currency-prefixed)
	Currency      string
	FinalizedDate string // formatted finalized-at date (the date portion of the audit timestamp)
	ReportHref    string // /reports/reconciliation_statement?reconciliation={id}
}

// reconListModel is the GET /reconciliations model: the reconcilable-account rows.
type reconListModel struct {
	Rows []reconAccountRow
}

// reconStatementReportHref builds the statement report URL for a finalized recon
// (p16.4): the report's /reports/{id} route with the reconciliation id param.
func reconStatementReportHref(reconID int64) string {
	return "/reports/" + reports.ReconciliationStatementReportID +
		"?reconciliation=" + strconv.FormatInt(reconID, 10)
}

// reconList handles GET /reconciliations (TxnRead): every reconcilable account with
// its last finalized statement (opening prefill) and any open recon to continue.
func (s *server) reconList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	accts, err := s.store.ReconcilableAccounts(ctx)
	if err != nil {
		s.serverError(w)
		return
	}
	exps, err := currencyExponents(ctx, s.store)
	if err != nil {
		s.serverError(w)
		return
	}
	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	model := reconListModel{}
	for _, a := range accts {
		row := reconAccountRow{
			AccountID:   a.ID,
			AccountName: s.accountName(ctx, a.ID, lang),
			Currency:    a.DefaultCurrency,
		}
		recs, err := s.store.ReconciliationsForAccount(ctx, a.ID)
		if err != nil {
			s.serverError(w)
			return
		}
		// Find the latest finalized recon in the account's default currency (the
		// opening prefill) and any open recon in that currency (the continue link).
		for _, rc := range recs {
			if rc.Currency != a.DefaultCurrency {
				continue
			}
			if rc.Status == "open" && row.OpenReconID == 0 {
				row.OpenReconID = rc.ID
			}
			if rc.Status == "finalized" && row.LastDate == "" {
				row.LastDate = money.FormatDate(parseISOForDisplay(rc.StatementDate), df)
				row.LastBalanceFmt = rc.Currency + " " + money.Format(rc.StatementBalance, exps[rc.Currency], opts)
			}
		}
		row.StartForm = reconStartForm{
			AccountID:   a.ID,
			AccountName: row.AccountName,
			Currency:    a.DefaultCurrency,
		}

		// History (p16.4): every FINALIZED reconciliation on this account (both
		// currencies), newest first, each linking to its statement report.
		fins, err := s.store.FinalizedReconciliationsForAccount(ctx, a.ID)
		if err != nil {
			s.serverError(w)
			return
		}
		for _, fr := range fins {
			finDate := ""
			if fr.FinalizedAt != "" {
				finDate = money.FormatDate(parseISOForDisplay(dateOnly(fr.FinalizedAt)), df)
			}
			row.History = append(row.History, reconHistoryRow{
				ReconID:       fr.ID,
				StatementDate: money.FormatDate(parseISOForDisplay(fr.StatementDate), df),
				BalanceFmt:    fr.Currency + " " + money.Format(fr.StatementBalance, exps[fr.Currency], opts),
				Currency:      fr.Currency,
				FinalizedDate: finDate,
				ReportHref:    reconStatementReportHref(fr.ID),
			})
		}

		model.Rows = append(model.Rows, row)
	}
	s.render(w, r, http.StatusOK, "reconciliations.tmpl", s.newShellPage(r, model))
}

// ---- START ----------------------------------------------------------------

// reconStartForm is the model for a 422 re-render of the (per-account) start form.
type reconStartForm struct {
	AccountID    int64
	AccountName  string
	Currency     string
	StatementDay string // echoed statement date input
	Balance      string // echoed balance input
	Errors       formErrors
}

// reconStart handles POST /reconciliations (TxnWrite): start a new reconciliation on
// (account, currency) with the user-entered statement date + ending balance. The
// balance is parsed via the money parser honoring the user's number format (rule 10);
// the date via the money date parser (rule 10). Both go through the p10.3 form-error
// convention (422 + the "recon-start-form" partial + i18n key + autofocus) on bad
// input or a rejected StartReconciliation.
func (s *server) reconStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	accountID := parseID(r.PostFormValue("account_id"))
	currency := r.PostFormValue("currency")
	dayStr := r.PostFormValue("statement_date")
	balStr := r.PostFormValue("balance")

	form := reconStartForm{
		AccountID:    accountID,
		AccountName:  s.accountName(ctx, accountID, lang),
		Currency:     currency,
		StatementDay: dayStr,
		Balance:      balStr,
	}

	df := dateFormatFor(u)
	day, derr := money.ParseDate(dayStr, df)
	if derr != nil {
		form.Errors.add("statement_date", "error.recon.bad_date")
		s.renderFormError(w, r, "recon-start-form", form)
		return
	}
	exp := s.currencyExponent(ctx, currency)
	bal, berr := money.Parse(balStr, exp, numberFormatFor(u))
	if berr != nil {
		form.Errors.add("balance", "error.recon.bad_balance")
		s.renderFormError(w, r, "recon-start-form", form)
		return
	}

	id, err := s.store.StartReconciliation(s.actorCtx(ctx), accountID, currency, day.Format("2006-01-02"), bal)
	if err != nil {
		field, key := reconStartErrorField(err)
		if key == "" {
			s.serverError(w)
			return
		}
		form.Errors.add(field, key)
		s.renderFormError(w, r, "recon-start-form", form)
		return
	}
	redirectAfterForm(w, r, reconWorkspacePath(id))
}

// reconStartErrorField maps a StartReconciliation typed error to a (field, i18n key)
// pair. An unrecognized error is a real server fault (returns "").
func reconStartErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrNotReconcilable):
		return "account_id", "error.recon.not_reconcilable"
	case errors.Is(err, store.ErrReconciliationCurrency):
		return "currency", "error.recon.currency"
	case errors.Is(err, store.ErrOpenReconciliationExists):
		return "statement_date", "error.recon.open_exists"
	case errors.Is(err, store.ErrBadDate):
		return "statement_date", "error.recon.bad_date"
	default:
		return "", ""
	}
}

// ---- WORKSPACE ------------------------------------------------------------

// reconSplitRow is one workspace split line: date/payee/memo/fund-chip/amount plus
// its cleared state and the stable toggle/row ids.
type reconSplitRow struct {
	SplitID   int64
	RowID     string
	ToggleID  string
	Date      string
	PayeeName string
	Memo      string
	FundName  string
	AmountFmt string
	Cleared   bool
}

// reconSummary is the sticky cleared/difference summary. Finalizable is the
// server-authoritative enable state (difference == 0), driving the Finalize button's
// disabled attribute.
type reconSummary struct {
	StatementFmt  string
	OpeningFmt    string
	ClearedFmt    string
	DifferenceFmt string
	Finalizable   bool
}

// reconWorkspaceModel is the GET workspace model: the recon header, its split rows,
// the summary, and whether the recon is finalized (read-only + a Reopen action).
type reconWorkspaceModel struct {
	ReconID     int64
	AccountName string
	Currency    string
	Finalized   bool

	Rows    []reconSplitRow
	Summary reconSummary

	TogglePathBase string // "/reconciliations/{id}/splits" (row builds the rest)
	FinalizePath   string
	ReopenPath     string
}

// reconWorkspace handles GET /reconciliations/{id} (TxnRead): the workspace for one
// recon -- its splits (each with a cleared toggle) + the sticky summary. A finalized
// recon renders read-only with a Reopen action (this also serves the "the finalized
// recon shows" post-finalize view; p16.4 owns the statement report + history).
func (s *server) reconWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	recon, err := s.store.GetReconciliation(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	model, err := s.buildWorkspace(ctx, recon.ID)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "reconcile.tmpl", s.newShellPage(r, model))
}

// buildWorkspace assembles the workspace model for reconID: the split rows (names +
// formatting resolved), the summary, and the finalized state. Shared by the full
// page GET and the toggle partial so the two never drift.
func (s *server) buildWorkspace(ctx context.Context, reconID int64) (reconWorkspaceModel, error) {
	u := currentUser(ctx)
	lang := langOf(ctx)

	recon, err := s.store.GetReconciliation(ctx, reconID)
	if err != nil {
		return reconWorkspaceModel{}, err
	}
	splits, err := s.store.ReconciliationWorkspaceSplits(ctx, reconID)
	if err != nil {
		return reconWorkspaceModel{}, err
	}
	sum, err := s.store.ReconciliationSummaryFor(ctx, reconID)
	if err != nil {
		return reconWorkspaceModel{}, err
	}

	payees, err := payeeNameMap(ctx, s.store)
	if err != nil {
		return reconWorkspaceModel{}, err
	}
	funds, err := fundNameMap(ctx, s.store)
	if err != nil {
		return reconWorkspaceModel{}, err
	}
	exp := s.currencyExponent(ctx, recon.Currency)
	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	model := reconWorkspaceModel{
		ReconID:        reconID,
		AccountName:    s.accountName(ctx, recon.AccountID, lang),
		Currency:       recon.Currency,
		Finalized:      recon.Status == "finalized",
		TogglePathBase: "/reconciliations/" + strconv.FormatInt(reconID, 10) + "/splits",
		FinalizePath:   reconFinalizePath(reconID),
		ReopenPath:     reconReopenPath(reconID),
	}
	for _, sp := range splits {
		payee := ""
		if sp.PayeeID != nil {
			payee = payees[*sp.PayeeID]
		}
		fund := ""
		if sp.FundID != nil {
			fund = funds[*sp.FundID]
		}
		memo := sp.SplitMemo
		if memo == "" {
			memo = sp.TxnMemo
		}
		model.Rows = append(model.Rows, reconSplitRow{
			SplitID:   sp.SplitID,
			RowID:     reconRowID(sp.SplitID),
			ToggleID:  reconToggleID(sp.SplitID),
			Date:      money.FormatDate(parseISOForDisplay(sp.Date), df),
			PayeeName: payee,
			Memo:      memo,
			FundName:  fund,
			AmountFmt: recon.Currency + " " + money.Format(sp.Amount, exp, opts),
			Cleared:   sp.Cleared,
		})
	}
	// Uncleared first, then cleared, keeping each group in chronological order (the
	// store already ordered by date, id) -- the workspace's "uncleared first" ask.
	sort.SliceStable(model.Rows, func(i, j int) bool {
		return !model.Rows[i].Cleared && model.Rows[j].Cleared
	})

	model.Summary = reconSummary{
		StatementFmt:  recon.Currency + " " + money.Format(sum.StatementBalance, exp, opts),
		OpeningFmt:    recon.Currency + " " + money.Format(sum.Opening, exp, opts),
		ClearedFmt:    recon.Currency + " " + money.Format(sum.Cleared, exp, opts),
		DifferenceFmt: recon.Currency + " " + money.Format(sum.Difference, exp, opts),
		Finalizable:   sum.Difference == 0,
	}
	return model, nil
}

// ---- TOGGLE (targeted partial + OOB summary) -----------------------------

// reconToggle handles POST /reconciliations/{id}/splits/{sid}/toggle (TxnWrite): it
// flips the split's cleared state (SetSplitReconciled) and returns a PARTIAL --
// ONLY the flipped row plus an out-of-band swap of the sticky summary -- so nothing
// above shifts and scroll/focus survive (Appendix C anti-jank, the p16.3
// requirement). The new cleared state is derived from the split's CURRENT state so a
// repeated POST round-trips.
func (s *server) reconToggle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reconID := parseID(r.PathValue("id"))
	splitID := parseID(r.PathValue("sid"))

	// Determine the current state so the toggle flips it. A split absent from the
	// workspace set (wrong account/currency, deleted, or prior-finalized) is a 404.
	splits, err := s.store.ReconciliationWorkspaceSplits(ctx, reconID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var found bool
	var cleared bool
	for _, sp := range splits {
		if sp.SplitID == splitID {
			found = true
			cleared = sp.Cleared
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	if err := s.store.SetSplitReconciled(s.actorCtx(ctx), reconID, splitID, !cleared); err != nil {
		// A finalized recon (not open) or a rejected clear is a clean guard, not a 500.
		if errors.Is(err, store.ErrReconciliationNotOpen) {
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		}
		s.serverError(w)
		return
	}

	model, err := s.buildWorkspace(ctx, reconID)
	if err != nil {
		s.serverError(w)
		return
	}
	// Find the flipped row for the targeted swap; the summary rides along OOB.
	var flipped reconSplitRow
	for _, row := range model.Rows {
		if row.SplitID == splitID {
			flipped = row
		}
	}
	s.render(w, r, http.StatusOK, "recon-toggle-response", reconToggleResponse{
		Row:          flipped,
		Summary:      model.Summary,
		Base:         model.TogglePathBase,
		FinalizePath: model.FinalizePath,
	})
}

// reconToggleResponse is the toggle PARTIAL's model: the flipped row (targeted swap)
// + the summary (OOB swap, carrying the finalize path so the re-rendered Finalize
// button reflects the new difference) + the toggle path base (so the row's button
// rebuilds its hx-post).
type reconToggleResponse struct {
	Row          reconSplitRow
	Summary      reconSummary
	Base         string
	FinalizePath string
}

// ---- FINALIZE / REOPEN ----------------------------------------------------

// reconFinalize handles POST /reconciliations/{id}/finalize (TxnWrite): finalize the
// recon. The store re-checks the zero-difference gate; a nonzero difference is a
// CLEAN 422 guard (ErrReconciliationDifference), never a 500 -- the disabled button
// is only a UI aid, so a client that POSTs anyway gets a clean rejection.
func (s *server) reconFinalize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.Finalize(s.actorCtx(ctx), id); err != nil {
		switch {
		case errors.Is(err, store.ErrReconciliationDifference):
			// Re-render the workspace at 422 (the diff chip explains why); the button
			// is disabled, so this only fires on a stale/forced POST.
			model, berr := s.buildWorkspace(ctx, id)
			if berr != nil {
				s.serverError(w)
				return
			}
			s.render(w, r, http.StatusUnprocessableEntity, "reconcile.tmpl", s.newShellPage(r, model))
			return
		case errors.Is(err, store.ErrReconciliationNotOpen):
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		case errors.Is(err, store.ErrReconciliationNotFound):
			http.NotFound(w, r)
			return
		default:
			s.serverError(w)
			return
		}
	}
	redirectAfterForm(w, r, reconWorkspacePath(id))
}

// reconReopen handles POST /reconciliations/{id}/reopen (TxnWrite): reopen a
// finalized recon (the audited unreconcile, D13) so its splits are editable again.
func (s *server) reconReopen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := parseID(r.PathValue("id"))
	if err := s.store.Reopen(s.actorCtx(ctx), id); err != nil {
		switch {
		case errors.Is(err, store.ErrReconciliationNotFinalized),
			errors.Is(err, store.ErrReconciliationNotLatest),
			errors.Is(err, store.ErrOpenReconciliationExists):
			// A later finalized recon must be reopened first (p16.5, in-order), or a
			// later OPEN recon already stands on this (account, currency) (p22.5, one
			// open at a time): a plain 409 conflict, matching the not-finalized case --
			// never a 500.
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		case errors.Is(err, store.ErrReconciliationNotFound):
			http.NotFound(w, r)
			return
		default:
			s.serverError(w)
			return
		}
	}
	redirectAfterForm(w, r, reconWorkspacePath(id))
}

// ---- template funcs ------------------------------------------------------

// reconRowCtx pairs a workspace split row with the toggle path base and the
// finalized gate so the shared "recon-row" template (used by the full page AND the
// toggle partial) can reach them without the row model carrying them. It is the
// `reconRowCtx` template func: {{reconRowCtx .Row base finalized}}.
type reconRowCtx struct {
	Row       reconSplitRow
	Base      string
	Finalized bool
}

func makeReconRowCtx(row reconSplitRow, base string, finalized bool) reconRowCtx {
	return reconRowCtx{Row: row, Base: base, Finalized: finalized}
}

// reconSummaryCtx wraps the summary for the "recon-summary" template. OOB marks the
// out-of-band swap (set only in the toggle response). Model carries the summary +
// the finalize path + finalized gate (the full page and the OOB swap share the same
// template; the OOB swap does not re-render the Finalize form's action, but keeping
// one template avoids drift). It is the `reconSummaryCtx` template func.
type reconSummaryCtx struct {
	OOB   bool
	Model reconSummaryModel
}

// reconSummaryModel is the summary template's inner data.
type reconSummaryModel struct {
	Summary      reconSummary
	Finalized    bool
	FinalizePath string
}

func makeReconSummaryCtx(summary reconSummary, oob, finalized bool, finalizePath string) reconSummaryCtx {
	return reconSummaryCtx{
		OOB: oob,
		Model: reconSummaryModel{
			Summary:      summary,
			Finalized:    finalized,
			FinalizePath: finalizePath,
		},
	}
}
