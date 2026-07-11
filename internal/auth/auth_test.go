package auth_test

import (
	"strings"
	"testing"

	"cuento/internal/auth"
)

// TestHashVerify proves the argon2id wrapper (D9): a hash verifies for the
// password it was made from, rejects any other password, and is salted so two
// hashes of the SAME password differ. This is the only place argon2id is called
// (rule 13) — everything else in the app goes through Hash/Verify.
func TestHashVerify(t *testing.T) {
	const pw = "correct horse battery staple"

	h, err := auth.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h == "" {
		t.Fatal("Hash returned an empty encoded hash")
	}
	// The encoded form is the standard argon2id PHC string; a sanity check that
	// we stored an argon2id hash, not the plaintext.
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Errorf("encoded hash %q does not start with $argon2id$", h)
	}
	if strings.Contains(h, pw) {
		t.Fatalf("encoded hash %q contains the plaintext password", h)
	}

	ok, err := auth.Verify(pw, h)
	if err != nil {
		t.Fatalf("Verify(right password): %v", err)
	}
	if !ok {
		t.Error("Verify returned false for the correct password")
	}

	ok, err = auth.Verify("wrong password", h)
	if err != nil {
		t.Fatalf("Verify(wrong password): %v", err)
	}
	if ok {
		t.Error("Verify returned true for a wrong password")
	}

	// Salt: hashing the same password twice must yield different encoded hashes
	// (a random per-hash salt), so identical passwords are not detectable by
	// comparing stored hashes.
	h2, err := auth.Hash(pw)
	if err != nil {
		t.Fatalf("Hash (second): %v", err)
	}
	if h == h2 {
		t.Error("two hashes of the same password are identical; salt is missing")
	}
	// ...and the second hash still verifies.
	ok, err = auth.Verify(pw, h2)
	if err != nil {
		t.Fatalf("Verify(second hash): %v", err)
	}
	if !ok {
		t.Error("Verify returned false for the correct password against the second hash")
	}
}
