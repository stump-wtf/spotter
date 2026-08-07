# Design: Stranded-Work Reconciliation

**Spec:** stranded-work-reconciliation
**Status:** accepted
**Last Updated:** 2026-07-29

## Context

When spotter converged onto `stump.wtf/spotter`, the GitHub repos were left unarchived and kept living their own lives. By 2026-07-29 the divergence was:

```
                    984fdd3 (merge-base with the fork)
                       │
   canonical main ─────┼──── 7 commits (#352–#356 + 2 CI) ──── d7b5ecf
                       │
   fork main ──────────┴──── 35 commits (#16–#60) ──────────── dea34d5
                                                    └── 14 open PRs (#61–#74)

   joestump/spotter main ── f403524 + 1 commit ─── 7260224 (design system)
```

The 35 fork commits contain zero patch-equivalent matches against canonical (`git cherry` reports all `+`), but three of them are textual ports of canonical's still-open PRs #357–#359, and canonical's #353–#356 re-landed the content of fork PRs #70–#72/#74 — supersession runs in *both directions*, which is why the map in spec.md is normative rather than something each agent re-derives.

## Decision: stacked contiguous rebase, one PR per wave

Three strategies were considered:

1. **One mega-PR** (fork main → canonical main). Rejected: a +25k diff is unreviewable, and a single red CI run blocks everything.
2. **One PR per original fork PR** (~35 PRs). Rejected: maximal review overhead, and later commits depend on earlier ones anyway — 35 PRs would still have to merge in strict order, so the granularity buys nothing.
3. **Contiguous waves in original commit order** (chosen). Each wave is a reviewable, thematically-coherent PR (7–9 commits, roughly the 200–500-line-per-concern spirit at migration scale); strict ordering reproduces the history the fork already proved green, so conflicts only occur against canonical's 7 divergent commits — and those are concentrated in exactly the files the supersession map already flags.

Cherry-picks are verified **byte-identical** to the fork originals (`diff <(git show fork-sha --format="") <(git show landed-sha --format="")`) — the same verification standard used for the reduit migration, which caught nothing precisely because it forces honesty about every conflict resolution.

## Wave boundaries

Boundaries follow the fork's own thematic seams (visible in its PR numbering), with two adjustments:

- The **ports** (`#32–#34`) are excised because canonical's native #357–#359 must win — they are canonical-lineage and already open; merging the fork's copies instead would strand those PRs in the same state this spec exists to fix.
- The **design system lands last**. It is one squash commit whose parent (`f403524`) is two commits behind today's canonical main — it would cherry-pick cleanly *now* — but it rewrites templates that Waves 1–6 also touch (tag badges in #25, toasts in #28, login feedback in #20). Landing it first would force every subsequent wave to resolve presentation conflicts; landing it last resolves them once, in a PR whose author can see both sides.

## Issue-number hygiene

Fork references `#16`–`#74` and old-upstream references (e.g. the design-system commit's own `(#360)`) collide with live canonical numbers: canonical `#360` is a CI PR, not the design system. The reduit migration hit the identical trap (`Closes #180` would have closed an unrelated canonical issue). Hence the Stale Reference Hygiene requirement: every landed commit message and PR body either rewrites the number, expands it to a full source-repo URL, or annotates it as historical — and never carries a `Closes`/`Fixes` trailer at face value.

## Why restore fork issues instead of linking the backup

Wave 6's nine PRs each close a fork issue that describes a real defect (watermark gaps, concurrency holes, error-swallowing). Leaving those only in a JSON backup makes the canonical PRs close nothing, and the defects would be invisible to future triage. Restoring them (with provenance notes) keeps the tracker the single source of truth — the same reasoning as reduit's restored #252/#253.

## Verification gates

- Per wave: `make test lint` green on the branch before the PR opens; CI green before merge (branch protection on canonical requires the gitleaks + aibot contexts as of 2026-07-29 — the required-check set should be tightened per stump.wtf/ci#8's contract when the pipeline template is adopted, but that is out of scope here).
- Per cherry-pick: byte-identical patch or explained deviation.
- Terminal: both GitHub repos gone, `refs/archive/spotter/*` durable on canonical, zero open fork PRs/issues unaccounted for.

## Rollback

Waves are plain merges of rebased copies; the originals stay pinned under `refs/archive/spotter/*`. Reverting a wave is `git revert` of its merge — no history rewriting is ever needed, and no stranded lineage is consumed destructively.
