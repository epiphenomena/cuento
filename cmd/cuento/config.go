package main

import (
	"flag"
	"path/filepath"
)

// Config is the resolved runtime configuration for `serve`. It is produced by
// resolveConfig from the process environment (the base) and the parsed serve
// flags (which override), so the same knobs work from a systemd unit's
// Environment= lines or an interactive invocation.
//
// The four env vars (p18.2): CUENTO_DATA_DIR (the directory holding the db,
// the autocert cache, and the litestream replica), CUENTO_ADDR (the plain-HTTP
// listen address when not serving TLS), CUENTO_DOMAIN (set => serve TLS via
// autocert on :443 with a :80 redirect), CUENTO_DEV (dev mode: non-Secure
// cookie, plain HTTP).
type Config struct {
	DataDir string // holds the db, autocert cache, litestream replica
	Addr    string // plain-HTTP listen address (ignored when UseTLS)
	Domain  string // TLS hostname; empty => plain HTTP
	Dev     bool   // dev mode: non-Secure cookie, forces plain HTTP
	DBPath  string // resolved SQLite path (explicit -db, else <DataDir>/cuento.db)
}

// Default values applied when neither env nor flag sets a knob.
const (
	defaultDataDir = "."
	defaultAddr    = ":8080"
)

// UseTLS reports whether serve should run autocert on :443 with a :80 redirect.
// TLS is enabled exactly when a domain is configured AND we are not in dev mode:
// a non-Secure session cookie (the point of -dev) over real TLS is incoherent,
// so dev always means plain HTTP even if a domain leaks in from the environment.
func (c Config) UseTLS() bool { return c.Domain != "" && !c.Dev }

// AutocertCacheDir is where autocert.DirCache persists issued certificates and
// ACME account state, under the data dir so it rides the litestream-independent
// persistent disk (certs are cheap to re-issue but a cache avoids Let's Encrypt
// rate limits across restarts).
func (c Config) AutocertCacheDir() string { return filepath.Join(c.DataDir, "autocert") }

// resolveConfig computes the effective Config from environment values (the base)
// and parsed serve flags (overrides). It is a PURE function — no os.Getenv, no
// filesystem, no clock — so TestConfigParsing exercises the full precedence
// matrix without spawning a server or touching disk.
//
// Precedence: a flag wins only when it was explicitly passed on the command
// line. set is the set of flag names actually seen (built from fs.Visit by the
// caller) — this distinguishes "-addr left at its default" from "-addr :9000",
// so a default flag value never clobbers a set env var. When neither env nor an
// explicit flag provides a value, the package defaults apply.
//
// DBPath: an explicit -db always wins; otherwise it is derived from the data dir
// as <DataDir>/cuento.db. The derivation uses filepath.Join only (no
// filepath.Abs, which would read the cwd and break purity); serve() resolves the
// data dir to an absolute path BEFORE calling this, honoring the p06.4 db.Open
// cwd-sensitivity note in production while keeping this function hermetic.
func resolveConfig(env map[string]string, fs *flag.FlagSet, set map[string]bool) Config {
	// flagStr returns the flag's current value if it was explicitly set,
	// otherwise "" so the env/default fallback applies.
	flagStr := func(name string) string {
		if set[name] {
			return fs.Lookup(name).Value.String()
		}
		return ""
	}
	pick := func(flagName, envKey, def string) string {
		if v := flagStr(flagName); v != "" {
			return v
		}
		if v := env[envKey]; v != "" {
			return v
		}
		return def
	}

	c := Config{
		DataDir: pick("data-dir", "CUENTO_DATA_DIR", defaultDataDir),
		Addr:    pick("addr", "CUENTO_ADDR", defaultAddr),
		Domain:  pick("domain", "CUENTO_DOMAIN", ""),
	}

	// Dev: -dev (explicit) overrides; else CUENTO_DEV truthy; else false.
	if set["dev"] {
		c.Dev = fs.Lookup("dev").Value.String() == "true"
	} else {
		c.Dev = truthyEnv(env["CUENTO_DEV"])
	}

	// DBPath: explicit -db wins; else derive from the data dir.
	if db := flagStr("db"); db != "" {
		c.DBPath = db
	} else {
		c.DBPath = filepath.Join(c.DataDir, defaultDBFile)
	}

	return c
}

// defaultDBFile is the SQLite filename inside the data dir when -db is not given.
const defaultDBFile = "cuento.db"

// truthyEnv interprets CUENTO_DEV: "1", and "true"/"yes"/"on" in lower-, Title-,
// or UPPER-case (the exact spellings listed below) are true; everything else
// (incl. empty and mixed-case like "tRuE") is false. Kept liberal so operators can
// write CUENTO_DEV=1 in a unit file without surprises.
func truthyEnv(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}
