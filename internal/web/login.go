package web

import (
	"database/sql"
	"errors"
	"net/http"

	"cuento/internal/auth"
)

// decoyHash is a real argon2id hash of a throwaway password, computed once at
// package init. The unknown-user (and passwordless-user) login path verifies the
// submitted password against THIS hash and discards the result, so it spends the
// same ~argon2 time as a genuine verify. Without it, an early return on unknown
// username would be a timing oracle that enumerates valid usernames (rule 13).
var decoyHash = mustDecoyHash()

func mustDecoyHash() string {
	h, err := auth.Hash("cuento-login-decoy-password")
	if err != nil {
		// Hashing a constant cannot fail in practice; a failure here is a broken
		// build environment, so fail loudly at startup rather than silently
		// weakening the no-enumeration guarantee.
		panic("web: build decoy hash: " + err.Error())
	}
	return h
}

// loginData is the login page's template model. Error is a catalog KEY (rule 9),
// empty when the page is first shown; the template renders it via {{t}}.
type loginData struct {
	Error string
}

// loginPage renders the login form. If the request already resolves to a
// logged-in user, it redirects to the app root instead of showing the form —
// this is also how the login/session tests confirm "subsequent request
// authenticated" without depending on the p06.3 route registry.
func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	if currentUser(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.tmpl", loginData{})
}

// loginSubmit authenticates a POST /login. The flow (rule 13):
//
//  1. rate-limit by IP+username FIRST, before any argon2 work, so a brute-force
//     attempt is throttled without paying the hashing cost (and answered 429);
//  2. look up the user; on unknown user OR a passwordless user, verify against
//     the decoy hash so timing does not leak existence;
//  3. verify the password; a disabled user is rejected like a bad password;
//  4. on success RenewToken (defeats session fixation) + store the user id, then
//     redirect;
//  5. on ANY failure, render the SAME uniform error (auth.invalid) — unknown
//     user, wrong password, and disabled account are indistinguishable.
func (s *server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, "login.tmpl", loginData{Error: "auth.invalid"})
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	if !s.limiter.allow(clientIP(r), username) {
		// Over the limit: do no auth work at all. Answer 429 with the login page
		// carrying a rate-limit message.
		s.render(w, r, http.StatusTooManyRequests, "login.tmpl", loginData{Error: "auth.rate_limited"})
		return
	}

	ok, uid := s.authenticate(r, username, password)
	if !ok {
		// Uniform failure: identical status + body for unknown user, wrong
		// password, and disabled account (no user enumeration).
		s.render(w, r, http.StatusUnauthorized, "login.tmpl", loginData{Error: "auth.invalid"})
		return
	}

	// Success: rotate the token (session fixation defense) then bind the identity
	// into the session. Only the user id is persisted server-side; the UI language
	// is resolved each request from the stored user's locale (resolveLang), so a
	// session-level locale copy would be a dead store that drifts from the setting.
	ctx := r.Context()
	if err := s.sessions.RenewToken(ctx); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	s.sessions.Put(ctx, sessionUserKey, uid)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// authenticate performs the constant-shape credential check. It ALWAYS runs one
// argon2id verify (against the real hash when the user exists with a password,
// otherwise against the decoy) so its timing does not depend on whether the
// username exists. It returns ok=false for unknown user, missing password hash,
// disabled account, or wrong password — the caller renders one uniform error for
// all of them.
func (s *server) authenticate(r *http.Request, username, password string) (ok bool, uid int64) {
	creds, err := s.store.CredentialsByUsername(r.Context(), username)
	if errors.Is(err, sql.ErrNoRows) {
		// No such user: spend the verify time on the decoy, then fail.
		_, _ = auth.Verify(password, s.decoyHash)
		return false, 0
	}
	if err != nil {
		// A real DB error: still fail closed, but spend the decoy time so the
		// error path is not a timing tell either.
		_, _ = auth.Verify(password, s.decoyHash)
		return false, 0
	}

	hash := s.decoyHash
	real := creds.PasswordHash != nil && !creds.Disabled
	if real {
		hash = *creds.PasswordHash
	}

	match, verr := auth.Verify(password, hash)
	if verr != nil || !match || !real {
		return false, 0
	}
	return true, creds.ID
}

// logout destroys the session (clearing the server-side row and expiring the
// cookie) and redirects to the login page. Destroying is idempotent — logging
// out with no session is harmless.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
