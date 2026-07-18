// p28.3 shared key-decision helper for the fuzzy comboboxes (combobox.js) AND the
// description autocomplete (descfield.js). PURE (rule 12, trap 2: node-tested, no
// `document`): both modules feed it the pressed key + whether their suggestion list is
// OPEN with a HIGHLIGHTED item, and it returns what to do so Enter/Tab behave identically
// across the account/fund/program combos and the description field.
//
// The contract the task names: when the suggestion list is OPEN and an item is
// HIGHLIGHTED, BOTH Enter and Tab COMMIT that highlighted choice; Enter additionally
// advances focus to the next field (and must preventDefault so it doesn't submit / bubble
// to a grid Enter=save), while Tab lets the browser's NATIVE Tab do the advancing (so we
// do NOT preventDefault it -- committing first is enough; native Tab then moves on). When
// the list is CLOSED / nothing highlighted, neither key is special: Enter falls through
// (so a closed-list Enter still reaches the grid's save handler) and Tab moves natively.
//
//   comboKeyAction(key, state) where state = { open: bool, hasActive: bool }.
//   `open` should already fold in "list not hidden AND has items" (an empty list is
//   hidden, so open+!hasActive cannot occur in practice, but the helper stays correct if
//   it does: no commit without a highlighted item).
//
// Returns { commit, preventDefault, focusNext }:
//   - commit       : pick/commit the highlighted item now.
//   - preventDefault: the caller should evt.preventDefault() (Enter only).
//   - focusNext    : the caller should programmatically move focus to the next field
//                    (Enter only; Tab relies on the browser's native advance).
export function comboKeyAction(key, state) {
  const open = !!(state && state.open);
  const active = open && !!(state && state.hasActive);
  if (key === 'Enter' && active) {
    return { commit: true, preventDefault: true, focusNext: true };
  }
  if (key === 'Tab' && active) {
    // Commit the highlight, but leave native Tab to advance focus (no preventDefault).
    return { commit: true, preventDefault: false, focusNext: false };
  }
  return { commit: false, preventDefault: false, focusNext: false };
}
