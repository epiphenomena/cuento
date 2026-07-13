package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeadersEveryRoute is the p18.4 hardening sweep (rule 13): EVERY
// registered route's response must carry the full security-header set. Because
// secureHeaders is the OUTERMOST middleware in chain(), the headers are written
// before enforce() or any handler runs, so they are present regardless of the
// response status — 200, 302 (anon->login), 403, the /healthz JSON, the /static
// asset, the ops-backup octet-stream. The sweep is therefore STATUS-INDEPENDENT:
// it iterates the live registry (like the permission matrix, never a hardcoded
// list, so a route added in a later phase is covered with zero edits) as the Admin
// persona and asserts the header VALUES against the middleware's own constants —
// so the test and the middleware cannot drift. A single missing or altered header
// on any route fails here.
//
// This is a lock-in test: the middleware already sets these headers on the whole
// mux, so the sweep passes on first run. Its job is to keep them locked across all
// current and future routes, not to drive new code.
func TestSecurityHeadersEveryRoute(t *testing.T) {
	h, registry, st, db, sm := newMatrixApp(t)
	personas := buildPersonas(t, st, db)
	admin := personas[len(personas)-1]
	if admin.name != "Admin" {
		t.Fatalf("expected last persona to be Admin, got %q", admin.name)
	}

	// The headers every response must carry, asserted by exact value against the
	// middleware's own source of truth (contentSecurityPolicy) so the two cannot
	// silently diverge. Strict-Transport-Security is NOT here: it is prod-only and
	// this test app is built without -dev-vs-prod distinction (Config.Dev false ==
	// prod), so it is covered separately by TestHSTSDevVsProd to keep this sweep
	// focused on the always-on set.
	want := map[string]string{
		"Content-Security-Policy":    contentSecurityPolicy,
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
	}

	for _, r := range registry {
		rec := doAs(t, h, sm, admin, r)
		for name, exp := range want {
			if got := rec.Header().Get(name); got != exp {
				t.Errorf("%s %s: header %s = %q, want %q (status %d)",
					r.Method, r.Pattern, name, got, exp, rec.Code)
			}
		}
	}
}

// TestHSTSDevVsProd pins the one status of the one gated header: Strict-Transport-
// Security is emitted in production (the autocert TLS deployment, p18.2) and
// ABSENT under -dev (the plain-HTTP dev server), mirroring the Secure-cookie gate.
// Sending HSTS over -dev's http would pin the browser to https for a host that
// only speaks http, breaking local development.
func TestHSTSDevVsProd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dev     bool
		wantHdr string // "" == header must be absent
	}{
		{"prod emits HSTS", false, hstsMaxAge},
		{"dev omits HSTS", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, Config{Dev: tc.dev})
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get("Strict-Transport-Security")
			if got != tc.wantHdr {
				t.Errorf("Strict-Transport-Security = %q, want %q", got, tc.wantHdr)
			}
			// The always-on headers are present in BOTH modes.
			if csp := rec.Header().Get("Content-Security-Policy"); csp != contentSecurityPolicy {
				t.Errorf("Content-Security-Policy = %q, want the strict policy", csp)
			}
		})
	}
}
