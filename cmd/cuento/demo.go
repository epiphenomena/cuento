package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"cuento/internal/db"
	"cuento/internal/store"
	"cuento/internal/synth"
)

// demoCmd implements `cuento demo -o <path>`: it generates a FRESH, fully-populated,
// 100% SYNTHETIC demo database (a fictional multi-subsidiary nonprofit with a full
// chart of accounts, restricted funds incl. a capital campaign, several years of
// multi-currency transactions, a sample budget, expense reports in draft/submitted/
// posted states, a finalized + an in-progress reconciliation, a bank-import profile +
// staged batch, and three demo logins across permission levels) for a public hosted
// demo where visitors click around.
//
// It is BUILT ON THE STORE (the write funnel + every invariant, rule 2) via the shared
// internal/synth.BuildDemo -- NOT raw SQL -- so a schema/invariant/API change forces the
// generator to keep up. It is DETERMINISTIC (a fixed monotonic clock + fixed base dates,
// no time.Now, no network), so runs are reproducible; the only non-reproducible surface
// is the argon2id password salts on the seeded users. Every value is SYNTHETIC (rule 11):
// nothing from real data ever enters here, so the result is safe to host publicly.
//
// The target file must NOT already exist (a demo is a from-scratch artifact; refusing an
// existing file avoids accidentally scribbling into a real db). The auto-reset host wipes
// and regenerates it on an interval (see docs/deploy.md).
func demoCmd(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("o", "", "output path for the generated demo database (required; must not exist)")
	force := fs.Bool("force", false, "overwrite the output file if it already exists (the auto-reset host uses this)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *out == "" {
		demoUsage()
		return fmt.Errorf("demo: -o <path> is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if _, err := os.Stat(*out); err == nil {
		if !*force {
			return fmt.Errorf("demo: %s already exists (use -force to overwrite, or pick a fresh path)", *out)
		}
		// -force: remove the file (and its WAL/SHM siblings) so we build from scratch.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(*out + suffix); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("demo: remove %s%s: %w", *out, suffix, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("demo: stat %s: %w", *out, err)
	}

	if err := generateDemo(ctx, *out); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "wrote demo database to %s\n\ndemo logins (all synthetic; safe to host publicly):\n", *out)
	for _, u := range synth.DemoUsers() {
		fmt.Fprintf(stdout, "  %-10s %-18s %s\n", u.Username, u.Password, u.Role)
	}
	return nil
}

func demoUsage() {
	fmt.Fprintf(stdout, "usage: cuento demo -o <path> [-force]\n\n"+
		"Generate a fresh, fully-populated, 100%% SYNTHETIC demo database.\n")
}

// generateDemo migrates a fresh db at path, opens it, installs the deterministic
// monotonic clock, and runs synth.BuildDemo. Factored out so the anti-drift test can
// build against a temp path through the exact same path the CLI uses.
func generateDemo(ctx context.Context, path string) error {
	if err := db.Migrate(ctx, path); err != nil {
		return fmt.Errorf("demo: migrate: %w", err)
	}
	sqldb, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("demo: open: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	// Deterministic clock (rule: no time.Now) so the generated data is reproducible.
	s := store.New(sqldb, store.WithClock(synth.BuildClock()))
	ctx = store.WithActor(ctx, systemActor)

	if _, err := synth.BuildDemo(ctx, s); err != nil {
		return fmt.Errorf("demo: build: %w", err)
	}
	return nil
}
