// Package web is the HTTP layer: the route registry, middleware, handlers, and
// embedded templates/static assets. This is the only place the static FS is
// embedded (AGENTS repository layout).
//
// p06.2 turns the phase-0 hello server into the security spine every later web
// step inherits: a scs session manager backed by the goose-managed `sessions`
// table, and a middleware chain (secure headers -> cross-origin protection ->
// session load/save -> auth identity -> language resolution) that every route
// runs through. Login/logout live here with a rate limiter and uniform auth
// errors (no user enumeration). The formal route registry + permission matrix is
// p06.3; here we mount /login, /logout, /healthz and /static and resolve
// identity so p06.3 can layer enforcement on top.
package web

import (
	"database/sql"
	"embed"
	"encoding/json"
	htmltemplate "html/template"
	"net/http"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/reports"
	"cuento/internal/store"
)

// staticFS holds the embedded static assets served under /static/. Files keep
// their "static/" path prefix, so serving strips the prefix to match.
//
//go:embed static
var staticFS embed.FS

// Config carries the web layer's construction-time settings. Version is surfaced
// by /healthz (set at release via -ldflags). Dev flips security posture that
// only makes sense over TLS: when Dev is true the session cookie's Secure
// attribute is OFF so the dev server works over plain HTTP (rule 13, D9);
// everywhere else Secure is ON.
type Config struct {
	Version string
	Dev     bool
}

// App is the built web application: the constructed handler plus the session
// manager (retained so cmd/cuento can start/stop its background cleanup cleanly
// in later ops work; today Handler is all callers need).
type App struct {
	handler  http.Handler
	sessions *scs.SessionManager
	srv      *server // retained for in-package tests that enumerate the registry
}

// Handler returns the composed HTTP handler. See NewApp for the wiring.
func Handler(cfg Config, db *sql.DB, st *store.Store) http.Handler {
	return NewApp(cfg, db, st).handler
}

// NewApp builds the web application: it constructs the scs session manager over
// the sessions table (db), wires the login/logout handlers and public routes
// onto a mux, and wraps everything in the security middleware chain. st is the
// single writer/read funnel handlers use to authenticate and resolve identity.
func NewApp(cfg Config, db *sql.DB, st *store.Store) *App {
	sessions := newSessionManager(db, cfg.Dev)

	srv := &server{
		cfg:      cfg,
		store:    st,
		sessions: sessions,
		tmpl:     mustParseTemplates(),
		// The report registry is assembled ONCE at startup (p15.1): every shipped
		// report registered, iterable so routes() auto-mounts one route pair per
		// report and the index lists them. reports.Default panics on a malformed
		// registration -- a build-time defect surfaced at startup, like template
		// parsing (mustParseTemplates).
		reports: reports.Default(),
		// The asset manifest is built ONCE here (a single walk of the embedded
		// static FS), not per request: it maps each asset's logical name to its
		// content-hashed URL and back (p10.1).
		assets:  buildAssetManifest(),
		limiter: newLoginLimiter(),
		// A single fixed decoy hash lets the unknown-user login path spend the
		// same argon2id time as a real verify, closing the timing side channel
		// that would otherwise enumerate usernames (rule 13). Built once at
		// startup; see verifyDecoy.
		decoyHash: decoyHash,
	}

	// Mount is the ONLY place routes attach to a mux (rule 8): it builds the mux
	// from the route registry alone, wrapping every route in the permission
	// enforcement middleware and the load-bearing security chain (secureHeaders ->
	// crossOrigin -> session -> auth -> lang). Nothing is mounted outside it --
	// health, static, login, logout and the landing page are all registry entries.
	handler := srv.Mount()

	return &App{handler: handler, sessions: sessions, srv: srv}
}

// server bundles the dependencies the handlers and middleware share. It is the
// per-application state; there is no package-level mutable state (AGENTS Style).
type server struct {
	cfg       Config
	store     *store.Store
	sessions  *scs.SessionManager
	tmpl      *htmltemplate.Template
	assets    *assetManifest
	limiter   *loginLimiter
	decoyHash string
	reports   *reports.Registry
}

// healthz reports liveness as JSON: {"status":"ok","version":<version>}.
func healthz(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Encode into a buffer-free path; on failure the status is already 200,
		// but a health probe that can't marshal a two-field struct is a bug, not
		// a runtime condition — so just best-effort write.
		_ = json.NewEncoder(w).Encode(struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}{Status: "ok", Version: version})
	}
}
