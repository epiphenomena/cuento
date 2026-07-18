// p28.9 EPHEMERAL fuzzy search for the chart of accounts -- a CSP-safe ES module
// (rule 12: external, no inline handler, script-src 'self'). It filters the ALREADY
// server-rendered chart rows in the browser; it is NEVER sent to the server, has no
// session key and no route, so it resets on every navigation (unlike the persisted
// sub/active/type filters, p26.14/p26.75). That absence is the ephemerality.
//
// It reuses the shared fuzzy scorer (combofilter.js `scoreMatch`) for MEMBERSHIP
// only (score !== null) -- not ranking: the tree ORDER is preserved, rows are only
// shown/hidden. Each account row is matched against its DOTTED PATH (ancestor names
// joined by '.', e.g. "Expenses.Salaries"), so typing a PARENT name reveals its whole
// subtree (the parent's name is a prefix of every descendant's path) and typing a
// leaf reveals it plus its ancestors (so the hierarchy still reads).
//
// It does NOT touch `tr.hidden` (which treetable.js owns via its collapse/expand
// render). Instead it toggles a `searching` class on the TABLE and a `search-visible`
// class on the rows to show; CSS (`.tree-table.searching ...`) then wins over the
// `[hidden]` UA rule via an author `display` declaration. So the two mechanisms never
// fight: while searching, this module's CSS governs; when the box empties, `searching`
// is dropped and treetable's collapse state governs again (restored for free).
//
// The PURE decisions (rowPaths, visibleSet) are node-tested (chartsearch.test.js);
// the DOM glue is guarded so importing under Node is side-effect free.

import { scoreMatch } from './combofilter.js';

// rowPaths builds each row's dotted account path from the parallel arrays:
//   names[i]   -- the row's own account name ('' for a type-header row)
//   depths[i]  -- the row's tree depth (0 = the injected type header, p26.74)
//   header[i]  -- true for a display-only type-header row (not a real account)
// A real account's path is its ancestor account names + itself, joined by '.'. Type
// headers are skipped in the chain (they are depth-0 grouping rows, not accounts), so
// a root account under the "Assets" header has a path of just its own name.
export function rowPaths(names, depths, header) {
  const paths = new Array(names.length).fill('');
  // stack[d] = the name of the nearest ancestor ACCOUNT at depth d.
  const stack = [];
  for (let i = 0; i < names.length; i += 1) {
    if (header[i]) {
      // A header resets nothing but is not itself an account; leave its path ''.
      continue;
    }
    const d = depths[i];
    stack.length = d; // drop any deeper ancestors from a previous branch
    stack[d] = names[i];
    // Join the account names along the chain, skipping empty slots (the header depth).
    paths[i] = stack.filter((n) => n).join('.');
  }
  return paths;
}

// visibleSet returns the Set of row indices to SHOW for `query` given the paths,
// depths, and header flags. A row is visible iff:
//   - it MATCHES (its path is a fuzzy subsequence of the query), OR
//   - it is an ANCESTOR of a match (so the hierarchy reads), OR
//   - it is a type HEADER with at least one visible account under it.
// An empty/blank query yields null (caller: show everything, drop `searching`).
export function visibleSet(paths, depths, header, query) {
  const q = String(query == null ? '' : query).trim();
  if (q === '') return null;

  const visible = new Set();
  // First pass: direct matches (accounts only).
  for (let i = 0; i < paths.length; i += 1) {
    if (header[i]) continue;
    if (scoreMatch(paths[i].toLowerCase(), q.toLowerCase()) !== null) {
      visible.add(i);
    }
  }
  // Second pass: reveal each match's ancestors (the nearest shallower account rows,
  // and the type header above the block). Walk upward from each matched row.
  for (const i of Array.from(visible)) {
    let wantDepth = depths[i] - 1;
    for (let j = i - 1; j >= 0 && wantDepth >= 0; j -= 1) {
      if (depths[j] === wantDepth) {
        visible.add(j);
        wantDepth -= 1;
        if (header[j]) break; // reached the type header -> chain done
      }
    }
  }
  // Third pass: a type header is visible iff a row in its block (until the next
  // header) is visible.
  for (let i = 0; i < paths.length; i += 1) {
    if (!header[i]) continue;
    for (let j = i + 1; j < paths.length && !header[j]; j += 1) {
      if (visible.has(j)) {
        visible.add(i);
        break;
      }
    }
  }
  return visible;
}

// -------------------------- DOM glue (browser only) ------------------------

// applyFilter reads the fresh rows of a chart table and shows/hides them per the
// current query, using the CSS-class mechanism (never tr.hidden).
function applyFilter(table, query) {
  const rows = Array.from(table.querySelectorAll('tbody > tr.acct-row'));
  if (rows.length === 0) return;
  const depths = rows.map((tr) => Number(tr.getAttribute('data-depth')) || 0);
  const header = rows.map((tr) => tr.classList.contains('acct-type-header'));
  const names = rows.map((tr, i) => {
    if (header[i]) return '';
    const cell = tr.querySelector('.acct-name');
    // The name is the row's <a> text (the account name); fall back to the cell text.
    const a = cell && cell.querySelector('a');
    return (a ? a.textContent : cell ? cell.textContent : '').trim();
  });
  const paths = rowPaths(names, depths, header);
  const vis = visibleSet(paths, depths, header, query);

  if (vis === null) {
    table.classList.remove('searching');
    rows.forEach((tr) => tr.classList.remove('search-visible'));
    return;
  }
  table.classList.add('searching');
  rows.forEach((tr, i) => tr.classList.toggle('search-visible', vis.has(i)));
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const wire = () => {
    const input = document.getElementById('acct-search');
    if (!input) return;
    const table = () => document.querySelector('#accounts-results table.tree-table')
      || document.querySelector('table.tree-table');
    if (!input.dataset.chartSearchWired) {
      input.dataset.chartSearchWired = '1';
      input.addEventListener('input', () => {
        const t = table();
        if (t) applyFilter(t, input.value);
      });
    }
    // On a filter swap (#accounts-results replaced) re-apply the current query to the
    // FRESH table, so the search survives a sub/active/type change.
    const t = table();
    if (t) applyFilter(t, input.value);
  };
  document.addEventListener('DOMContentLoaded', wire);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', wire);
  }
}
