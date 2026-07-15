package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// p26.37 last-used header account. READ-ONLY convenience lookup (rule 2 permits reads
// via sqlc): the header (position-0) account of the transaction the given user MOST
// RECENTLY ENTERED, used to prefill a NEW transaction opened from the top nav (no
// register origin). Returns 0 (no error) when the user has entered no non-deleted
// transaction yet -- the caller leaves the header account blank.

// LastHeaderAccountForActor returns the position-0 split account of the actor's
// most-recently-created (non-deleted) transaction, or 0 when they have none. Recency is
// by create-change id (insertion order), not the transaction's business date, so a
// backdated entry does not win.
func (s *Store) LastHeaderAccountForActor(ctx context.Context, actorID int64) (int64, error) {
	if actorID == 0 {
		return 0, nil
	}
	acct, err := s.q.LastHeaderAccountForActor(ctx, actorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: last header account for actor: %w", err)
	}
	return acct, nil
}
