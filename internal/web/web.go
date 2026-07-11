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
		limiter:  newLoginLimiter(),
		// A single fixed decoy hash lets the unknown-user login path spend the
		// same argon2id time as a real verify, closing the timing side channel
		// that would otherwise enumerate usernames (rule 13). Built once at
		// startup; see verifyDecoy.
		decoyHash: decoyHash,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(cfg.Version))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("GET /login", srv.loginPage)
	mux.HandleFunc("POST /login", srv.loginSubmit)
	mux.HandleFunc("POST /logout", srv.logout)

	// The chain order is load-bearing (rule 13): secure headers first so every
	// response (including errors thrown deeper) carries them; cross-origin next
	// so a spoofed cross-site mutating request is rejected before any session or
	// business work; then session load/save; then identity resolution; then
	// language. See chain() for the composition.
	handler := srv.chain(mux)

	return &App{handler: handler, sessions: sessions}
}

// server bundles the dependencies the handlers and middleware share. It is the
// per-application state; there is no package-level mutable state (AGENTS Style).
type server struct {
	cfg       Config
	store     *store.Store
	sessions  *scs.SessionManager
	tmpl      *htmltemplate.Template
	limiter   *loginLimiter
	decoyHash string
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
