// Command cuento is the single binary for the whole application. The first CLI
// argument selects a subcommand. Phase 0 implements serve; migrate arrives in
// p01.2; check, user, and ratesync arrive in later phases (p08.3, p06.4, p14.2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
// -ldflags "-X main.version=...". It is not wired to ldflags yet (p18.1).
var version = "dev"

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
	default:
		fmt.Fprintf(os.Stderr, "cuento: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: cuento <command> [flags]\n\ncommands:\n  serve     run the HTTP server (auto-migrates on start; -dev relaxes cookie Secure)\n  migrate   apply pending database migrations\n")
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
