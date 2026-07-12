# QA script — transaction entry-flow hardening (p12.6)

A concrete, human-runnable manual QA pass for the transaction editor
(`/transactions/new` and `/transactions/{id}/edit`). It covers focus retention,
zero layout shift, scroll preservation, a full **keyboard-only** 4-split
mixed-fund entry, and an **es (Spanish)** locale pass. The automatable core of the
keyboard pass is `e2e/tests/entry-keyboard.spec.js`; this script is what a person
runs to catch what a headless browser cannot judge (visual jank, focus feel).

Run against a `-dev` server:

```
make run            # cuento serve -dev on 127.0.0.1:8080 (or your -addr)
```

Log in as an admin (admin ⇒ TxnWrite). Create the fixtures below through the app —
**synthetic data only** (DATA RULE 11); never copy real ledger values in.

## 0. Setup (once, through the app)

1. Chart of accounts (`/accounts` → **New account**): create two **asset** leaves
   mapped to the root subsidiary — `QA Checking`, `QA Savings`. Also create one
   **expense** leaf `QA Rent` with a default functional class (e.g. Management &
   general) and one **revenue** leaf `QA Donations`, both on the root subsidiary.
2. Funds (`/funds` → **New fund**): create a restricted fund `QA Water Grant`
   scoped to the root subsidiary + the root program (General).

These give you enough to enter a mixed-fund transaction and to exercise the
program/class reveal on R/E rows.

---

## 1. Focus retention across in-flow swaps

The editor swaps its whole `#txn-form` node on two in-flow actions: the header
**subsidiary** change (`hx-get` re-filter) and a **422 re-render** (a save that
fails validation). Input ids are keyed to row position (`txn-account-0`, …) and are
byte-identical across every swap (single-sourced `transaction-form` partial), so a
focus/tab target must not jump.

1. Open `/transactions/new`. Click into the **date** field, then Tab into the grid.
2. **Subsidiary re-filter:** change the header **Subsidiary** select. The grid
   re-renders (accounts + funds re-filtered).
   - **Checkpoint:** the page does **not** flash a full reload; only the form area
     updates. Any row whose account left the new subsidiary is *flagged in place*
     (a per-row error), never silently cleared.
   - **Checkpoint:** typed amounts/memos survive the swap.
