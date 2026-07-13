package bankimport

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"  ACME   Inc.\t", "acme inc."},
		{"acme inc.", "acme inc."},
		{"A\tB  C", "a b c"},
		{"", ""},
		{"   ", ""},
		{"MixedCASE Text", "mixedcase text"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDedupeHashStableAcrossFormatting proves two descriptions of the SAME event
// (differing only in case/whitespace) hash identically, while a different amount,
// date, account, or payee/memo changes the hash.
func TestDedupeHashStableAcrossFormatting(t *testing.T) {
	base := DedupeHash(7, "2025-01-15", 10000, "ACME  Inc", "Invoice 5")
	same := DedupeHash(7, "2025-01-15", 10000, "acme inc", "invoice   5")
	if base != same {
		t.Errorf("case/whitespace variants should hash the same:\n %s\n %s", base, same)
	}

	// Each component change must move the hash.
	for name, other := range map[string]string{
		"account": DedupeHash(8, "2025-01-15", 10000, "ACME Inc", "Invoice 5"),
		"date":    DedupeHash(7, "2025-01-16", 10000, "ACME Inc", "Invoice 5"),
		"amount":  DedupeHash(7, "2025-01-15", 9999, "ACME Inc", "Invoice 5"),
		"payee":   DedupeHash(7, "2025-01-15", 10000, "Other", "Invoice 5"),
		"memo":    DedupeHash(7, "2025-01-15", 10000, "ACME Inc", "Invoice 6"),
		"sign":    DedupeHash(7, "2025-01-15", -10000, "ACME Inc", "Invoice 5"),
	} {
		if other == base {
			t.Errorf("changing %s should change the hash but did not", name)
		}
	}
}

// TestDedupeHashPayeeMemoBoundary proves the payee|memo separator prevents
// aliasing: ("AB","C") and ("A","BC") must NOT collide.
func TestDedupeHashPayeeMemoBoundary(t *testing.T) {
	a := DedupeHash(1, "2025-01-15", 100, "AB", "C")
	b := DedupeHash(1, "2025-01-15", 100, "A", "BC")
	if a == b {
		t.Error("payee/memo boundary aliases: (AB,C) hashed same as (A,BC)")
	}
}
