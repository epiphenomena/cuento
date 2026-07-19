package reports

// drill.go is the p15.3d DRILL-DOWN framework capability: every report
// balance/activity figure can "click through" to the underlying transactions that
// produce it — a filtered split list whose signed NATIVE sum equals the figure. A
// report attaches a Drill descriptor to a Cell (or Row); the web layer turns a
// drillable cell into a link to /reports/{id}/drill?{encoded filter} (gated by the
// SAME ReportGroup as the report, so "you can see the number => you can drill it"),
// and the drill handler decodes the filter, re-fetches exactly the contributing
// splits via the store, and lists them (reusing the register row rendering) each
// linking to the txn editor/history (p12.4).
//
// This file owns the pure, HTTP-free half: the Drill shape and its query-string
// encode/decode. Keeping it data-only means the RECONCILIATION invariant is unit-
// testable without a browser, and the CSV/text renderers can ignore Drill entirely
// (the golden is unchanged). The web layer (internal/web/reports_drill.go) owns the
// route, the store query, and the HTML render.
//
// RECONCILIATION invariant (the whole point): the signed sum of the drilled NATIVE
// splits (in the cell's native currency) equals the report's PRE-conversion native
// figure for that cell. A converted/consolidated cell still drills to its NATIVE
// underlying splits (the drill header annotates that the report figure was
// converted); the invariant holds against the native figure, not the converted one.

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// DrillMode selects how a Drill reconstructs the underlying figure: an AS-OF
// cumulative balance (trial balance, balance sheet) or a PERIOD activity (income
// statement, program/functional reports). It decides which date bound the store
// query applies (t.date <= AsOf, versus From <= t.date <= To), so the drilled
// split set matches the toolkit method that produced the cell.
type DrillMode int

const (
	// DrillAsOf reconstructs an as-of cumulative balance: every split whose txn
	// date <= AsOf. The trial balance uses this.
	DrillAsOf DrillMode = iota
	// DrillPeriod reconstructs a period activity: every split with From <= txn
	// date <= To. The income-statement / program / functional reports use this.
	DrillPeriod
)

// Drill is the filter that reconstructs the transactions behind ONE report figure.
// A nil *Drill on a Cell means "not drillable" (label cells, totals a report chooses
// not to drill). It is pure data (no store, no HTTP) so it round-trips through a
// query string and the reconciliation test can build one by hand.
//
// The account SET (not a single id) generalizes to rollup/subtotal cells a later
// report may drill (a placeholder account's subtree); the trial-balance retrofit
// attaches exactly one account per cell. Fund/Program/Class are optional extra
// filters (a fund-balances or program-statement cell narrows to one fund/program/
// class); nil/"" means "no filter on that dimension". Currency is REQUIRED for a
// money cell — each toolkit balance/activity cell is per-currency, so the drill must
// filter t.currency to reconcile (FX Clearing holds USD and MXN; only the currency-
// filtered sum equals a single cell).
type Drill struct {
	// Scope is the subsidiary the figure consolidates: this subsidiary plus ALL its
	// descendants (D18), exactly as the toolkit computed it. The store query closes
	// the descendant set itself (the same recursive CTE the balance queries use), so
	// a root-scope drill picks up splits across every descendant sub.
	Scope SubsidiaryID

	// AccountIDs is the account set the figure sums over. One id for a leaf-account
	// cell (the trial-balance retrofit); a subtree's account ids for a rollup cell a
	// later report may drill. Empty = no accounts => an empty drill (renders nothing).
	AccountIDs []AccountID

	// Currency is the native ISO currency of the cell (REQUIRED for a money cell):
	// the drill filters t.currency to it so a multi-currency account's per-currency
	// cell reconciles. Empty currency means "any currency" (only meaningful for a
	// degenerate/empty drill).
	Currency string

	// FundID, ProgramID, Class are optional narrowing filters (nil/"" = no filter on
	// that dimension). A fund-balances cell sets FundID; a program-statement cell
	// sets ProgramID; a functional-expense cell sets Class.
	FundID    *FundID
	ProgramID *ProgramID
	Class     *string

	// FundIDs is an optional fund SET (p15.9): the drilled figure sums splits across
	// SEVERAL funds at once (the activities-by-restriction "net assets released" line
	// aggregates applications across every RESTRICTED fund, so its USD cell spans Beca
	// Agua + Building Fund — a single FundID cannot express it). When non-empty the
	// drill unions the per-fund split sets (account SET × fund SET), reconciling to the
	// multi-fund figure; the store's per-cell query still filters ONE fund at a time
	// (no SQL change), the caller loops the set. FundID and FundIDs are mutually
	// exclusive: a cell sets at most one (FundIDs when it aggregates funds, FundID for a
	// single fund).
	FundIDs []FundID

	// ProgramIDs is an optional program SET (p15.10): the drilled figure sums splits
	// across SEVERAL programs at once (the program statement's ROLLUP cells — a parent
	// program's figure includes its descendant programs, so General's cell spans
	// General + Educación + Food Pantry — which a single ProgramID cannot express). When
	// non-empty the drill unions the per-program split sets (account SET × program SET),
	// reconciling to the rolled-up figure; the store's per-cell query still filters ONE
	// program at a time (no SQL change), the caller loops the set. ProgramID and
	// ProgramIDs are mutually exclusive: a cell sets at most one (ProgramIDs when it
	// aggregates a program subtree, ProgramID for a single leaf program).
	ProgramIDs []ProgramID

	// Mode selects the date treatment (as-of cumulative vs period activity).
	Mode DrillMode

	// AsOf bounds a DrillAsOf figure (t.date <= AsOf, YYYY-MM-DD). From/To bound a
	// DrillPeriod figure (From <= t.date <= To, inclusive).
	AsOf string
	From string
	To   string
}

