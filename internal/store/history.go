package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Transaction history reconstruction (p12.4) -- the ordered audit timeline for one
// transaction, rebuilt from the append-only version twins (transactions_versions +
// splits_versions) tied to the changes spine. Each timeline ENTRY is one change:
// its actor + timestamp (from changes/users) and the set of DIFFS that change made,
// both to the header and to the split set.
//
// The DIFF is computed HERE, in Go (testable), by walking consecutive snapshots per
// entity (the header as one entity; each split by its id) and comparing each version
// row to its predecessor. The diffs are STRUCTURED (typed old/new values, ids and
// int64 amounts, nullable fund/program/class) -- NOT rendered strings: amount
// formatting is per-user (rule 10) and account/fund/program names are language-
// dependent (rule 9), so the WEB layer resolves names + formats amounts + applies
// i18n field labels. This keeps the store diff unit-testable (assert old=X/new=Y)
// and rules 9/10 intact.
//
// CRITICAL: history loads from the VERSION rows by id (which exist regardless of the
// live row's deleted flag), NOT via GetTransaction (which 404s a soft-deleted txn).
// The one case history most needs is a VOIDED transaction; guarding with
// GetTransaction would hide exactly that. A txn with zero version rows is
// ErrTransactionNotFound (it never existed).

// HistoryField identifies which business field a diff concerns. The web layer maps
// it to an i18n label key and knows how to render the value (amount via the money
// formatter, ids via name maps).
type HistoryField string

const (
	FieldDate       HistoryField = "date"
	FieldSubsidiary HistoryField = "subsidiary"
	FieldMemo       HistoryField = "memo"
	FieldNotes      HistoryField = "notes"
	FieldCurrency   HistoryField = "currency"
	FieldAccount    HistoryField = "account"
	FieldAmount     HistoryField = "amount"
	FieldFund       HistoryField = "fund"
	FieldProgram    HistoryField = "program"
	FieldFunctional HistoryField = "functional_class"
	FieldSplitMemo  HistoryField = "memo"
	// FieldSplitDescription is a split's per-line free-text description (p26.15/p26.17),
	// surfaced in the split diff now that splits_versions carries it.
	FieldSplitDescription HistoryField = "description"
)

// DiffValue is one side (old or new) of a field change. Exactly one representation
// is meaningful per field: Text for strings (date/memo/currency/functional class),
// ID for entity references (subsidiary/payee/account/fund/program; Valid=false ==
// none/unrestricted), Amount for money (net-debit minor units). The web layer reads
// the field to know which to render.
type DiffValue struct {
	Text   string
	ID     sql.NullInt64
	Amount int64
}

// FieldDiff is one changed business field: which field, and its old/new value.
type FieldDiff struct {
	Field HistoryField
	Old   DiffValue
	New   DiffValue
}

// SplitDiff is a change to ONE split within a change: added, removed, or changed
// (fields carrying the per-field deltas). For added/removed every field is a diff
// against the empty side so the web layer can render the whole line. Position lets
// the web layer label the split (e.g. "line 1").
type SplitDiff struct {
	SplitID  int64
	Op       string // "create" | "update" | "delete"
	Position int64
	Fields   []FieldDiff
}

// HistHeaderState is the FULL reconstructed header of a transaction as of one version
// (structured, not rendered -- the web layer resolves the subsidiary name and formats
// nothing here). Deleted marks the void state (the delete version).
type HistHeaderState struct {
	Date         string
	SubsidiaryID ids.SubsidiaryID
	Memo         string
	Notes        string
	Currency     string
	Deleted      bool
}

// HistSplitState is one split as it stood live at a version, plus its change status vs the
// prior version so the web layer can highlight it: Status is "" (unchanged), "create"
// (added this version), "update" (changed this version), or "delete" (removed this
// version -- a ghost row shown at its last position). ChangedFields names the fields
// that moved on an update so the web layer can emphasize exactly those cells; Old
// carries the prior-version value of each changed field (for the struck old value).
type HistSplitState struct {
	SplitID         int64
	AccountID       int64
	Amount          int64
	FundID          sql.NullInt64
	ProgramID       sql.NullInt64
	FunctionalClass sql.NullString
	Memo            string
	Description     string
	Position        int64
	Status          string      // "" | "create" | "update" | "delete"
	ChangedFields   []FieldDiff // populated for Status=="update"; carries old/new per changed field
}

// HistVersionState is the FULL transaction state at one version: its header plus the live
// split set (ordered by Position, with removed rows folded in as ghosts at their last
// position). It lets the web layer render each SAVED STATE in full (rule: the owner
// wants to SEE every state), with per-row change status overlaid for highlighting.
type HistVersionState struct {
	Header HistHeaderState
	Splits []HistSplitState
}

