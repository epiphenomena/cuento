package web

import (
	"context"
	"errors"
	"net/http"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p11.5b merge-programs UI (Perm TxnWrite). Merging folds a SOURCE program into a
// DESTINATION program: every reference on src (transaction splits' program_id,
// program-subtree report grants, direct child programs' parent_id) is repointed to
// dst and src is deactivated (store.MergeProgram). This MIRRORS the merge-accounts
// flow (merge.go, p11.2) exactly -- a TWO-STEP flow so a destructive-feeling
// operation is never one click:
//
//   1. GET  /programs/merge  -> the merge form (pick a source + a destination).
//   2. POST /programs/merge WITHOUT confirm -> a CONSEQUENCES PREVIEW: how many
//      transaction lines will repoint, plus a Confirm button. This branch performs
//      ZERO writes (it only reads the split count).
//   3. POST /programs/merge WITH confirm=1 -> store.MergeProgram. On a typed store
//      error the handler maps the sentinel to an i18n KEY and re-renders the form
//      region at 422 (the p10.3 convention); the store validates-then-writes
//      atomically, so a rejected merge writes nothing.
//
// The preview does NOT re-validate the merge's legality (the store is the source of
// truth) -- self/root/cycle/into-inactive errors surface on the confirm POST, exactly
// like every other form. Every string via {{t}} (rule 9); no inline script (rule 12).

// programMergeOption is one selectable program in the source/destination pickers.
// Programs are a single dimension tree, so both selects offer every program labelled
// by its dotted hierarchy path (p29.13), fuzzy-ranked by the shared combobox.
type programMergeOption struct {
	ID   int64
	Name string
	// Path is the program's dotted ancestor chain (e.g. "General.Education"); the
	// shared program combobox fuzzy-ranks on it (data-path).
	Path string
}

// programMergeForm is the merge form model: the program option lists and the current
// src/dst selection (echoed back on a 422 re-render) plus an embedded formErrors so
// the p10.3 partial renders field errors + autofocus.
type programMergeForm struct {
	Src     int64
	Dst     int64
	Options []programMergeOption

	Errors formErrors
}

// programMergePreview is the consequences model rendered before the confirm step: the
// resolved src/dst names and the number of transaction lines that will repoint. The
// template turns these into the human-readable summary + a Confirm control that
// re-POSTs with confirm=1.
type programMergePreview struct {
	Src        int64
	Dst        int64
	SrcName    string
	DstName    string
	SplitCount int
}

// programMergeFormPartial handles GET /programs/merge (TxnWrite): the merge form,
// swapped into #program-form by htmx (like the create/edit form). It lists every
// program for both selects.
func (s *server) programMergeFormPartial(w http.ResponseWriter, r *http.Request) {
	form, err := s.buildProgramMergeForm(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "program-merge-form", form)
}

// buildProgramMergeForm assembles the program option lists (every program, with its
// dotted path) and echoes any submitted src/dst so a re-render keeps the selection.
func (s *server) buildProgramMergeForm(r *http.Request) (programMergeForm, error) {
	ctx := r.Context()
	rows, err := s.store.ProgramTree(ctx)
	if err != nil {
		return programMergeForm{}, err
	}
	paths, err := s.store.ProgramPaths(ctx)
	if err != nil {
		return programMergeForm{}, err
	}
	form := programMergeForm{}
	for _, row := range rows {
		form.Options = append(form.Options, programMergeOption{ID: int64(row.ID), Name: row.Name, Path: paths[row.ID]})
	}
	form.Src = parseID(r.FormValue("src"))
	form.Dst = parseID(r.FormValue("dst"))
	return form, nil
}

// programMerge handles POST /programs/merge (TxnWrite): the two-step flow. Without
// the confirm flag it renders the consequences preview (no writes); with confirm=1 it
// executes store.MergeProgram and, on a typed error, re-renders the form at 422.
func (s *server) programMerge(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	src := ids.ProgramID(parseID(r.PostFormValue("src")))
	dst := ids.ProgramID(parseID(r.PostFormValue("dst")))
	confirmed := r.PostFormValue("confirm") != ""

	// A missing selection is a client-side field error, not a store call.
	if src == 0 || dst == 0 {
		form, err := s.buildProgramMergeForm(r)
		if err != nil {
			s.serverError(w)
			return
		}
		form.Errors.add("src", "error.program_merge.select_both")
		s.renderFormError(w, r, "program-merge-form", form)
		return
	}

	if !confirmed {
		// PREVIEW: summarize consequences and show the Confirm control. READ ONLY --
		// the split count comes from the SAME predicate MergeProgram repoints from, so
		// the preview never lies about how many lines move.
		count, err := s.store.SplitCountForProgram(ctx, src)
		if err != nil {
			s.serverError(w)
			return
		}
		preview := programMergePreview{
			Src:        int64(src),
			Dst:        int64(dst),
			SrcName:    s.programName(ctx, src),
			DstName:    s.programName(ctx, dst),
			SplitCount: count,
		}
		s.render(w, r, http.StatusOK, "program-merge-preview", preview)
		return
	}

	// CONFIRMED: execute. The store validates-then-writes atomically; a typed error
	// means nothing was written (rollback), so "no execution" holds on any error.
	if err := s.store.MergeProgram(s.actorCtx(ctx), src, dst); err != nil {
		s.renderProgramMergeFormError(w, r, err)
		return
	}
	redirectAfterForm(w, r, "/programs")
}

// programName resolves a program's display name via ProgramTree. Program names are
// single stored proper nouns (no per-language variant), so there is no lang param.
func (s *server) programName(ctx context.Context, id ids.ProgramID) string {
	rows, err := s.store.ProgramTree(ctx)
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}

// renderProgramMergeFormError maps a store TYPED merge error to an i18n key and
// re-renders the merge form region at 422 (the p10.3 convention). It never
// re-validates -- it only TRANSLATES the store's sentinels to a (field, key) pair. An
// unrecognized error is a real server fault.
func (s *server) renderProgramMergeFormError(w http.ResponseWriter, r *http.Request, err error) {
	field, key := programMergeErrorField(err)
	if key == "" {
		s.serverError(w)
		return
	}
	form, berr := s.buildProgramMergeForm(r)
	if berr != nil {
		s.serverError(w)
		return
	}
	form.Errors.add(field, key)
	s.renderFormError(w, r, "program-merge-form", form)
}

// programMergeErrorField maps each store merge sentinel to a (form field, i18n key)
// pair. ALL merge sentinels are mapped so no legitimate validation failure falls
// through to the 500 path (mirrors mergeErrorField). The field drives autofocus:
// self / root / cycle problems point at the source select; into-inactive points at
// the destination.
func programMergeErrorField(err error) (field, key string) {
	switch {
	case errors.Is(err, store.ErrProgramMergeSelf):
		return "src", "error.program_merge.self"
	case errors.Is(err, store.ErrProgramMergeRoot):
		return "src", "error.program_merge.root"
	case errors.Is(err, store.ErrCycle):
		return "dst", "error.program_merge.cycle"
	case errors.Is(err, store.ErrProgramMergeIntoInactive):
		return "dst", "error.program_merge.into_inactive"
	case errors.Is(err, store.ErrProgramMergeFundScoped):
		return "dst", "error.program_merge.fund_scoped"
	default:
		return "", ""
	}
}
