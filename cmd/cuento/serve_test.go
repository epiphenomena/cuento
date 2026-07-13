package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeBadDataDirFails covers the step's "a bad config fails with a clear
// error, not a panic" requirement. serve() creates the data dir (os.MkdirAll)
// BEFORE it migrates, opens the db, or binds any socket, so pointing -data-dir
// at a path under a regular file makes MkdirAll fail with ENOTDIR and serve
// returns a wrapped "data dir" error immediately — no network, no db, no ACME.
func TestServeBadDataDirFails(t *testing.T) {
	// Create a regular file, then ask for a data dir *under* it: MkdirAll cannot
	// create a directory beneath a non-directory.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	badDir := filepath.Join(f, "sub")

	err := serve([]string{"-data-dir", badDir})
	if err == nil {
		t.Fatal("serve with an unwritable data dir returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "data dir") {
		t.Fatalf("error %q does not mention the data dir (want a clear message)", err)
	}
}
