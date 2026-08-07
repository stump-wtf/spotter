# Stranded-Work Reconciliation

**Status:** accepted
**Version:** 0.1.0
**Last Updated:** 2026-07-29
**Governing ADRs:** ADR-0030 (ListenBrainz LB Radio playlist generation — lands with Wave 5)

## Overview

The 2026-07-15 convergence made `stump.wtf/spotter` (Gitea) the canonical repository, but two GitHub repositories kept accumulating work that never landed here:

- **`github.com/joestump-agent/spotter`** (the fork) — 35 merged commits and 9 genuinely-new open PRs that canonical does not have: an audit-remediation series, platform hardening, and the **entire ListenBrainz integration** (foundation, history sync, metadata enricher, playlist sync, LB Radio, scrobbling). 198 files, +25,275/−4,275 versus canonical `main`.
- **`github.com/joestump/spotter`** (the old upstream) — one squash-merged commit carrying the **design-system frontend redesign** (72 files, +2,079/−830).

This spec governs landing that work into canonical in strictly-ordered waves, and retiring the orphaned repositories afterwards. It exists so that the work can be executed by multiple agents across sessions without re-deriving the supersession analysis, and so the traps that bit the sibling reduit migration (stale issue-number references silently closing unrelated issues) are requirements here rather than tribal knowledge.

## Provenance

All source lineages are pinned as git refs in the maintainer's clone and MUST be pushed to canonical as `refs/archive/*` before any wave lands (see Requirement: Provenance Pinning):

| Ref | Points at | Content |
|---|---|---|
| `refs/archive/spotter/agent-fork-main` | `dea34d5` | Fork `main`: the 35-commit series |
| `refs/archive/spotter/agent-fork-pr-61` … `pr-74` | (14 refs) | Every open fork PR head |
| `refs/archive/spotter/github-joestump-main` | `7260224` | Design-system redesign (tree-identical to local `feat/spotter-design-system`) |

Fork issue/PR metadata is backed up at `~/Managed Files/backups/joestump-agent-spotter-issues-2026-07-29.json` (74 items: 24 issues, 50 PRs).

## Supersession Map

Work that MUST NOT be re-landed, verified 2026-07-29:

| Stranded item | Superseded by (canonical) |
|---|---|
| Fork PR #74 (enc:v1 credential migration) | merged `f403524` (#353) |
| Fork PR #72 (typed HTTPStatusError) | merged `c062d1b` (#354) |
| Fork PR #71 (enrichment batch starvation / best-image) | merged `93ae172` (#355) |
| Fork PR #70 (pin templ version) | merged `f3b4cbb` (#356) |
| Fork PR #73 (admin CLI config via config package) | likely merged `e7670d6` (#352, Viper consolidation) — verify before closing |
| Fork commits `6ee9b37`/`cea724c`/`4e10be6` ("Port upstream #357/358/359") | canonical **open** PRs #357, #358, #359 — merge those first, then skip these three commits during the Wave 2 rebase |

## Requirements

### Requirement: Provenance Pinning

The archive refs listed in Provenance SHALL be pushed to `stump.wtf/spotter` under `refs/archive/spotter/*` before any wave PR is merged, so every stranded lineage is durably reachable server-side and the GitHub repositories can be retired without data loss.

#### Scenario: Refs are durable before work begins

- WHEN Wave 1's PR is opened
- THEN `git ls-remote origin 'refs/archive/spotter/*'` on canonical lists at least 16 refs, including `agent-fork-main` and `github-joestump-main`

### Requirement: Wave-Ordered Landing

Stranded fork-`main` commits SHALL land as contiguous ranges in original commit order, one PR per wave, each wave based on the previous wave's merged result. Waves MUST NOT be reordered or landed in parallel: the fork history is linear, and out-of-order landing manufactures conflicts that the original order never had.

The wave boundaries are:

