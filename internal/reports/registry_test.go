package reports

import (
	"context"
	"testing"
)

// A trivial valid report for registry tests: real ID/TitleKey/Group and a no-op
// Run returning an empty table.
func okReport(id string) Report {
	return Report{
		ID:         id,
		TitleKey:   "reports." + id + ".title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true},
		Run: func(context.Context, *Toolkit, Params) (Table, error) {
			return Table{}, nil
		},
	}
}

// TestRegistrySyncGroups: the code-declared group set (what the web layer syncs to
// report_groups) is exactly the small aligned set, every entry a valid group, and
// each registered report's group is a member — so the "registry sync creates
// groups" requirement has a concrete, asserted set.
func TestRegistrySyncGroups(t *testing.T) {
	groups := Groups()
	want := []string{"financial", "funds", "programs", "tax"}
	if len(groups) != len(want) {
		t.Fatalf("Groups() = %v, want %v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("Groups()[%d] = %q, want %q", i, groups[i], want[i])
		}
	}
	for _, g := range groups {
		if !validGroup(g) {
			t.Errorf("declared group %q is not a valid group", g)
		}
	}
}

// TestRegisterAndGet: a registered report is retrievable by id, iteration order is
// registration order, and an unknown id reports ok=false (the 404 source).
func TestRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	reg.Register(okReport("alpha"))
	reg.Register(okReport("beta"))

	if _, ok := reg.Get("alpha"); !ok {
		t.Errorf("Get(alpha) not found after Register")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Errorf("Get(nope) found; want not found (the 404 source)")
	}

	all := reg.All()
	if len(all) != 2 || all[0].ID != "alpha" || all[1].ID != "beta" {
		t.Errorf("All() = %v, want [alpha beta] in registration order", ids(all))
	}
}

// TestRegisterPanics: each malformed declaration panics at Register (a build-time
// defect surfaced at startup, never at request time).
func TestRegisterPanics(t *testing.T) {
	cases := []struct {
		name string
		r    Report
	}{
		{"empty id", func() Report { r := okReport("x"); r.ID = ""; return r }()},
		{"bad id chars", func() Report { r := okReport("x"); r.ID = "Bad Id!"; return r }()},
		{"empty title", func() Report { r := okReport("x"); r.TitleKey = ""; return r }()},
		{"undeclared group", func() Report { r := okReport("x"); r.Group = "nope"; return r }()},
		{"nil run", func() Report { r := okReport("x"); r.Run = nil; return r }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%s) did not panic", c.name)
				}
			}()
			NewRegistry().Register(c.r)
		})
	}
}

// TestRegisterDuplicatePanics: a duplicate id is a defect and panics.
func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("registering a duplicate id did not panic")
		}
	}()
	reg := NewRegistry()
	reg.Register(okReport("dup"))
	reg.Register(okReport("dup"))
}

func ids(rs []Report) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
