package main

import (
	"io"
	"sort"
)

// runAccounts reads the source export and emits a reviewable account-mapping CSV
// skeleton: one row per DISTINCT source account (keyed by `acct`), with
// best-guess columns prefilled for the human to edit. It is the p09.3 `accounts`
// subcommand's core, factored to take io.Reader/io.Writer so tests drive it with
// synthetic content (the CLI wrapper opens the real file at runtime, p09.4).
//
// Best-guesses per column:
//   - cuento_type  from `stmt` (A/L/I/E/O -> asset/liability/revenue/expense/equity)
//   - cuento_parent from `parent`
//   - subsidiaries  the UNION of every `country` the account appears under
//   - functional_class_default from `kls` (best guess; human confirms per D21)
//   - default_program from `kat` (best guess; human confirms per D24)
//   - name_en/name_es prefilled from the source `acct` path for the human to
//     translate.
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

	for _, r := range recs {
		if r.Acct == "" {
			continue // no account name -> nothing to map
		}
		a, ok := byAcct[r.Acct]
		if !ok {
			a = &agg{
				am: AccountMap{
					SourceAcct:      r.Acct,
					CuentoType:      stmtToType(r.Stmt),
					CuentoParent:    r.Parent,
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
		}
		if r.Country != "" {
			a.subs[r.Country] = true
		}
		// Fill a type/parent if the first sighting lacked it but a later one has it.
		if a.am.CuentoType == "" {
			a.am.CuentoType = stmtToType(r.Stmt)
		}
		if a.am.CuentoParent == "" {
			a.am.CuentoParent = r.Parent
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
