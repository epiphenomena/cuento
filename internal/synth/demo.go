package synth

import (
	"context"
	"fmt"

	"cuento/internal/auth"
	"cuento/internal/bankimport"
	"cuento/internal/ids"
	"cuento/internal/reports"
	"cuento/internal/store"
)

// Demo login credentials. The `cuento demo` generator seeds these four users and
// docs/deploy.md prints the same contract, so the constants are the single source of
// truth. They are DEMO-ONLY passwords for a throwaway, publicly-hosted, auto-resetting
// database -- never real credentials (rule 11 / rule 13). Each covers one permission
// level so a visitor can experience every posture.
const (
	// DemoAdminUser is a full administrator (write txn perm, admin, can submit).
	DemoAdminUser = "admin"
	DemoAdminPass = "demo-admin-2026"

	// DemoSubmitterUser is a non-admin expense submitter (write txn perm + can submit).
	DemoSubmitterUser = "submitter"
	DemoSubmitterPass = "demo-submit-2026"

	// DemoViewerUser is a read-only viewer (read txn perm, no admin, no submit).
	DemoViewerUser = "viewer"
	DemoViewerPass = "demo-view-2026"

	// DemoCampDirectorUser is a read-only user with a PROGRAM-SUBTREE-SCOPED report
	// grant (p27.4d): the "financial" report group scoped to the Educacion program
	// subtree. It demonstrates the p27.4 data-scoping axis in the hosted demo -- this
	// user's income statement (a program-dimensioned report) shows ONLY Educacion's
	// rows, and a demoted non-program report in that group (e.g. the balance sheet) is
	// denied (needs an unscoped grant). Named for the "camp director" persona in PLAN Q5.
	DemoCampDirectorUser = "campdir"
	DemoCampDirectorPass = "demo-camp-2026"
)

// DemoUser pairs a demo login with its human-readable role, for docs + the CLI.
type DemoUser struct {
	Username string
	Password string
	Role     string
}

// DemoUsers is the ordered demo credential set the generator seeds and deploy.md
// documents. Exported so the CLI can print exactly what the docs promise.
func DemoUsers() []DemoUser {
	return []DemoUser{
		{DemoAdminUser, DemoAdminPass, "administrator (full access)"},
		{DemoSubmitterUser, DemoSubmitterPass, "expense submitter (write, can submit)"},
		{DemoViewerUser, DemoViewerPass, "read-only viewer"},
		{DemoCampDirectorUser, DemoCampDirectorPass, "program-scoped viewer (financial reports, Educacion subtree only)"},
	}
}

// DemoIDs holds the demo-only entity ids (users, expense reports, the open recon, the
// import profile/batch) on top of the shared IDs. It exists so the anti-drift test can
// assert feature coverage without re-querying.
type DemoIDs struct {
	IDs

	AdminUser        ids.UserID
	SubmitterUser    ids.UserID
	ViewerUser       ids.UserID
	CampDirectorUser ids.UserID

	DraftReport     ids.ExpenseReportID
	SubmittedReport ids.ExpenseReportID
	PostedReport    ids.ExpenseReportID

	OpenRecon ids.ReconciliationID // an in-progress (unfinalized) reconciliation on Checking MX

	MappingProfile ids.MappingProfileID
	ImportBatch    ids.ImportBatchID // a staged (unposted) import batch on Checking US
}

