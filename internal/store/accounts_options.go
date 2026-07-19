package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// This file holds the READ-ONLY option/lookup queries the chart-of-accounts form
// (p11.1) needs. They live in the store (rule 2 permits reads via sqlc queries)
// so the web handler never reasons about tree types, cycles, or the 990 CSV
// itself -- it just renders what the store returns, and the same predicates that
// the write path enforces (typeCompatible, the check990Type CSV membership) are
// reused here so the offered options and the accepted writes can never disagree.

// accountPathFunc builds the shared dotted-ancestor-path resolver from the FULL
// (unfiltered) account tree (p28.2, build-once). The returned closure walks an id ->
// parent -> ... to the root and joins the lang-resolved names with "." (root first);
// a top-level account's path is just its name. Every account-picker option label
// carries this path so the combobox's fuzzy ranking (combofilter.js) sees the same
// hierarchy at every site. The type-group is layered on separately as an <optgroup>
// (presentation only), never folded into this string.
func accountPathFunc(full []TreeRow) func(int64) string {
	parentOf := make(map[int64]sql.NullInt64, len(full))
	nameOf := make(map[int64]string, len(full))
	for _, r := range full {
		parentOf[r.ID] = r.ParentID
		nameOf[r.ID] = r.Name
	}
	return func(id int64) string {
		var seg []string
		for n, valid := id, true; valid; {
			seg = append(seg, nameOf[n])
			p := parentOf[n]
			if !p.Valid || p.Int64 == n {
				break
			}
			n = p.Int64
		}
		for i, j := 0, len(seg)-1; i < j; i, j = i+1, j-1 {
			seg[i], seg[j] = seg[j], seg[i]
		}
		return strings.Join(seg, ".")
	}
}

// AccountPaths returns id -> dotted-ancestor-path for every account in the chart,
// name-resolved for lang (p28.2). The web layer uses it to stamp the SAME hierarchy
// path onto the non-grid account pickers (merge, account-ledger report filter,
// account parent, import) that previously showed a flat name, so the shared combobox
// fuzzy-ranks them exactly like the entry-grid account selects. Read via Tree (rule 2).
func (s *Store) AccountPaths(ctx context.Context, lang string) (map[int64]string, error) {
	full, err := s.Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	pathOf := accountPathFunc(full)
	out := make(map[int64]string, len(full))
	for _, r := range full {
		out[r.ID] = pathOf(r.ID)
	}
	return out, nil
}

// Form990Option is a 990 line offered in the form's select: its code, a
// human-facing label (part.line -- label, assembled by the caller if desired) and
// the raw fields the template shows. Only lines valid for the account's type are
// returned (D25).
type Form990Option struct {
	Code  string
	Part  string
	Line  string
	Label string
}

// Form990LinesForType returns the 990 lines whose allowed account_types CSV
// includes accountType (D25), in report order. It reuses the SAME CSV-membership
// rule as check990Type (the write-side validator), so the select never offers a
// code the store would reject with Err990TypeMismatch. An empty accountType
// returns no lines (a not-yet-typed form offers nothing).
func (s *Store) Form990LinesForType(ctx context.Context, accountType string) ([]Form990Option, error) {
	if accountType == "" {
		return nil, nil
	}
	lines, err := s.q.ListForm990Lines(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list 990 lines: %w", err)
	}
	var out []Form990Option
	for _, l := range lines {
		if csvContains(l.AccountTypes, accountType) {
			out = append(out, Form990Option{Code: l.Code, Part: l.Part, Line: l.Line, Label: l.Label})
		}
	}
	return out, nil
}

