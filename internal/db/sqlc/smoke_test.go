package sqlc_test

import (
	"context"
	"testing"

	"cuento/internal/db/sqlc"
	"cuento/internal/testutil"
)

// TestSqlcSmoke proves the sqlc-generated query layer round-trips against a
// migrated harness database: one trivial, schema-less query (SELECT 1) executes
// through the generated Queries type and returns the expected value. It ties
// together sqlc code generation, the migration runner, and the test harness.
func TestSqlcSmoke(t *testing.T) {
	d := testutil.NewDB(t)
	q := sqlc.New(d)

	got, err := q.SelectOne(context.Background())
	if err != nil {
		t.Fatalf("SelectOne: %v", err)
	}
	if got != 1 {
		t.Errorf("SelectOne = %d, want 1", got)
	}
}
