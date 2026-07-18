package synth

import (
	"context"
	"fmt"

	"cuento/internal/auth"
	"cuento/internal/bankimport"
	"cuento/internal/store"
)

// Demo login credentials. The `cuento demo` generator seeds these three users and
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
	}
}

// DemoIDs holds the demo-only entity ids (users, expense reports, the open recon, the
// import profile/batch) on top of the shared IDs. It exists so the anti-drift test can
// assert feature coverage without re-querying.
type DemoIDs struct {
	IDs

	AdminUser     int64
	SubmitterUser int64
	ViewerUser    int64

	DraftReport     int64
	SubmittedReport int64
	PostedReport    int64

	OpenRecon int64 // an in-progress (unfinalized) reconciliation on Checking MX

	MappingProfile int64
	ImportBatch    int64 // a staged (unposted) import batch on Checking US
}

// BuildDemo builds the FULL demo dataset into the store: the canonical synthetic org
// (Build) + every opt-in seam (rates, reconciliation, capital campaign, sample budget)
// + demo-only data (three users across permission levels, expense reports in
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

	ids, err := Build(ctx, s)
	if err != nil {
		return d, fmt.Errorf("build base: %w", err)
	}
	d.IDs = ids

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

	// Capital campaign (posts new Checking US/MX splits) AFTER the recon is finalized.
	if err := ExtendCapitalCampaign(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	// Sample budget (old schedule-based model; no splits; order-independent).
	if err := ExtendSampleBudget(ctx, s, &d.IDs); err != nil {
		return d, err
	}

	// Sample budget PLAN (new p27.2 split-derived model; order-independent). Coexists
	// with the old SampleBudget until p27.3 retires the schedule model.
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

// buildDemoUsers seeds the three demo logins across permission levels with known
// passwords (DemoUsers). Passwords are hashed via auth.Hash (argon2id) exactly like
// `cuento user add`, so login works out of the box.
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

func createDemoUser(ctx context.Context, s *store.Store, username, display string, admin bool, txnPerm string) (int64, error) {
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
	// --- Draft: created with lines, left in draft.
	draft, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US)
	if err != nil {
		return fmt.Errorf("create draft expense report: %w", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, draft, store.ExpenseReportLineInput{
		AccountID: d.Occupancy, Amount: 45_000, ProgramID: &d.General, Description: "Office supplies (draft)",
	}); err != nil {
		return fmt.Errorf("add draft line: %w", err)
	}
	d.DraftReport = draft

	// --- Submitted: created, a line added, then submitted (awaiting review).
	submitted, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US)
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
	posted, err := s.CreateExpenseReport(ctx, d.SubmitterUser, d.US)
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