// HistoryEntry is one change in the timeline: its actor + timestamp + op, the
// header field diffs, the split-set diffs, and the FULL reconstructed state as of this
// version. Op is the HEADER op when the change touched the header
// ("create"/"update"/"delete"); a change that touched only splits (a future
// account-merge repoint) carries op="update" with no header diffs.
type HistoryEntry struct {
	ChangeID    int64
	ActorID     int64
	ActorName   string
	At          string // RFC3339Nano (== changes.at); the web layer formats the date
	Op          string
	HeaderDiffs []FieldDiff
	SplitDiffs  []SplitDiff
	State       HistVersionState
}

// TransactionHistory returns the ordered change timeline for one transaction,
// reconstructed from its version twins (D4/D5). Entries are ordered by change
// timestamp then change id (stable across same-instant edits). A transaction with
// no version rows returns ErrTransactionNotFound. Diffs are structured; the web
// layer renders them (rules 9/10).
func (s *Store) TransactionHistory(ctx context.Context, id int64) ([]HistoryEntry, error) {
	hdrRows, err := s.q.TransactionVersionHistory(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: transaction %d header history: %w", id, err)
	}
	if len(hdrRows) == 0 {
		return nil, ErrTransactionNotFound
	}
	splitRows, err := s.q.SplitVersionHistory(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: transaction %d split history: %w", id, err)
	}

	// Entry per change_id. The final order is by change_id ASCENDING (below), which is
	// monotonic with the change timestamp -- changes.id is autoincrement at write time,
	// so a larger id is a later change. This is the "ordered by the change timestamp"
	// the audit backbone requires, and it is correct even for a SPLIT-ONLY change (an
	// account-merge repoint versions a split with NO transactions_versions row): such a
	// change is placed by its own change_id, not appended after all header changes.
	entries := make(map[int64]*HistoryEntry, len(hdrRows))
	ensure := func(changeID, actorID int64, actorName, at string) *HistoryEntry {
		e, ok := entries[changeID]
		if !ok {
			e = &HistoryEntry{
				ChangeID:  changeID,
				ActorID:   actorID,
				ActorName: actorName,
				At:        at,
				Op:        "update", // header op overrides below when present
			}
			entries[changeID] = e
		}
		return e
	}

	// Header diffs: walk consecutive header snapshots (oldest first). The FIRST is a
	// create (diff against empty); each later one diffs against its predecessor.
	var prevHdr *sqlc.TransactionVersionHistoryRow
	for i := range hdrRows {
		row := hdrRows[i]
		e := ensure(row.ChangeID, row.ActorID, row.ActorName, row.At)
		e.Op = row.Op
		e.HeaderDiffs = headerDiff(prevHdr, &hdrRows[i])
		prevHdr = &hdrRows[i]
	}

	// Split diffs: group each split's snapshots by entity_id (rows are oldest-first,
	// so per split the sequence is chronological). For each snapshot compute its diff
	// vs the split's previous snapshot and attach it to the snapshot's change entry.
	prevSplit := make(map[int64]*sqlc.SplitVersionHistoryRow)
	for i := range splitRows {
		row := splitRows[i]
		e := ensure(row.ChangeID, row.ActorID, row.ActorName, row.At)
		sd := SplitDiff{SplitID: row.EntityID, Op: row.Op, Position: row.Position}
		switch row.Op {
		case "create":
			sd.Fields = splitFieldsFull(&splitRows[i], true) // new-side only
		case "delete":
			sd.Fields = splitFieldsFull(&splitRows[i], false) // old-side only
		default: // update
			sd.Fields = splitDiff(prevSplit[row.EntityID], &splitRows[i])
		}
		e.SplitDiffs = append(e.SplitDiffs, sd)
		prevSplit[row.EntityID] = &splitRows[i]
	}

	out := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, *e)
	}
	// Order by change_id ascending (monotonic with the change timestamp).
	sort.Slice(out, func(i, j int) bool { return out[i].ChangeID < out[j].ChangeID })

	reconstructStates(out, hdrRows, splitRows)
	return out, nil
}

