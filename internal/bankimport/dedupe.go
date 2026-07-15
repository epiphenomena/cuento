package bankimport

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// dedupe_hash is the natural key of a bank-statement line, used to FLAG (never
// enforce, DECISIONS p17.1) a row that duplicates either an already-staged import
// row or an already-posted ledger split on the SAME account. It is computed by
// exactly ONE function, DedupeHash, called by BOTH the staging path (a freshly
// parsed row) AND the ledger-split-derivation path (an existing posted split), so
// the two sides can only collide when they truly describe the same event.
//
// Formula (PLAN p17.2): sha256(account | date | amount | normalized(desc+memo)),
// the four parts joined by '|'. The "description" is the bank statement line's
// descriptive TEXT (formerly called the "payee" line; the payee ENTITY is retired
// as of p26.20 and that text now feeds the split's per-line description). Concretely:
//
//	account  = the account id, base-10.
//	date     = the canonical YYYY-MM-DD (money's ISO form -- both sides use it).
//	amount   = strconv.FormatInt(minor, 10) -- net-debit signed minor units (D2),
//	           so a deposit into an asset bank account is a POSITIVE debit on both
//	           the parsed row (after any sign flip) and the ledger split.
//	descmemo = Normalize(description) + "\x1f" + Normalize(memo) -- description and
//	           memo are each normalized SEPARATELY, then joined by a unit separator,
//	           so "AB"+"C" and "A"+"BC" never alias AND whitespace adjacent to the
//	           separator cannot leak a stray space into the boundary.
//
// The '\x1f' unit separator between the normalized description and memo and the '|'
// between the four top-level parts are distinct, and neither can appear in a
// base-10 int or a YYYY-MM-DD date, so the join is unambiguous.

const (
	// fieldSep joins the four top-level parts of the natural key.
	fieldSep = "|"
	// pairSep joins description and memo before normalization (a byte that will not
	// occur in normalized human text).
	pairSep = "\x1f"
)

// DedupeHash computes the natural-key hash for one bank-statement line. Both the
// staging path and the ledger-split path call this, guaranteeing an identical hash
// for the same (account, date, amount, description/memo) event.
func DedupeHash(accountID int64, date string, amountMinor int64, description, memo string) string {
	key := strings.Join([]string{
		strconv.FormatInt(accountID, 10),
		date,
		strconv.FormatInt(amountMinor, 10),
		Normalize(description) + pairSep + Normalize(memo),
	}, fieldSep)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Normalize canonicalizes free text for the dedupe key: trim outer whitespace,
// lowercase, and collapse every internal run of whitespace to a single space. So
// "  ACME   Inc.\t" and "acme inc." normalize identically. It is applied to the
// description and the memo SEPARATELY (DedupeHash joins the two normalized results
// with a unit separator), so trailing/leading whitespace on either can never leak a
// stray space into the boundary between them.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
