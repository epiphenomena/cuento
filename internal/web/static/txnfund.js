// p12.2 transaction editor -- PURE fund logic (trap 2): per-fund imbalance
// computation (the live chips are DISPLAY only; the server revalidates the D20
// per-fund zero-sum, trap 5). No `document` access; unit-tested under `node --test`.
// (p26.23 removed the fund apply-to-all helper along with its header control -- the fund
// now defaults to Unrestricted, so the whole-grid apply is unwanted.)

// fundImbalances computes, over the editor's split rows, the OVERALL net-debit sum
// and the per-fund-group nonzero sums (D2/D20). Each row is { fund, amount }: `fund`
// is the fund id string ("" = unrestricted), `amount` is signed minor units (null =
// blank, ignored). Returns { total, perFund } where perFund maps fund key -> nonzero
// sum. Per-fund entries are suppressed unless >=2 distinct funds are in play
// (Appendix C: the per-fund chip appears only when >=2 funds), while `total` always
// reflects the overall imbalance. Pure display; the store is the sole validator.
export function fundImbalances(rows) {
  let total = 0;
  const sums = new Map();
  for (const r of rows) {
    const amt = r.amount;
    if (amt === null || amt === undefined || Number.isNaN(amt)) continue;
    total += amt;
    const key = r.fund || '';
    sums.set(key, (sums.get(key) || 0) + amt);
  }
  const perFund = {};
  if (sums.size >= 2) {
    for (const [key, sum] of sums) {
      if (sum !== 0) perFund[key] = sum;
    }
  }
  return { total, perFund };
}

// overallImbalance computes the OVERALL transaction imbalance (p28.4) from the BODY
// splits' net sum and whether the header MAIN split is present. `bodyTotal` is the signed
// net-debit sum of the body rows (fundImbalances().total). When the main split is present
// (the normal editor), it auto-balances the body -- its amount is the residual -(bodyTotal)
// -- so the overall transaction is ALWAYS zero (a genuinely-balanced entry; the Total chip
// must render NEUTRAL, not red). In the flat fallback grid (no main split: import /
// expense-review / multi-fund) there is no balancing split, so the overall imbalance IS the
// body sum. Pure (no DOM), so it is node-tested; the caller drives the chip color off it.
export function overallImbalance(bodyTotal, mainPresent) {
  return mainPresent ? bodyTotal + -bodyTotal : bodyTotal;
}

// chipLabel resolves a per-fund imbalance chip's LABEL from a fund key. `key` is
// the fund id string ("" = unrestricted, per fundImbalances). `names` maps fund id
// -> fund NAME (proper nouns, stored data — NOT a catalog key, AGENTS rule 9), and
// `unrestrictedLabel` is the localized "Unrestricted" string. The unrestricted
// bucket ("") uses the label; a known id uses its name; an unknown id falls back to
// the raw id (defensive — the chip stays visible rather than blank). Pure (no DOM),
// so it is node-tested; the caller supplies names read from the fund <select>
// options and the label from the catalog.
export function chipLabel(key, names, unrestrictedLabel) {
  if (key === '') return unrestrictedLabel;
  return (names && names[key]) || key;
}
