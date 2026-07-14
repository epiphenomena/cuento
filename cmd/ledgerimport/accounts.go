package main

import (
	"io"
	"sort"
	"strings"
)

// runAccounts reads the source export and emits a reviewable account-mapping CSV
// skeleton: one row per DISTINCT source account (keyed by `acct`) PLUS the
// synthetic two-level parent tiers derived from stmt + typ, with best-guess
// columns prefilled for the human to edit. It is the p09.3 `accounts` subcommand's
// core, factored to take io.Reader/io.Writer so tests drive it with synthetic
// content (the CLI wrapper opens the real file at runtime, p09.4).
//
// Best-guesses per leaf column:
//   - cuento_type  from `stmt` (A/L/I/E/O -> asset/liability/revenue/expense/equity)
//   - cuento_parent DERIVED as a two-level chain from stmt + typ (see below)
//   - subsidiaries  the UNION of every `country` the account appears under
//   - functional_class_default from `kls` (best guess; human confirms per D21)
//   - default_program from `kat` (best guess; human confirms per D24)
//   - name_en/name_es prefilled from the source `acct` path for the human to
//     translate.
//
// Parent derivation (D26; supersedes the explicit `parent` COLUMN, which is now
// IGNORED for structure). The hierarchy is a deterministic two-level chain:
//
//	<stmt super-parent>  ->  <(stmt,typ) intermediate>  ->  <leaf acct>
//
// e.g. stmt=A + typ="Bank" gives  Assets -> Bank -> BOA Checking. The importer
// synthesizes the intermediate and super-parent rows so build.go (which only sees
// the reviewed CSV, no `typ`) can create the tree parent-before-child unchanged.
//
// Edge cases:
//   - blank `typ`: the leaf is parented DIRECTLY under the stmt super-parent
//     (the intermediate tier is skipped -- there is nothing to name it).
//   - `typ` colliding with a real leaf `acct` name: intermediates carry a reserved,
//     non-account synthetic KEY (typParentKey / superParentKey) as their
//     SourceAcct, so a leaf literally named the same as a typ never collides.
//   - same `typ` under different `stmt`: intermediates are keyed by (stmt-type,typ),
//     so an asset "Bank" tier and an expense "Bank" tier stay DISTINCT -- this also
//     enforces type consistency, since a leaf always parents under an intermediate
//     of its OWN stmt type (D26: prefer the leaf's own stmt).
//   - a `typ` whose normalized text equals the super-parent's own name: the tier is
//     collapsed (leaf parented directly under the super-parent) to avoid a
//     self-referential intermediate.
//
// Synthetic parents carry the UNION of their descendants' subsidiaries (rule 7:
// parent subs must be a superset of every child's) and the leaf's cuento type.
//
// form990_code is left blank (a human decision, D25). Consolidation-marker rows
// (blank country/currency) are collected too -- the human decides in review
// whether their accounts belong; the skeleton does not silently drop them.
func runAccounts(source io.Reader, out io.Writer) error {
	recs, err := ParseRecords(source)
	if err != nil {
		return err
	}

	type agg struct {
		am   AccountMap
		subs map[string]bool // union of countries seen for this account
	}
	byAcct := map[string]*agg{}
	var order []string // first-seen order, then sorted for a stable skeleton

	// ensure returns the aggregate for a synthetic parent (intermediate or super),
	// creating it once (memoized) with the given display name and type. Its own
	// subsidiary union is accumulated as descendants are attached.
	ensure := func(key, name, ctype, parentKey string) *agg {
		a, ok := byAcct[key]
		if !ok {
			a = &agg{
				am: AccountMap{
					SourceAcct:   key,
					CuentoType:   ctype,
					CuentoParent: parentKey,
					Active:       true,
					NameEN:       name,
					NameES:       name,
				},
				subs: map[string]bool{},
			}
			byAcct[key] = a
			order = append(order, key)
		}
		return a
	}

	var leaves []string // real-account keys, to propagate subs up their chains (pass 2)
	for _, r := range recs {
		if r.Acct == "" {
			continue // no account name -> nothing to map
		}
		ctype := stmtToType(r.Stmt)

		// Resolve the leaf's immediate parent KEY via the stmt/typ chain, creating the
		// super-parent and (when typ is present) the (stmt,typ) intermediate. The
		// parent chain is fixed at the leaf's FIRST sighting (deterministic: records
		// are in file order); a leaf that later recurs under a different typ keeps its
		// first chain but still contributes its subsidiaries up it (pass 2 below), so
		// the parent's subsidiary set stays a superset of every descendant's (rule 7).
		leafParent := "" // top-level fallback when stmt is unknown (blank super-parent)
		superName := stmtToSuperParent(r.Stmt)
		if superName != "" {
			superKey := superParentKey(superName)
			ensure(superKey, superName, ctype, "")
			leafParent = superKey

			typName := strings.TrimSpace(r.Typ)
			// A typ that IS the super-parent name collapses the tier (no self-parent).
			if typName != "" && !strings.EqualFold(typName, superName) {
				interKey := typParentKey(ctype, typName)
				ensure(interKey, typName, ctype, superKey)
				leafParent = interKey
			}
		}

		a, ok := byAcct[r.Acct]
		if !ok {
			a = &agg{
				am: AccountMap{
					SourceAcct:      r.Acct,
					CuentoType:      ctype,
					CuentoParent:    leafParent, // fixed at first sighting
					FunctionalClass: functionalGuess(r.Kls),
					DefaultProgram:  r.Kat,
					Active:          true, // accounts default active; the human flags "(deleted)"
					NameEN:          r.Acct,
					NameES:          r.Acct,
				},
				subs: map[string]bool{},
			}
			byAcct[r.Acct] = a
			order = append(order, r.Acct)
			leaves = append(leaves, r.Acct)
		}
		if r.Country != "" {
			a.subs[r.Country] = true
		}
		// Fill a type if the first sighting lacked it but a later one has it. Parent is
		// deliberately NOT refilled here: it is fixed at first sighting (see above).
		if a.am.CuentoType == "" {
			a.am.CuentoType = ctype
		}
	}

	// Pass 2: propagate each leaf's FULL subsidiary set up its chosen parent chain, so
	// every synthetic intermediate/super-parent's subsidiaries are the UNION of the
	// leaves actually parented under it (rule 7 / Z12 superset -- build.go/CreateAccount
	// and cuento check reject a parent whose subs are not a superset of its children's,
	// and a leaf that recurs under several typs would otherwise leave a country only on
	// the leaf). Walks CuentoParent keys, bounded against a malformed chain.
	for _, leaf := range leaves {
		a := byAcct[leaf]
		for cur, depth := a.am.CuentoParent, 0; cur != "" && depth < 1024; depth++ {
			p, ok := byAcct[cur]
			if !ok {
				break // synthetic parents are always ensured; defensive
			}
			for c := range a.subs {
				p.subs[c] = true
			}
			cur = p.am.CuentoParent
		}
	}

	sort.Strings(order)
	rows := make([]AccountMap, 0, len(order))
	for _, name := range order {
		a := byAcct[name]
		subs := make([]string, 0, len(a.subs))
		for c := range a.subs {
			subs = append(subs, c)
		}
		sort.Strings(subs) // deterministic order for a reviewable diff
		a.am.Subsidiaries = subs
		rows = append(rows, a.am)
	}

	return WriteAccountMap(out, rows)
}

