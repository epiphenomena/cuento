package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
	"cuento/internal/synth"
)

// ExtendCapitalCampaign is the p26.51 CAPITAL-CAMPAIGN seam wrapper: it builds the
// restricted campaign fund ("Restore the Way") + its accounts and a multi-quarter,
// multi-currency lifecycle (synth.ExtendCapitalCampaign). It is legitimate
// restricted-fund SAMPLE DATA that other reports' fixtures draw on (the balance
// sheet's nested-tree case, fund conservation). It is OPT-IN: New does NOT call it,
// so the base fixture and every existing golden/tally stay byte-identical.
//
// The seam is designed so it can be layered WITH ExtendRates (the campaign
// transactions fall inside the 2025 rate schedule so every split has an on-or-before
// rate).
func (f *Fixture) ExtendCapitalCampaign(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	if err := synth.ExtendCapitalCampaign(ctx, f.Store, &f.IDs); err != nil {
		t.Fatalf("fixture: ExtendCapitalCampaign: %v", err)
	}
}
