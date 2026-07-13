package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"cuento/internal/ledger"
)

// p18.3 ops: the admin ops page (/admin/ops, Perm Admin). It surfaces three things
// an operator needs from a running instance:
//
//   1. BUILD INFO -- the running version (cfg.Version, set at release via ldflags,
//      the same string /healthz and the footer show) plus the Go runtime version.
//   2. The INTEGRITY CHECK -- it runs the SAME ledger.Check the `cuento check` CLI
//      runs (rule 7's "enforced twice" suite, Z1-Z19), grouped by severity: errors
//      first, then warnings (incl. Z17-Z19), each with its rule + detail. A clean
//      db shows a "no violations" notice. The check is READ-ONLY (no audit change).
//   3. A BACKUP SNAPSHOT action -- POST /admin/ops/backup streams a consistent
//      SQLite snapshot (VACUUM INTO) as an attachment download, and AUDITS the act.
//
// Backup is a POST (not a GET download link) precisely because it MUTATES the audit
// trail (rule 14): a mutating route must sit behind the cross-origin guard (rule
// 13), which the middleware applies to POST but not idempotent GET. Every string
// via {{t}} (rule 9); no inline script (rule 12).

// opsPageModel is the GET /admin/ops model: build info plus the integrity-check
// result grouped by severity for rendering (errors first, then warnings).
type opsPageModel struct {
	Version   string          // the running build version (cfg.Version)
	GoVersion string          // runtime.Version()
	Errors    []violationView // error-severity violations (rendered first)
	Warnings  []violationView // warning-severity violations (incl. Z17-Z19)
}

// Clean reports whether the integrity check found nothing -- the template shows a
// "no violations" notice in that case.
func (m opsPageModel) Clean() bool { return len(m.Errors) == 0 && len(m.Warnings) == 0 }

// violationView is one rendered integrity violation: the rule id and its detail
// (the human-readable text ledger.Check already produced, naming the offending
// ids). Severity is implied by which slice it lands in, so it is not repeated here.
type violationView struct {
	Rule   string
	Detail string
}

// opsPage handles GET /admin/ops (Admin): build info + the integrity check result.
func (s *server) opsPage(w http.ResponseWriter, r *http.Request) {
	model, err := s.buildOpsModel(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "ops.tmpl", s.newShellPage(r, model))
}

// buildOpsModel runs the integrity suite and assembles the page model. It groups
// the violations by severity and sorts each group deterministically (by rule, then
// detail) so the page is stable across runs -- the same ordering `cuento check`
// uses (cmd/cuento/check.go printViolations).
func (s *server) buildOpsModel(r *http.Request) (opsPageModel, error) {
	violations, err := ledger.Check(r.Context(), s.db)
	if err != nil {
		return opsPageModel{}, err
	}

	model := opsPageModel{Version: s.cfg.Version, GoVersion: runtime.Version()}
	for _, v := range violations {
		view := violationView{Rule: v.Rule, Detail: v.Detail}
		if v.Severity == ledger.Error {
			model.Errors = append(model.Errors, view)
		} else {
			model.Warnings = append(model.Warnings, view)
		}
	}
	sortViolations(model.Errors)
	sortViolations(model.Warnings)
	return model, nil
}

// sortViolations orders a severity group by rule then detail (stable), matching the
// CLI's deterministic output so the page never reshuffles between reloads.
func sortViolations(vs []violationView) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Rule != vs[j].Rule {
			return vs[i].Rule < vs[j].Rule
		}
		return vs[i].Detail < vs[j].Detail
	})
}

// opsBackup handles POST /admin/ops/backup (Admin): it writes a consistent SQLite
// snapshot via VACUUM INTO, streams it as an attachment download, and AUDITS the
// action (one ops.backup change naming the admin). The audit is written only AFTER
// the snapshot succeeds, so a failed backup leaves no misleading audit row.
//
// The snapshot goes to a UNIQUE temp file (per-request dir) so concurrent backups
// never collide and VACUUM INTO never hits an existing target; the whole temp dir
// is removed after the response is streamed (no leak). We never serve the LIVE db
// file directly -- VACUUM INTO gives a transactionally consistent copy even under
// concurrent writes.
func (s *server) opsBackup(w http.ResponseWriter, r *http.Request) {
	// A per-request temp dir: VACUUM INTO refuses to overwrite, so we point it at a
	// path that does not yet exist inside a dir we fully own and clean up.
	dir, err := os.MkdirTemp("", "cuento-backup-")
	if err != nil {
		s.serverError(w)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ts := time.Now().UTC().Format("20060102-150405")
	filename := "cuento-backup-" + ts + ".db"
	snapPath := filepath.Join(dir, filename)

	if err := s.store.Backup(r.Context(), snapPath); err != nil {
		s.serverError(w)
		return
	}

	// Audit the action ONLY after a successful snapshot (rule 14): who took a backup
	// and when. A failed backup above returned already, leaving no audit trace.
	if _, err := s.store.RecordBackup(s.actorCtx(r.Context())); err != nil {
		s.serverError(w)
		return
	}

	f, err := os.Open(snapPath)
	if err != nil {
		s.serverError(w)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		s.serverError(w)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	h.Set("Content-Length", fmt.Sprintf("%d", fi.Size()))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Best-effort stream: the headers (incl. 200) are already committed, so a copy
	// error mid-body cannot be turned into an error status; log-free like healthz.
	_, _ = io.Copy(w, f)
}
