// Package db owns the SQLite connection: Open configures the DSN and pragmas
// (this is the only place pragmas are set — AGENTS repository layout), and later
// phases add the embedded goose migrations and sqlc-generated queries here.
package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	// Registers the "sqlite" driver for database/sql. Pure Go, CGO-free (D7).
	_ "modernc.org/sqlite"
)

// busyTimeout is how long a connection waits for a lock before returning
// SQLITE_BUSY. SQLite is a single writer; a few seconds absorbs the brief
// contention between our one writer and concurrent readers under WAL.
const busyTimeout = 5 * time.Second

// Open opens (creating if absent) the SQLite database at path and returns a
// pooled *sql.DB with pragmas applied to every connection.
//
// Per-connection pragmas (foreign_keys, busy_timeout, synchronous) are set via
// modernc's _pragma DSN parameters so they apply to each connection the pool
// opens, not just the first — the robust pattern with database/sql pooling.
// journal_mode=WAL is a persistent database-level setting; setting it per
// connection is harmless. synchronous(1) is NORMAL, the right durability level
// under WAL. The DSN readbacks in the package tests verify this empirically.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(" + fmt.Sprint(busyTimeout.Milliseconds()) + ")" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(1)"

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// SQLite allows exactly one writer at a time; the store funnels all writes
	// through a single path, so a small pool suffices. Because correctness rides
	// on the DSN pragmas (applied to every connection), allowing a few pooled
	// connections for concurrent WAL readers is safe.
	sqldb.SetMaxOpenConns(4)
	sqldb.SetMaxIdleConns(4)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)

	// Force a real connection so a bad path fails here, not on first query.
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("open db: ping: %w", err)
	}

	return sqldb, nil
}
