package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// build (p09.3) converts the cleaned full-ledger export into a cuento SQLite db
// by DRIVING internal/store -- so every produced row passes the same store
// invariants (rule 2/7). It never opens a transaction itself and never touches
// the amount columns via floating point (rule 3): amounts come from nativeNetDebit
// over the authoritative base (db/cr) or native (fdb/fcr) column pair, selected by
// the split's currency (p26.56).
//
// The core (runBuild) takes an io.Reader + a *store.Store so tests exercise it
// with synthetic content into a temp db (AGENTS rule 11: no real value here); the
// CLI wrapper migrates+opens the real file and constructs the store.

// BuildResult reports what the build produced, for the operator and for tests:
// the created entity ids (by source key) and the surfaced warnings (non-balancing
// groups etc. -- NEVER silently forced, docs hazard #4).
type BuildResult struct {
	SubsidiaryIDs map[string]ids.SubsidiaryID // subsidiary name -> id (incl. renamed root)
	ProgramIDs    map[string]ids.ProgramID    // program name -> id (incl. seeded root "General"? no -- created)
	FundIDs       map[string]ids.FundID       // source donor -> fund id
	AccountIDs    map[string]ids.AccountID    // source_acct -> account id
	Warnings      []string

	// CampusFundID is the id of the marker-driven "campus" fund (cfg.CampusFund),
	// or nil when none is configured. Set in the scaffold and rehydrated on reload.
	CampusFundID *ids.FundID

	// tidTxns records, per source tid, the transaction ids produced (one for a
	// single-currency group, N for a decomposed multi-currency group).
	tidTxns map[string][]ids.TransactionID
	// splitAccounts is the set of account ids that received at least one split.
	splitAccounts map[ids.AccountID]bool
}

func (r *BuildResult) txnCountForTid(tid string) int { return len(r.tidTxns[tid]) }

// fundCount is the number of funds created: the donor-keyed funds plus the
// marker-driven campus fund when configured (which is NOT in FundIDs). Used only
// for the operator summary log so a created campus fund is not undercounted.
func (r *BuildResult) fundCount() int {
	n := len(r.FundIDs)
	if r.CampusFundID != nil {
		n++
	}
	return n
}

func (r *BuildResult) accountHasSplit(id ids.AccountID) bool { return r.splitAccounts[id] }

// rootSubsidiaryID is the seeded root subsidiary (migration id 1); build renames
// it rather than creating a second root (single-root trigger, D18).
const rootSubsidiaryID = ids.SubsidiaryID(1)

// newResult returns an empty BuildResult with every map initialized.
func newResult() *BuildResult {
	return &BuildResult{
		SubsidiaryIDs: map[string]ids.SubsidiaryID{},
		ProgramIDs:    map[string]ids.ProgramID{},
		FundIDs:       map[string]ids.FundID{},
		AccountIDs:    map[string]ids.AccountID{},
		tidTxns:       map[string][]ids.TransactionID{},
		splitAccounts: map[ids.AccountID]bool{},
	}
}

// runScaffold creates all subsidiary-INDEPENDENT reference data in a fresh db:
// rates, both subsidiaries, the program tree, funds, and the WHOLE account chart
// (incl. grouping placeholders and the synthetic FX Clearing / Opening Balances
// accounts) with each account's deactivations. It reads NO source rows and posts
// NO transactions -- those are the per-subsidiary, additive phase
// (runImportSubsidiary). This is the "scaffold once" half of the split-import
// model (D26): reference data is created here and only ever LOOKED UP afterwards.
// ctx must carry an actor (store.WithActor); the CLI binds the system actor.
func runScaffold(
	ctx context.Context,
	accMap []AccountMap,
	cfg Config,
	rates []store.Rate,
	st *store.Store,
	anonymize bool,
) (*BuildResult, error) {
	// Load historical FX rates first (reference data, D12): the currencies they
	// reference are all migration-seeded. An empty batch is a no-op.
	if err := st.PutRates(ctx, rates); err != nil {
		return nil, fmt.Errorf("load rates: %w", err)
	}
	res := newResult()
	b := &builder{st: st, cfg: cfg, res: res, anonymize: anonymize}
	if err := b.subsidiaries(ctx); err != nil {
		return nil, err
	}
	if err := b.programs(ctx); err != nil {
		return nil, err
	}
	if err := b.funds(ctx); err != nil {
		return nil, err
	}
	if err := b.accounts(ctx, accMap); err != nil {
		return nil, err
	}
	// NB scaffold does NOT deactivate the source-inactive ("(deleted)") accounts:
	// an inactive account may still hold historical splits that a later per-sub
	// import must post, and the leaf-active trigger forbids posting to an inactive
	// account. Deactivation is deferred to the per-sub phase, once every subsidiary
	// in an account's scope has been imported (deactivateReadyAccounts).
	return res, nil
}

