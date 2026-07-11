package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"cuento/internal/i18n"
	"cuento/internal/store"
)

// contentSecurityPolicy is the strict CSP every response carries (rule 12, D9).
// It permits ONLY same-origin resources and NO inline script: default-src,
// script-src and style-src are 'self' only, so an injected <script> or inline
// handler cannot execute. object-src 'none' kills plugins; base-uri 'none'
// blocks <base> hijacking; frame-ancestors 'none' forbids framing (clickjacking
// defense, superseding X-Frame-Options); form-action 'self' keeps form posts on
// our origin. Vendored htmx and our hand-written ES modules load as same-origin
// files (phase 10), so 'self' is sufficient — no 'unsafe-inline' ever.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'"

// ctxKey is an unexported context-key type so no other package can read or
// collide with the values the auth/lang middleware attaches.
type ctxKey int

const (
	ctxKeyUser ctxKey = iota // *store.CurrentUser (nil == anonymous)
	ctxKeyLang               // resolved UI language string
)

// chain composes the security middleware around next, in the load-bearing order
// (rule 13, D9):
//
//	secureHeaders -> crossOrigin -> session(LoadAndSave) -> auth -> lang -> next
//
// Outermost runs first: headers wrap everything (so even a deep error response
// carries the CSP), then cross-origin protection rejects spoofed cross-site
// mutations before any session or business work, then scs loads/saves the
// session, then auth resolves the current user from it, then lang binds the UI
// language. next (the mux) sees a fully-decorated request.
func (s *server) chain(next http.Handler) http.Handler {
	h := s.langMiddleware(next)
	h = s.authMiddleware(h)
	h = s.sessions.LoadAndSave(h)
	h = crossOrigin(h)
	h = secureHeaders(h)
	return h
}

// secureHeaders sets the response security headers on every request (rule 13;
// the full cross-route sweep is p18.4). It sets them BEFORE calling next so they
// are present even if a downstream handler writes headers and a body. HTML
// cache posture (no-store) is applied by the HTML handlers themselves, not here,
// so static assets and /healthz keep their own caching (p10 owns asset caching).
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// crossOrigin wraps next with the stdlib Cross-Origin Protection (Go 1.25, D9):
// it rejects non-safe (mutating) cross-site browser requests using the
// Sec-Fetch-Site header / Origin-vs-Host comparison. Safe methods (GET/HEAD/
// OPTIONS) and same-origin or non-browser requests pass through; a spoofed
// `Sec-Fetch-Site: cross-site` on a POST is answered 403. Nothing is
// hand-rolled — this is the whole CSRF defense (rule 13).
func crossOrigin(next http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(next)
}

// authMiddleware resolves the current user from the session and attaches it to
// the request context (nil == anonymous). It reads the user id scs stored at
// login, then re-loads the identity from the store each request so permission
// and locale changes take effect without re-login. A session pointing at a
// missing or disabled user is treated as anonymous (the stale/forged-session
// case); enforcement of which routes REQUIRE a user is p06.3 — this step only
// RESOLVES identity and protects /login itself.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if id := s.sessions.GetInt64(ctx, sessionUserKey); id != 0 {
			cu, err := s.store.UserByID(ctx, id)
			switch {
			case err == nil && !cu.Disabled:
				ctx = context.WithValue(ctx, ctxKeyUser, &cu)
			case errors.Is(err, sql.ErrNoRows) || (err == nil && cu.Disabled):
				// Stale/forged/disabled: drop the identity for this request. The
				// session cookie is left as-is; a re-login (or logout) rewrites
				// it. Treated as anonymous, not an error.
			default:
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// langMiddleware resolves the request's UI language and attaches it to the
// context (D14): logged-in user's locale setting, else the `lang` query param or
// cookie, else the base language (en). Only known languages (i18n.Langs) are
// honored; anything else falls through to the next source. The resolved lang is
// what render()'s per-request `t` binds to.
func (s *server) langMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := s.resolveLang(r)
		ctx := context.WithValue(r.Context(), ctxKeyLang, lang)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveLang applies the D14 precedence: user setting -> lang query -> lang
// cookie -> en. The query beats the cookie so the login page's ?lang= switch is
// immediate; a logged-in user's stored locale wins over both (their setting is
// authoritative once authenticated).
func (s *server) resolveLang(r *http.Request) string {
	if cu := currentUser(r.Context()); cu != nil && known(cu.Locale) {
		return cu.Locale
	}
	if q := r.URL.Query().Get("lang"); known(q) {
		return q
	}
	if c, err := r.Cookie("lang"); err == nil && known(c.Value) {
		return c.Value
	}
	return "en"
}

// known reports whether lang is one of the catalog languages (i18n.Langs).
func known(lang string) bool {
	for _, l := range i18n.Langs() {
		if l == lang {
			return true
		}
	}
	return false
}

// currentUser returns the identity the auth middleware resolved, or nil when the
// request is anonymous. Handlers and (via render) templates read it through
// here.
func currentUser(ctx context.Context) *store.CurrentUser {
	cu, _ := ctx.Value(ctxKeyUser).(*store.CurrentUser)
	return cu
}

// langOf returns the UI language the lang middleware resolved for the request,
// defaulting to en if (impossibly) unset so render never binds an empty lang.
func langOf(ctx context.Context) string {
	if l, ok := ctx.Value(ctxKeyLang).(string); ok && l != "" {
		return l
	}
	return "en"
}