// Encode serializes d to a URL query string (stable key order) the drill route
// decodes. It is the inverse of DecodeDrill. Only set fields are emitted, so an
// as-of drill carries no from/to and vice versa, keeping the URL tight. Account ids
// are a comma-joined list under one key.
func (d Drill) Encode() string {
	q := url.Values{}
	q.Set("scope", strconv.FormatInt(int64(d.Scope), 10))
	if len(d.AccountIDs) > 0 {
		ids := make([]string, len(d.AccountIDs))
		for i, id := range d.AccountIDs {
			ids[i] = strconv.FormatInt(int64(id), 10)
		}
		q.Set("accts", strings.Join(ids, ","))
	}
	if d.Currency != "" {
		q.Set("ccy", d.Currency)
	}
	if d.FundID != nil {
		q.Set("fund", strconv.FormatInt(int64(*d.FundID), 10))
	}
	if len(d.FundIDs) > 0 {
		ids := make([]string, len(d.FundIDs))
		for i, id := range d.FundIDs {
			ids[i] = strconv.FormatInt(int64(id), 10)
		}
		q.Set("funds", strings.Join(ids, ","))
	}
	if d.ProgramID != nil {
		q.Set("prog", strconv.FormatInt(int64(*d.ProgramID), 10))
	}
	if len(d.ProgramIDs) > 0 {
		ids := make([]string, len(d.ProgramIDs))
		for i, id := range d.ProgramIDs {
			ids[i] = strconv.FormatInt(int64(id), 10)
		}
		q.Set("progs", strings.Join(ids, ","))
	}
	if d.Class != nil {
		q.Set("class", *d.Class)
	}
	switch d.Mode {
	case DrillPeriod:
		q.Set("mode", "period")
		if d.From != "" {
			q.Set("from", d.From)
		}
		if d.To != "" {
			q.Set("to", d.To)
		}
	default:
		q.Set("mode", "asof")
		if d.AsOf != "" {
			q.Set("asof", d.AsOf)
		}
	}
	return q.Encode()
}

// DecodeDrill parses a Drill from a parsed query (url.Values), the inverse of
// Encode. It is forgiving: a malformed id/date is dropped (yielding a narrower or
// empty drill) rather than erroring, so a hand-tampered URL degrades to an empty
// list (a 200 with no rows) rather than a 500 — matching the framework rule that a
// report with nothing to show returns empty, not an error. The empty query (the
// permission-matrix's bare /reports/{id}/drill hit) decodes to a zero Drill with no
// accounts, which the store treats as an empty result.
func DecodeDrill(q url.Values) Drill {
	var d Drill
	if v := strings.TrimSpace(q.Get("scope")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			d.Scope = SubsidiaryID(id)
		}
	}
	if v := strings.TrimSpace(q.Get("accts")); v != "" {
		for _, part := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id != 0 {
				d.AccountIDs = append(d.AccountIDs, AccountID(id))
			}
		}
		sort.Slice(d.AccountIDs, func(i, j int) bool { return d.AccountIDs[i] < d.AccountIDs[j] })
	}
	d.Currency = strings.TrimSpace(q.Get("ccy"))
	if v := strings.TrimSpace(q.Get("fund")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			fid := FundID(id)
			d.FundID = &fid
		}
	}
	if v := strings.TrimSpace(q.Get("funds")); v != "" {
		for _, part := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id != 0 {
				d.FundIDs = append(d.FundIDs, FundID(id))
			}
		}
		sort.Slice(d.FundIDs, func(i, j int) bool { return d.FundIDs[i] < d.FundIDs[j] })
	}
	if v := strings.TrimSpace(q.Get("prog")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			pid := ProgramID(id)
			d.ProgramID = &pid
		}
	}
	if v := strings.TrimSpace(q.Get("progs")); v != "" {
		for _, part := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id != 0 {
				d.ProgramIDs = append(d.ProgramIDs, ProgramID(id))
			}
		}
		sort.Slice(d.ProgramIDs, func(i, j int) bool { return d.ProgramIDs[i] < d.ProgramIDs[j] })
	}
	if v := q.Get("class"); v != "" {
		c := v
		d.Class = &c
	}
	if q.Get("mode") == "period" {
		d.Mode = DrillPeriod
		d.From = strings.TrimSpace(q.Get("from"))
		d.To = strings.TrimSpace(q.Get("to"))
	} else {
		d.Mode = DrillAsOf
		d.AsOf = strings.TrimSpace(q.Get("asof"))
	}
	return d
}
