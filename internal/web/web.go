// Package web is the HTTP layer: the route registry, middleware, handlers, and
// embedded templates/static assets. This is the only place the static FS is
// embedded (AGENTS repository layout).
package web

import (
	"embed"
	"encoding/json"
	"net/http"
)

// staticFS holds the embedded static assets served under /static/. Files keep
// their "static/" path prefix, so serving strips the prefix to match.
//
//go:embed static
var staticFS embed.FS

// Handler builds the application's HTTP handler. version is surfaced by
// /healthz (it is set at release via -ldflags -X main.version=...). Later
// phases mount authenticated routes, templates, and reports through the route
// registry (p06.3); phase 0 only needs health and static assets.
func Handler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(version))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	return mux
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
