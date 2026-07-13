# Build log

`build-session.jsonl` is the transcript of the Claude Code build session that
produced cuento, committed as a historical record of how the system was built
(the AGENTS.md working method, the per-step delegation, the decisions and their
rationale). It is a raw JSONL — one message per line.

## RULE-11 scrub

Per the project's data constraint (AGENTS rule 11: real ledger data never enters
the repo), this transcript was **scrubbed before committing**. The early go-live
import work (p09.4) profiled the gitignored real export (`fixtures/source/`) and
that structural profiling appeared in the session. Two-tier redaction was applied:

- **Wholesale**: any message containing a real-data profiling dump (record/tid
  counts, per-column distributions, currency/country/functional-class/program-code
  frequency tables, exchange-rate distributions) had its text replaced with
  `[REDACTED per RULE 11 …]`.
- **Token**: the org-internal program/department codes, the exact USD/HNL exchange
  rates, the consolidation marker, the real-data start date, and the exact
  fine-grained record/tid counts were replaced with `[REDACTED-RULE11]` wherever
  they appeared (prose, prompts, tool output).

Coarse aggregates already published in the committed docs (e.g. the ~49.5k
transaction / 293-account / 151-warning rehearsal counts in `docs/DECISIONS.md`
and `docs/golive.md`) were left intact. The scrubbed transcript was verified to
contain none of the redacted tokens or profiling signatures. The scrubbing script
itself is intentionally **not** committed (it held the literal patterns).
