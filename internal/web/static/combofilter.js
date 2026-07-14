// p26.2 fuzzy-filter combobox -- PURE ranking logic (rule 12, trap 2: node-tested,
// no `document`). combobox.js is the DOM glue that drives this; the scoring here is
// what the account/fund/program/payee comboboxes will all share (p26.3/p26.4).
//
// rankOptions(query, options) ranks option labels (dotted paths like "Cash.BOA") by a
// simple, deterministic fuzzy score:
//   - empty/blank query  -> every option, ORIGINAL order (stable).
//   - non-empty query    -> subsequence match only; non-matches EXCLUDED; best first.
//
// The score rewards (a) contiguous runs, (b) matches at a word/segment boundary (start,
// or right after a '.' / space / '-' / '_'), and (c) matching the leaf segment (the text
// after the last dot) so a bare `boa` still ranks `Cash.BOA` well. Ties break by the
// option's ORIGINAL index, so equal-scoring options keep input order (stable sort).
//
// Each option is { label, value }; the returned array is a NEW array of the same option
// objects (never mutated) in ranked order.

const BOUNDARY = new Set(['.', ' ', '-', '_', '/']);

// scoreMatch returns a numeric score for matching `q` (already lower-cased) as a
// subsequence of `label` (lower-cased), or null if `q` is not a subsequence at all.
// Higher is better. Deterministic; no randomness.
function scoreMatch(label, q) {
  if (q === '') return 0;
  let score = 0;
  let qi = 0;
  let prevMatchIdx = -2; // so the first match is never "contiguous" with a phantom -1
  for (let li = 0; li < label.length && qi < q.length; li += 1) {
    if (label[li] !== q[qi]) continue;
    // Base point for the matched char.
    score += 1;
    // Contiguity bonus: this match immediately follows the previous match.
    if (li === prevMatchIdx + 1) score += 3;
    // Boundary bonus: matched char starts a segment/word (index 0, or preceded by a
    // boundary char). This is what makes "c.boa" line up with "Cash.BOA" cleanly and
    // lifts leaf-segment starts.
    if (li === 0 || BOUNDARY.has(label[li - 1])) score += 5;
    prevMatchIdx = li;
    qi += 1;
  }
  if (qi < q.length) return null; // not a full subsequence -> no match
  // Prefix bonus: the whole query matched starting at index 0 (a true prefix of label).
  if (label.startsWith(q)) score += 4;
  // Leaf bonus: the query is a substring of the leaf segment (text after the last dot),
  // so a bare `boa` favors "Cash.BOA" over an account merely containing b/o/a scattered.
  const dot = label.lastIndexOf('.');
  const leaf = dot >= 0 ? label.slice(dot + 1) : label;
  if (leaf.includes(q)) score += 6;
  // Shorter labels are a tighter fit for the same query -> tiny tie-breaking nudge that
  // stays below the structural bonuses above.
  score += Math.max(0, 3 - label.length * 0.01);
  return score;
}

function rankOptions(query, options) {
  const q = String(query == null ? '' : query).trim().toLowerCase();
  if (q === '') return options.slice();
  const scored = [];
  options.forEach((opt, idx) => {
    const label = String(opt.label == null ? '' : opt.label).toLowerCase();
    const s = scoreMatch(label, q);
    if (s !== null) scored.push({ opt, idx, s });
  });
  scored.sort((a, b) => (b.s - a.s) || (a.idx - b.idx));
  return scored.map((x) => x.opt);
}

export { rankOptions, scoreMatch };
