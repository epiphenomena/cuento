package sqlc_test

import (
	"context"
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// TestExponentBounds proves the currencies.exponent CHECK (BETWEEN 0 AND 4, D1)
// rejects out-of-range values. There is no writer for currencies this step
// (it's static reference data seeded by the migration), so the rejected insert
// is attempted with raw SQL — the task explicitly sanctions raw SQL for this
// CHECK negative test. The assertion matches the CHECK-constraint message so it
// fails for the right reason (a bare err != nil would also pass on "no such
// table" before the migration exists).
func TestExponentBounds(t *testing.T) {
	ctx := context.Background()
	d := testutil.NewDB(t)

	for _, exp := range []int{5, -1} {
		_, err := d.ExecContext(ctx,
			`INSERT INTO currencies (code, exponent, symbol, name)
			 VALUES ('XXX', ?, 'x', 'Test')`, exp)
		if err == nil {
			t.Errorf("insert with exponent %d succeeded, want CHECK rejection", exp)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "check") {
			t.Errorf("exponent %d error = %v, want a CHECK constraint failure", exp, err)
		}
	}

	// A value inside 0..4 is accepted — proving the CHECK isn't rejecting
	// everything (e.g. a broken table). Exponent 0 (JPY-style) is a valid edge.
	if _, err := d.ExecContext(ctx,
		`INSERT INTO currencies (code, exponent, symbol, name)
		 VALUES ('JPY', 0, '¥', 'Japanese Yen')`); err != nil {
		t.Fatalf("insert with exponent 0 failed, want acceptance: %v", err)
	}
}
