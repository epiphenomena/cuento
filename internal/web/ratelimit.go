package web

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Login rate-limit parameters (rule 13, D9). The limiter is keyed by IP+username
// so one attacker cannot lock out a victim across the whole site, and a single
// (ip, username) pair is throttled independently. Burst covers a handful of
// legitimate typo retries; refill is slow so sustained guessing is choked.
const (
	loginBurst    = 5               // immediate attempts allowed per (ip, username)
	loginRefill   = time.Minute / 2 // one token back every 30s (2/min sustained)
	limiterMaxKey = 4096            // hard cap on tracked keys (bounded memory)
)

// loginLimiter throttles POST /login attempts per (ip, username). It is a small
// bounded map of token-bucket limiters guarded by a mutex — no background
// eviction goroutine; when the map hits limiterMaxKey it is cleared wholesale
// (the simplest bound that stays correct: the worst case is that a burst of
// distinct keys briefly resets everyone's budget, which only ever LOOSENS the
// limit and never locks a legitimate user out).
type loginLimiter struct {
	mu   sync.Mutex
	keys map[string]*rate.Limiter
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{keys: make(map[string]*rate.Limiter)}
}

// allow reports whether an attempt from ip for username may proceed, consuming a
// token when it returns true. It is safe for concurrent use.
func (l *loginLimiter) allow(ip, username string) bool {
	key := ip + "\x00" + username

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.keys) >= limiterMaxKey {
		l.keys = make(map[string]*rate.Limiter)
	}

	lim, ok := l.keys[key]
	if !ok {
		lim = rate.NewLimiter(rate.Every(loginRefill), loginBurst)
		l.keys[key] = lim
	}
	return lim.Allow()
}

// clientIP extracts the client IP for rate-limit keying. It uses only the direct
// remote address (never a spoofable forwarded header), stripping the port; when
// there is no proxy this is the real client, and a reverse proxy (D8/D2 deploy)
// terminates locally so the direct peer is stable. Falls back to the raw
// RemoteAddr string if it has no port.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
