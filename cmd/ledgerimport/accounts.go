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
// IGNORED for structure). The hierarchy is a deterministic type-keyed tier:
//
//	<(stmt,typ) intermediate>  ->  <leaf acct>
//
// e.g. stmt=A + typ="Bank" gives  Bank -> BOA Checking, and the Bank tier is a
// ROOT account (nil parent). The importer synthesizes the intermediate rows so
// build.go (which only sees the reviewed CSV, no `typ`) can create the tree
// parent-before-child unchanged.
//
// p26.73: the stmt-tier super-parent accounts (Assets/Liabilities/Equity/Revenue/
// Expenses) are NO LONGER stored. They were redundant with each account's `type`
// field, and every report ALSO injects those tiers as section headers, so the
// stored chart doubled the header ("Assets > Assets"). Dropping them re-parents the
// type-level (`::typ:`) intermediates to ROOTS; a display-grouping tier keyed on
// `type` is injected downstream (chart + account selector), and reports keep their
// own section headers. The leaf's cuento `type` (still derived from stmt) is what
// display grouping keys on -- the stmt super-parent tier carried no additional
// information.
//
// Edge cases:
//   - blank `typ`: the leaf is a ROOT (nil parent) -- there is no intermediate tier
//     to name it, and the stmt super-parent it used to nest under is gone.
//   - `typ` colliding with a real leaf `acct` name: intermediates carry a reserved,
//     non-account synthetic KEY (typParentKey) as their SourceAcct, so a leaf
//     literally named the same as a typ never collides.
//   - same `typ` under different `stmt`: intermediates are keyed by (stmt-type,typ),
//     so an asset "Bank" tier and an expense "Bank" tier stay DISTINCT -- this also
//     enforces type consistency, since a leaf always parents under an intermediate
//     of its OWN stmt type (D26: prefer the leaf's own stmt).
//
// Synthetic intermediates carry the UNION of their descendants' subsidiaries (rule 7:
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

		// Resolve the leaf's immediate parent KEY from the (stmt,typ) intermediate
		// tier, creating that intermediate as a ROOT (p26.73: the stmt super-parent
		// tier is no longer stored). The parent is fixed at the leaf's FIRST sighting
		// (deterministic: records are in file order); a leaf that later recurs under a
		// different typ keeps its first parent but still contributes its subsidiaries up
		// it (pass 2 below), so the parent's subsidiary set stays a superset of every
		// descendant's (rule 7). A leaf with a blank typ (or unknown stmt) is itself a
		// ROOT -- there is no intermediate tier to nest it under.
		leafParent := ""
		if ctype != "" {
			typName := strings.TrimSpace(r.Typ)
			if typName != "" {
				interKey := typParentKey(ctype, typName)
				ensure(interKey, typName, ctype, "") // intermediate is a root now
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

// typKeyPrefix namespaces the synthetic intermediate rows the skeleton emits. It is
// placed in each row's SourceAcct (the account map's key), NOT in a display name:
// build.go resolves cuento_parent against SourceAcct, so the key must be stable and
// unique. The prefix makes a synthetic intermediate impossible to confuse with a
// real source `acct` (an org account path never starts with it), which is what lets
// a leaf literally named the same as a `typ` coexist with the (stmt,typ)
// intermediate tier. The human sees the clean name_en/name_es; these keys are
// internal wiring, reviewable but not the leaf's business name. (p26.73 removed the
// `::super:` stmt-tier prefix -- those accounts are no longer stored.)
const typKeyPrefix = "::typ:"

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
