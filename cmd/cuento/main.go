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
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

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
	case "expense-report":
		if err := expenseReportCmd(args); err != nil {
			log.Fatalf("expense-report: %v", err)
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
	fmt.Fprintf(os.Stderr, "usage: cuento <command> [flags]\n\ncommands:\n  serve     run the HTTP server (auto-migrates on start; -dev relaxes cookie Secure)\n  migrate   apply pending database migrations\n  user      manage users (add|passwd|disable)\n  check     run the ledger integrity suite ([-db PATH] [--strict])\n  ratesync  fetch configured currency pairs from Yahoo Finance into exchange rates ([-db PATH])\n  expense-report  maintenance over expense reports (reject <id> --reason ...) ([-db PATH])\n")
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

// newServeFlags builds the serve subcommand's flag set. Each of the four
// configurable knobs (data dir, addr, domain, dev) has a flag that mirrors a
// CUENTO_* env var; -db is an explicit override with no env twin (so the e2e
// harness's `serve -dev -db <path>` keeps resolving to one physical file).
// Factored out so config_test.go drives the exact same parse path resolveConfig
// sees at runtime — the flag DEFAULTS must never clobber a set env var, and the
// test proves it.
func newServeFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.String("data-dir", "", "data directory (db, autocert cache, litestream replica); env CUENTO_DATA_DIR")
	fs.String("addr", "", "plain-HTTP listen address when not serving TLS; env CUENTO_ADDR")
	fs.String("domain", "", "TLS hostname: if set, serve HTTPS on :443 with a :80 redirect via autocert; env CUENTO_DOMAIN")
	// -db overrides the derived <data-dir>/cuento.db. No env twin by design.
	fs.String("db", "", "path to the SQLite database file (default <data-dir>/cuento.db)")
	// -dev relaxes the session cookie's Secure attribute so the dev server works
	// over plain HTTP (rule 13, D9). The Makefile `run` target passes it.
	fs.Bool("dev", false, "development mode: session cookie not marked Secure (plain HTTP); env CUENTO_DEV")
	return fs
}

// osEnv snapshots the CUENTO_* environment into the map resolveConfig consumes.
// Keeping resolveConfig env-driven (not calling os.Getenv itself) is what lets
// the config test stay pure.
func osEnv() map[string]string {
	keys := []string{"CUENTO_DATA_DIR", "CUENTO_ADDR", "CUENTO_DOMAIN", "CUENTO_DEV"}
	env := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env
}

// serve runs the HTTP server until SIGINT/SIGTERM, then shuts down gracefully.
// Configuration comes from CUENTO_* env vars overridden by flags (resolveConfig,
// tested in isolation). With a domain configured it serves HTTPS on :443 via
// autocert plus a :80 ACME/redirect listener; otherwise plain HTTP on the addr.
func serve(args []string) error {
	fs := newServeFlags()
	if err := fs.Parse(args); err != nil {
		// flag.ErrHelp (from -h) is not a failure: usage was already printed.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	cfg := resolveConfig(osEnv(), fs, set)

	// Resolve the data dir to an absolute path so the derived db path and the
	// autocert cache are cwd-independent (p06.4 db.Open note). Done HERE, not in
	// the pure resolveConfig, to keep that function hermetic. An explicit -db is
	// left as given (the e2e harness relies on that).
	if abs, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = abs
		// Re-derive the db path under the now-absolute data dir, unless -db was
		// passed explicitly (that value is used verbatim — the e2e harness's
		// relative -db must keep resolving to its own file).
		if !set["db"] {
			cfg.DBPath = filepath.Join(cfg.DataDir, defaultDBFile)
		}
	}

	// The data dir must exist and be writable: the db, the autocert cache, and
	// the litestream replica all live under it. Create it up front so a bad
	// config (e.g. an unwritable path) fails with a clear error, not a panic
	// mid-startup.
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir %q: %w", cfg.DataDir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Auto-migrate before listening so the running binary always matches its
	// schema (backup-before-apply is handled inside db.Migrate).
	if err := db.Migrate(ctx, cfg.DBPath); err != nil {
		return err
	}

	// Open the pooled handle the web layer needs: the store (single writer/read
	// funnel) and the scs session store both operate over it.
	sqldb, err := db.Open(cfg.DBPath)
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

	handler := web.Handler(web.Config{Version: version, Dev: cfg.Dev}, sqldb, st)

	if cfg.UseTLS() {
		return serveTLS(ctx, cfg, handler)
	}
	return servePlain(ctx, cfg.Addr, handler)
}

// servePlain runs a single plain-HTTP server on addr (the -dev / no-domain
// path), shutting down gracefully on ctx cancellation.
func servePlain(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("cuento %s listening on %s (plain HTTP)", version, addr)
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
		return shutdown(srv)
	}
}

// serveTLS runs the production TLS setup (D8): an autocert manager fronting an
// HTTPS server on :443 whose certificates are provisioned on demand and cached
// in <data-dir>/autocert, plus a :80 listener serving the manager's HTTP handler
// (ACME http-01 challenges + redirect-to-HTTPS for everything else). Both
// servers shut down gracefully on ctx cancellation.
func serveTLS(ctx context.Context, cfg Config, handler http.Handler) error {
	if err := os.MkdirAll(cfg.AutocertCacheDir(), 0o700); err != nil {
		return fmt.Errorf("autocert cache dir %q: %w", cfg.AutocertCacheDir(), err)
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.Domain),
		Cache:      autocert.DirCache(cfg.AutocertCacheDir()),
	}

	httpsSrv := &http.Server{
		Addr:              ":443",
		Handler:           handler,
		TLSConfig:         m.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// :80 serves ACME http-01 challenges and 301-redirects everything else to
	// HTTPS (autocert's HTTPHandler does exactly this when given a nil fallback).
	httpSrv := &http.Server{
		Addr:              ":80",
		Handler:           m.HTTPHandler(nil),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 2)
	go func() {
		log.Printf("cuento %s serving ACME/redirect on :80 for %s", version, cfg.Domain)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http :80: %w", err)
			return
		}
		errc <- nil
	}()
	go func() {
		log.Printf("cuento %s listening on :443 (TLS, autocert cache %s)", version, cfg.AutocertCacheDir())
		// Certs come from the manager's TLSConfig, so ListenAndServeTLS takes no
		// cert/key files.
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("https :443: %w", err)
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		// One listener failed; tear the other down and report.
		_ = shutdown(httpSrv)
		_ = shutdown(httpsSrv)
		return err
	case <-ctx.Done():
		log.Print("shutting down")
		e1 := shutdown(httpSrv)
		e2 := shutdown(httpsSrv)
		if e1 != nil {
			return e1
		}
		return e2
	}
}

// shutdown gracefully stops one server with a bounded timeout.
func shutdown(srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
