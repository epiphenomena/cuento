// p27.2 budget-split CADENCE helper -- a tiny CSP-safe ES module (rule 12: external,
// no inline handler, script-src 'self') that GENERATES a list of dated rows from a
// start date + interval + count, as pure ENTRY sugar. It materializes the dates and
// forgets -- there is NO stored schedule object (the key simplification replacing the
// retired schedule concept; DECISIONS "Budget redesign").
//
// The PURE core is `cadenceDates(startISO, interval, count)`: it steps a canonical
// "YYYY-MM-DD" date forward `count` times at the given interval (weekly / biweekly /
// monthly) and returns the ISO date strings (the FIRST date is the start itself).
// It is clock-free and DOM-free so `node --test` can exhaustively table-test it,
// including the month-end clamp (Jan 31 + 1 month -> Feb 28/29, NOT a rollover to
// March). All arithmetic is in UTC to avoid any local-zone DST drift.
//
// The browser glue clones the plan grid's current row across the generated dates
// (setting each clone's date field), reusing the existing auto-row grid. The dates
// stay ISO end-to-end; the datefield module renders them for the user (rule 10:
// never format dates directly, never input[type=date]).
//
// Guarded so importing under Node is side-effect free (no `document`).

// Interval keys (kept in sync with the cadence <select> options rendered server-side).
export const INTERVAL_WEEKLY = 'weekly';
export const INTERVAL_BIWEEKLY = 'biweekly';
export const INTERVAL_MONTHLY = 'monthly';

// parseISO parses a strict "YYYY-MM-DD" into {y, m, d} integers (m is 1..12). Returns
// null on any malformed / impossible input (the caller then generates nothing).
export function parseISO(s) {
  if (typeof s !== 'string') return null;
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (!m) return null;
  const y = Number(m[1]);
  const mo = Number(m[2]);
  const d = Number(m[3]);
  if (mo < 1 || mo > 12 || d < 1 || d > 31) return null;
  // Reject impossible days (e.g. 2026-02-30) by round-tripping through a UTC Date.
  const dt = new Date(Date.UTC(y, mo - 1, d));
  if (dt.getUTCFullYear() !== y || dt.getUTCMonth() !== mo - 1 || dt.getUTCDate() !== d) {
    return null;
  }
  return { y, m: mo, d };
}

// formatISO renders {y, m, d} back to "YYYY-MM-DD" (zero-padded).
export function formatISO(parts) {
  const p2 = (n) => String(n).padStart(2, '0');
  return `${String(parts.y).padStart(4, '0')}-${p2(parts.m)}-${p2(parts.d)}`;
}

// daysInMonth returns the number of days in month m (1..12) of year y.
function daysInMonth(y, m) {
  // Day 0 of the NEXT month is the last day of month m (UTC).
  return new Date(Date.UTC(y, m, 0)).getUTCDate();
}

// addDaysISO steps an ISO date forward n days (UTC), returning an ISO string.
function addDaysISO(iso, n) {
  const p = parseISO(iso);
  if (!p) return null;
  const dt = new Date(Date.UTC(p.y, p.m - 1, p.d + n));
  return formatISO({ y: dt.getUTCFullYear(), m: dt.getUTCMonth() + 1, d: dt.getUTCDate() });
}

// addMonthsISO steps an ISO date forward n calendar months, CLAMPING the day to the
// target month's last day (Jan 31 + 1mo -> Feb 28/29, NOT March). This is the key
// edge case a naive Date.setMonth rolls over -- the clamp keeps monthly cadences on a
// sane day.
export function addMonthsISO(iso, n) {
  const p = parseISO(iso);
  if (!p) return null;
  // Absolute month index, then split back to (year, month).
  const total = (p.y * 12 + (p.m - 1)) + n;
  const y = Math.floor(total / 12);
  const m = (total % 12) + 1;
  const d = Math.min(p.d, daysInMonth(y, m));
  return formatISO({ y, m, d });
}

// cadenceDates is the PURE generator: from startISO, produce `count` dates at the
// given interval. The FIRST date is startISO itself; each subsequent date steps one
// interval. Returns [] for a bad start, a bad interval, or count < 1.
//   weekly   -> +7 days
//   biweekly -> +14 days
//   monthly  -> +1 calendar month (day-clamped)
export function cadenceDates(startISO, interval, count) {
  if (!parseISO(startISO)) return [];
  const n = Math.floor(Number(count));
  if (!Number.isFinite(n) || n < 1) return [];
  const step = (iso, i) => {
    switch (interval) {
      case INTERVAL_WEEKLY:
        return addDaysISO(iso, 7 * i);
      case INTERVAL_BIWEEKLY:
        return addDaysISO(iso, 14 * i);
      case INTERVAL_MONTHLY:
        return addMonthsISO(startISO, i);
      default:
        return null;
    }
  };
  const out = [];
  for (let i = 0; i < n; i += 1) {
    const d = step(startISO, i);
    if (!d) return [];
    out.push(d);
  }
  return out;
}

// --- browser glue (guarded for Node) ---------------------------------------
// The plan grid exposes a cadence control block `[data-cadence]` holding a start
// date input (`.cadence-start`, an ISO hidden mirror), an interval <select>
// (`.cadence-interval`), a count input (`.cadence-count`), and a generate button
// (`.cadence-generate`). On click it computes the dates and dispatches a
// `budget:cadence` CustomEvent carrying them, which the grid module (expensegrid /
// its budget analog) consumes to clone the current row across the dates. Keeping the
// grid mutation in the grid module (not here) preserves the single auto-row owner.
function wireCadence(block) {
  const btn = block.querySelector('.cadence-generate');
  if (!btn) return;
  btn.addEventListener('click', (ev) => {
    ev.preventDefault();
    const startEl = block.querySelector('.cadence-start');
    const intervalEl = block.querySelector('.cadence-interval');
    const countEl = block.querySelector('.cadence-count');
    const start = startEl ? startEl.value : '';
    const interval = intervalEl ? intervalEl.value : '';
    const count = countEl ? countEl.value : '';
    const dates = cadenceDates(start, interval, count);
    if (dates.length === 0) return;
    block.dispatchEvent(new CustomEvent('budget:cadence', {
      bubbles: true,
      detail: { dates },
    }));
  });
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('[data-cadence]').forEach((b) => {
      if (!b.dataset.cadenceWired) {
        b.dataset.cadenceWired = '1';
        wireCadence(b);
      }
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (typeof window !== 'undefined' && window.htmx) {
    document.body && document.body.addEventListener('htmx:afterSwap', initAll);
  }
}
