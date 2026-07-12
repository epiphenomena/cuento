// p12.2 transaction editor -- PURE fund logic (trap 2): per-fund imbalance
// computation (the live chips are DISPLAY only; the server revalidates the D20
// per-fund zero-sum, trap 5) and fund apply-to-all (fills EMPTY rows only,
// Appendix C). No `document` access; unit-tested under `node --test`.

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

// applyFundToAll returns a new array of fund selections where every EMPTY entry is
// set to `value` and every already-set entry is left untouched (Appendix C: the
// header apply-to-all fills empty selections only). Pure; the DOM glue writes the
// result back into the per-row selects.
export function applyFundToAll(funds, value) {
  return funds.map((f) => (f === '' || f === null || f === undefined ? value : f));
}
