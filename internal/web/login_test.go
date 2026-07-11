package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuento/internal/auth"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// testApp builds a real handler over a migrated temp db plus a store, and seeds
// one user with password "correct-horse". It returns the handler and the store
// (some tests need it for setup). Uses the system actor (id 1) so CreateUser's
// write funnel has an actor.
func testApp(t *testing.T, cfg Config) (http.Handler, *store.Store) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if cfg.Version == "" {
		cfg.Version = "test"
	}

	hash, err := auth.Hash("correct-horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if _, err := st.CreateUser(ctx, store.CreateUserInput{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: &hash,
		TxnPerm:      "write",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	return Handler(cfg, db, st), st
}

// postLogin submits credentials from a fixed remote IP and returns the recorder.
func postLogin(t *testing.T, h http.Handler, ip, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := "username=" + username + "&password=" + password
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionCookie returns the first Set-Cookie value named cuento_session, or "".
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "cuento_session" {
			return c
		}
	}
	return nil
}

func TestLoginSuccessSetsSession(t *testing.T) {
	h, _ := testApp(t, Config{})

	rec := postLogin(t, h, "10.0.0.1", "alice", "correct-horse")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	cookie := sessionCookie(rec)
	if cookie == nil {
		t.Fatal("no session cookie set on successful login")
	}

	// A subsequent request carrying the cookie is authenticated: GET /login on an
	// authenticated request redirects (loginPage sees a current user).
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("authenticated GET /login status = %d, want %d (should redirect when logged in)", rec2.Code, http.StatusSeeOther)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h, _ := testApp(t, Config{})

	// Wrong password for a real user, and an unknown user, must be
	// indistinguishable: same status and same body (no user enumeration).
	wrongPw := postLogin(t, h, "10.0.0.2", "alice", "nope")
	unknown := postLogin(t, h, "10.0.0.3", "nobody", "whatever")

	if wrongPw.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want %d", wrongPw.Code, http.StatusUnauthorized)
	}
	if unknown.Code != wrongPw.Code {
		t.Fatalf("unknown-user status = %d, want %d (must match wrong-password)", unknown.Code, wrongPw.Code)
	}
	if unknown.Body.String() != wrongPw.Body.String() {
		t.Fatalf("unknown-user body differs from wrong-password body (user enumeration):\n unknown=%q\n wrong  =%q",
			unknown.Body.String(), wrongPw.Body.String())
	}
	if sessionCookie(wrongPw) != nil {
		t.Error("a failed login set a session cookie")
	}
}

func TestLoginRateLimited(t *testing.T) {
	h, _ := testApp(t, Config{})

	// From one IP+username, keep submitting wrong passwords; after the burst is
	// exhausted the limiter must answer 429 before doing auth work.
	var got429 bool
	for i := 0; i < loginBurst+3; i++ {
		rec := postLogin(t, h, "10.0.0.9", "alice", "nope")
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("never saw 429 after %d attempts; rate limit not enforced", loginBurst+3)
	}
}

func TestCookieFlags(t *testing.T) {
	// The session cookie is only emitted when the session is modified — i.e. on a
	// successful login. Assert flags there, for both -dev off and on.
	cases := []struct {
		name       string
		dev        bool
		wantSecure bool
	}{
		{"prod-secure", false, true},
		{"dev-insecure", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := testApp(t, Config{Dev: tc.dev})
			rec := postLogin(t, h, "10.0.0.1", "alice", "correct-horse")
			c := sessionCookie(rec)
			if c == nil {
				t.Fatal("no session cookie on successful login")
			}
			if !c.HttpOnly {
				t.Error("session cookie missing HttpOnly")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v, want Lax", c.SameSite)
			}
			if c.Secure != tc.wantSecure {
				t.Errorf("Secure = %v, want %v (dev=%v)", c.Secure, tc.wantSecure, tc.dev)
			}
		})
	}
}

func TestCrossOriginBlocked(t *testing.T) {
	h, _ := testApp(t, Config{})

	// A mutating request (POST) spoofing a cross-site fetch must be rejected with
	// 403 before any auth work (stdlib CrossOriginProtection).
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("username=alice&password=correct-horse"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.RemoteAddr = "10.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _ := testApp(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	hdr := rec.Header()
	csp := hdr.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("missing Content-Security-Policy")
	}
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows unsafe-inline: %q", csp)
	}
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := hdr.Get("Referrer-Policy"); got == "" {
		t.Error("missing Referrer-Policy")
	}
}

func TestLoginPageLocalized(t *testing.T) {
	h, _ := testApp(t, Config{})

	// Default (en): the English sign-in title is present.
	reqEN := httptest.NewRequest(http.MethodGet, "/login", nil)
	recEN := httptest.NewRecorder()
	h.ServeHTTP(recEN, reqEN)
	if !strings.Contains(recEN.Body.String(), "Sign in") {
		t.Errorf("en login page missing English title; body=%q", recEN.Body.String())
	}

	// ?lang=es renders the Spanish catalog strings.
	reqES := httptest.NewRequest(http.MethodGet, "/login?lang=es", nil)
	recES := httptest.NewRecorder()
	h.ServeHTTP(recES, reqES)
	if !strings.Contains(recES.Body.String(), "Iniciar sesion") {
		t.Errorf("es login page missing Spanish title; body=%q", recES.Body.String())
	}
	if strings.Contains(recES.Body.String(), "Sign in") {
		t.Errorf("es login page still shows English title; body=%q", recES.Body.String())
	}
}