3. **422 re-render (focus retention):** enter a deliberately **unbalanced** entry
   (e.g. two splits that don't sum to zero) and press **Save**.
   - **Checkpoint:** the form re-renders in place at the same scroll position with
     the imbalance error in the sticky totals bar; `autofocus` lands on the totals
     error (or the first flagged row), and every field keeps its value and its id —
     nothing you'd Tab to has moved.
   - Automated by `TestTxnStableInputIDsAcrossAllSwaps` +
     `TestTxnStableInputIDsAcrossRerender` (ids stable across the re-filter GET
     swap **and** the 422 POST swap) and `TestTxnInFlowActionsNeverFullReload`
     (both swaps return the form-region partial, never a full page).

## 2. Zero layout shift (program/class reveal)

Program and class cells are always rendered (stable ids) and toggled with
`visibility:hidden`, which **reserves layout box** — so revealing them must not
shift the grid.

1. In a fresh grid, set row 0's account to `QA Checking` (asset): the program and
   class cells are hidden.
2. Set row 0's account to `QA Rent` (expense): the **class** select appears
   (prefilled from the account default) and the **program** select appears.
   - **Checkpoint:** columns to the right (memo, error) do **not** jump left/right;
     the row height is unchanged. `visibility:hidden` (not `display:none`) is why —
     confirm nothing reflows.
3. Set row 0's account to `QA Donations` (revenue): program shows, class hides.
   - **Checkpoint:** again no horizontal shift.

## 3. Scroll preservation

1. Add several rows (**Add row** button) until the grid scrolls.
2. Scroll down, then trigger a 422 (unbalance the entry) and Save.
   - **Checkpoint:** after the in-place re-render the viewport stays where it was
     (the swap replaces `#txn-form` outerHTML; the browser does not scroll to top).
3. Change the subsidiary while scrolled.
   - **Checkpoint:** scroll position is preserved across the re-filter swap.

## 4. Keyboard-only entry of a 4-split mixed-fund transaction (end to end)

Enter this balanced, mixed-fund transfer **without touching the mouse**. It
balances overall **and per fund** (D20 per-fund zero-sum), which is what makes it a
genuine mixed-fund entry:

| row | account     | amount  | fund           |
|-----|-------------|---------|----------------|
| 0   | QA Savings  | 40.00   | QA Water Grant |
| 1   | QA Checking | -40.00  | QA Water Grant |
| 2   | QA Savings  | 10.00   | Unrestricted   |
| 3   | QA Checking | -10.00  | Unrestricted   |

Water Grant: +40 − 40 = 0 · Unrestricted: +10 − 10 = 0 · overall = 0.

All four accounts are **asset** leaves, so the program/class cells stay hidden and
are out of the native Tab order — linear Tab walks exactly
account → amount → fund → memo per row.

Steps (keyboard only):

1. Navigate to `/transactions/new` (or Tab to **New transaction** from a register
   and press Enter).
2. The grid starts with two rows. Tab to the **Add row** button and press
   **Enter/Space** twice to get four rows.
3. **Row 0:** Tab (or type-ahead) to focus the account select; pick `QA Savings`
   with **↓/↑ + Enter** (or type-ahead). Tab → amount, type `40.00`. Tab → fund,
   pick `QA Water Grant` with the arrows.
4. **Row 1:** account `QA Checking`, amount `-40.00`, fund `QA Water Grant`.
5. **Row 2:** account `QA Savings`, amount `10.00`, leave fund at **Unrestricted**.
6. **Row 3:** account `QA Checking`, amount `-10.00`, Unrestricted.
7. Focus the **date** field and press **t** (today) — the date shortcut works from
   the keyboard (t / + / − are wired on the date field's keydown).
8. Tab to **Save** and press Enter.
   - **Checkpoint:** the entry posts and you navigate (HX-Redirect) to the first
     split's register, where the 40.00 leg is visible.
   - **Checkpoint (focus):** every field is reachable and operable by keyboard;
     hidden program/class cells are correctly **skipped** by Tab (no dead stops).

Automated by `e2e/tests/entry-keyboard.spec.js` (real `page.keyboard` events:
Tab, Enter, Arrow-driven select operation, typed amounts, the date `t` shortcut;
no `selectOption`). Stability: run ×20 with zero flakes.

### Keyboard grid state machine — FINDING (documented, flagged, not fixed here)

`internal/web/static/txngrid.js` exports a **pure** grid state machine `nextCell`
(Tab/Enter advance, **Alt+Arrow** row move up/down, **Ctrl/Cmd+Enter** save,
**Escape** cancel, Enter-on-last-cell add-row). It is node-tested
(`txngrid.test.js`) and **imported** by the DOM glue `txneditor.js` — **but the glue
never calls it.** There is no `keydown` listener on the grid that consults
`nextCell`, so in the real browser today:

- **Enter** does not advance cell-to-cell and does not add a row on the last cell;
- **Alt+ArrowDown / Alt+ArrowUp** do not reorder rows;
- **Ctrl/Cmd+Enter** does not save; **Escape** does not cancel.

**Is keyboard entry broken?** No. Native **Tab / Shift+Tab** walks every visible
field in order, native `<select>` keyboard operation (arrows / type-ahead / Enter)
picks options, and **Add row** is a real `<button>` in the tab order
(Space/Enter → add). Hidden program/class cells (`visibility:hidden`) are correctly
skipped by native Tab. A book-keeper can complete the full 4-split mixed-fund entry
by keyboard **today** — proven by the e2e spec above. So the unwired state machine
is a **missing ergonomic enhancement** (Enter-to-advance, Alt+Arrow reordering,
Ctrl+Enter save), **not** a blocker, and per the p12.6 posture ("do NOT refactor
unless it genuinely breaks keyboard entry") it is **documented and flagged for the
orchestrator**, not wired in this step. Wiring it correctly is more than a hardening
fix: `nextCell` moves col+1 blindly and would land focus on the hidden program/class
cells, so correct wiring needs a skip-hidden traversal that does not exist and is not
tested — i.e. p12.2-scoped feature work to green-light separately.

### p12.2 settle-wiring follow-up — CONFIRMED result

The p12.2 concern: htmx wires a swapped-in node's `hx-*` triggers on the settle tick
(after paint), so an interaction within ~1 frame of a form swap can miss the trigger
(seen with the account-form type select's `hx-get` re-fetch).

**Confirmed during the keyboard-only pass: it does NOT bite keyboard entry.** The
account form's self-swapping type select is **not part of the transaction editor**.
Within the editor, the only `hx-*` triggers on swapped-in nodes are (a) the header
**subsidiary** re-filter and (b) the **payee suggest** input — neither is driven
"within a frame" of its own swap during a normal 4-split entry: the subsidiary is set
once before typing rows, and the payee autofill fetch is fired **programmatically**
(a manual `fetch`, not an `hx-*` trigger on a freshly-swapped suggestion node — see
`txneditor.js` `fetchAndApplyTemplate`), so it never races the settle tick. The
keyboard entry completes with no dropped interactions (e2e ×20, no flakes). **No
account-form refactor needed** (and the account form is out of scope for this step).

## 5. es (Spanish) locale pass

Repeat sections 1–4 in Spanish and verify every string translates — **no raw i18n
keys leak** (e.g. you must never see literal `txn.col.account` or
`error.txn.unbalanced` on screen).

1. Switch language to Spanish. A **logged-in** user's stored locale is authoritative
   (D14: user setting beats `?lang=`/cookie), so set Spanish in **My Settings** (once
   p13.1 lands) — or, until then, set `users.locale='es'` for your account and reload.
   (`?lang=es` only switches the language for the login page / anonymous requests; it
   does not override an authenticated user's stored locale.)
2. **Checkpoint (chrome + editor):** the nav, headings, column headers (Cuenta /
   Importe / Fondo / Programa / Clase / Nota …), the fund/date/payee labels, the
   Add-row and Save/Cancel buttons, and the apply-to-all control all render in
   Spanish.
3. **Checkpoint (errors):** trigger a 422 (unbalanced entry) → the totals-bar error
   renders the Spanish string (Importe desbalanceado …), not the key. Trigger a
   per-row error (an account outside the chosen subsidiary via the re-filter) → the
   row error is Spanish.
4. **Checkpoint (functional classes):** the class select options (Programa /
   Administración y general / Recaudación de fondos) render in Spanish.
5. **Checkpoint (no leak):** proper nouns (account/fund/payee names) render verbatim
   as stored data (they are **not** catalog entries — expected); everything else is
   Spanish.

**Structural guard:** the en/es catalogs are enforced to have identical key sets
(the i18n parity test), and Go returns error *keys* rendered via `{{t}}`, so a raw
key on screen would mean a missing/typo'd template call — none observed. All 52 `txn.*`
and `error.txn.*` keys exist in both `en.toml` and `es.toml`.

---

## Findings summary

| Area | Result |
|------|--------|
| Focus retention across swaps | OK — ids stable, autofocus lands correctly (automated). |
| Zero layout shift (program/class reveal) | OK — `visibility:hidden` reserves box; no reflow. |
| Scroll preservation | OK — outerHTML swap keeps viewport. |
| Keyboard-only 4-split mixed-fund entry | OK via native Tab + select keyboard ops + Add-row button (automated, ×20 stable). |
| **`nextCell` grid state machine unwired** | **FINDING** — imported but never called; Enter-advance / Alt+Arrow / Ctrl+Enter / Escape inert in-browser. Native keyboard covers linear entry, so **not a blocker**; flagged as a follow-up (needs skip-hidden traversal — p12.2-scoped, not a hardening fix). |
| p12.2 settle-wiring follow-up | CONFIRMED — does not bite keyboard entry; account form out of scope; no refactor. |
| es locale | OK — no raw keys leak; catalog parity enforced. |
