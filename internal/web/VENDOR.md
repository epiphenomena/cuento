# Vendored front-end assets

Per AGENTS rule 12 the frontend is boring: **vendored, pinned** libraries served
locally through the asset pipeline — never a CDN at runtime, never a bundler.
Each entry records the exact version and the SHA-256 of the committed file so its
integrity is auditable and a re-vendor is verifiable. Runtime and tests NEVER hit
the network; the download below happened once, locally, to vendor the file.

This note lives OUTSIDE `static/` deliberately: `buildAssetManifest` walks
`static/` and hashes every file it finds, so keeping the note here means it is
not served or content-addressed as an asset.

## htmx (p10.3)

- File: `internal/web/static/htmx.min.js`
- Version: **htmx 2.0.4** (pinned; bump deliberately, re-record the hash)
- SHA-256: `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447`
- Size: 50917 bytes
- Source (fetched once to vendor; verified byte-identical across two independent
  CDNs — jsdelivr and unpkg — before committing):
  - https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js
  - https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js

Verify the committed file:

```
sha256sum internal/web/static/htmx.min.js
# e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447
```

It is served under `script-src 'self'` (the strict CSP in middleware.go already
covers it — no CDN, no inline script) via `<script src="{{asset "htmx.min.js"}}">`
in `base.tmpl`, and content-addressed/immutable-cached in prod by the p10.1 asset
pipeline (`{{asset}}`), unhashed in `-dev`.
