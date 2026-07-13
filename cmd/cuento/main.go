// Command cuento is the single binary for the whole application. The first CLI
// argument selects a subcommand. Phase 0 implements serve; migrate arrives in
// p01.2; user arrives in p06.4; check arrives in p08.3; ratesync arrives in p14.2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cuento/internal/db"
	"cuento/internal/store"
	"cuento/internal/web"
)

// defaultDBPath is where the SQLite file lives when no -db flag is given. serve
// and migrate share it so they operate on the same database by default.
const defaultDBPath = "cuento.db"

// version is the build version, overridden at release via
// -ldflags "-X main.version=..." (the Makefile `release` target sets it from
// `git describe`). It flows into web.Config.Version, which surfaces it on
// /healthz and in the app footer (p18.1). Defaults to "dev" for plain builds.
var version = "dev"

// stdout is the writer command output goes to (indirected so tests can capture
// it). Defaults to os.Stdout.
var stdout io.Writer = os.Stdout

// exitError carries a specific process exit code up to main from a subcommand
// that fails deliberately (not a Go error to log). `cuento check` returns it so
// a ledger with violations exits non-zero without printing a spurious log line.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		if err := serve(args); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "migrate":
		if err := migrate(args); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	case "user":
		if err := userCmd(args); err != nil {
			log.Fatalf("user: %v", err)
		}
	case "ratesync":
		if err := ratesyncCmd(args); err != nil {
			log.Fatalf("ratesync: %v", err)
		}
	case "check":
		if err := checkCmd(args); err != nil {
			// A deliberate non-zero exit (ledger violations) carries its own
			// code and needs no log line -- the violations were already printed.
			var ee exitError
			if errors.As(err, &ee) {
				os.Exit(ee.code)
			}
			log.Fatalf("check: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "cuento: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: cuento <command> [flags]\n\ncommands:\n  serve     run the HTTP server (auto-migrates on start; -dev relaxes cookie Secure)\n  migrate   apply pending database migrations\n  user      manage users (add|passwd|disable)\n  check     run the ledger integrity suite ([-db PATH] [--strict])\n  ratesync  fetch configured currency pairs from Yahoo Finance into exchange rates ([-db PATH])\n")
}

// migrate applies any pending embedded migrations to the configured database
// (backing the file up first when it already carries schema, AGENTS rule 4).
func migrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, *dbPath); err != nil {
		return err
	}
	log.Printf("migrations applied to %s", *dbPath)
	return nil
}

// serve runs the HTTP server until SIGINT/SIGTERM, then shuts down gracefully.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	// -dev relaxes the session cookie's Secure attribute so the dev server works
	// over plain HTTP (rule 13, D9). The Makefile `run` target passes it.
	dev := fs.Bool("dev", false, "development mode: session cookie not marked Secure (plain HTTP)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Auto-migrate before listening so the running binary always matches its
	// schema (backup-before-apply is handled inside db.Migrate).
	if err := db.Migrate(ctx, *dbPath); err != nil {
		return err
	}

	// Open the pooled handle the web layer needs: the store (single writer/read
	// funnel) and the scs session store both operate over it.
	sqldb, err := db.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = sqldb.Close() }()

	st := store.New(sqldb)

	// Sync the code-declared report groups into the report_groups reference table
	// (D10). Idempotent and outside the write funnel (reference data, like
	// currencies); safe on every boot. The route registry's ReportGroup perms
	// reference these names, so this runs before we start serving.
	if err := web.SyncReportGroups(ctx, st); err != nil {
		return fmt.Errorf("sync report groups: %w", err)
	}

	// Bootstrap hint: with no human users (only the seeded system user id 1), the
	// operator cannot log in yet. Log a friendly pointer to create the first
	// admin. Operator-facing console output, not a UI string (no i18n catalog
	// entry); a lookup failure is non-fatal (never block startup on a hint).
	if n, err := st.CountHumanUsers(ctx); err != nil {
		log.Printf("bootstrap check failed (non-fatal): %v", err)
	} else if n == 0 {
		log.Print("no users yet: create the first admin with `cuento user add <username> --admin`")
	}

	handler := web.Handler(web.Config{Version: version, Dev: *dev}, sqldb, st)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("cuento %s listening on %s", version, *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
