package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// p10.1 content-addresses the embedded static assets. At construction we hash
// each file's bytes and expose it under a URL carrying the first 8 hex chars of
// its SHA-256 (app.css -> /static/app.<hash>.css). Because the URL changes the
// instant the content changes, the hashed URL can be served immutable-cached
// forever with no stale-asset risk — while HTML stays no-store (render.go). In
// -dev the unhashed URL is used instead so a designer editing app.css sees it
// live without a rebuild, and dev assets are never immutable-cached.
//
// The manifest is built ONCE in NewApp (a single fs.WalkDir), not per request.

// assetManifest maps between an asset's logical name and its content-hashed URL.
// Both directions come from one walk: forward feeds the `asset` template func,
// reverse lets the static handler recognize a hashed request and locate the
// unhashed embedded file (so stdlib ServeContent still handles content-type and
// range).
type assetManifest struct {
	// byName: logical name ("app.css") -> hashed URL ("/static/app.<hash>.css").
	byName map[string]string
	// byHashedPath: hashed URL path -> unhashed URL path ("/static/app.css").
	byHashedPath map[string]string
}

// buildAssetManifest walks the embedded static FS once, hashing every file, and
// returns the name<->hashed-URL maps. A read/walk failure is a build-time defect
// (the FS is embedded), so it panics — the sanctioned startup panic, mirroring
// mustParseTemplates.
func buildAssetManifest() *assetManifest {
	m := &assetManifest{
		byName:       make(map[string]string),
		byHashedPath: make(map[string]string),
	}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// JS unit tests (*.test.js) live beside the modules they cover (node --test
		// reads them from disk) but are NOT web assets: //go:embed static grabs the
		// whole tree, so skip them here so they are never hashed, manifested, or
		// served — test code has no business on a Public /static URL (staticHandler
		// 404s them too).
		if isTestAsset(p) {
			return nil
		}
		b, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])[:8]

		// p is "static/<name>"; the logical name is everything under "static/".
		name := p[len("static/"):]
		unhashedURL := "/" + p // "/static/<name>"
		hashedURL := "/static/" + insertHash(name, hash)

		m.byName[name] = hashedURL
		m.byHashedPath[hashedURL] = unhashedURL
		return nil
	})
	if err != nil {
		panic("web: build asset manifest: " + err.Error())
	}
	return m
}

// isTestAsset reports whether an embedded static path is a JS unit test (*.test.js)
// rather than a shippable asset. Such files ride along in the embed (go:embed can't
// exclude by suffix) but must never be hashed, manifested, or served — they are test
// code, not part of the boring frontend's runtime surface (rule 12).
func isTestAsset(p string) bool { return strings.HasSuffix(p, ".test.js") }

// insertHash puts the content hash before the file's last extension:
// "app.css" -> "app.<hash>.css". Files without an extension get the hash
// appended ("robots" -> "robots.<hash>").
func insertHash(name, hash string) string {
	ext := path.Ext(name)
	base := name[:len(name)-len(ext)]
	if ext == "" {
		return base + "." + hash
	}
	return base + "." + hash + ext
}

// assetURL returns the URL a template should use for the named asset: the
// content-hashed URL in prod (immutable-cacheable), the plain /static/ URL in
// -dev (live-editable). An unknown name falls back to the unhashed URL rather
// than panicking, so a typo degrades to a 404 at request time instead of
// crashing render.
func (s *server) assetURL(name string) string {
	if s.cfg.Dev {
		return "/static/" + name
	}
	if u, ok := s.assets.byName[name]; ok {
		return u
	}
	return "/static/" + name
}

// staticHandler serves the embedded static assets. For a recognized hashed URL
// it stamps immutable long-lived caching and rewrites the path to the unhashed
// file before handing off to the stdlib FileServer (which owns content-type,
// If-Modified-Since and range handling). Unhashed URLs — the only kind used in
// -dev, and also the p00.2 /static/app.css contract — pass straight through with
// no immutable header. The FileServer is created once and shared.
func (s *server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// JS unit tests are embedded (go:embed static) but are not assets: never
		// serve them, even by their plain unhashed path (they are absent from the
		// manifest, so this guards the -dev / direct-path route too).
		if isTestAsset(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if unhashed, ok := s.assets.byHashedPath[r.URL.Path]; ok {
			// One year, immutable: the URL is content-addressed, so a changed
			// file ships under a new URL and this cached copy is never wrong.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			r = r.Clone(r.Context())
			r.URL.Path = unhashed
		}
		fileServer.ServeHTTP(w, r)
	})
}