| Wave | Fork-main range (old fork PR #s) | Theme |
|---|---|---|
| 1 | `95e7ab7`…`035aa4d` (#16–#22) | Audit remediation A: security, key rotation, schedulers, login/auth + tests |
| 2 | `dba343f`…`68d3960` (#23–#31), **skipping** `6ee9b37`/`cea724c`/`4e10be6` | Audit remediation B: sync correctness, tag taxonomy, SSE, Spotify conformance, charts |
| 3 | `83eefaa`…`52a90c3` (#37–#46) | Platform hardening + **ListenBrainz foundation** (`554b017`) |
| 4 | `e5aea82`…`4976cc8` (#47–#52) | LB history sync, LB metadata enricher, coverage |
| 5 | `fa22fdc`…`dea34d5` (#56–#60) | LB playlist sync, LB Radio (ADR-0030), scrobbling |

#### Scenario: A wave lands

- WHEN a wave's commits are rebased onto the current canonical `main`
- THEN each cherry-picked patch is byte-identical to its fork original (verified by diffing `git show` output), or every deviation is a conflict resolution explained commit-by-commit in the PR body
- AND `make test lint` passes on the wave branch before the PR is opened

#### Scenario: A skipped commit

- WHEN the Wave 2 rebase reaches `6ee9b37`, `cea724c`, or `4e10be6`
- THEN the commit is skipped, and the PR body records that its content lands via canonical PRs #357–#359 instead

### Requirement: Native PRs Merge First

Canonical's own open PRs #357, #358, #359, and #361 SHALL be reviewed and merged before Wave 2 lands, because Wave 2 would otherwise re-land the same content from the fork's ported copies and the two lineages would conflict textually.

#### Scenario: Ordering enforced

- WHEN Wave 2's PR is opened
- THEN PRs #357, #358, and #359 are already merged or explicitly closed, and the Wave 2 body links to whichever happened

### Requirement: Stale Reference Hygiene

Commit messages and PR/issue bodies migrated from either GitHub repository SHALL NOT carry bare `#N` references, because fork numbers `#16`–`#74` resolve against canonical's tracker where they identify unrelated issues, and a carried `Closes #N` would auto-close a stranger on merge. Every migrated reference MUST be rewritten to the correct canonical number, expanded into a full URL against the source repo, or explicitly annotated as historical.

#### Scenario: A migrated commit message

- WHEN a fork commit whose message contains `(#43)` is cherry-picked
- THEN the landed commit message either keeps `(#43)` only alongside an explicit provenance note naming the fork, or rewrites it — and never introduces a `Closes`/`Fixes` trailer pointing at an unintended canonical issue

### Requirement: Fork Issue Restoration

The fork's 9 open issues (#35, #36, #41, #48, #50, #53, #54, #58, #59) SHALL be restored as canonical issues before the fork PRs that close them are rebased, each carrying a provenance note naming the original fork issue, so the Wave 6 PRs have real issues to close.

#### Scenario: Restored issue

- WHEN fork issue #48 (playlist-sync concurrency guard) is restored
- THEN a canonical issue exists with equivalent body content and a provenance line, and the rebased fork PR #64 closes the canonical number, not `#48`

### Requirement: Open Fork PR Rebasing

The fork's 9 genuinely-new open PRs (#61–#69) SHALL be rebased onto post-Wave-5 canonical `main` and landed as individual canonical PRs, and the 5 superseded fork PRs (#70–#74) SHALL be closed on the fork with a comment linking the canonical commit that superseded each (after verifying #73 against canonical #352).

#### Scenario: A rebased fork PR

- WHEN fork PR #65 (history-sync watermark gap) is rebased
- THEN its patch is byte-identical to the archived `refs/archive/spotter/agent-fork-pr-65` content or deviations are explained, `make test lint` is green, and the canonical PR closes the restored issue

### Requirement: Design-System Landing

The design-system redesign (`refs/archive/spotter/github-joestump-main`, tree-identical to `feat/spotter-design-system`) SHALL land as the final code wave, after Wave 6, because it rewrites templates that earlier waves also touch (tag badges, toasts, login feedback) and landing it last resolves those conflicts exactly once.

#### Scenario: Redesign lands last

- WHEN the design-system PR is opened
- THEN Waves 1–6 are already merged, and every template conflict is resolved in favour of keeping both the wave's behaviour and the redesign's presentation

### Requirement: Orphan Retirement

After all waves are merged, the two GitHub repositories SHALL be retired: `joestump-agent/spotter` deleted (its every lineage is in `refs/archive/spotter/*` on canonical) and `github.com/joestump/spotter` deleted or archived, leaving `github.com/stump-wtf/spotter` (the push mirror) as the only GitHub presence.

#### Scenario: Nothing dangles

- WHEN retirement completes
- THEN no open PR or issue exists on either GitHub repository, and canonical's `refs/archive/spotter/*` still resolve

### Requirement: Tracker Configuration

The repository's `CLAUDE.md` SHALL carry an `### SDD Configuration` section naming the Gitea tracker (`stump.wtf/spotter`) so sprint tooling files issues against canonical rather than the deleted GitHub repos.

#### Scenario: Tooling reads the config

- WHEN `/sdd:plan` or `/sdd:work` runs in this repository
- THEN it detects tracker type `gitea`, owner `stump.wtf`, repo `spotter` without prompting
