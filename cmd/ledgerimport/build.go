package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"cuento/internal/store"
)

// build (p09.3) converts the cleaned full-ledger export into a cuento SQLite db
// by DRIVING internal/store -- so every produced row passes the same store
// invariants (rule 2/7). It never opens a transaction itself and never touches
// the amount columns via floating point (rule 3): amounts come from NetDebit over
// the authoritative db/cr columns.
//
// The core (runBuild) takes an io.Reader + a *store.Store so tests exercise it
// with synthetic content into a temp db (AGENTS rule 11: no real value here); the
// CLI wrapper migrates+opens the real file and constructs the store.

// BuildResult reports what the build produced, for the operator and for tests:
// the created entity ids (by source key) and the surfaced warnings (non-balancing
// groups etc. -- NEVER silently forced, docs hazard #4).
type BuildResult struct {
	SubsidiaryIDs map[string]int64 // subsidiary name -> id (incl. renamed root)
	ProgramIDs    map[string]int64 // program name -> id (incl. seeded root "General"? no -- created)
	FundIDs       map[string]int64 // source donor -> fund id
	AccountIDs    map[string]int64 // source_acct -> account id
	Warnings      []string

	// tidTxns records, per source tid, the transaction ids produced (one for a
	// single-currency group, N for a decomposed multi-currency group).
	tidTxns map[string][]int64
	// splitAccounts is the set of account ids that received at least one split.
	splitAccounts map[int64]bool
}

func (r *BuildResult) txnCountForTid(tid string) int { return len(r.tidTxns[tid]) }

func (r *BuildResult) accountHasSplit(id int64) bool { return r.splitAccounts[id] }

// rootSubsidiaryID is the seeded root subsidiary (migration id 1); build renames
// it rather than creating a second root (single-root trigger, D18).
const rootSubsidiaryID = int64(1)

// runBuild is the build subcommand's core. ctx must already carry an actor
// (store.WithActor) -- the CLI wrapper binds the system actor (id 1).
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

	// Load historical FX rates first (reference data, D12): the currencies they
	// reference are all migration-seeded, so this needs none of the entity phases,
	// and loading it up front means a report run against the produced db can convert
	// immediately. An empty batch is a no-op.
	if err := st.PutRates(ctx, rates); err != nil {
		return nil, fmt.Errorf("load rates: %w", err)
	}

	res := &BuildResult{
		SubsidiaryIDs: map[string]int64{},
		ProgramIDs:    map[string]int64{},
		FundIDs:       map[string]int64{},
		AccountIDs:    map[string]int64{},
		tidTxns:       map[string][]int64{},
		splitAccounts: map[int64]bool{},
	}

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
	if err := b.transactions(ctx, recs); err != nil {
		return nil, err
	}
	return res, nil
}

// builder holds the cross-phase state (id maps, currency exponents) so the phase
// methods stay small.
type builder struct {
	st        *store.Store
	cfg       Config
	res       *BuildResult
	anonymize bool

	exponent map[string]int // currency code -> minor-unit exponent (D1)
	payeeID  map[string]int64

	// acctType maps a created account id -> its cuento type. resolveSplit consults
	// it so a source dimension (functional class from kls, program from kat) is only
	// applied on the account types the store accepts it on (D21/D24) -- the source
	// populates kls on non-expense lines too, and the store rejects a functional
	// class on a non-expense split (ErrNonExpenseFunction).
	acctType map[int64]string
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
const rootProgramID = int64(1)

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
		var prog *int64
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
	return nil
}

// accounts builds the account tree from the reviewed account-mapping rows. It
// orders rows so every parent is created before its children (D11: a child needs
// its parent's id), and records the source_acct -> account id map.
func (b *builder) accounts(ctx context.Context, rows []AccountMap) error {
	ordered, err := topoAccounts(rows)
	if err != nil {
		return err
	}
	if b.acctType == nil {
		b.acctType = map[int64]string{}
	}

	for _, r := range ordered {
		var parent *int64
		if r.CuentoParent != "" {
			pid, ok := b.res.AccountIDs[r.CuentoParent]
			if !ok {
				return fmt.Errorf("account %q: parent %q not created", r.SourceAcct, r.CuentoParent)
			}
			parent = &pid
		}
		subs, err := b.subIDs(r.Subsidiaries)
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
		if r.NameES != "" {
			in.Names["es"] = r.NameES
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
	}
	return nil
}

// subIDs resolves subsidiary NAMES to ids via the created-subsidiary map.
func (b *builder) subIDs(names []string) ([]int64, error) {
	out := make([]int64, 0, len(names))
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

// hashText returns a short hex digest of s, used when --anonymize hides real
// payee/memo text so a shareable sample db carries no names or notes.
func hashText(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