// csvContains reports whether csv (comma-separated, possibly space-padded)
// contains want. Extracted so Form990LinesForType and check990Type share one
// membership rule (the write path splits inline; this is the same test).
func csvContains(csv, want string) bool {
	for _, part := range strings.Split(csv, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// ParentOption is a candidate parent account for the form's move/parent select:
// its id and resolved name plus its type (so the template can group/label). Only
// type-compatible, non-self, non-descendant accounts appear.
type ParentOption struct {
	ID   int64
	Name string
	Type string
	// Path (p28.2) is the account's dotted ancestor chain; the account-form parent
	// picker is a shared combobox that fuzzy-ranks on it, like every account picker.
	Path string
}

// ParentOptions returns the accounts eligible to be the parent of an account of
// accountType, name-resolved for lang, in tree order. It EXCLUDES:
//   - the account itself and all its descendants (excludeID; a move there would
//     make the account its own ancestor -- ErrCycle), and
//   - any account whose type cannot host a child of accountType (D11), reusing
//     typeCompatible so the offered set matches exactly what UpdateAccount accepts.
//
// excludeID <= 0 means "new account, nothing to exclude" (create form): only the
// type filter applies. The result never includes leaf-vs-placeholder distinctions
// -- any type-compatible account may be a parent (it simply stops being a leaf).
func (s *Store) ParentOptions(ctx context.Context, lang, accountType string, excludeID int64) ([]ParentOption, error) {
	rows, err := s.q.AccountTree(ctx, lang)
	if err != nil {
		return nil, fmt.Errorf("store: parent options tree: %w", err)
	}

	excluded := map[int64]bool{}
	if excludeID > 0 {
		desc, err := s.q.AccountDescendants(ctx, excludeID)
		if err != nil {
			return nil, fmt.Errorf("store: parent options descendants of %d: %w", excludeID, err)
		}
		for _, id := range desc { // AccountDescendants includes self
			excluded[id] = true
		}
	}

	// Path builder over the FULL tree (p28.2): every candidate parent's option label
	// carries its dotted ancestor chain so the shared combobox fuzzy-ranks it. Built
	// from these same (unfiltered) tree rows -- no extra query.
	parentOf := make(map[int64]sql.NullInt64, len(rows))
	nameOf := make(map[int64]string, len(rows))
	for _, r := range rows {
		parentOf[r.ID] = r.ParentID
		nameOf[r.ID] = r.Name
	}
	pathOf := func(id int64) string {
		var seg []string
		for n, valid := id, true; valid; {
			seg = append(seg, nameOf[n])
			p := parentOf[n]
			if !p.Valid || p.Int64 == n {
				break
			}
			n = p.Int64
		}
		for i, j := 0, len(seg)-1; i < j; i, j = i+1, j-1 {
			seg[i], seg[j] = seg[j], seg[i]
		}
		return strings.Join(seg, ".")
	}

	var out []ParentOption
	for _, r := range rows {
		if excluded[r.ID] {
			continue
		}
		if !typeCompatible(r.Type, accountType) {
			continue
		}
		out = append(out, ParentOption{ID: r.ID, Name: r.Name, Type: r.Type, Path: pathOf(r.ID)})
	}
	return out, nil
}

// AccountSubsidiaryIDs returns the subsidiary ids currently mapped to an account,
// so the form's subsidiary checklist can pre-check the account's memberships. It
// wraps the AccountSubsidiaries query directly (read; no ordering guarantee is
// needed -- the caller builds a set).
func (s *Store) AccountSubsidiaryIDs(ctx context.Context, id int64) ([]int64, error) {
	ids, err := s.q.AccountSubsidiaries(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: account %d subsidiaries: %w", id, err)
	}
	return ids, nil
}

// AllSubsidiaries returns every subsidiary in tree order for the form's
// subsidiary checklist (the full option set; the account's own memberships come
// from AccountSubsidiaryIDs). A thin wrapper over SubTree so the handler need not
// import sqlc types beyond what the store exposes.
func (s *Store) AllSubsidiaries(ctx context.Context) ([]sqlc.SubTreeRow, error) {
	return s.SubTree(ctx)
}

// ListFunds returns every fund (active AND closed), id-ordered. The register
// (p12.1) uses it for the fund-name lookup (a chip may name a now-closed fund) and
// the fund-filter option list; unlike ActiveFunds it is NOT subsidiary-scoped and
// includes closed funds, since a historical split may reference either. A read via
// sqlc (rule 2).
func (s *Store) ListFunds(ctx context.Context) ([]sqlc.Fund, error) {
	rows, err := s.q.ListFunds(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list funds: %w", err)
	}
	return rows, nil
}

// AccountEditorOption is one selectable account in the transaction editor's account
// combobox (p12.2): a LEAF, ACTIVE account mapped to the editor's subsidiary, with
// the metadata the editor's row logic needs -- Type (to show the program select on
// R/E rows and the functional-class select on expense rows) and the account's
// DEFAULTS (default program id, default functional class) so the client can prefill
// those selects (the server re-defaults authoritatively, D21/D24). SubsidiaryIDs is
// the account's full subsidiary set, stamped as data-subsidiaries so the client's
// pure re-filter (txngrid.js) can flag rows that go out of scope when the header
// subsidiary changes (Appendix C: never silent-clear).
type AccountEditorOption struct {
	ID   int64
	Name string
	// Path is the account's dotted ancestor chain ending in its own name (e.g.
	// "Cash.BOA"; a top-level account's Path is just its name). It is joined from
	// the same lang-resolved tree names, so the last segment always equals Name.
	// The combobox (p26.2) displays it and fuzzy-matches on it; it is never stored.
	Path           string
	Type           string
	DefaultProgram *ids.ProgramID
	DefaultClass   string // "" = none
	// OpenItem marks an A/R-A/P open-line account (p27.1). The budget-split editor
	// (p27.2c) offers R/E leaves AND open_item asset/liability leaves, so it needs
	// this flag to include/filter A/L options (the txn/expense editors ignore it).
	OpenItem      bool
	SubsidiaryIDs []int64
	// Unavailable marks an option that would NOT normally be offered (it is inactive,
	// a placeholder, or outside the editor's subsidiary) but was force-included because
	// an existing split references it (p26.10). The web layer marks it visibly (an
	// i18n suffix + a data-* attribute) so the user sees WHY the row's account is
	// special, while its value stays the real account id and it renders SELECTED. A
	// normally-offered option is never Unavailable.
	Unavailable bool
}

// AccountEditorOptions returns the leaf+active accounts mapped to subID, in tree
// order, name-resolved for lang, with the per-account metadata the editor needs. It
// mirrors the store's own write-side account rules (leaf via the full tree's parent
// set, active via the row) so the offered options can never disagree with what
// PostTransaction accepts. The chart is small; per-account metadata is read once.
func (s *Store) AccountEditorOptions(ctx context.Context, lang string, subID int64) ([]AccountEditorOption, error) {
	return s.AccountEditorOptionsWith(ctx, lang, subID, nil)
}

// AccountEditorOptionsWith is AccountEditorOptions plus a "must-appear" set: any id
// in include that is NOT already offered (because it is inactive / a placeholder /
// out-of-subsidiary) is APPENDED as an Unavailable option carrying its real
// name/path/type/defaults, so the transaction editor can always display -- SELECTED --
// the account a live split references, even one deactivated after the split was
// posted (the display-only "missing accounts" bug, p26.10). With include=nil the
// result is byte-for-byte the normal offered set (the NEW-transaction case is
// unchanged). All names/paths come from the SAME lang-resolved full tree, so an
// injected option's label matches a normal one's.
func (s *Store) AccountEditorOptionsWith(ctx context.Context, lang string, subID int64, include []int64) ([]AccountEditorOption, error) {
	// Full tree (unfiltered) -> which ids are placeholders (have children).
	full, err := s.Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	hasChild := make(map[int64]bool, len(full))
	nameOf := make(map[int64]string, len(full))
	for _, r := range full {
		if r.ParentID.Valid {
			hasChild[r.ParentID.Int64] = true
		}
		nameOf[r.ID] = r.Name
	}
	// pathOf: the shared dotted-ancestor-path builder (build-once, p28.2). The
	// combobox (combofilter.js) fuzzy-ranks on this path so a query like "c.boa"
	// lines up with "Cash.BOA"; the type-group is presentation (an <optgroup>), NOT
	// part of the fuzzy string. Every account-picker site (txn/expense/budget grids,
	// merge, account-ledger report filter, account parent, import) uses THIS builder
	// so their option labels carry the same hierarchy.
	pathOf := accountPathFunc(full)

	// Sub-filtered tree -> the candidate accounts in tree order.
	inSub, err := s.Tree(ctx, lang, &subID)
	if err != nil {
		return nil, err
	}

	var out []AccountEditorOption
	for _, r := range inSub {
		if hasChild[r.ID] || r.Active == 0 {
			continue // placeholders and inactive accounts are not selectable
		}
		acct, err := s.GetAccount(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		subs, err := s.AccountSubsidiaryIDs(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		opt := AccountEditorOption{
			ID:            r.ID,
			Name:          r.Name,
			Path:          pathOf(r.ID),
			Type:          r.Type,
			OpenItem:      acct.OpenItem != 0,
			SubsidiaryIDs: subs,
		}
		opt.DefaultProgram = ids.Ptr[ids.ProgramID](acct.DefaultProgramID)
		if acct.FunctionalClass.Valid {
			opt.DefaultClass = acct.FunctionalClass.String
		}
		out = append(out, opt)
	}

	// Force-include any referenced account not already offered (p26.10). These are the
	// ids used by an existing transaction's splits: an inactive / placeholder /
	// out-of-subsidiary account has no normal <option>, so the editor's select would
	// render blank (the "missing accounts" bug). Append each such id as an Unavailable
	// option with its real metadata, so the row can render it SELECTED and marked.
	if len(include) > 0 {
		offered := make(map[int64]bool, len(out))
		for _, o := range out {
			offered[o.ID] = true
		}
		for _, id := range include {
			if id <= 0 || offered[id] {
				continue
			}
			offered[id] = true // dedup repeated include ids
			if _, ok := nameOf[id]; !ok {
				continue // an id not in the chart at all (should not happen); skip silently
			}
			acct, err := s.GetAccount(ctx, id)
			if err != nil {
				return nil, err
			}
			subs, err := s.AccountSubsidiaryIDs(ctx, id)
			if err != nil {
				return nil, err
			}
			opt := AccountEditorOption{
				ID:            id,
				Name:          nameOf[id],
				Path:          pathOf(id),
				Type:          acct.Type,
				OpenItem:      acct.OpenItem != 0,
				SubsidiaryIDs: subs,
				Unavailable:   true,
			}
			opt.DefaultProgram = ids.Ptr[ids.ProgramID](acct.DefaultProgramID)
			if acct.FunctionalClass.Valid {
				opt.DefaultClass = acct.FunctionalClass.String
			}
			out = append(out, opt)
		}
	}
	return out, nil
}

// TransactionSplits returns the live split set of one transaction in display
// order. The register (p12.1) uses it to resolve a row's COUNTER-ACCOUNT: for a
// 2-split transaction the other split's account is the counter-account; for >2 the
// UI shows "Split". A read via sqlc (rule 2).
func (s *Store) TransactionSplits(ctx context.Context, txnID int64) ([]sqlc.Split, error) {
	rows, err := s.q.SplitsByTransaction(ctx, txnID)
	if err != nil {
		return nil, fmt.Errorf("store: splits for transaction %d: %w", txnID, err)
	}
	return rows, nil
}
