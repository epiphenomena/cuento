package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"cuento/internal/store"
	"cuento/internal/testutil"
)

// newTestHandler builds the real mounted handler over a migrated temp db, the
// way p06.2 onward exercise the web layer (AGENTS testing conventions: hit the
// real router via httptest, no store mocks). cfg lets a test flip -dev.
func newTestHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	db := testutil.NewDB(t)
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	return Handler(cfg, db, store.New(db))
}

func TestHealthz(t *testing.T) {
	const version = "test-1.2.3"
	h := newTestHandler(t, Config{Version: version})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	if body.Version != version {
		t.Errorf("version = %q, want %q", body.Version, version)
	}
}

func TestStaticEmbedded(t *testing.T) {
	h := newTestHandler(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (asset should serve from the embedded static FS)", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() == 0 {
		t.Errorf("empty body; expected embedded app.css content")
	}
}
