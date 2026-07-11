package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
)

// migrationsFS holds the production migrations. go:embed requires at least one
// matching file; the baseline 00001_init.sql is a no-op (real business schema
// arrives in p02.1 onward). Forward-only, numbered, never edited once applied
// (AGENTS rule 4). The files live under migrations/; Migrate strips that prefix
// with fs.Sub so goose sees the .sql files at the FS root.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending embedded migrations to the database at path,
// backing the file up first when it already carries schema (AGENTS rule 4).
// It is the production entry point used by `cuento migrate` and serve startup.
func Migrate(ctx context.Context, path string) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrate: sub fs: %w", err)
	}
	return MigrateFS(ctx, path, sub)
}

// MigrateFS is the core runner: it applies the pending migrations found at the
// root of migrations to the database at path. It accepts an injected fs.FS so
// tests can exercise real backup/idempotency/latest behavior with synthetic
// migrations; production callers use the thin Migrate wrapper.
//
// Before applying anything to an EXISTING (non-brand-new) database it writes a
// consistent snapshot to <path>.pre-<current-version>.bak via VACUUM INTO
// (which produces a clean copy regardless of WAL state). The backup is skipped
// for a brand-new database (current version 0) — there is nothing to lose — and
// when there is nothing pending the whole call is a no-op (no backup written).
func MigrateFS(ctx context.Context, path string, migrations fs.FS) error {
	sqldb, err := Open(path)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	// Provider is instance-scoped (no goose global mutable state, per AGENTS
	// style). We pass an already-open *sql.DB, so the dialect only selects the
	// version-table SQL — the "sqlite" driver of db.Open is untouched.
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqldb, migrations)
	if err != nil {
		return fmt.Errorf("migrate: new provider: %w", err)
	}

	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("migrate: read versions: %w", err)
	}
	if current == target {
		return nil // Up to date: no backup, no apply.
	}

	// Back up an existing db before mutating it. current == 0 means brand-new
	// (no prior migrations applied): nothing to preserve, so skip.
	if current > 0 {
		if err := backup(ctx, sqldb, path, current); err != nil {
			return fmt.Errorf("migrate: backup: %w", err)
		}
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: apply: %w", err)
	}
	return nil
}

// backup writes a consistent snapshot of sqldb to <path>.pre-<version>.bak.
// VACUUM INTO produces a clean, fully-checkpointed copy regardless of WAL
// state, unlike copying the main file (which may omit pages held in the WAL).
// VACUUM cannot run inside a transaction and its target is a filename literal
// (not a bindable parameter), so the path is embedded with SQL-quote escaping.
func backup(ctx context.Context, sqldb *sql.DB, path string, version int64) error {
	bak := fmt.Sprintf("%s.pre-%d.bak", path, version)
	quoted := "'" + strings.ReplaceAll(bak, "'", "''") + "'"
	if _, err := sqldb.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("vacuum into %s: %w", bak, err)
	}
	return nil
}
