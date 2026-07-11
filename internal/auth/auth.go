// Package auth is the ONLY place cuento calls argon2id (AGENTS rule 13, D9).
// Password hashing and verification go through Hash and Verify; nothing else in
// the program touches the underlying library, so the parameters and the encoded
// format live in exactly one reviewed spot.
package auth

import (
	"fmt"

	"github.com/alexedwards/argon2id"
)

// params are the argon2id cost parameters. They are the library's DefaultParams
// (alexedwards/argon2id v1.0.0): Memory 64 MiB (65536 KiB), Iterations 1,
// Parallelism = runtime.NumCPU() (machine-dependent, encoded into each PHC
// string so verification is self-describing), SaltLength 16 bytes, KeyLength 32
// bytes -- the RFC-recommended t=1/high-memory setting, ample for a small
// self-hosted deployment (D8). Kept in one place so a future tuning (e.g. a
// fixed Parallelism for reproducible cost across hosts) is a single edit.
var params = argon2id.DefaultParams

// Hash returns the argon2id encoded-hash string (the standard PHC format,
// "$argon2id$v=19$m=...,t=...,p=...$<salt>$<key>") for password. A fresh random
// salt is generated per call, so two hashes of the same password differ.
func Hash(password string) (string, error) {
	encoded, err := argon2id.CreateHash(password, params)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return encoded, nil
}

// Verify reports whether password matches the argon2id encodedHash. It returns
// (false, nil) for a mismatch and a non-nil error only when encodedHash is
// malformed (never for a plain wrong password), so callers distinguish "wrong
// password" from "corrupt stored hash".
func Verify(password, encodedHash string) (bool, error) {
	ok, err := argon2id.ComparePasswordAndHash(password, encodedHash)
	if err != nil {
		return false, fmt.Errorf("auth: verify password: %w", err)
	}
	return ok, nil
}