// superKeyPrefix / typKeyPrefix namespace the synthetic parent rows the skeleton
// emits. They are placed in each row's SourceAcct (the account map's key), NOT in
// a display name: build.go resolves cuento_parent against SourceAcct, so the key
// must be stable and unique. The prefixes make a synthetic parent impossible to
// confuse with a real source `acct` (an org account path never starts with these),
// which is what lets a leaf literally named the same as a `typ` coexist with the
// (stmt,typ) intermediate tier. The human sees the clean name_en/name_es; these
// keys are internal wiring, reviewable but not the leaf's business name.
const (
	superKeyPrefix = "::super:"
	typKeyPrefix   = "::typ:"
)

// superParentKey is the account-map key of the stmt super-parent named name
// (Assets/Liabilities/Revenue/Expenses/Equity).
func superParentKey(name string) string {
	return superKeyPrefix + name
}

// typParentKey is the account-map key of the (cuento-type, typ) intermediate tier.
// It is keyed by the cuento TYPE (not the raw stmt char) plus the typ text, so the
// same typ under two different stmt supertypes yields two DISTINCT intermediates
// (type consistency: a leaf parents under an intermediate of its own type).
func typParentKey(ctype, typ string) string {
	return typKeyPrefix + ctype + ":" + typ
}

// functionalGuess maps a raw source `kls` to a functional-class best guess for
// the skeleton. Only obvious matches are prefilled; anything else is left blank
// for the human (the real kls vocabulary is org data, folded in the mapping).
func functionalGuess(kls string) string {
	switch kls {
	case "program", "management", "fundraising":
		return kls
	default:
		return ""
	}
}
