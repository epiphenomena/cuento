package fixture_test

import (
	"context"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil/fixture"
)

// TestExtendCapitalCampaignSeamOptIn proves the p26.51 capital-campaign seam is
// OPT-IN: New leaves no campaign (the Expected zero value), so the default fixture
// is unchanged and no "Restore the Way" fund exists.
func TestExtendCapitalCampaignSeamOptIn(t *testing.T) {
	f := fixture.New(t)
	if f.Expected.Campaign.Fund != 0 {
		t.Fatalf("campaign populated before ExtendCapitalCampaign; seam should be opt-in")
	}
	var n int
	if err := f.DB.QueryRow(`SELECT COUNT(*) FROM funds WHERE name = 'Restore the Way'`).Scan(&n); err != nil {
		t.Fatalf("count campaign funds: %v", err)
	}
	if n != 0 {
		t.Errorf("campaign funds before seam = %d, want 0", n)
	}
}

// TestExtendCapitalCampaignLedgerClean proves the seam posts a balanced, ledger-clean
// campaign (each transaction nets to zero overall AND within the fund, D20/Z10), and
// introduces no new warning beyond the fixture's baseline Z19 (unmapped Event Income).
func TestExtendCapitalCampaignLedgerClean(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)

	if f.Expected.Campaign.Fund == 0 {
		t.Fatalf("seam did not populate the campaign fund")
	}

	vs, err := ledger.Check(context.Background(), f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		switch v.Severity {
		case ledger.Error:
			t.Errorf("unexpected Error violation after ExtendCapitalCampaign: %s: %s", v.Rule, v.Detail)
		case ledger.Warning:
			if v.Rule != "Z19" {
				t.Errorf("unexpected warning rule %s after ExtendCapitalCampaign", v.Rule)
			}
		}
	}
}