// runImportSubsidiary ADDITIVELY imports one subsidiary's transactions into an
// already-scaffolded db. It creates nothing shared: it reloads the subsidiary,
// program, fund and account id maps FROM the db (fail loud if the scaffold is
// missing), refuses a subsidiary that already has transactions (re-import means a
// fresh scaffold, D26), then posts only that subsidiary's currency buckets and
// runs a native reconciliation gate. Safe to run once per subsidiary against a
// live, in-use db (every write still goes through the store, versioned).
func runImportSubsidiary(
	ctx context.Context,
	recs []Record,
	accMap []AccountMap,
	cfg Config,
	st *store.Store,
	subName string,
	anonymize bool,
) (*BuildResult, error) {
	res := newResult()
	b := &builder{st: st, cfg: cfg, res: res, anonymize: anonymize}
	if err := b.reloadState(ctx, accMap); err != nil {
		return nil, err
	}
	subID, ok := b.res.SubsidiaryIDs[subName]
	if !ok {
		return nil, fmt.Errorf("import-subsidiary: subsidiary %q not found; run scaffold first", subName)
	}
	// Idempotency guard: a subsidiary that already has transactions is not a clean
	// additive target (re-import = fresh scaffold + import, D26).
	n, err := st.SubsidiaryTxnCount(ctx, subID)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("import-subsidiary: subsidiary %q already has %d transactions; "+
			"re-import means a fresh db (scaffold + import from scratch)", subName, n)
	}
	// Shared counter accounts must exist AND be scoped to this subsidiary, else the
	// store rejects every counter-leg mid-import. Catch it before posting.
	if err := b.preflightSharedAccounts(ctx, subID); err != nil {
		return nil, err
	}
	b.importSub = subName
	if err := b.transactions(ctx, recs); err != nil {
		return nil, err
	}
	// Deactivate the source-inactive ("(deleted)") accounts whose whole subsidiary
	// scope is now imported (rule 14: deactivate, never delete). Deferred to here so
	// the historical splits post first.
	if err := b.deactivateReadyAccounts(ctx, accMap); err != nil {
		return nil, err
	}
	if err := b.reconcile(ctx, subName, subID); err != nil {
		return nil, err
	}
	return res, nil
}