// reconstructStates fills each entry's State with the FULL transaction state as of its
// version, by CARRY-FORWARD in change order: a running header snapshot and a running
// map of live splits keyed by split id. For each change (already ordered by ChangeID),
// apply that change's header/split version rows to the running state, then snapshot it
// into the entry.
//
// The subtle correctness point (the bug to avoid): the state at version N is EVERY
// split live as of N, not merely the splits N touched. An untouched split from an
// earlier version must carry forward. A "delete" split row removes it from the live
// set; it is shown once, as a ghost row, on the change that removed it (Status
// "delete"), then dropped from later states.
//
// A header "delete" (void) writes NO split versions, so the split set carries forward
// unchanged onto the void card -- the reviewer sees the transaction's final live splits
// with the header marked Deleted. Each split row also carries its change status vs the
// prior version (create/update/delete) so the web layer can highlight what moved; a
// carried-forward untouched split has Status "".
func reconstructStates(out []HistoryEntry, hdrRows []sqlc.TransactionVersionHistoryRow, splitRows []sqlc.SplitVersionHistoryRow) {
	// Index the raw version rows by change id (a change touches at most one header row
	// and any number of split rows). Rows are already oldest-first.
	hdrByChange := make(map[int64]*sqlc.TransactionVersionHistoryRow, len(hdrRows))
	for i := range hdrRows {
		hdrByChange[hdrRows[i].ChangeID] = &hdrRows[i]
	}
	splitsByChange := make(map[int64][]*sqlc.SplitVersionHistoryRow, len(splitRows))
	for i := range splitRows {
		cid := splitRows[i].ChangeID
		splitsByChange[cid] = append(splitsByChange[cid], &splitRows[i])
	}

	var header HistHeaderState
	live := make(map[int64]*HistSplitState) // split id -> carried-forward live state
	prevSplitRow := make(map[int64]*sqlc.SplitVersionHistoryRow)

	for i := range out {
		cid := out[i].ChangeID

		// Header: apply this change's header row when present; otherwise carry forward.
		if hr := hdrByChange[cid]; hr != nil {
			header = HistHeaderState{
				Date:         hr.Date,
				SubsidiaryID: hr.SubsidiaryID,
				Memo:         hr.Memo,
				Notes:        hr.Notes,
				Currency:     hr.Currency,
				Deleted:      hr.Deleted != 0,
			}
		}

		// Reset every carried-forward split to "unchanged" for this version, then apply
		// this change's split rows to set the per-row status.
		for _, sp := range live {
			sp.Status = ""
			sp.ChangedFields = nil
		}
		var ghosts []HistSplitState // "delete" rows: shown once, then dropped
		for _, row := range splitsByChange[cid] {
			switch row.Op {
			case "delete":
				g := splitSnapshot(row)
				g.Status = "delete"
				ghosts = append(ghosts, g)
				delete(live, row.EntityID)
			default: // create | update
				sp := splitSnapshot(row)
				if row.Op == "update" {
					sp.Status = "update"
					sp.ChangedFields = splitDiff(prevSplitRow[row.EntityID], row)
				} else {
					sp.Status = "create"
				}
				live[row.EntityID] = &sp
			}
			prevSplitRow[row.EntityID] = row
		}

		// Assemble the ordered split set: live rows plus this change's ghosts, sorted by
		// Position (ghosts at their last position keep the table coherent).
		state := make([]HistSplitState, 0, len(live)+len(ghosts))
		for _, sp := range live {
			state = append(state, *sp)
		}
		state = append(state, ghosts...)
		sort.SliceStable(state, func(a, b int) bool { return state[a].Position < state[b].Position })

		out[i].State = HistVersionState{Header: header, Splits: state}
	}
}

// splitSnapshot copies one split version row into a HistSplitState (structured, unresolved).
func splitSnapshot(row *sqlc.SplitVersionHistoryRow) HistSplitState {
	return HistSplitState{
		SplitID:         row.EntityID,
		AccountID:       row.AccountID,
		Amount:          row.Amount,
		FundID:          row.FundID,
		ProgramID:       row.ProgramID,
		FunctionalClass: row.FunctionalClass,
		Memo:            row.Memo,
		Description:     row.Description,
		Position:        row.Position,
	}
}

// headerDiff computes the changed header fields between two snapshots. prev == nil
// means a create (every non-empty field is a diff against the empty side, so the
// timeline shows the initial values).
func headerDiff(prev, cur *sqlc.TransactionVersionHistoryRow) []FieldDiff {
	var diffs []FieldDiff
	add := func(field HistoryField, oldV, newV DiffValue, changed bool) {
		if changed {
			diffs = append(diffs, FieldDiff{Field: field, Old: oldV, New: newV})
		}
	}
	if prev == nil {
		// Create: show the initial header values as "" -> value.
		add(FieldDate, DiffValue{}, DiffValue{Text: cur.Date}, true)
		add(FieldSubsidiary, DiffValue{}, DiffValue{ID: valid(cur.SubsidiaryID)}, true)
		add(FieldMemo, DiffValue{}, DiffValue{Text: cur.Memo}, cur.Memo != "")
		add(FieldNotes, DiffValue{}, DiffValue{Text: cur.Notes}, cur.Notes != "")
		add(FieldCurrency, DiffValue{}, DiffValue{Text: cur.Currency}, true)
		return diffs
	}
	add(FieldDate, DiffValue{Text: prev.Date}, DiffValue{Text: cur.Date}, prev.Date != cur.Date)
	add(FieldSubsidiary, DiffValue{ID: valid(prev.SubsidiaryID)}, DiffValue{ID: valid(cur.SubsidiaryID)}, prev.SubsidiaryID != cur.SubsidiaryID)
	add(FieldMemo, DiffValue{Text: prev.Memo}, DiffValue{Text: cur.Memo}, prev.Memo != cur.Memo)
	add(FieldNotes, DiffValue{Text: prev.Notes}, DiffValue{Text: cur.Notes}, prev.Notes != cur.Notes)
	add(FieldCurrency, DiffValue{Text: prev.Currency}, DiffValue{Text: cur.Currency}, prev.Currency != cur.Currency)
	return diffs
}

