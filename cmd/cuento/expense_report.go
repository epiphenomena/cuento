package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"cuento/internal/store"
)

// expense-report is a small MAINTENANCE/SEED verb over the p20.1 expense-report
// store methods -- the CLI face of `store.RejectExpenseReport`. It exists so the
// functional (e2e) harness can drive a submitted->rejected transition out of band
// (the p20.3 reviewer WEB queue is a later step; this is NOT that UI, just the CLI
// entry to the SAME store method the Go tests call directly). It runs through the
// write funnel as the system actor, so the rejection is a real audited, versioned
// change like any other. Kept obviously seed-shaped and documented in the CLI
// reference (p22.1).
//
//	cuento expense-report reject <id> --reason "<text>" [-db PATH]
func expenseReportCmd(args []string) error {
	if len(args) < 1 {
		expenseReportUsage()
		return errors.New("expense-report: missing sub-subcommand")
	}
	sub, rest := args[0], args[1:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "reject":
		return expenseReportRejectCmd(ctx, rest)
	default:
		expenseReportUsage()
		return fmt.Errorf("expense-report: unknown sub-subcommand %q", sub)
	}
}

func expenseReportUsage() {
	fmt.Fprintf(os.Stderr, "usage: cuento expense-report <command> [flags]\n\ncommands:\n"+
		"  reject <id> --reason \"<text>\" [-db PATH]   reject a submitted expense report (maintenance/seed)\n")
}

// expenseReportRejectCmd rejects a SUBMITTED report by id with a required reason. It
// is the CLI face of store.RejectExpenseReport (p20.1): the report must be in the
// submitted state and the reason must be non-empty (the store enforces both).
func expenseReportRejectCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expense-report reject", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	reason := fs.String("reason", "", "the rejection reason (required)")
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		// flag.ErrHelp (from -h) is not a failure: flag already printed usage.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positionals) != 1 {
		return errors.New("expense-report reject: exactly one <id> is required")
	}
	id, err := strconv.ParseInt(positionals[0], 10, 64)
	if err != nil {
		return fmt.Errorf("expense-report reject: bad id %q: %w", positionals[0], err)
	}
	if *reason == "" {
		return errors.New("expense-report reject: --reason is required")
	}

	st, closeStore, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeStore()

	if err := st.RejectExpenseReport(store.WithActor(ctx, systemActor), id, *reason); err != nil {
		return fmt.Errorf("expense-report reject %d: %w", id, err)
	}
	_, _ = fmt.Fprintf(stdout, "expense report %d rejected\n", id)
	return nil
}
