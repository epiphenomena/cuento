package web

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
)

// sessionLifetime is the absolute maximum age of a session before it must be
// re-established, independent of activity. Twelve hours comfortably covers a
// working day for the bookkeepers (D8) without leaving a session valid
// indefinitely.
const sessionLifetime = 12 * time.Hour

// sessionIdleTimeout expires a session after this much inactivity, so an
// abandoned browser tab does not stay authenticated for the full lifetime.
const sessionIdleTimeout = 2 * time.Hour

// sessionUserKey is the scs data key under which the authenticated user's id is
// stored. Only the id is persisted server-side; the middleware re-reads the
// full identity from the store each request so permission/locale changes take
// effect without re-login.
const sessionUserKey = "user_id"

// newSessionManager builds the scs SessionManager backed by the goose-managed
// `sessions` table via sqlite3store (D9). Cookie posture follows rule 13:
// HttpOnly and SameSite=Lax (scs defaults, set explicitly for clarity and to
// pin them against a library default change), Path="/", and Secure ON except in
// -dev where the dev server speaks plain HTTP.
//
// sqlite3store.New starts a background cleanup goroutine that deletes expired
// rows every 5 minutes; because goose (not scs) created the table, that cleanup
// operates on our canonical schema.
func newSessionManager(db *sql.DB, dev bool) *scs.SessionManager {
	m := scs.New()
	m.Store = sqlite3store.New(db)
	m.Lifetime = sessionLifetime
	m.IdleTimeout = sessionIdleTimeout

	m.Cookie.Name = "cuento_session"
	m.Cookie.Path = "/"
	m.Cookie.HttpOnly = true
	m.Cookie.SameSite = http.SameSiteLaxMode
	m.Cookie.Secure = !dev // Secure everywhere except -dev (rule 13, D9).

	return m
}
