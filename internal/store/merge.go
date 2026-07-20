package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Account merge (p08.5). MergeAccount folds a SOURCE leaf account into a
// DESTINATION leaf account: every split on src is repointed to dst (each moved
// split versioned op='update', snapshot-from-live so the new snapshot carries
// account_id = dst), and src is deactivated (active=0, op='update') so it drops
// out of active trees while keeping its history. dst is NEVER written -- its own
// attributes (functional default, default program, 990 code, ...) are kept as-is;
// it only receives the moved splits. Everything happens under ONE change: one
// changes row anchors every version row (the repointed splits AND src's
// deactivation).
//
// AUDIT DISCIPLINE (rule 5/14, D4): the pre-merge op='create' split version rows
// (which carry account_id = src) are left UNTOUCHED. Appending an op='update'
// snapshot per moved split is what keeps as-of history intact -- TransactionAsOf
// at a time BEFORE the merge still reconstructs the split on the SOURCE account;
// after the merge it reconstructs it on dst.
//
// RECONCILIATIONS (p22.5 block-guard): full recon repointing from src to dst stays
// backlog (a real design problem -- recons are per (account, currency); dst may hold
// overlapping recons; the finalized opening chain). But the naive "repoint the split,
// leave its reconciliation_id pointing at a src-account recon" is an INTEGRITY HOLE:
// for an OPEN recon it leaves account_id=dst on a split whose recon has account_id=src
// (Z8 fires); for a FINALIZED recon the 00014 split-lock trigger ABORTs the repoint
// with a raw SQLite error. So MergeAccount REFUSES the merge (ErrMergeSourceReconciled,
// a clean typed rejection) when src has ANY split with a non-NULL reconciliation_id --
// unreconcile/reopen them first. See DECISIONS p22.5 and docs/deferred.md.

// Merge sentinel errors handlers and tests branch on (errors.Is), wrapped with %w
// at the call site so errors.Is sees them through the funnel.
var (
	// ErrMergeNotLeaf: the SOURCE account has children (only leaves merge, D11).
	ErrMergeNotLeaf = errors.New("store: merge source is not a leaf account")
	// ErrMergeIntoPlaceholder: the DESTINATION account has children (D11).
	ErrMergeIntoPlaceholder = errors.New("store: merge destination is a placeholder (has children)")
	// ErrMergeIntoInactive: the DESTINATION account is inactive -- it cannot take
	// the moved splits (the leaf-active trigger would abort the repoint).
	ErrMergeIntoInactive = errors.New("store: merge destination is inactive")
	// ErrMergeCrossTypeClass: src and dst have different types. STRICT equality:
	// a revenue split repointed onto an expense account (or vice-versa) would be an
	// invalid split (functional_class/program mismatch, D21/D24) -- the repoint
	// UPDATE would abort on the split triggers and Z14/Z15 would flag it. So merge
	// requires src.Type == dst.Type, not merely a compatible type-class.
	ErrMergeCrossTypeClass = errors.New("store: merge source and destination have different types")
	// ErrMergeSubsetSubs: dst's subsidiary set does not cover src's (D18). Every
	// moved split's account must still include its txn's subsidiary; if dst lacks a
	// subsidiary src had, a moved split could leave dst unmapped to its txn's sub.
	ErrMergeSubsetSubs = errors.New("store: merge destination's subsidiary set does not cover the source's")
	// ErrMergeSelf: src and dst are the same account.
	ErrMergeSelf = errors.New("store: cannot merge an account into itself")
	// ErrMergeSourceReconciled: the SOURCE account has at least one split cleared
	// against a reconciliation (non-NULL reconciliation_id, p22.5). Merging would
	// repoint that split to dst while its recon stays on src -- Z8 for an open recon,
	// a raw trigger ABORT for a finalized one. Refuse cleanly; unreconcile/reopen the
	// affected reconciliation(s) first. Full recon repointing is backlog.
	ErrMergeSourceReconciled = errors.New("store: the source account has reconciled splits; unreconcile or reopen them first")
)

