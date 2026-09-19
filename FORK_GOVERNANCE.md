# Fork governance — `lutzkind/github-mcp-server`

Applies to the maintained fork `lutzkind/github-mcp-server` of
`github/github-mcp-server` (upstream `main`:
`85598ba6e1256f7ebf4867b95d63b833c4549264`, tag `v1.12.2`).
Verified against the live GitHub API and remotes on **2026-09-19**; fork `main`
was `655a7f1310d16c615f3e808a3a89fa9fcda09f51`. This file closes audit finding
PA-62 ("Guarded work merged to main; ~1153-commit upstream drift").

## 1. Canonical branch model

| Ref | Role | Rules |
|---|---|---|
| `main` | **Canonical fork branch and default branch** | Guarded work is merged here (`655a7f13`). Integration via PR only; direct pushes discouraged. |
| `upstream-sync/YYYYMMDD` | Upstream integration branch | Created by `script/upstream-sync` / `Upstream Sync` workflow; PRs target `main`; automation never pushes `main`. |

`origin/HEAD -> origin/main`; the fork default branch is `main` (verified via
`gh api repos/lutzkind/github-mcp-server`).

> Note: `docs/repository-hygiene.md` documents the *branch/file hygiene MCP tools
> exposed by the server*; it is not fork governance. This file is.

## 2. Ownership map (what is local vs upstream)

| Divergent area | Owner | Authoritative source | Primary paths |
|---|---|---|---|
| Guarded mutation surface: preview/target binding, `expected_*` guards, target attestations, post-write branch verification retries | **fork maintainers** | fork `main` (guarded patch set, §3) | `pkg/github/repositories.go`, `pkg/github/retry.go`, `pkg/github/dependencies.go`, `pkg/github/mutation_guard_test.go`, `pkg/github/__toolsnaps__/apply_repository_changes.snap`, `pkg/github/__toolsnaps__/create_repository.snap`, `pkg/github/__toolsnaps__/create_or_update_file*.snap`, `pkg/github/__toolsnaps__/push_files*.snap`, `pkg/github/__toolsnaps__/manage_branch.snap` |
| Atomic multi-file patch commits (`file_patches.go`) and pull-request comment hardening | **fork maintainers** | fork `main` | `pkg/github/file_patches.go`, `pkg/github/file_patches_test.go`, `pkg/github/file_patches_handler_test.go`, `pkg/github/pullrequests.go`, `pkg/github/pullrequests_test.go`, `pkg/github/issues_test.go` |
| Base tool surface, toolsets, OAuth, remote server, docs, CI | upstream `github` | upstream `main` | everything not listed above |
| Shared generated artifacts (`__toolsnaps__` not listed above, `README.md`, `docs/`) | upstream `github` | upstream `main` | regenerate instead of hand-merging |

## 3. Guarded patch set (preserved on `main`)

Fork-only commits on `main` (verified 2026-09-19, all present):

