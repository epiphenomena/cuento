package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"sort"
	"syscall"

	"cuento/internal/db"
	"cuento/internal/ledger"
)

// checkCmd implements `cuento check [-db PATH] [--strict]`: open the database,
// run the integrity suite, print every violation (rule, severity, detail), and
// exit non-zero when the suite says so (see checkExitCode). The exit decision is
// factored into checkExitCode so it is unit-testable without a database.
func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	strict := fs.Bool("strict", false, "treat warnings as failures (non-zero exit on any warning)")
	if err := fs.Parse(args); err != nil {
		// flag.ErrHelp (from -h) is not a failure: usage was already printed.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sqldb, err := db.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = sqldb.Close() }()

	violations, err := ledger.Check(ctx, sqldb)
	if err != nil {
		return err
	}

	printViolations(stdout, violations)
	if code := checkExitCode(violations, *strict); code != 0 {
		// A failing check exits non-zero without logging a Go error (the
		// violations ARE the message); signal the exit code up to main.
		return exitError{code: code}
	}
	return nil
}

// printViolations writes each violation as `SEVERITY RULE: detail`, in a stable
// order (errors before warnings, then by rule, then by detail) so output is
// deterministic across runs, and a trailing summary line.
func printViolations(w io.Writer, vs []ledger.Violation) {
	sorted := make([]ledger.Violation, len(vs))
	copy(sorted, vs)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Severity != b.Severity {
			// errors first
			return a.Severity == ledger.Error
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Detail < b.Detail
	})

	var errs, warns int
	for _, v := range sorted {
		if v.Severity == ledger.Error {
			errs++
		} else {
			warns++
		}
		_, _ = fmt.Fprintf(w, "%s %s: %s\n", v.Severity, v.Rule, v.Detail)
	}
	if len(sorted) == 0 {
		_, _ = fmt.Fprintln(w, "check: clean (no violations)")
		return
	}
	_, _ = fmt.Fprintf(w, "check: %d error(s), %d warning(s)\n", errs, warns)
}

// checkExitCode is the pure exit decision: 1 on any error-severity violation;
// 1 on any warning too when strict; 0 otherwise. Factored out so the exit
// behavior (error → always fail; warning → fail only under --strict) is tested
// without a database.
func checkExitCode(vs []ledger.Violation, strict bool) int {
	if ledger.HasErrors(vs) {
		return 1
	}
	if strict && len(vs) > 0 {
		return 1
	}
	return 0
}
