// Command ledgerimport is cuento's one-shot historical-ledger importer -- a
// LOCAL-ONLY tool, never deployed (AGENTS repository layout; PLAN p09.3). It
// converts the cleaned full-ledger CSV export (docs/ledger-export.md) into a
// cuento SQLite database by DRIVING internal/store, so every produced row passes
// the same store invariants the app enforces (rule 2/7).
//
// Two subcommands:
//
//	ledgerimport accounts -source <csv> -o <mapping-accounts.csv>
//	    Emit a reviewable account-mapping CSV skeleton (one row per distinct
//	    source account, best-guess columns prefilled) for the human to edit.
//
//	ledgerimport build -source <csv> -map <mapping-accounts.csv>
//	                   -config <mapping.json> -o <out.db> [--anonymize]
//	    Convert the source into a cuento db: subsidiaries, programs, funds, the
//	    account tree, opening balances, and transactions (single-currency directly;
//	    multi-currency decomposed through FX Clearing, D3). --anonymize hashes ONLY
//	    the per-split (and correction) DESCRIPTIONS; entity names (funds, donors,
//	    subsidiaries, accounts) are NOT anonymized, so the result is NOT publicly
//	    shareable — use `cuento demo` for a fully synthetic database.
//
// The subcommand CORES (runAccounts/runBuild) take io.Reader/io.Writer and a
// *store.Store so they are unit-tested against SYNTHETIC data (AGENTS rule 11 --
// the real fixtures/source/ file is only ever read at RUNTIME here, never in
// tests). Mapping files are stdlib-parseable (CSV + JSON), NOT YAML -- a
// deliberate within-allowlist substitution (D15; DECISIONS p09.3).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"cuento/internal/db"
	"cuento/internal/ids"
	"cuento/internal/ledger"
	"cuento/internal/store"
)

// systemActorID is the seeded system user (id 1). The importer posts as it -- a
// local one-shot tool has no interactive user (D22/D26).
const systemActorID = ids.UserID(1)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "accounts":
		if err := accountsCmd(args); err != nil {
			log.Fatalf("accounts: %v", err)
		}
	case "build":
		if err := buildCmd(ctx, args); err != nil {
			log.Fatalf("build: %v", err)
		}
	case "scaffold":
		if err := scaffoldCmd(ctx, args); err != nil {
			log.Fatalf("scaffold: %v", err)
		}
	case "import-subsidiary":
		if err := importSubCmd(ctx, args); err != nil {
			log.Fatalf("import-subsidiary: %v", err)
		}
	case "finalize":
		if err := finalizeCmd(ctx, args); err != nil {
			log.Fatalf("finalize: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "ledgerimport: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, "usage: ledgerimport <command> [flags]\n\ncommands:\n"+
		"  accounts           emit a reviewable account-mapping CSV skeleton from the source export\n"+
		"  build              convert the source export + mapping into a FRESH cuento SQLite db (all subsidiaries)\n"+
		"  scaffold           create a fresh db with reference data only (subs, programs, funds, chart, rates)\n"+
		"  import-subsidiary  additively import ONE subsidiary's transactions into a scaffolded db\n"+
		"  finalize           post cross-subsidiary corrections (run ONCE after the LAST import-subsidiary)\n")
}

