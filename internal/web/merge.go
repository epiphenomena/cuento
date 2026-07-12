package web

import (
	"errors"
	"net/http"

	"cuento/internal/store"
)

// p11.2 merge-accounts UI (Perm TxnWrite). Merging folds a SOURCE leaf account
// into a DESTINATION leaf account: every split on src is repointed to dst and src
// is deactivated (store.MergeAccount, p08.5). The UI is a TWO-STEP flow so a
// destructive, irreversible-feeling operation is never one click:
//
//   1. GET  /accounts/merge  -> the merge form (pick a source leaf + a dest leaf).
//   2. POST /accounts/merge WITHOUT confirm -> a CONSEQUENCES PREVIEW: how many
//      splits will repoint, that 0 reconciliations move (deferred to p16), and the
//      destination-must-cover-source-subs rule -- plus a Confirm button. This
//      branch performs ZERO writes (it only reads the split count).
//   3. POST /accounts/merge WITH confirm=1 -> store.MergeAccount. On a typed store
//      error the handler maps the sentinel to an i18n KEY and re-renders the form
//      region at 422 (the p10.3 convention); the store validates-then-writes
//      atomically, so a rejected merge writes nothing.
//
// The preview does NOT re-validate the merge's legality (the store is the source
// of truth, rule: don't duplicate validation) -- coverage/type errors surface on
// the confirm POST, exactly like every other form in the app. Every string via
// {{t}} (rule 9); no inline script (rule 12).

// leafOption is one selectable account in the source/destination pickers: only
// active leaf accounts can merge (src and dst are both leaves, D11), so the form
// offers exactly those, labelled by name.
type leafOption struct {
	ID   int64
	Name string
	Type string
}

// mergeForm is the merge form model. It carries the leaf-account option lists and
// the current src/dst selection (echoed back on a 422 re-render) plus an embedded
// formErrors so the p10.3 partial renders field errors + autofocus.
type mergeForm struct {
	Src    int64
	Dst    int64
	Leaves []leafOption

	Errors formErrors
}

// mergePreview is the consequences model rendered before the confirm step: the
// resolved src/dst names, the number of splits that will repoint, and the count of
// reconciliations that will move (0 until p16). The template turns these into the
// human-readable summary + a Confirm control that re-POSTs with confirm=1.
type mergePreview struct {
	Src        int64
	Dst        int64
	SrcName    string
	DstName    string
	SplitCount int
	ReconCount int // 0 for now (p16.1 repoints reconciliations)
}

// mergeFormPartial handles GET /accounts/merge (TxnWrite): the merge form, swapped
// into #account-form by htmx (like the create/edit form). It lists active leaf
// accounts for both selects.
func (s *server) mergeFormPartial(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildMergeForm(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "merge-form", form)
}

// buildMergeForm assembles the leaf-account option lists (active leaves only) and
// echoes any submitted src/dst so a re-render keeps the selection. Names resolve in
// the request language via Tree (p05.3 fallback).
func (s *server) buildMergeForm(r *http.Request) (mergeForm, error) {
	ctx := r.Context()
	rows, err := s.store.Tree(ctx, langOf(ctx), nil)
	if err != nil {
		return mergeForm{}, err
	}
	// A leaf is an account with no children among the tree rows.
	hasChild := map[int64]bool{}
	for _, row := range rows {
		if row.ParentID.Valid {
			hasChild[row.ParentID.Int64] = true
		}
	}
	form := mergeForm{}
	for _, row := range rows {
		if row.Active == 0 || hasChild[row.ID] {
			continue // only active leaves merge (src and dst are both leaves)
		}
		form.Leaves = append(form.Leaves, leafOption{ID: row.ID, Name: row.Name, Type: row.Type})
	}
	form.Src = parseID(r.FormValue("src"))
	form.Dst = parseID(r.FormValue("dst"))
	return form, nil
}

// merge handles POST /accounts/merge (TxnWrite): the two-step flow. Without the
// confirm flag it renders the consequences preview (no writes); with confirm=1 it
// executes store.MergeAccount and, on a typed error, re-renders the form at 422.
func (s *server) merge(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	src := parseID(r.PostFormValue("src"))
	dst := parseID(r.PostFormValue("dst"))
	confirmed := r.PostFormValue("confirm") != ""

	// A missing selection is a client-side field error, not a store call.
	if src == 0 || dst == 0 {
		form, err := s.buildMergeForm(r)
		if err != nil {
			s.serverError(w)
			return
		}
		form.Errors.add("src", "error.merge.select_both")
		s.renderFormError(w, r, "merge-form", form)
		return
	}

	if !confirmed {
		// PREVIEW: summarize consequences and show the Confirm control. READ ONLY --
		// the split count comes from the SAME query MergeAccount repoints from, so the
		// preview never lies about how many splits move.
		ids, err := s.store.SplitIDsForAccount(ctx, src)
		if err != nil {
			s.serverError(w)
			return
		}
		preview := mergePreview{
			Src:        src,
			Dst:        dst,
			SrcName:    s.accountName(ctx, src, langOf(ctx)),
			DstName:    s.accountName(ctx, dst, langOf(ctx)),
			SplitCount: len(ids),
			ReconCount: 0, // p16.1 will repoint reconciliations; today none move.
		}
		s.render(w, r, http.StatusOK, "merge-preview", preview)
		return
	}

	// CONFIRMED: execute. The store validates-then-writes atomically; a typed error
	// means nothing was written (rollback), so "no execution" holds on any error.
	if err := s.store.MergeAccount(s.actorCtx(ctx), src, dst); err != nil {
		s.renderMergeFormError(w, r, err)
		return
	}
	redirectAfterForm(w, r, "/accounts")
}

// renderMergeFormError maps a store TYPED merge error to an i18n key and re-renders
// the merge form region at 422 (the p10.3 convention). It never re-validates -- it
// only TRANSLATES the store's sentinels to a (field, key) pair. An unrecognized
// error is a real server fault.
func (s *server) renderMergeFormError(w http.ResponseWriter, r *http.Request, err error) {
	field, key := mergeErrorField(err)
	if key == "" {
		s.serverError(w)
		return
	}
	form, berr := s.buildMergeForm(r)
	if berr != nil {
		s.serverError(w)
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "merge-form", form)
}

// mergeErrorField maps each store merge sentinel to a (form field, i18n key) pair.
// ALL six sentinels are mapped so no legitimate validation failure falls through to
// the 500 path (mirrors accountErrorField). The field drives autofocus: coverage /
// type / self / leaf problems point at the source select; placeholder / inactive
// point at the destination.
func mergeErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrMergeSelf):
		return "src", "error.merge.self"
	case errors.Is(err, store.ErrMergeNotLeaf):
		return "src", "error.merge.not_leaf"
	case errors.Is(err, store.ErrMergeCrossTypeClass):
		return "src", "error.merge.cross_type_class"
	case errors.Is(err, store.ErrMergeSubsetSubs):
		return "src", "error.merge.subset_subs"
	case errors.Is(err, store.ErrMergeIntoPlaceholder):
		return "dst", "error.merge.into_placeholder"
	case errors.Is(err, store.ErrMergeIntoInactive):
		return "dst", "error.merge.into_inactive"
	default:
		return "", ""
	}
}