// BuildDemo builds the FULL demo dataset into the store: the canonical synthetic org
// (Build) + every opt-in seam (rates, reconciliation, capital campaign, sample budget)
// + demo-only data (four users across permission levels incl. a program-scoped viewer,
// expense reports in
// draft/submitted/posted states, an in-progress reconciliation, and a bank-import
// mapping profile with a staged batch) so EVERY feature/report renders substantively.
//
// It is store-built (rule 2), deterministic given a monotonic clock installed by the
// caller (see BuildClock; the argon2id password salts are the one non-reproducible
// surface), and 100% SYNTHETIC (rule 11).
//
// ORDERING NOTE (load-bearing): ExtendReconciliation FINALIZES Checking US against a
// statement balance hand-computed for the BASE fixture's Checking US splits. The
// capital campaign posts additional Checking US splits, so it MUST run AFTER the
// reconciliation is finalized -- otherwise Finalize's cleared-sum check would include
// the campaign splits and fail. The campaign's 2025 splits then land as pre-statement
// outstanding items (ledger-valid; cosmetically fine for a demo). Do not reorder.
//
// Everything reads and writes through the store s (rule 2): ExtendReconciliation now
// enumerates the clearable splits via s.SplitsByAccountCurrency, so no *sql.DB is threaded.
func BuildDemo(ctx context.Context, s *store.Store) (DemoIDs, error) {
	var d DemoIDs

	baseIDs, err := Build(ctx, s)
	if err != nil {
		return d, fmt.Errorf("build base: %w", err)
	}
	d.IDs = baseIDs

	// Rates (no splits; order-independent, but before the campaign so campaign splits
	// have on-or-before rates for a converted report run).
	if err := ExtendRates(ctx, s); err != nil {
		return d, err
	}

	// Reconciliation BEFORE the campaign (see ordering note): finalize Checking US
	// against the base-fixture statement balance while only base splits exist.
	if _, err := ExtendReconciliation(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	// FX remeasurement (the Lempira example): adds an HNL bank in the USD-functional US
	// sub + single-currency HNL flows, so the demo demonstrates an ASC 830-20
	// remeasurement gain/loss in income. Touches only Banco Lempira / Contributions /
	// Food Purchases (never Checking US), so it is order-independent w.r.t. the recon.
	if err := ExtendFX(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	// Capital campaign (posts new Checking US/MX splits) AFTER the recon is finalized.
	if err := ExtendCapitalCampaign(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	// Sample budget PLAN (the p27.2 split-derived model; order-independent) — the
	// budget the p27.3 cash-flow / variance reports run over.
	if err := ExtendSampleBudgetPlan(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	if err := buildDemoUsers(ctx, s, &d); err != nil {
		return d, err
	}
	if err := buildDemoExpenseReports(ctx, s, &d); err != nil {
		return d, err
	}
	if err := buildDemoOpenReconciliation(ctx, s, &d); err != nil {
		return d, err
	}
	if err := buildDemoImport(ctx, s, &d); err != nil {
		return d, err
	}
	return d, nil
}

// buildDemoUsers seeds the four demo logins across permission levels with known
// passwords (DemoUsers). Passwords are hashed via auth.Hash (argon2id) exactly like
// `cuento user add`, so login works out of the box. The fourth (camp director) is a
// read-only user carrying a program-subtree-scoped report grant (p27.4d).
func buildDemoUsers(ctx context.Context, s *store.Store, d *DemoIDs) error {
	admin, err := createDemoUser(ctx, s, DemoAdminUser, "Demo Administrator", true, "write")
	if err != nil {
		return err
	}
	d.AdminUser = admin

	sub, err := createDemoUser(ctx, s, DemoSubmitterUser, "Demo Submitter", false, "write")
	if err != nil {
		return err
	}
	if err := s.SetUserCanSubmitExpenses(ctx, sub, true); err != nil {
		return fmt.Errorf("grant submit to demo submitter: %w", err)
	}
	d.SubmitterUser = sub

	viewer, err := createDemoUser(ctx, s, DemoViewerUser, "Demo Viewer", false, "read")
	if err != nil {
		return err
	}
	d.ViewerUser = viewer

	// A program-subtree-scoped viewer (p27.4d): read-only, holding the "financial" report
	// group scoped to the Educacion program subtree. income_statement (program-dimensioned)
	// then shows only Educacion's rows; a demoted non-program report in that group (e.g. the
	// balance sheet) is denied. GrantReportGroup versions the grant under the actor in ctx.
	//
	// The report groups are code-declared reference data (D10) the SERVE path syncs at
	// startup (web.SyncReportGroups); the demo generator has no server boot, so sync them
	// here first -- else the grant's group_name FK has nothing to reference. Sync via the
	// canonical reports.Groups() set (the same one web syncs), an idempotent upsert outside
	// the write funnel (reference data, rule 2 -- like currencies).
	if err := s.SyncReportGroups(ctx, reports.Groups()); err != nil {
		return fmt.Errorf("sync report groups for demo grant: %w", err)
	}
	campDir, err := createDemoUser(ctx, s, DemoCampDirectorUser, "Demo Camp Director", false, "read")
	if err != nil {
		return err
	}
	educacion := d.Educacion
	if err := s.GrantReportGroup(ctx, campDir, "financial", &educacion); err != nil {
		return fmt.Errorf("grant scoped financial to demo camp director: %w", err)
	}
	d.CampDirectorUser = campDir
	return nil
}

// passwordFor returns the demo password for a username (kept next to createDemoUser so
// the seeded hash and the documented plaintext cannot drift). An unknown username is a
// programmer error (a typo in a createDemoUser call) and returns an error rather than
// silently hashing "" into an unusable, undocumented login.
func passwordFor(username string) (string, error) {
	for _, u := range DemoUsers() {
		if u.Username == username {
			return u.Password, nil
		}
	}
	return "", fmt.Errorf("no demo password for unknown username %q", username)
}

func createDemoUser(ctx context.Context, s *store.Store, username, display string, admin bool, txnPerm string) (ids.UserID, error) {
	password, err := passwordFor(username)
	if err != nil {
		return 0, err
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return 0, fmt.Errorf("hash demo password for %q: %w", username, err)
	}
	id, err := s.CreateUser(ctx, store.CreateUserInput{
		Username:     username,
		DisplayName:  display,
		PasswordHash: &hash,
		IsAdmin:      admin,
		TxnPerm:      txnPerm,
	})
	if err != nil {
		return 0, fmt.Errorf("create demo user %q: %w", username, err)
	}
	return id, nil
}

// buildDemoExpenseReports creates one expense report in EACH of the three states the
// review workflow supports -- draft, submitted, posted (converted) -- all in the US
// subsidiary, submitted by the demo submitter, so the expense-report screens and the
// reviewer queue all show something.
func buildDemoExpenseReports(ctx context.Context, s *store.Store, d *DemoIDs) error {
	// 8a: give the US subsidiary a default AP account (the Credit Card liability leaf) so a
	// new report's ap_account_id (the main-split payable) populates from it -- the demo shows
	// the transaction-form main-header prefill, not a blank AP.
	apAcct := d.CreditCard
	if err := s.UpdateSubsidiary(ctx, d.US, store.UpdateSubsidiaryInput{DefaultAPAccountID: &apAcct}); err != nil {
		return fmt.Errorf("set US default AP account: %w", err)
	}

	// --- Draft: created with lines, left in draft. A report DATE is set on the header so
	// the 8b "my reports" list shows its Date column populated (the submitter side leaves
	// date blank by default -- the demo fills one so the hosted demo showcases the column).
	draft, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US, store.CreateExpenseReportInput{
		Description: "Demo Submitter", Memo: "Expense report",
	})
	if err != nil {
		return fmt.Errorf("create draft expense report: %w", err)
	}
	if err := s.SetExpenseReportHeader(ctx, draft, "2026-06-18", "Demo Submitter", "Office supplies reimbursement", ""); err != nil {
		return fmt.Errorf("set draft expense report header: %w", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, draft, store.ExpenseReportLineInput{
		AccountID: d.Occupancy, Amount: 45_000, ProgramID: &d.General, Description: "Office supplies (draft)",
	}); err != nil {
		return fmt.Errorf("add draft line: %w", err)
	}
	d.DraftReport = draft

	// --- Submitted: created, a line added, then submitted (awaiting review).
	submitted, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US, store.CreateExpenseReportInput{
		Description: "Demo Submitter", Memo: "Expense report",
	})
	if err != nil {
		return fmt.Errorf("create submitted expense report: %w", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, submitted, store.ExpenseReportLineInput{
		AccountID: d.Insurance, Amount: 32_000, ProgramID: &d.General, Description: "Travel insurance (submitted)",
	}); err != nil {
		return fmt.Errorf("add submitted line: %w", err)
	}
	if err := s.SubmitExpenseReport(ctx, submitted); err != nil {
		return fmt.Errorf("submit expense report: %w", err)
	}
	d.SubmittedReport = submitted

	// --- Posted: created, a line added, submitted, then posted+converted to a real
	// balanced ledger transaction (Occupancy expense funded from Checking US).
	posted, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US, store.CreateExpenseReportInput{
		Description: "Demo Submitter", Memo: "Expense report",
	})
	if err != nil {
		return fmt.Errorf("create posted expense report: %w", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, posted, store.ExpenseReportLineInput{
		AccountID: d.EventCosts, Amount: 60_000, ProgramID: &d.General, Description: "Volunteer appreciation (posted)",
	}); err != nil {
		return fmt.Errorf("add posted line: %w", err)
	}
	if err := s.SubmitExpenseReport(ctx, posted); err != nil {
		return fmt.Errorf("submit posted expense report: %w", err)
	}
	// Reviewer posts a balanced txn: DR Event Costs 600.00 / CR Checking US 600.00.
	if _, err := s.PostAndConvertExpenseReport(ctx, posted, store.PostTransactionInput{
		Date:         "2026-06-20",
		SubsidiaryID: d.US,
		Currency:     "USD",
		Memo:         "Expense report reimbursement (demo)",
		Splits: []store.SplitInput{
			{AccountID: d.EventCosts, Amount: 60_000, ProgramID: &d.General, Position: 0, Description: "Volunteer appreciation (posted)"},
			{AccountID: d.CheckingUS, Amount: -60_000, Position: 1, Description: "Reimbursement payment"},
		},
	}); err != nil {
		return fmt.Errorf("post+convert expense report: %w", err)
	}
	d.PostedReport = posted
	return nil
}

// buildDemoOpenReconciliation starts (but does NOT finalize) a reconciliation on
// Checking MX -- a DIFFERENT reconcilable account than the finalized Checking US one,
// so no balance-sum assertion fires and it composes robustly. It clears one of the
// account's splits so the workspace shows partial progress.
func buildDemoOpenReconciliation(ctx context.Context, s *store.Store, d *DemoIDs) error {
	// Statement balance is not asserted until Finalize, so any plausible figure is
	// fine for an OPEN recon; use the Checking MX opening deposit magnitude.
	reconID, err := s.StartReconciliation(ctx, d.CheckingMX, "MXN", "2026-06-30", 30_000_000)
	if err != nil {
		return fmt.Errorf("start open reconciliation: %w", err)
	}
	d.OpenRecon = reconID

	// Clear the first Checking MX split so the workspace shows in-progress clearing.
	splits, err := s.ReconciliationWorkspaceSplits(ctx, reconID)
	if err != nil {
		return fmt.Errorf("load open recon workspace: %w", err)
	}
	if len(splits) > 0 {
		if err := s.SetSplitReconciled(ctx, reconID, splits[0].SplitID, true); err != nil {
			return fmt.Errorf("clear open recon split: %w", err)
		}
	}
	return nil
}

// buildDemoImport creates a bank-import mapping profile and a staged (unposted) import
// batch on Checking US so the import screens (profiles list + staged-row review) show
// something. Rows are left STAGED (not posted) so the reviewer queue is populated.
func buildDemoImport(ctx context.Context, s *store.Store, d *DemoIDs) error {
	cfg := bankimport.Config{
		Delimiter: bankimport.DelimiterComma,
		HasHeader: true,
		Amount:    bankimport.AmountSingle,
		DateFmt:   bankimport.DateISO,
		DateCol:   0,
		DescCol:   1,
		AmountCol: 2,
		MemoCol:   -1, // unmapped
	}
	profileID, err := s.CreateMappingProfile(ctx, "Demo Bank CSV", cfg)
	if err != nil {
		return fmt.Errorf("create demo mapping profile: %w", err)
	}
	d.MappingProfile = profileID

	// A staged batch on Checking US (US subsidiary). uploadedAt is deterministic (no
	// wall clock): the day after the last fixture activity.
	batchID, err := s.CreateImportBatch(ctx, "demo-statement.csv", d.CheckingUS, d.US, profileID, "2026-07-01")
	if err != nil {
		return fmt.Errorf("create demo import batch: %w", err)
	}
	d.ImportBatch = batchID

	// Two synthetic parsed rows, left pending for the reviewer.
	rows := []bankimport.ParsedRow{
		{Date: "2026-06-28", AmountMinor: 125_000, Description: "Grant disbursement", Raw: []string{"2026-06-28", "Grant disbursement", "1250.00"}},
		{Date: "2026-06-29", AmountMinor: -48_000, Description: "Office rent", Raw: []string{"2026-06-29", "Office rent", "-480.00"}},
	}
	if _, err := s.StageImportRows(ctx, batchID, d.CheckingUS, rows); err != nil {
		return fmt.Errorf("stage demo import rows: %w", err)
	}
	return nil
}
