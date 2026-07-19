// p26.2 fuzzy-filter combobox -- PURE ranking logic (rule 12, trap 2: node-tested,
// no `document`). combobox.js is the DOM glue that drives this; the scoring here is
// what the account/fund/program/payee comboboxes all share (p26.3/p26.4), and chartsearch.js
// reuses scoreMatch for membership.
//
// rankOptions(query, options) ranks option labels (dotted paths like "Cash.BOA") by a
// simple, deterministic ADJACENCY match (p30.13):
//   - empty/blank query  -> every option, ORIGINAL order (stable).
//   - non-empty query    -> the query is TOKENIZED on segment/word separators into
//     fragments; EVERY fragment must appear as a CONTIGUOUS (case-insensitive) substring
//     somewhere in the label, else the option is EXCLUDED; best first.
//
// Why adjacency (not scattered subsequence): a scattered matcher lets "fp" match
// "Food Purchases" by picking f...p, which is hard to reason about. Requiring each typed
// fragment to be contiguous makes "why did this match?" obvious: every fragment is a
// literal substring. The tokenization on the BOUNDARY set preserves the dotted-path and
// spaced patterns -- "c.boa" -> ["c","boa"], "f p" -> ["f","p"] -- while "fp" (one
// fragment) is only kept if the literal "fp" appears somewhere.
//
// The score rewards (a) each fragment matching at a word/segment BOUNDARY (start, or right
// after a '.' / space / '-' / '_' / '/'), (b) matching in the leaf segment (text after the
// last dot) so a bare `boa` still ranks `Cash.BOA` well, (c) a prefix bonus when the first
// fragment starts the label, and (d) fragments matching in left-to-right ORDER. Ties break
// by the option's ORIGINAL index, so equal-scoring options keep input order (stable sort).
//
// Each option is { label, value }; the returned array is a NEW array of the same option
// objects (never mutated) in ranked order.

const BOUNDARY = new Set(['.', ' ', '-', '_', '/']);

// tokenizeQuery splits `q` (already lower-cased) into fragments on the BOUNDARY chars,
// dropping empties. "c.boa" -> ["c","boa"]; "f p" -> ["f","p"]; "fp" -> ["fp"];
// a separator-only query -> [].
function tokenizeQuery(q) {
  const frags = [];
  let cur = '';
  for (let i = 0; i < q.length; i += 1) {
    const ch = q[i];
    if (BOUNDARY.has(ch)) {
      if (cur !== '') frags.push(cur);
      cur = '';
    } else {
      cur += ch;
    }
  }
  if (cur !== '') frags.push(cur);
  return frags;
}

// atBoundary reports whether the substring starting at `idx` in `label` begins a
// word/segment (index 0, or the preceding char is a boundary char).
function atBoundary(label, idx) {
  return idx === 0 || BOUNDARY.has(label[idx - 1]);
}

// bestOccurrence finds where `frag` (a contiguous substring) best matches in `label`:
// it prefers an occurrence at a segment/word BOUNDARY over a mid-word one; among equal
// classes it takes the EARLIEST. Returns { idx, boundary } or null if `frag` is absent.
function bestOccurrence(label, frag) {
  let first = -1;
  let firstBoundary = -1;
  for (let idx = label.indexOf(frag); idx !== -1; idx = label.indexOf(frag, idx + 1)) {
    if (first === -1) first = idx;
    if (atBoundary(label, idx)) {
      firstBoundary = idx;
      break; // earliest boundary hit is the best we can do
    }
  }
  if (firstBoundary !== -1) return { idx: firstBoundary, boundary: true };
  if (first !== -1) return { idx: first, boundary: false };
  return null;
}

// scoreMatch returns a numeric score for matching `q` (already lower-cased) against
// `label` (lower-cased) under the ADJACENCY rule, or null if ANY tokenized fragment is
// not a contiguous substring of the label. Higher is better. Deterministic; no randomness.
function scoreMatch(label, q) {
  if (q === '') return 0;
  const frags = tokenizeQuery(q);
  if (frags.length === 0) return 0; // separator-only query behaves like empty

  const dot = label.lastIndexOf('.');
  const leafStart = dot >= 0 ? dot + 1 : 0;

  let score = 0;
  let prevIdx = -1;
  let ordered = true;
  for (let f = 0; f < frags.length; f += 1) {
    const frag = frags[f];
    const occ = bestOccurrence(label, frag);
    if (occ === null) return null; // a fragment with no contiguous match -> not a match
    // Base point per matched fragment.
    score += 1;
    // Boundary bonus: the fragment starts a segment/word. Lifts "Expenses.Rent" over
    // "Parenting" for the query "rent".
    if (occ.boundary) score += 5;
    // Leaf bonus: the fragment falls inside the leaf segment (text after the last dot),
    // so a bare `boa` favors "Cash.BOA" over "Cash.BOA.Payroll".
    if (occ.idx >= leafStart) score += 3;
    // Track left-to-right order across fragments.
    if (occ.idx < prevIdx) ordered = false;
    prevIdx = occ.idx;
  }
  // Prefix bonus: the FIRST fragment starts the whole label (a true prefix).
  if (label.startsWith(frags[0])) score += 4;
  // Order bonus: the fragments matched in increasing position (left-to-right).
  if (ordered) score += 2;
  // Shorter labels are a tighter fit for the same query -> tiny tie-breaking nudge that
  // stays below the structural bonuses above.
  score += Math.max(0, 3 - label.length * 0.01);
  return score;
}

function rankOptions(query, options) {
  const q = String(query == null ? '' : query).trim().toLowerCase();
  if (q === '' || tokenizeQuery(q).length === 0) return options.slice();
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