// MergeAccount folds src into dst under ONE change (see the package-level doc
// comment above). Validation runs inside fn on the tx-bound q (p08.2's TOCTOU
// discipline). dst is never written; src's splits are repointed (each versioned
// op='update') and src is deactivated (op='update').
func (s *Store) MergeAccount(ctx context.Context, src, dst ids.AccountID) error {
	if src == dst {
		return ErrMergeSelf
	}
	_, err := s.write(ctx, "account.merge", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			srcAcct, err := q.GetAccount(ctx, src)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load source %d: %w", src, ErrAccountNotFound)
				}
				return fmt.Errorf("load source %d: %w", src, err)
			}
			dstAcct, err := q.GetAccount(ctx, dst)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load destination %d: %w", dst, ErrAccountNotFound)
				}
				return fmt.Errorf("load destination %d: %w", dst, err)
			}

			// src must be a leaf (no children) -- only leaves merge (D11).
			srcLeaf, err := q.AccountIsLeaf(ctx, ids.Null(&src))
			if err != nil {
				return fmt.Errorf("leaf check source %d: %w", src, err)
			}
			if !srcLeaf {
				return ErrMergeNotLeaf
			}
			// dst must be a leaf too -- a placeholder holds no splits (D11).
			dstLeaf, err := q.AccountIsLeaf(ctx, ids.Null(&dst))
			if err != nil {
				return fmt.Errorf("leaf check destination %d: %w", dst, err)
			}
			if !dstLeaf {
				return ErrMergeIntoPlaceholder
			}
			// dst must be active -- an inactive account takes no splits (the
			// leaf-active trigger would abort the repoint UPDATE).
			if dstAcct.Active == 0 {
				return ErrMergeIntoInactive
			}
			// STRICT same-type (not type-class): a repointed split must stay valid
			// for dst's type (functional_class/program, D21/D24). See the sentinel.
			if srcAcct.Type != dstAcct.Type {
				return ErrMergeCrossTypeClass
			}
			// dst's subsidiary set must cover src's (D18 superset), mirroring
			// validateMove: every moved split must still map its txn's subsidiary.
			srcSubs, err := subSet(ctx, q, src)
			if err != nil {
				return err
			}
			dstSubs, err := subSet(ctx, q, dst)
			if err != nil {
				return err
			}
			for sid := range srcSubs {
				if !dstSubs[sid] {
					return ErrMergeSubsetSubs
				}
			}

			// Recon block-guard (p22.5): refuse the merge when src has ANY split
			// cleared against a reconciliation. Repointing such a split to dst leaves
			// it linked to a recon on the OLD src account -- Z8 fires (open recon) or
			// the 00014 split-lock trigger ABORTs (finalized recon). Full recon
			// repointing stays backlog; this closes the integrity hole cleanly. Checked
			// inside fn on the tx-bound q (TOCTOU), before any write.
			reconciled, err := q.CountReconciledSplitsForAccount(ctx, src)
			if err != nil {
				return fmt.Errorf("count reconciled splits on source %d: %w", src, err)
			}
			if reconciled > 0 {
				return ErrMergeSourceReconciled
			}

			// Repoint every split on src to dst, versioning each op='update'.
			// Capture the ids FIRST -- after the first repoint, a WHERE account_id
			// lookup would confuse moved splits with dst's pre-existing ones.
			splitIDs, err := q.SplitIdsByAccount(ctx, src)
			if err != nil {
				return fmt.Errorf("list source splits %d: %w", src, err)
			}
			for _, sid := range splitIDs {
				if err := q.RepointSplitAccount(ctx, sqlc.RepointSplitAccountParams{AccountID: dst, ID: sid}); err != nil {
					return fmt.Errorf("repoint split %d -> account %d: %w", sid, dst, err)
				}
				// Snapshot-from-live AFTER the live update, so the new op='update'
				// snapshot records account_id = dst. The pre-merge op='create'
				// snapshots (account_id = src) are left untouched -- that is what
				// keeps as-of history intact.
				if err := insertSplitVersion(ctx, q, changeID, "update", sid); err != nil {
					return err
				}
			}

			// Reconciliations: full repointing from src to dst stays backlog (see the
			// package doc comment). The block-guard above guarantees no split reaching
			// this point carries a reconciliation_id, so the repoint is always safe
			// (no Z8 violation, no split-lock-trigger abort). See DECISIONS p22.5.

			// Deactivate src (active=0, op='update'). Inlined rather than calling
			// the public DeactivateAccount so it rides the SAME change as the
			// repoints. Every other column is carried through from srcAcct.
			if err := q.UpdateAccount(ctx, sqlc.UpdateAccountParams{
				ParentID:          srcAcct.ParentID,
				Type:              srcAcct.Type,
				DefaultCurrency:   srcAcct.DefaultCurrency,
				FunctionalClass:   srcAcct.FunctionalClass,
				Form990Code:       srcAcct.Form990Code,
				DefaultProgramID:  srcAcct.DefaultProgramID,
				Intercompany:      srcAcct.Intercompany,
				Reconcilable:      srcAcct.Reconcilable,
				Active:            0,
				SortOrder:         srcAcct.SortOrder,
				CreatedAt:         srcAcct.CreatedAt,
				CurrentCash:       srcAcct.CurrentCash,
				ReceivablePayable: srcAcct.ReceivablePayable,
				ID:                src,
			}); err != nil {
				return fmt.Errorf("deactivate source %d: %w", src, err)
			}
			return insertAccountVersion(ctx, q, changeID, "update", src)
		})
	if err != nil {
		return fmt.Errorf("merge account %d into %d: %w", src, dst, err)
	}
	return nil
}