// accountsCmd wires runAccounts to the real files. It reads the source CSV at
// RUNTIME (this is where the real fixtures/source/ file is legitimately opened,
// p09.4) and writes the skeleton to -o (or stdout).
func accountsCmd(args []string) error {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	source := fs.String("source", "", "path to the source ledger CSV export")
	outPath := fs.String("o", "", "output account-mapping CSV path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		return fmt.Errorf("-source is required")
	}

	src, err := os.Open(*source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	if err := runAccounts(src, out); err != nil {
		return err
	}
	if *outPath != "" {
		log.Printf("account-mapping skeleton written to %s", *outPath)
	}
	return nil
}

// buildCmd wires runBuild to the real files: it migrates+opens the output db,
// constructs the store, binds the system actor, runs the conversion, then runs
// ledger.Check and prints any violations for the operator (D26 go-live gate). It
// exits non-zero if the produced db has Error violations.
func buildCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	source := fs.String("source", "", "path to the source ledger CSV export")
	mapPath := fs.String("map", "", "path to the reviewed account-mapping CSV")
	configPath := fs.String("config", "", "path to the global mapping config JSON")
	ratesPath := fs.String("rates", "", "optional path to a historical FX-rates CSV (rate_date,base,quote,rate,source)")
	outPath := fs.String("o", "fixtures/sample.db", "output SQLite db path")
	anonymize := fs.Bool("anonymize", false, "hash per-split/correction DESCRIPTIONS only; entity names are NOT anonymized (use `cuento demo` for a shareable synthetic db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *mapPath == "" || *configPath == "" {
		return fmt.Errorf("-source, -map and -config are all required")
	}

	// Read the mapping files first (fail fast on a bad mapping).
	accMap, err := readAccountMapFile(*mapPath)
	if err != nil {
		return err
	}
	cfg, err := readConfigFile(*configPath)
	if err != nil {
		return err
	}
	// Optional historical FX rates (D12): without them the produced db cannot
	// render any converted (consolidated-currency) report.
	var rates []store.Rate
	if *ratesPath != "" {
		rates, err = readRatesFile(*ratesPath)
		if err != nil {
			return err
		}
	}

	src, err := os.Open(*source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()

	// A one-shot build wants a fresh db: refuse to overwrite an existing file so a
	// re-run is an explicit `rm` (avoids silently mixing two imports).
	if _, err := os.Stat(*outPath); err == nil {
		return fmt.Errorf("output %s already exists; remove it first for a clean build", *outPath)
	}

	if err := db.Migrate(ctx, *outPath); err != nil {
		return fmt.Errorf("migrate output db: %w", err)
	}
	sqldb, err := db.Open(*outPath)
	if err != nil {
		return fmt.Errorf("open output db: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	st := store.New(sqldb)
	actorCtx := store.WithActor(ctx, store.Actor{ID: systemActorID})

	res, err := runBuild(actorCtx, src, accMap, cfg, rates, st, *anonymize)
	if err != nil {
		return err
	}

	for _, w := range res.Warnings {
		log.Printf("WARNING: %s", w)
	}
	log.Printf("built %s: %d subsidiaries, %d programs, %d funds, %d accounts",
		*outPath, len(res.SubsidiaryIDs), len(res.ProgramIDs), res.fundCount(), len(res.AccountIDs))

	// Run the integrity suite on the produced db (the D26 rehearsal gate). Errors
	// mean the import is inconsistent -- exit non-zero so `make fixture` fails loud.
	vs, err := ledger.Check(ctx, sqldb)
	if err != nil {
		return fmt.Errorf("ledger check: %w", err)
	}
	for _, v := range vs {
		log.Printf("%s %s: %s", v.Severity, v.Rule, v.Detail)
	}
	if ledger.HasErrors(vs) {
		return fmt.Errorf("produced db has integrity errors; see above")
	}
	return nil
}

// scaffoldCmd creates a FRESH db populated with subsidiary-independent reference
// data only (rates, subsidiaries, program tree, funds, the whole account chart) --
// the "scaffold once" half of the split-import model (D26). It reads no source
// rows and posts no transactions; per-subsidiary transaction imports follow via
// import-subsidiary. Like build, it refuses an existing output file.
func scaffoldCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	mapPath := fs.String("map", "", "path to the reviewed account-mapping CSV")
	configPath := fs.String("config", "", "path to the global mapping config JSON")
	ratesPath := fs.String("rates", "", "optional path to a historical FX-rates CSV (rate_date,base,quote,rate,source)")
	outPath := fs.String("o", "fixtures/sample.db", "output SQLite db path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mapPath == "" || *configPath == "" {
		return fmt.Errorf("-map and -config are both required")
	}
	accMap, err := readAccountMapFile(*mapPath)
	if err != nil {
		return err
	}
	cfg, err := readConfigFile(*configPath)
	if err != nil {
		return err
	}
	var rates []store.Rate
	if *ratesPath != "" {
		rates, err = readRatesFile(*ratesPath)
		if err != nil {
			return err
		}
	}
	// A fresh scaffold wants a fresh db: refuse to overwrite (a re-run is an explicit rm).
	if _, err := os.Stat(*outPath); err == nil {
		return fmt.Errorf("output %s already exists; remove it first for a clean scaffold", *outPath)
	}
	if err := db.Migrate(ctx, *outPath); err != nil {
		return fmt.Errorf("migrate output db: %w", err)
	}
	sqldb, err := db.Open(*outPath)
	if err != nil {
		return fmt.Errorf("open output db: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	st := store.New(sqldb)
	actorCtx := store.WithActor(ctx, store.Actor{ID: systemActorID})

	res, err := runScaffold(actorCtx, accMap, cfg, rates, st, false)
	if err != nil {
		return err
	}
	log.Printf("scaffolded %s: %d subsidiaries, %d programs, %d funds, %d accounts (0 transactions)",
		*outPath, len(res.SubsidiaryIDs), len(res.ProgramIDs), res.fundCount(), len(res.AccountIDs))

	vs, err := ledger.Check(ctx, sqldb)
	if err != nil {
		return fmt.Errorf("ledger check: %w", err)
	}
	for _, v := range vs {
		log.Printf("%s %s: %s", v.Severity, v.Rule, v.Detail)
	}
	if ledger.HasErrors(vs) {
		return fmt.Errorf("produced db has integrity errors; see above")
	}
	return nil
}

// importSubCmd additively imports ONE subsidiary's transactions into an
// already-scaffolded db (it REQUIRES the db to exist and does not migrate). It
// prints the surfaced warnings and a per-currency/type native trial-balance for the
// operator to reconcile against the source books, then runs the whole-db integrity
// suite. Safe to run against a live, in-use db; re-importing a subsidiary is
// refused (fresh scaffold + import, D26).
func importSubCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-subsidiary", flag.ContinueOnError)
	source := fs.String("source", "", "path to the source ledger CSV export")
	mapPath := fs.String("map", "", "path to the reviewed account-mapping CSV")
	configPath := fs.String("config", "", "path to the global mapping config JSON")
	outPath := fs.String("o", "", "existing scaffolded SQLite db path")
	subName := fs.String("subsidiary", "", "the subsidiary NAME to import (must match the config)")
	anonymize := fs.Bool("anonymize", false, "hash per-split/correction DESCRIPTIONS only; entity names are NOT anonymized (use `cuento demo` for a shareable synthetic db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *mapPath == "" || *configPath == "" || *outPath == "" || *subName == "" {
		return fmt.Errorf("-source, -map, -config, -o and -subsidiary are all required")
	}
	accMap, err := readAccountMapFile(*mapPath)
	if err != nil {
		return err
	}
	cfg, err := readConfigFile(*configPath)
	if err != nil {
		return err
	}
	src, err := os.Open(*source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()
	recs, err := ParseRecords(src)
	if err != nil {
		return err
	}

	// An additive import REQUIRES a scaffolded db: refuse a missing file (never
	// migrate here -- migration is the scaffold's job).
	if _, err := os.Stat(*outPath); err != nil {
		return fmt.Errorf("output db %s does not exist; run `ledgerimport scaffold` first", *outPath)
	}
	sqldb, err := db.Open(*outPath)
	if err != nil {
		return fmt.Errorf("open output db: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	st := store.New(sqldb)
	actorCtx := store.WithActor(ctx, store.Actor{ID: systemActorID})

	res, err := runImportSubsidiary(actorCtx, recs, accMap, cfg, st, *subName, *anonymize)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		log.Printf("WARNING: %s", w)
	}
	log.Printf("imported subsidiary %q into %s: %d source groups posted, %d warnings",
		*subName, *outPath, len(res.tidTxns), len(res.Warnings))

	// Operator reconciliation: per currency/type native totals (compare to the books).
	if subID, ok := subsidiaryIDByName(actorCtx, st, *subName); ok {
		totals, terr := st.SubsidiaryNativeTotals(actorCtx, subID)
		if terr != nil {
			return terr
		}
		log.Printf("native totals for %q (net-debit minor units; grand total per currency must be 0):", *subName)
		for _, t := range totals {
			log.Printf("  %s %-9s %d", t.Currency, t.Type, t.Total)
		}
	}

	vs, err := ledger.Check(ctx, sqldb)
	if err != nil {
		return fmt.Errorf("ledger check: %w", err)
	}
	for _, v := range vs {
		log.Printf("%s %s: %s", v.Severity, v.Rule, v.Detail)
	}
	if ledger.HasErrors(vs) {
		return fmt.Errorf("produced db has integrity errors; see above")
	}
	return nil
}

// finalizeCmd posts the config's cross-subsidiary corrections into an already
// scaffolded + subsidiary-imported db -- the FINALIZE step of the split-import
// go-live (p27.0). It opens (never migrates) the existing db, rehydrates the id
// maps FROM the db (the same rehydration import-subsidiary uses), and posts
// cfg.Corrections through the store, then runs the whole-db integrity suite.
//
// It MUST be run ONCE, after the LAST import-subsidiary: corrections span
// subsidiaries, so runFinalize refuses unless every configured subsidiary is
// present, and there is no natural key to detect a double-run -- re-running
// double-posts. Recovery is restore-from-backup (golive.md), so ALWAYS back up
// first.
func finalizeCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("finalize", flag.ContinueOnError)
	mapPath := fs.String("map", "", "path to the reviewed account-mapping CSV")
	configPath := fs.String("config", "", "path to the global mapping config JSON")
	outPath := fs.String("o", "", "existing scaffolded + subsidiary-imported SQLite db path")
	anonymize := fs.Bool("anonymize", false, "hash correction DESCRIPTIONS only; entity names are NOT anonymized")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mapPath == "" || *configPath == "" || *outPath == "" {
		return fmt.Errorf("-map, -config and -o are all required")
	}
	accMap, err := readAccountMapFile(*mapPath)
	if err != nil {
		return err
	}
	cfg, err := readConfigFile(*configPath)
	if err != nil {
		return err
	}

	// Finalize REQUIRES an existing db (scaffolded + imported): refuse a missing file
	// (never migrate here -- migration is the scaffold's job).
	if _, err := os.Stat(*outPath); err != nil {
		return fmt.Errorf("output db %s does not exist; run `ledgerimport scaffold` + `import-subsidiary` first", *outPath)
	}
	sqldb, err := db.Open(*outPath)
	if err != nil {
		return fmt.Errorf("open output db: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	st := store.New(sqldb)
	actorCtx := store.WithActor(ctx, store.Actor{ID: systemActorID})

	res, err := runFinalize(actorCtx, accMap, cfg, st, *anonymize)
	if err != nil {
		return err
	}
	// Count the posted correction transactions (one synthetic tid per correction).
	nTxns := 0
	for _, txns := range res.tidTxns {
		nTxns += len(txns)
	}
	log.Printf("finalized %s: posted %d correction transaction(s) from %d configured correction(s). "+
		"Run finalize ONCE only -- it has no double-run guard (golive.md).", *outPath, nTxns, len(cfg.Corrections))

	vs, err := ledger.Check(ctx, sqldb)
	if err != nil {
		return fmt.Errorf("ledger check: %w", err)
	}
	for _, v := range vs {
		log.Printf("%s %s: %s", v.Severity, v.Rule, v.Detail)
	}
	if ledger.HasErrors(vs) {
		return fmt.Errorf("produced db has integrity errors; see above")
	}
	return nil
}

// subsidiaryIDByName resolves a subsidiary name to its id from the db (for the
// CLI's post-import reconciliation output).
func subsidiaryIDByName(ctx context.Context, st *store.Store, name string) (int64, bool) {
	subs, err := st.SubTree(ctx)
	if err != nil {
		return 0, false
	}
	for _, s := range subs {
		if s.Name == name {
			return s.ID, true
		}
	}
	return 0, false
}

func readAccountMapFile(path string) ([]AccountMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open account map: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ReadAccountMap(f)
}

func readConfigFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ReadConfig(f)
}

func readRatesFile(path string) ([]store.Rate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rates: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ReadRates(f)
}