| Commit | Date | Subject |
|---|---|---|
| `aff33990` | 2026-07-23 | feat: add guarded create_repository capability |
| `7e7a5977` | 2026-07-24 | test: cover issue comment API behavior |
| `cb638104` | 2026-07-26 | bind repository mutations to verified previews |
| `10abed17` | 2026-07-27 | fix: retry GitHub branch verification after writes |
| `a348d71e` | 2026-07-30 | attach target attestations to GitHub mutations |
| `1fea55f0` | 2026-07-21 | test: make post-write verification fixtures target-aware (#2) |
| `e677885c` | 2026-07-21 | test: separate GitHub fields variant snapshots (#3) |
| `655a7f13` | 2026-07-30 | Merge GitHub MCP response binding hardening |

Combined fork delta vs the merge base: 27 files, ~+5.4k/−0.8k lines, dominated by
`pkg/github/repositories.go` (+~3.2k) and `pkg/github/file_patches.go` (new).
Invariant tests: `pkg/github/mutation_guard_test.go`,
`pkg/github/file_patches_test.go`, `pkg/github/file_patches_handler_test.go`.

## 4. Drift status and maintenance item (revalidated 2026-09-19)

- Merge base: `c36e4e44` ("MCP: name-based resolution for Projects fields (#2760)",
  2026-07-10).
- Upstream-only commits since merge base: **172**; upstream `main` totals **1153**
  commits (this is the "~1153-commit upstream drift" figure), latest tag `v1.12.2`.
- Fork-only commits: **8**, all merged on `main` (guarded patch set above).
- Operational maintenance item: run the Upstream Sync workflow (or the script)
  and resolve conflicts per §5. The large upstream merge is intentionally **not**
  performed by the PA-62 governance change; the gap closed here is the missing
  governance/sync mechanism itself.

## 5. Conflict policy (upstream sync)

1. Integration only on `upstream-sync/YYYYMMDD`; automation never pushes `main`;
   merging remains a human PR decision.
2. Upstream wins for everything except the guarded areas in §2. Expect heavy
   conflicts in `pkg/github/repositories.go` (upstream churn to the same file).
3. When resolving `repositories.go`: keep upstream handler/tool wiring, then
   re-apply the fork's preview binding, `expected_*` guards, attestations and
   verification retries on top. Never drop a guard silently.
4. `__toolsnaps__` snapshots: prefer regenerating the affected snapshots with the
   repo's snapshot tooling/tests over hand-merging, then diff for unintended tool
   schema changes.
5. `pkg/github/dependencies.go`, `retry.go`, `file_patches.go` are fork-owned;
   keep them unless upstream introduces an equivalent feature, in which case
   upstream's implementation wins and fork tests are re-pointed at it.
6. Acceptance gate before merging the sync PR:
   `./script/test` (Go race tests) and `./script/lint`, plus a
   `go build ./cmd/github-mcp-server` with UI assets built.

## 6. Branch inventory and cleanup (2026-09-19)

Start: 272 remote branches. Deletion criteria (zero-loss only): tip fully
contained in `main`, verified with
`gh api repos/lutzkind/github-mcp-server/compare/main...<branch>` showing
`ahead_by: 0`, and no open PR on the branch. **15 branches qualified and were
deleted:**

| Branch | Tip (deleted) |
|---|---|
| `420-failed-api-calls-should-not-be-failures-not-errors` | `414309cc` |
| `add-lockdown-cache` | `efe9d40b` |
| `albarry-mcp` | `89e3afdb` |
| `categorise-error-types-in-monitors` | `23b16cfe` |
| `codex/isolation-auth-git-inspection-20260726` | `a348d71e` (tip is an ancestor of `main`) |
| `copilot/mcp-servers-plan-mode` | `3a6a6f66` |
| `full-go-version` | `02ef88d8` |
| `juruen/fix-log-file` | `bb9f2b8e` |
| `juruen/iologging` | `6d637779` |
| `next` | `3deaca89` |
| `o1-mise-instructions-update` | `2cece8f9` |
| `pagination-cursor-rethink` | `05e0f8f1` |
| `pre-minimal-changes` | `b2faa1c3` |
| `sammorrowdrums/add-agent-skills` | `b1575edf` |
| `sampling-context-reduction` | `bc4555f0` |

Remaining after cleanup: 257 refs (256 branches + `main`). **All remaining
branches contain unmerged commits and are retained** under the zero-loss rule.
Notable retained branches:

| Branch | Tip | Status | Disposition |
|---|---|---|---|
| `fix-fork-pr-review-comments` | `91b767e` | 1 commit ahead / 395 behind; "Fix add_comment_to_pending_review for fork PRs" (2026-01-06) | genuine fork candidate patch; rebase/evaluate or close |
| `codex/mcp-defect-fixes` | `0c09fd6` | 47 commits ahead / 5 behind (agent scratch from 2026-07) | retained; evaluate or delete in a follow-up |
| `copilot/*` (66), `sammorrowdrums/*` + `SamMorrowDrums/*` (27), `dependabot/*` (7), `tommy/*` (13), `tonytrg/*` (9), `juruen/*` (9), `wm/*` (8), other author/topic branches | — | contain unmerged commits | mirrored upstream contributor/topic refs; do **not** delete from the fork without confirming upstream state |

Policy going forward: branch deletion is allowed only with `ahead_by: 0` (or
after the unique commits are demonstrably merged/obsolete), no open PR, and a
recorded tip SHA in this file or the sync PR.

## 7. Upstream sync mechanism

- **Script**: `script/upstream-sync` — fetches `upstream/main`, creates
  `upstream-sync/YYYYMMDD` from `origin/main`, attempts merge (`--mode rebase`
  available), records conflicts in `UPSTREAM_SYNC_CONFLICTS.md` with
  `--report-conflicts`, pushes with `--push`, opens a PR against `main` with
  `--pr`. Refuses to run on `main`; refuses to overwrite an existing branch.
- **Workflow**: `.github/workflows/upstream-sync.yml` — `workflow_dispatch`
  (merge/rebase choice) plus a monthly schedule; `contents: write` +
  `pull-requests: write` only; `actions/checkout` pinned to
  `3d3c42e5aac5ba805825da76410c181273ba90b1` (v7.0.1, matching the major the
  repo already uses). The workflow only pushes `upstream-sync/<date>` and opens
  a PR.
- **Upstream remote**: `https://github.com/github/github-mcp-server.git`, added
  as remote `upstream` (the script adds/updates it idempotently).
- **Cadence**: monthly on the first business day (scheduled run), plus within
  5 business days of an upstream release tag; dispatch manually any time.
- `workflow_dispatch` becomes available once this workflow is on the default
  branch (`main`).

## 8. Known gaps / recommendations

- `main` is **not branch-protected** (verified `404` on 2026-09-19). Recommend a
  ruleset requiring PR review and `Build and Test Go Project` checks.
- Actions on the fork must be allowed to write (`contents: write`) and create
  PRs (repo setting "Allow GitHub Actions to create and approve pull requests")
  for the sync workflow to open its PR; otherwise use the script locally.
- Existing upstream CI workflows use tag-pinned actions (`actions/checkout@v7`,
  `setup-go@v6`, …). They are upstream-owned; SHA-pinning the new sync workflow
  only is deliberate to avoid merge churn (PA-62 scope).