// runFinalize posts the config's cross-subsidiary corrections (cfg.Corrections,
// D p26.72) into an already scaffolded + subsidiary-imported db -- the FINALIZE
// half of the split-import go-live (p27.0). The monolithic runBuild posts these
// on a fresh builder over the rehydrated id maps at its tail (see below); the
// split path (scaffold -> import-subsidiary per sub) had no such tail, so a phased
// go-live silently dropped every correction. This restores them as an explicit
// last step, run ONCE after the final import-subsidiary.
//
// It creates nothing shared: it reloads the subsidiary/program/fund/account id maps
// FROM the db (b.reloadState -- the SAME rehydration import-subsidiary uses, so a
// fresh process resolves every correction key) and posts cfg.Corrections through
// b.corrections -- via the store, versioned, invariant-checked (rule 5/7); a
// residual is a LOUD failure, never plugged. Exponents come from the db too
// (loadExponents reads st.Currencies), so no source is needed.
//
// Corrections reference accounts across MULTIPLE subsidiaries, so finalize refuses
// unless every configured (child) subsidiary already has transactions -- a
// correction touching a not-yet-imported subsidiary must fail loudly, telling the
// operator to import that subsidiary first. There is no natural key on a correction
// and no schema marker: finalize MUST be run once. Re-running double-posts every
// correction; recovery is restore-from-backup (like import-subsidiary, golive.md).
func runFinalize(
	ctx context.Context,
	accMap []AccountMap,
	cfg Config,
	st *store.Store,
	anonymize bool,
) (*BuildResult, error) {
	if len(cfg.Corrections) == 0 {
		return nil, fmt.Errorf("finalize: config has no corrections; nothing to post")
	}
	res := newResult()
	b := &builder{st: st, cfg: cfg, res: res, anonymize: anonymize}
	if err := b.reloadState(ctx, accMap); err != nil {
		return nil, err
	}
	// Every configured (child) subsidiary must be imported: a correction can span
	// subsidiaries, so finalize is only valid after the LAST import-subsidiary. Refuse
	// loudly and name the missing subsidiary so the operator imports it first.
	for _, subName := range configuredSubsidiaryNames(cfg) {
		subID, ok := res.SubsidiaryIDs[subName]
		if !ok {
			return nil, fmt.Errorf("finalize: subsidiary %q not found; run scaffold first", subName)
		}
		n, err := st.SubsidiaryTxnCount(ctx, subID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("finalize: subsidiary %q has no transactions yet; "+
				"import it (ledgerimport import-subsidiary) before finalize -- corrections "+
				"reference accounts across subsidiaries and must run after the LAST import", subName)
		}
	}
	if err := b.corrections(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// runBuild is the all-in-one build: scaffold a fresh db, then import every
// configured subsidiary additively. It preserves the original one-shot behavior
// (make fixture / dev-db) as a convenience wrapper over the split-import core.
// ctx must carry an actor (store.WithActor).
func runBuild(
	ctx context.Context,
	source io.Reader,
	accMap []AccountMap,
	cfg Config,
	rates []store.Rate,
	st *store.Store,
	anonymize bool,
) (*BuildResult, error) {
	recs, err := ParseRecords(source)
	if err != nil {
		return nil, err
	}
	res, err := runScaffold(ctx, accMap, cfg, rates, st, anonymize)
	if err != nil {
		return nil, err
	}
	for _, subName := range configuredSubsidiaryNames(cfg) {
		subRes, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, subName, anonymize)
		if err != nil {
			return nil, err
		}
		mergeResult(res, subRes)
	}
	// Manual adjustment transactions (D p26.72) post AFTER every subsidiary's
	// transactions, so any account/fund/program they touch is live. The builder
	// carries the scaffold's id maps on `res`, so a fresh builder over the same res
	// resolves the correction keys.
	cb := &builder{st: st, cfg: cfg, res: res, anonymize: anonymize, acctType: nil}
	if err := cb.corrections(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// configuredSubsidiaryNames returns the distinct child-subsidiary names in the
// config, sorted for a deterministic import order.
func configuredSubsidiaryNames(cfg Config) []string {
	seen := map[string]bool{}
	var names []string
	for _, sc := range cfg.Subsidiaries {
		if !seen[sc.Name] {
			seen[sc.Name] = true
			names = append(names, sc.Name)
		}
	}
	sort.Strings(names)
	return names
}

// mergeResult folds a per-subsidiary result's posted transactions, touched
// accounts, and warnings into an accumulating result (the entity id maps already
// live on dst from the scaffold and are identical).
func mergeResult(dst, src *BuildResult) {
	for tid, txns := range src.tidTxns {
		dst.tidTxns[tid] = append(dst.tidTxns[tid], txns...)
	}
	for id := range src.splitAccounts {
		dst.splitAccounts[id] = true
	}
	dst.Warnings = append(dst.Warnings, src.Warnings...)
}

// builder holds the cross-phase state (id maps, currency exponents) so the phase
// methods stay small.
type builder struct {
	st        *store.Store
	cfg       Config
	res       *BuildResult
	anonymize bool

	exponent map[string]int // currency code -> minor-unit exponent (D1)

	// importSub is the subsidiary NAME a per-subsidiary import targets: postGroup
	// posts only the currency buckets resolving to it (empty = all subsidiaries,
	// the all-in-one build path). See transactions.go postGroup.
	importSub string

	// campusPlan is the Pass-1 result of the campus (Restore the Way) drawdown model
	// (D p26.43): for each campus revenue/expense split (keyed by tid + group index),
	// whether it is assigned the restricted fund (true) or overflowed the pool to
	// unrestricted (false). Empty when no campus fund is configured. Computed once at
	// the top of transactions() over the full skip-filtered export.
	campusPlan campusPlan

	// campusAssetAccts is the set of source_acct keys (cfg.CampusAssetAccounts) whose
	// FIXED-ASSET splits belong to the campus fund even without a kat=campus marker (D
	// p26.46). resolveSplit tags such a split RtW directly, bypassing the R/E-only
	// campus guard; buildCampusPlan ignores it (an asset swap does not touch the
	// drawdown pool). Built once at the top of transactions() from cfg (so it survives
	// the separate-process per-subsidiary reload -- cfg is always loaded).
	campusAssetAccts map[string]bool

	// acctType maps a created account id -> its cuento type. resolveSplit consults
	// it so a source dimension (functional class from kls, program from kat) is only
	// applied on the account types the store accepts it on (D21/D24) -- the source
	// populates kls on non-expense lines too, and the store rejects a functional
	// class on a non-expense split (ErrNonExpenseFunction).
	acctType map[ids.AccountID]string
}

// subsidiaries renames the seeded root and creates one child per configured
// country. It records the subsidiary NAME -> id map (funds/accounts reference
// subs by name, matching the human-readable mapping files).
func (b *builder) subsidiaries(ctx context.Context) error {
	rootName := b.cfg.RootSubsidiaryName
	rootCur := b.cfg.RootBaseCurrency
	if err := b.st.UpdateSubsidiary(ctx, rootSubsidiaryID, store.UpdateSubsidiaryInput{
		Name:         &rootName,
		BaseCurrency: &rootCur,
	}); err != nil {
		return fmt.Errorf("rename root subsidiary: %w", err)
	}
	b.res.SubsidiaryIDs[rootName] = rootSubsidiaryID

	// Deterministic order over the country map for reproducible ids/output.
	countries := sortedKeys(b.cfg.Subsidiaries)
	for _, c := range countries {
		sc := b.cfg.Subsidiaries[c]
		id, err := b.st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{
			ParentID:     rootSubsidiaryID,
			Name:         sc.Name,
			BaseCurrency: sc.BaseCurrency,
		})
		if err != nil {
			return fmt.Errorf("create subsidiary %q: %w", sc.Name, err)
		}
		b.res.SubsidiaryIDs[sc.Name] = id
	}
	return nil
}

// rootProgramID is the seeded root program ("General", migration id 1). Programs
// derived from `kat` are created under it (D24).
const rootProgramID = ids.ProgramID(1)

// programs creates the program tree: one program per distinct name in the kat
// map (Programs) and the klass map (ProgramClasses), nested per ProgramParents
// (default parent = root "General"). Names are created parent-before-child so a
// child's parent id exists, and each name is created once (many kats/klasses may
// map to one program). Records the program NAME -> id map.
func (b *builder) programs(ctx context.Context) error {
	// Root "General" is always addressable for the fallback default.
	b.res.ProgramIDs["General"] = rootProgramID

	// Collect every distinct program name referenced by either map.
	names := map[string]bool{}
	for _, n := range b.cfg.Programs {
		names[n] = true
	}
	for _, n := range b.cfg.ProgramClasses {
		names[n] = true
	}

	// parentOf resolves a name's parent name (default "General"). A parent that is
	// itself a mapped program is created first via the recursive create below.
	parentOf := func(name string) string {
		if p, ok := b.cfg.ProgramParents[name]; ok && p != "" {
			return p
		}
		return "General"
	}

	// create is parent-before-child, memoized via ProgramIDs; it rejects a cycle and
	// an unknown parent that is neither General nor a mapped program name.
	var create func(name string, stack map[string]bool) error
	create = func(name string, stack map[string]bool) error {
		if _, ok := b.res.ProgramIDs[name]; ok {
			return nil // already created (or the root)
		}
		if stack[name] {
			return fmt.Errorf("program tree cycle at %q", name)
		}
		parent := parentOf(name)
		if _, ok := b.res.ProgramIDs[parent]; !ok {
			if !names[parent] {
				return fmt.Errorf("program %q: parent %q is not a configured program", name, parent)
			}
			stack[name] = true
			if err := create(parent, stack); err != nil {
				return err
			}
			delete(stack, name)
		}
		id, err := b.st.CreateProgram(ctx, store.CreateProgramInput{
			ParentID: b.res.ProgramIDs[parent],
			Name:     name,
		})
		if err != nil {
			return fmt.Errorf("create program %q: %w", name, err)
		}
		b.res.ProgramIDs[name] = id
		return nil
	}

	for _, name := range sortedKeys(names) { // deterministic id order
		if err := create(name, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

// funds creates one fund per configured donor and records the DONOR -> fund id
// map. Subsidiary scope is resolved from subsidiary names; an optional program
// scope from the program name.
func (b *builder) funds(ctx context.Context) error {
	for _, donor := range sortedKeys(b.cfg.Funds) {
		fc := b.cfg.Funds[donor]
		subs, err := b.subIDs(fc.Subsidiaries)
		if err != nil {
			return fmt.Errorf("fund %q: %w", fc.Name, err)
		}
		var prog *ids.ProgramID
		if fc.Program != "" {
			pid, ok := b.res.ProgramIDs[fc.Program]
			if !ok {
				return fmt.Errorf("fund %q: program %q not configured", fc.Name, fc.Program)
			}
			prog = &pid
		}
		id, err := b.st.CreateFund(ctx, store.CreateFundInput{
			Name:         fc.Name,
			Funder:       fc.Funder,
			Purpose:      fc.Purpose,
			Restriction:  fc.Restriction,
			ProgramID:    prog,
			Subsidiaries: subs,
		})
		if err != nil {
			return fmt.Errorf("create fund %q: %w", fc.Name, err)
		}
		b.res.FundIDs[donor] = id
	}
	return b.campusFund(ctx)
}

// campusFund creates the marker-driven "campus" fund (cfg.CampusFund) if
// configured, scoping it to ALL configured child subsidiaries -- a superset,
// computed programmatically (not hardcoded), of every subsidiary that posts a
// kat=campus split (verified against the go-live data as exactly that child set).
// Scaffold reads no source rows, so the scope cannot be narrowed to the observed
// campus subs at scaffold time; the full child set is provably a superset and keeps
// the store's "txn subsidiary within the fund's subsidiary set" invariant satisfied
// for every campus posting. Its id is recorded on the builder so resolveSplit can
// tag campus splits with it (D p26.40).
func (b *builder) campusFund(ctx context.Context) error {
	if b.cfg.CampusFund == nil {
		return nil
	}
	fc := b.cfg.CampusFund
	subs, err := b.subIDs(configuredSubsidiaryNames(b.cfg))
	if err != nil {
		return fmt.Errorf("campus fund %q: %w", fc.Name, err)
	}
	if len(subs) == 0 {
		return fmt.Errorf("campus fund %q: no child subsidiaries configured", fc.Name)
	}
	var prog *ids.ProgramID
	if fc.Program != "" {
		pid, ok := b.res.ProgramIDs[fc.Program]
		if !ok {
			return fmt.Errorf("campus fund %q: program %q not configured", fc.Name, fc.Program)
		}
		prog = &pid
	}
	id, err := b.st.CreateFund(ctx, store.CreateFundInput{
		Name:         fc.Name,
		Funder:       fc.Funder,
		Purpose:      fc.Purpose,
		Restriction:  fc.Restriction,
		ProgramID:    prog,
		Subsidiaries: subs,
	})
	if err != nil {
		return fmt.Errorf("create campus fund %q: %w", fc.Name, err)
	}
	b.res.CampusFundID = &id
	return nil
}

// accounts builds the account tree from the reviewed account-mapping rows. It
// orders rows so every parent is created before its children (D11: a child needs
// its parent's id), and records the source_acct -> account id map.
func (b *builder) accounts(ctx context.Context, rows []AccountMap) error {
	merges, err := resolveMerges(rows)
	if err != nil {
		return err
	}

	ordered, err := topoAccounts(rows)
	if err != nil {
		return err
	}
	if b.acctType == nil {
		b.acctType = map[ids.AccountID]string{}
	}

	for _, r := range ordered {
		// A merge-in (merge_into SOURCE) row creates NO second account: it is wired
		// AFTER the loop, aliasing its source_acct onto the canonical account's id.
		// Skipping it here keeps the CreateAccount sequence (and thus every id)
		// identical to the historical single-pass build when no row declares a merge.
		if r.MergeInto != "" {
			continue
		}

		var parent *ids.AccountID
		if r.CuentoParent != "" {
			pid, ok := b.res.AccountIDs[r.CuentoParent]
			if !ok {
				return fmt.Errorf("account %q: parent %q not created", r.SourceAcct, r.CuentoParent)
			}
			parent = &pid
		}

		// If this row is a merge CANONICAL (a partner merges INTO it), it carries the
		// UNION of both rows' subsidiaries, and it is active if EITHER row is active.
		// name_en comes from this (canonical) row; name_es from the merge-in partner.
		subsNames := r.Subsidiaries
		nameES := r.NameES
		if mi, ok := merges[r.SourceAcct]; ok {
			subsNames = mi.subsidiaries
			if mi.nameES != "" {
				nameES = mi.nameES
			}
		}

		subs, err := b.subIDs(subsNames)
		if err != nil {
			return fmt.Errorf("account %q: %w", r.SourceAcct, err)
		}
		if len(subs) == 0 {
			return fmt.Errorf("account %q: no subsidiaries mapped", r.SourceAcct)
		}

		in := store.CreateAccountInput{
			ParentID:        parent,
			Type:            r.CuentoType,
			DefaultCurrency: b.cfg.BaseCurrency,
			Names:           map[string]string{"en": r.NameEN},
			Subsidiaries:    subs,
			Intercompany:    r.Intercompany,
		}
		if nameES != "" {
			in.Names["es"] = nameES
		}
		if r.CuentoType == "expense" && r.FunctionalClass != "" {
			fc := r.FunctionalClass
			in.FunctionalClass = &fc
		}
		if (r.CuentoType == "revenue" || r.CuentoType == "expense") && r.DefaultProgram != "" {
			pid, ok := b.res.ProgramIDs[r.DefaultProgram]
			if !ok {
				return fmt.Errorf("account %q: default program %q not configured", r.SourceAcct, r.DefaultProgram)
			}
			in.DefaultProgramID = &pid
		}
		if r.Form990Code != "" {
			code := r.Form990Code
			in.Form990Code = &code
		}

		id, err := b.st.CreateAccount(ctx, in)
		if err != nil {
			return fmt.Errorf("create account %q: %w", r.SourceAcct, err)
		}
		b.res.AccountIDs[r.SourceAcct] = id
		b.acctType[id] = r.CuentoType
		// Source-inactive ("(deleted)") accounts are created ACTIVE here and
		// deactivated later, per subsidiary, once their historical splits have posted
		// (deactivateReadyAccounts).
	}

	// Pass 2: wire each merge-in row's source_acct to its canonical account id, so
	// splits keyed by EITHER source string (transactions.go resolves via
	// b.res.AccountIDs[r.Acct]) land on the one merged account. Order-independent:
	// the canonical account is guaranteed created above (it is never a merge-in row).
	for _, r := range ordered {
		if r.MergeInto == "" {
			continue
		}
		id, ok := b.res.AccountIDs[r.MergeInto]
		if !ok {
			return fmt.Errorf("account %q: merge_into target %q not created", r.SourceAcct, r.MergeInto)
		}
		b.res.AccountIDs[r.SourceAcct] = id
		b.acctType[id] = r.CuentoType
	}
	return nil
}

// mergeInfo is the derived shape of a merged (bilingual) account, keyed by the
// CANONICAL row's source_acct in the map resolveMerges returns.
type mergeInfo struct {
	subsidiaries []string // union of both rows' subsidiaries, sorted
	nameES       string   // Spanish name, taken from the merge-in (merge_into SOURCE) row
}

// resolveMerges validates the merge_into links and derives, per CANONICAL row, the
// merged account's unioned subsidiaries and Spanish name. The MERGE RULE (D p26.116):
//
//   - the canonical (merge_into TARGET) row supplies name_en;
//   - the merge-in (merge_into SOURCE) row supplies name_es;
//   - subsidiaries are the UNION of both rows (sorted, deterministic);
//   - the account is active if EITHER row is active (handled where active is read).
//
// It rejects a merge_into pointing at a missing row, at another merge-in row
// (chains are not allowed -- the target must be a real canonical account), or two
// rows merging into the same canonical (a canonical takes exactly one partner:
// this is a US<->UPH pairing, not a many-way collapse).
func resolveMerges(rows []AccountMap) (map[string]mergeInfo, error) {
	byName := make(map[string]AccountMap, len(rows))
	for _, r := range rows {
		byName[r.SourceAcct] = r
	}
	// A merge-in row must be a LEAF (no other row parents under it): accounts() skips
	// it in pass 1, so a child depending on its id would fail its parent lookup. The
	// pairing sheet only ever proposes leaf pairs, so this is a guard, not a limit.
	hasChild := map[string]bool{}
	for _, r := range rows {
		if r.CuentoParent != "" {
			hasChild[r.CuentoParent] = true
		}
	}
	out := map[string]mergeInfo{}
	claimed := map[string]string{} // canonical source_acct -> the merge-in row that took it
	for _, r := range rows {
		if r.MergeInto == "" {
			continue
		}
		if hasChild[r.SourceAcct] {
			return nil, fmt.Errorf("account %q has merge_into set but is not a leaf (has children); only leaves may merge", r.SourceAcct)
		}
		canon, ok := byName[r.MergeInto]
		if !ok {
			return nil, fmt.Errorf("account %q: merge_into target %q not found", r.SourceAcct, r.MergeInto)
		}
		if canon.MergeInto != "" {
			return nil, fmt.Errorf("account %q: merge_into target %q is itself a merge-in row (chains not allowed)",
				r.SourceAcct, r.MergeInto)
		}
		if canon.CuentoType != r.CuentoType {
			return nil, fmt.Errorf("account %q merges into %q but types differ (%q vs %q)",
				r.SourceAcct, r.MergeInto, r.CuentoType, canon.CuentoType)
		}
		if prev, dup := claimed[r.MergeInto]; dup {
			return nil, fmt.Errorf("account %q: canonical %q already merged from %q (one partner per canonical)",
				r.SourceAcct, r.MergeInto, prev)
		}
		claimed[r.MergeInto] = r.SourceAcct
		out[r.MergeInto] = mergeInfo{
			subsidiaries: unionSorted(canon.Subsidiaries, r.Subsidiaries),
			nameES:       r.NameES,
		}
	}
	return out, nil
}

// unionSorted returns the sorted set-union of two subsidiary-name lists.
func unionSorted(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// subsOf returns the Subsidiaries of the row with the given source_acct (or nil).
func subsOf(rows []AccountMap, sourceAcct string) []string {
	for _, r := range rows {
		if r.SourceAcct == sourceAcct {
			return r.Subsidiaries
		}
	}
	return nil
}

// mergedActive reports the effective active flag for a (possibly merged) account:
// active if EITHER the row or its merge partner is active. merges is keyed by the
// CANONICAL source_acct. For a non-merge row it degrades to m.Active.
func mergedActive(m AccountMap, rows []AccountMap) bool {
	if m.Active {
		return true
	}
	// A canonical row: active if its merge-in partner is active.
	for _, r := range rows {
		if r.MergeInto == m.SourceAcct && r.Active {
			return true
		}
	}
	// A merge-in row: active if its canonical target is active.
	if m.MergeInto != "" {
		for _, r := range rows {
			if r.SourceAcct == m.MergeInto && r.Active {
				return true
			}
		}
	}
	return false
}

// deactivateReadyAccounts sets active=0 on the mapping's source-inactive
// ("(deleted)") accounts whose ENTIRE subsidiary scope now has transactions -- so a
// not-yet-imported subsidiary can still post its historical splits to the account
// first. Each is one versioned store change (op='update', never a hard delete --
// rule 14); an already-inactive account is skipped (idempotent across per-sub runs
// for an account shared by several subsidiaries).
func (b *builder) deactivateReadyAccounts(ctx context.Context, accMap []AccountMap) error {
	for _, m := range accMap {
		// A merged account is active if EITHER partner is active, so it is only a
		// deactivation candidate when both sides are inactive. This also means a
		// merge-in row never deactivates the SHARED (canonical) account out from
		// under an active partner (both rows resolve to the one id now).
		if mergedActive(m, accMap) {
			continue
		}
		id, ok := b.res.AccountIDs[m.SourceAcct]
		if !ok {
			continue
		}
		// Readiness spans the account's FULL merged subsidiary scope (a merge-in row
		// carries only its own subs, so consult the canonical when this is one).
		readySubs := m.Subsidiaries
		if m.MergeInto != "" {
			readySubs = unionSorted(m.Subsidiaries, subsOf(accMap, m.MergeInto))
		} else {
			for _, r := range accMap {
				if r.MergeInto == m.SourceAcct {
					readySubs = unionSorted(m.Subsidiaries, r.Subsidiaries)
				}
			}
		}
		ready := true
		for _, sn := range readySubs {
			sid, ok := b.res.SubsidiaryIDs[sn]
			if !ok {
				ready = false
				break
			}
			n, err := b.st.SubsidiaryTxnCount(ctx, sid)
			if err != nil {
				return err
			}
			if n == 0 {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		acct, err := b.st.GetAccount(ctx, id)
		if err != nil {
			return fmt.Errorf("deactivate: load account %d: %w", id, err)
		}
		if acct.Active == 0 {
			continue // already inactive
		}
		if err := b.st.DeactivateAccount(ctx, id); err != nil {
			return fmt.Errorf("deactivate account %d: %w", id, err)
		}
	}
	return nil
}

// reloadState rehydrates the cross-phase id maps FROM the scaffolded db (instead
// of creating), so a per-subsidiary import running in a separate process can
// resolve every shared entity by lookup. Fails loud if the scaffold is missing or
// inconsistent with the mapping -- shared entities are never created here.
func (b *builder) reloadState(ctx context.Context, accMap []AccountMap) error {
	subs, err := b.st.SubTree(ctx)
	if err != nil {
		return fmt.Errorf("reload subsidiaries: %w", err)
	}
	for _, s := range subs {
		b.res.SubsidiaryIDs[s.Name] = s.ID
	}
	progs, err := b.st.ProgramTree(ctx)
	if err != nil {
		return fmt.Errorf("reload programs: %w", err)
	}
	for _, p := range progs {
		b.res.ProgramIDs[p.Name] = p.ID
	}
	// Funds are keyed by DONOR in res.FundIDs (resolveSplit looks up by r.Donor);
	// the db stores them by name, so invert cfg.Funds (donor -> Name) to recover it.
	funds, err := b.st.ListFunds(ctx)
	if err != nil {
		return fmt.Errorf("reload funds: %w", err)
	}
	fundByName := make(map[string]ids.FundID, len(funds))
	for _, f := range funds {
		fundByName[f.Name] = f.ID
	}
	for donor, fc := range b.cfg.Funds {
		if id, ok := fundByName[fc.Name]; ok {
			b.res.FundIDs[donor] = id
		}
	}
	// Rehydrate the marker-driven campus fund (created in the scaffold under its
	// own name, not the donor-keyed map) so a per-subsidiary import running in a
	// separate process can tag kat=campus splits with it.
	if b.cfg.CampusFund != nil {
		if id, ok := fundByName[b.cfg.CampusFund.Name]; ok {
			campusID := id
			b.res.CampusFundID = &campusID
		} else {
			return fmt.Errorf("reload funds: campus fund %q not in db; scaffold first", b.cfg.CampusFund.Name)
		}
	}
	return b.reloadAccounts(ctx, accMap)
}

// reloadAccounts rebuilds res.AccountIDs (source_acct -> id) and acctType from the
// db by matching each account-mapping row to its db account via a key of its cuento
// TYPE + the FULL number-free NAME PATH (root..self). The path is the only stable
// name-based key (source_acct is not persisted on the account row): names are not
// globally unique (only siblings are disambiguated), but a full name path is nearly
// unique. The TYPE prefix is the disambiguator for the p26.73 model, where the stmt
// super-parent tier is gone and two same-named type-tier ROOTS of DIFFERENT types
// (e.g. a revenue "Transfers" tier and an expense "Transfers" tier) would otherwise
// collide on the bare path. Type is constant along a chain (a leaf parents under an
// intermediate of its own type), so keying on the self's type is consistent between
// the db and the mapping. Fails loud on any unmatched row or a duplicate (type,path)
// -- proof the scaffold used this same mapping.
func (b *builder) reloadAccounts(ctx context.Context, accMap []AccountMap) error {
	if b.acctType == nil {
		b.acctType = map[ids.AccountID]string{}
	}
	rows, err := b.st.Tree(ctx, "en", nil)
	if err != nil {
		return fmt.Errorf("reload accounts: %w", err)
	}
	name := make(map[ids.AccountID]string, len(rows))
	parent := make(map[ids.AccountID]sql.NullInt64, len(rows))
	typ := make(map[ids.AccountID]string, len(rows))
	for _, r := range rows {
		name[r.ID] = r.Name
		parent[r.ID] = r.ParentID
		typ[r.ID] = r.Type
	}
	dbPath := make(map[string]ids.AccountID, len(rows))
	for _, r := range rows {
		k := typedAccountKey(r.Type, dbAccountPath(r.ID, name, parent))
		if _, dup := dbPath[k]; dup {
			return fmt.Errorf("reload accounts: duplicate typed name path %q in db",
				strings.ReplaceAll(k, "\x00", ":"))
		}
		dbPath[k] = r.ID
	}

	nameEN := make(map[string]string, len(accMap)) // source_acct -> NameEN
	parEN := make(map[string]string, len(accMap))  // source_acct -> parent source_acct
	for _, m := range accMap {
		nameEN[m.SourceAcct] = m.NameEN
		parEN[m.SourceAcct] = m.CuentoParent
	}
	for _, m := range accMap {
		// A merge-in (merge_into SOURCE) row has NO account of its own in the db --
		// the scaffold created ONE bilingual account under the CANONICAL row's name_en
		// path. Resolve via the canonical's path (its own name_en/name_es may differ)
		// and alias this source_acct onto that id, mirroring accounts()' two-pass wiring
		// so both source strings resolve on the phased-import path too.
		key := m.SourceAcct
		if m.MergeInto != "" {
			key = m.MergeInto
		}
		p, err := mapAccountPath(key, nameEN, parEN)
		if err != nil {
			return fmt.Errorf("reload accounts: %w", err)
		}
		k := typedAccountKey(m.CuentoType, p)
		id, ok := dbPath[k]
		if !ok {
			return fmt.Errorf("reload accounts: account %q (typed path %q) not in db; scaffold with the same mapping first",
				m.SourceAcct, strings.ReplaceAll(k, "\x00", ":"))
		}
		b.res.AccountIDs[m.SourceAcct] = id
		b.acctType[id] = m.CuentoType
	}
	return nil
}

// typedAccountKey namespaces a NUL-joined name path by the account's cuento type, so
// two same-named roots of different types (p26.73: type-tier roots like a revenue vs
// an expense "Transfers") do not collide. Type is constant along a chain, so the
// self's type keys the whole path consistently on both the db and mapping sides.
func typedAccountKey(cuentoType, path string) string {
	return cuentoType + "\x00" + path
}

// dbAccountPath returns the NUL-joined name path root..self for a db account.
func dbAccountPath(id ids.AccountID, name map[ids.AccountID]string, parent map[ids.AccountID]sql.NullInt64) string {
	var segs []string
	for cur, depth := id, 0; depth < 1024; depth++ {
		segs = append([]string{name[cur]}, segs...)
		p := parent[cur]
		if !p.Valid {
			break
		}
		cur = ids.AccountID(p.Int64)
	}
	return strings.Join(segs, "\x00")
}

// mapAccountPath returns the NUL-joined NameEN path root..self for an account-map
// row, walking CuentoParent up. It rejects a missing parent or an over-deep chain
// (a cycle) rather than looping forever.
func mapAccountPath(sourceAcct string, nameEN, parEN map[string]string) (string, error) {
	var segs []string
	cur := sourceAcct
	for depth := 0; ; depth++ {
		if depth >= 1024 {
			return "", fmt.Errorf("account %q parent chain too deep (cycle?)", sourceAcct)
		}
		en, ok := nameEN[cur]
		if !ok {
			return "", fmt.Errorf("account %q references missing parent %q", sourceAcct, cur)
		}
		segs = append([]string{en}, segs...)
		par := parEN[cur]
		if par == "" {
			break
		}
		cur = par
	}
	return strings.Join(segs, "\x00"), nil
}

// preflightSharedAccounts verifies the synthetic counter accounts (FX Clearing,
// Opening Balances) exist in the scaffolded db AND are scoped to the importing
// subsidiary -- else the store rejects every counter-leg mid-import. Fails loud so
// a misconfigured scaffold is caught before any transaction posts.
func (b *builder) preflightSharedAccounts(ctx context.Context, subID ids.SubsidiaryID) error {
	for _, key := range []string{b.cfg.FXClearingAccount, b.cfg.OpeningBalanceAccount} {
		id, ok := b.res.AccountIDs[key]
		if !ok {
			return fmt.Errorf("shared counter account %q missing from db; run scaffold first", key)
		}
		subs, err := b.st.AccountSubsidiaryIDs(ctx, id)
		if err != nil {
			return err
		}
		found := false
		for _, s := range subs {
			if s == subID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("shared counter account %q not scoped to subsidiary id %d; "+
				"re-scaffold with it mapped to every posting subsidiary", key, subID)
		}
	}
	return nil
}

// reconcile is the per-subsidiary native reconciliation GATE: the net-debit split
// total per currency must be zero (every posted transaction balances, so the
// subsidiary total does too -- a nonzero total means a posted-splits bug, so fail
// loud). The per-type breakdown it reads is surfaced to the operator by the CLI
// (importSubCmd) for a manual trial-balance check against the source books.
func (b *builder) reconcile(ctx context.Context, subName string, subID ids.SubsidiaryID) error {
	totals, err := b.st.SubsidiaryNativeTotals(ctx, subID)
	if err != nil {
		return err
	}
	byCur := map[string]int64{}
	for _, t := range totals {
		byCur[t.Currency] += t.Total
	}
	for cur, sum := range byCur {
		if sum != 0 {
			return fmt.Errorf("subsidiary %q native imbalance in %s: %d minor units "+
				"(posted splits do not net to zero)", subName, cur, sum)
		}
	}
	return nil
}

// subIDs resolves subsidiary NAMES to ids via the created-subsidiary map.
func (b *builder) subIDs(names []string) ([]ids.SubsidiaryID, error) {
	out := make([]ids.SubsidiaryID, 0, len(names))
	for _, n := range names {
		id, ok := b.res.SubsidiaryIDs[n]
		if !ok {
			return nil, fmt.Errorf("subsidiary %q not configured", n)
		}
		out = append(out, id)
	}
	return out, nil
}

// sortedKeys returns the map keys in sorted order (deterministic build output).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// topoAccounts orders account-mapping rows parent-before-child. It rejects an
// unknown parent reference and a cycle (both mapping errors surfaced for review).
func topoAccounts(rows []AccountMap) ([]AccountMap, error) {
	byName := map[string]AccountMap{}
	for _, r := range rows {
		byName[r.SourceAcct] = r
	}
	var out []AccountMap
	placed := map[string]bool{}
	var visit func(name string, stack map[string]bool) error
	visit = func(name string, stack map[string]bool) error {
		if placed[name] {
			return nil
		}
		if stack[name] {
			return fmt.Errorf("account tree cycle at %q", name)
		}
		r, ok := byName[name]
		if !ok {
			return fmt.Errorf("account %q references missing parent", name)
		}
		if r.CuentoParent != "" {
			stack[name] = true
			if err := visit(r.CuentoParent, stack); err != nil {
				return err
			}
			delete(stack, name)
		}
		placed[name] = true
		out = append(out, r)
		return nil
	}
	// Deterministic: sort names first.
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.SourceAcct)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := visit(n, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

var errSkip = errors.New("skip")

// hashText returns a short hex digest of s. It is applied ONLY to per-split (and
// correction) DESCRIPTIONS under --anonymize; that is the sole redaction the flag
// performs. Entity names (funds, donors, subsidiaries, accounts) and every other
// field pass through raw, so an --anonymize db is NOT safe to share publicly — use
// `cuento demo` (a fully synthetic database) for that. See the --anonymize flag
// usage strings and DECISIONS (p26.106).
func hashText(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
