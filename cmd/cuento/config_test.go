package main

import (
	"flag"
	"testing"
)

// parseServeConfig mirrors what serve() does: build the serve flagset, parse
// args, collect the explicitly-set flag names via fs.Visit, then resolveConfig
// against a supplied env map. It lets the tests exercise the REAL precedence
// path (default flag values must not clobber env) without spawning a server.
func parseServeConfig(t *testing.T, env map[string]string, args []string) Config {
	t.Helper()
	fs := newServeFlags()
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return resolveConfig(env, fs, set)
}

func TestConfigParsing(t *testing.T) {
	t.Run("defaults when neither env nor flag", func(t *testing.T) {
		c := parseServeConfig(t, nil, nil)
		if c.DataDir != defaultDataDir {
			t.Errorf("DataDir = %q, want %q", c.DataDir, defaultDataDir)
		}
		if c.Addr != defaultAddr {
			t.Errorf("Addr = %q, want %q", c.Addr, defaultAddr)
		}
		if c.Domain != "" {
			t.Errorf("Domain = %q, want empty", c.Domain)
		}
		if c.Dev {
			t.Error("Dev = true, want false")
		}
		if c.DBPath != "cuento.db" {
			t.Errorf("DBPath = %q, want %q", c.DBPath, "cuento.db")
		}
		if c.UseTLS() {
			t.Error("UseTLS() = true with no domain, want false")
		}
	})

	t.Run("env-only resolves (no flags; default must not clobber)", func(t *testing.T) {
		env := map[string]string{
			"CUENTO_DATA_DIR": "/data",
			"CUENTO_ADDR":     ":9000",
			"CUENTO_DOMAIN":   "books.example.com",
			"CUENTO_DEV":      "",
		}
		c := parseServeConfig(t, env, nil)
		if c.DataDir != "/data" {
			t.Errorf("DataDir = %q, want /data", c.DataDir)
		}
		if c.Addr != ":9000" {
			t.Errorf("Addr = %q, want :9000 (env must beat the -addr default)", c.Addr)
		}
		if c.Domain != "books.example.com" {
			t.Errorf("Domain = %q, want books.example.com", c.Domain)
		}
		// DBPath derives from the data dir when -db is absent.
		if c.DBPath != "/data/cuento.db" {
			t.Errorf("DBPath = %q, want /data/cuento.db", c.DBPath)
		}
		if c.AutocertCacheDir() != "/data/autocert" {
			t.Errorf("AutocertCacheDir() = %q, want /data/autocert", c.AutocertCacheDir())
		}
		if !c.UseTLS() {
			t.Error("UseTLS() = false with domain set and not dev, want true")
		}
	})

	t.Run("flag overrides its env", func(t *testing.T) {
		env := map[string]string{
			"CUENTO_DATA_DIR": "/data",
			"CUENTO_ADDR":     ":9000",
			"CUENTO_DOMAIN":   "env.example.com",
		}
		c := parseServeConfig(t, env, []string{
			"-data-dir", "/srv/cuento",
			"-addr", ":7000",
			"-domain", "flag.example.com",
		})
		if c.DataDir != "/srv/cuento" {
			t.Errorf("DataDir = %q, want /srv/cuento (flag overrides env)", c.DataDir)
		}
		if c.Addr != ":7000" {
			t.Errorf("Addr = %q, want :7000 (flag overrides env)", c.Addr)
		}
		if c.Domain != "flag.example.com" {
			t.Errorf("Domain = %q, want flag.example.com (flag overrides env)", c.Domain)
		}
		if c.DBPath != "/srv/cuento/cuento.db" {
			t.Errorf("DBPath = %q, want /srv/cuento/cuento.db", c.DBPath)
		}
	})

	t.Run("explicit -db overrides derived path", func(t *testing.T) {
		env := map[string]string{"CUENTO_DATA_DIR": "/data"}
		c := parseServeConfig(t, env, []string{"-db", "relative/path.db"})
		if c.DBPath != "relative/path.db" {
			t.Errorf("DBPath = %q, want relative/path.db (explicit -db wins, stays relative)", c.DBPath)
		}
	})

	t.Run("domain set => TLS mode; empty => plain HTTP", func(t *testing.T) {
		tls := parseServeConfig(t, map[string]string{"CUENTO_DOMAIN": "x.example.com"}, nil)
		if !tls.UseTLS() {
			t.Error("UseTLS() = false, want true when CUENTO_DOMAIN set")
		}
		plain := parseServeConfig(t, map[string]string{"CUENTO_DOMAIN": ""}, nil)
		if plain.UseTLS() {
			t.Error("UseTLS() = true, want false when CUENTO_DOMAIN empty")
		}
	})

	t.Run("dev forces plain HTTP even with a domain", func(t *testing.T) {
		c := parseServeConfig(t, map[string]string{"CUENTO_DOMAIN": "x.example.com"}, []string{"-dev"})
		if !c.Dev {
			t.Error("Dev = false, want true with -dev")
		}
		if c.UseTLS() {
			t.Error("UseTLS() = true in dev mode, want false (non-Secure cookie over TLS is incoherent)")
		}
	})

	t.Run("CUENTO_DEV toggles dev; -dev overrides env", func(t *testing.T) {
		if c := parseServeConfig(t, map[string]string{"CUENTO_DEV": "1"}, nil); !c.Dev {
			t.Error("CUENTO_DEV=1 => Dev should be true")
		}
		if c := parseServeConfig(t, map[string]string{"CUENTO_DEV": "true"}, nil); !c.Dev {
			t.Error("CUENTO_DEV=true => Dev should be true")
		}
		if c := parseServeConfig(t, map[string]string{"CUENTO_DEV": "0"}, nil); c.Dev {
			t.Error("CUENTO_DEV=0 => Dev should be false")
		}
		// Explicit -dev sets it even when the env is falsey.
		if c := parseServeConfig(t, map[string]string{"CUENTO_DEV": "0"}, []string{"-dev"}); !c.Dev {
			t.Error("-dev should force Dev true regardless of CUENTO_DEV")
		}
	})
}
