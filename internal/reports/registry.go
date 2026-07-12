package reports

import (
	"fmt"
	"sort"
)

// Registry is the ordered, iterable set of all reports (rule 8's report analogue):
// the web layer iterates it to auto-mount one route per report and to build the
// /reports index, so p15.3–p15.11 add a report by registering it and NOTHING else
// changes. It is not package-level mutable state that mutates at runtime — it is
// built once (All) at startup and read thereafter, matching the "explicit
// registries synced at startup" carve-out (AGENTS Style).
type Registry struct {
	byID  map[string]Report
	order []string // ids in registration order, for stable iteration
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Report)}
}

// Default builds the registry with every shipped report registered, in declared
// order. Today that is the p15.3 trial-balance report (which replaced the p15.1
// framework smoke placeholder); p15.4–p15.11 append their registration here (one
// line each) and the web layer auto-mounts them. This is the single place the app
// assembles its report set; the web layer calls it once at startup. It PANICS on a
// malformed registration (a build-time defect), so a bad report declaration fails
// fast at startup.
func Default() *Registry {
	reg := NewRegistry()
	registerTrialBalance(reg)
	registerBalanceSheet(reg)
	registerIncomeStatement(reg)
	registerAccountLedger(reg)
	registerFunctionalExpenses(reg)
	return reg
}

// Register adds r to the registry. It PANICS on a programmer error — an empty/ill-
// formed ID, a duplicate ID, an empty TitleKey, an undeclared Group, or a nil Run —
// because these are build-time defects in a report's declaration, caught the first
// time All() runs (at startup), never at request time. This mirrors
// mustParseTemplates: a malformed declaration is a bug, not a runtime condition.
func (reg *Registry) Register(r Report) {
	if !validID(r.ID) {
		panic("reports: invalid report id " + fmt.Sprintf("%q", r.ID) +
			" (need non-empty lowercase ascii letters/digits/-/_)")
	}
	if _, dup := reg.byID[r.ID]; dup {
		panic("reports: duplicate report id " + fmt.Sprintf("%q", r.ID))
	}
	if r.TitleKey == "" {
		panic("reports: report " + fmt.Sprintf("%q", r.ID) + " has no TitleKey")
	}
	if !validGroup(r.Group) {
		panic("reports: report " + fmt.Sprintf("%q", r.ID) + " has undeclared group " +
			fmt.Sprintf("%q", r.Group))
	}
	if r.Run == nil {
		panic("reports: report " + fmt.Sprintf("%q", r.ID) + " has nil Run")
	}
	reg.byID[r.ID] = r
	reg.order = append(reg.order, r.ID)
}

// Get returns the report with id and whether it exists. The web layer uses the
// ok=false result to answer 404 for an unknown /reports/{id} — though because the
// framework mounts one CONCRETE route per registered report (not a wildcard), an
// unknown id simply never matches a route and the mux 404s on its own; Get is the
// lookup the mounted handler uses to fetch ITS report.
func (reg *Registry) Get(id string) (Report, bool) {
	r, ok := reg.byID[id]
	return r, ok
}

// All returns every registered report in registration order. The web layer ranges
// it to mount routes and build the index; the order is stable so the index and the
// permission matrix are deterministic.
func (reg *Registry) All() []Report {
	out := make([]Report, 0, len(reg.order))
	for _, id := range reg.order {
		out = append(out, reg.byID[id])
	}
	return out
}

// GroupsInUse returns the distinct report groups referenced by registered reports,
// sorted. Diagnostic/testing aid — the synced group set is Groups() (which may
// include groups no report references yet), not this.
func (reg *Registry) GroupsInUse() []string {
	seen := make(map[string]struct{})
	for _, id := range reg.order {
		seen[reg.byID[id].Group] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// validID reports whether id is a well-formed report slug: non-empty and only
// lowercase ascii letters, digits, '-' or '_' (so it is a safe URL path segment
// and a stable registry key). A leading '_' is allowed so a clearly-marked
// placeholder report (the p15.1 smoke report) is a valid, obvious id.
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