// splitDiff computes the changed fields between two split snapshots (an update).
// prev == nil is defensive (a stray update with no create): treated as a full new
// snapshot. NULL-aware for fund/program/functional class (rule 5's optional dims;
// the fund/functional deltas the step names).
func splitDiff(prev, cur *sqlc.SplitVersionHistoryRow) []FieldDiff {
	if prev == nil {
		return splitFieldsFull(cur, true)
	}
	var diffs []FieldDiff
	if prev.AccountID != cur.AccountID {
		diffs = append(diffs, FieldDiff{Field: FieldAccount, Old: DiffValue{ID: valid(prev.AccountID)}, New: DiffValue{ID: valid(cur.AccountID)}})
	}
	if prev.Amount != cur.Amount {
		diffs = append(diffs, FieldDiff{Field: FieldAmount, Old: DiffValue{Amount: prev.Amount}, New: DiffValue{Amount: cur.Amount}})
	}
	if !nullInt64Eq(prev.FundID, cur.FundID) {
		diffs = append(diffs, FieldDiff{Field: FieldFund, Old: DiffValue{ID: prev.FundID}, New: DiffValue{ID: cur.FundID}})
	}
	if !nullInt64Eq(prev.ProgramID, cur.ProgramID) {
		diffs = append(diffs, FieldDiff{Field: FieldProgram, Old: DiffValue{ID: prev.ProgramID}, New: DiffValue{ID: cur.ProgramID}})
	}
	if !nullStringEq(prev.FunctionalClass, cur.FunctionalClass) {
		diffs = append(diffs, FieldDiff{Field: FieldFunctional, Old: DiffValue{Text: prev.FunctionalClass.String}, New: DiffValue{Text: cur.FunctionalClass.String}})
	}
	if prev.Memo != cur.Memo {
		diffs = append(diffs, FieldDiff{Field: FieldSplitMemo, Old: DiffValue{Text: prev.Memo}, New: DiffValue{Text: cur.Memo}})
	}
	if prev.Description != cur.Description {
		diffs = append(diffs, FieldDiff{Field: FieldSplitDescription, Old: DiffValue{Text: prev.Description}, New: DiffValue{Text: cur.Description}})
	}
	return diffs
}

// splitFieldsFull renders every business field of one split snapshot as a diff
// against the empty side: newSide=true puts the snapshot on the New side (a create),
// false on the Old side (a delete). This lets the web layer render an added or
// removed split's full line through the same per-field machinery as an update.
func splitFieldsFull(row *sqlc.SplitVersionHistoryRow, newSide bool) []FieldDiff {
	put := func(field HistoryField, v DiffValue) FieldDiff {
		if newSide {
			return FieldDiff{Field: field, New: v}
		}
		return FieldDiff{Field: field, Old: v}
	}
	var diffs []FieldDiff
	diffs = append(diffs, put(FieldAccount, DiffValue{ID: valid(row.AccountID)}))
	diffs = append(diffs, put(FieldAmount, DiffValue{Amount: row.Amount}))
	diffs = append(diffs, put(FieldFund, DiffValue{ID: row.FundID}))
	if row.ProgramID.Valid {
		diffs = append(diffs, put(FieldProgram, DiffValue{ID: row.ProgramID}))
	}
	if row.FunctionalClass.Valid {
		diffs = append(diffs, put(FieldFunctional, DiffValue{Text: row.FunctionalClass.String}))
	}
	if row.Memo != "" {
		diffs = append(diffs, put(FieldSplitMemo, DiffValue{Text: row.Memo}))
	}
	if row.Description != "" {
		diffs = append(diffs, put(FieldSplitDescription, DiffValue{Text: row.Description}))
	}
	return diffs
}

// valid wraps a non-null id into a sql.NullInt64 (Valid=true).
func valid[T ~int64](id T) sql.NullInt64 { return sql.NullInt64{Int64: int64(id), Valid: true} }
