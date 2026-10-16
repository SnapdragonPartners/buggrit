# buggrit Repository Guide

## Project

buggrit is an automated pull-request reviewer that runs as a GitHub Action. It posts each finding as its own review thread, re-verifies earlier findings on every push, and drives the review → fix → review loop to an explicit final state. It reaches models through [`maestro-llms`](https://github.com/SnapdragonPartners/maestro-llms), so any provider that library supports works. The MVP targets Qwen and GLM through LunaRoute.

- Go 1.26, a single binary, standard library first
- Module path: `github.com/SnapdragonPartners/buggrit`
- **[docs/design.md](docs/design.md) is the binding design.** Read it before any non-trivial change.

## Durable Rules

- Create a branch and pull request for all changes, and follow the **Branch And Review Workflow** below: **review before push**, and Claude never merges. Dan approves pushes and merges
- **The design doc moves with the code.** A change to behavior the design describes updates `docs/design.md` in the same PR. If the code and the design disagree, that's a bug in one of them; decide which, and fix it rather than leaving the drift
- **Keep the review engine free of GitHub Actions specifics.** Environment variables, event payloads and Action outputs belong in `cmd/buggrit` (or a front-end package), not in the engine. The engine takes a `Workspace`, a `GitHub` interface and a `Config` (design §2), so a local or hosted front end stays cheap to add
- **Trust model** (design §1): buggrit runs on private, trusted repos. Anyone who can comment counts as a human, and agents never merge, which is the integrity control. Take the cheap precautions in design §10; don't build defenses against people who already have write access
- **Convergence beats coverage.** Every feature is judged by whether it helps a PR reach a final state (design §7). A change that could make the loop longer needs a stated reason
- Never bypass hooks with `--no-verify`; fix failures. The only exception, **push only**: a documentation-only push that Dan confirms for that specific push
- Changes needed in `maestro-llms` go there as issues or PRs, not as workarounds here. Open today: [#86](https://github.com/SnapdragonPartners/maestro-llms/issues/86) (reasoning-effort control). [#85](https://github.com/SnapdragonPartners/maestro-llms/issues/85) (empty reasoning turns) is merged but unreleased. When it ships, the empty-turn check in buggrit's `wellFormed` can go. Its other check, dropping a dangling tool-call turn from a run canceled mid-tool before the wrap-up, is buggrit's own and stays

## Dependency Discipline

Prefer the standard library. If the needed behavior can be written clearly in roughly 200 lines of well-tested Go, write it rather than adding a dependency. A third-party dependency needs both a domain that's complex or risky to get right and a mature, well-used package; justify it in the PR body. `maestro-llms` is first-party and exempt. Its provider packages are imported only in `internal/llm`.

`govulncheck` runs in CI. Vulnerable indirect dependencies (most arrive through `maestro-llms`' provider SDKs) are fixed by bumping them in `go.mod`, as long as tests stay green.

## Directory Layout

```
cmd/buggrit/       # Entrypoint: review (Action; --dry-run for local runs), config validate, version
internal/config/   # .github/buggrit.json: strict decoding, defaults, validation (design §4)
internal/llm/      # Config → maestro-llms chat clients per stage, shared limiter (design §4, §9)
internal/github/   # REST + GraphQL client, diff parsing, line → anchor mapping, head fence (design §2, §3, §8)
internal/github/githubtest/  # httptest fake of the GitHub endpoints we use (design §11)
internal/state/    # Comment markers and the own-comment check; later the ledger and state machine (design §6)
internal/glob/     # include/exclude path globs with ** (design §4)
internal/review/   # Engine: Review() entry point, workspace, read-only tools, prompts, agentic finder, reconcile (design §2, §5, §7)
internal/publish/  # Render finding comments and the pass review, post them behind the head fence (design §6, §8)
docs/design.md     # The binding design
hooks/             # Git hooks (pre-push: lint + test)
```

`internal/github` uses only the standard library (no go-github). Tests drive the real client against `githubtest`; `TestLive` is skipped unless `BUGGRIT_LIVE_*` is set (see `internal/github/live_test.go`).

## Makefile Targets

- `make build`: build `bin/buggrit`
- `make test`: unit tests with the race detector
- `make lint`: `go vet` + `golangci-lint run` (v1.64.8, pinned in CI)
- `make vuln`: `govulncheck ./...`
- `make check`: everything CI runs (lint, vuln, build, test)
- `make install-hooks`: install the pre-push hook
- `make deps`: check the required tools are installed

To develop against a local `../maestro-llms` checkout, use an uncommitted `go.work` (`go work init . ../maestro-llms`); `go.work` is gitignored. Never commit a `replace` directive.

## Branch And Review Workflow

**Roles:** Claude authors. Codex is the primary reviewer, before push, through Counterpoint. Dan orchestrates, resolves disagreements, approves pushes and merges. Post-push automated review is a final proofread: Copilot, and buggrit itself, which reviews every PR with the build from `main` (`.github/workflows/buggrit.yml`).

1. Work on a branch off `main` (`feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `chore/<slug>`). Never work directly on `main`.
2. Make the scoped change and run `make check`.
3. Commit locally.
4. Write branch notes (below) and submit the exact local commit for review with `mcp__counterpoint__review`. Pass the absolute repo path from `git rev-parse --show-toplevel`, the branch, the commit and the notes. The commit must be the branch tip and the HEAD of a clean worktree. Don't touch the worktree while a read-only review runs. Ask for a build-capable review (`build: true`) when a test run is material evidence: concurrency, state-machine logic, GitHub API interaction. If the tool isn't available, write the notes and ask Dan to relay them.
5. Address every blocking finding with a fix or a reasoned response, commit each round locally, and resubmit with updated notes.
6. Continue until Codex has no blocking findings, Dan overrides one, or a disputed finding is escalated to Dan.
7. **Stop for Dan's approval to push.** Quote Codex's approval or its remaining findings.
8. After approval, push and open the PR (see PR body conventions).
9. Address every post-push review thread with a fix or a reasoned reply, push, reply on the thread with the fixing commit, resolve it, and check again for new feedback.
10. Dan merges. Claude never merges.

### Responding to findings

Reviewers aren't authorities. **P0** (data loss, critical security exposure, unusable result) and **P1** (a concrete correctness, robustness or architectural defect) are blocking. P2 and below are at Claude's discretion; note how each was handled in the next round's notes. A finding Claude judges wrong gets a reasoned response that cites the design doc, a test or the deliberate choice. If Codex reaffirms a disputed finding, stop and bring both positions to Dan. Don't implement a change you believe is wrong just to end the loop.

**A repeated finding is a class.** When two or three findings share one shape, stop patching instances: name the class, fix it at one point, test each case, and ask the reviewer to check every input at that boundary.

### Branch notes

Lead with the exact commit hash and push state, then: review round; code, docs, or both; verification commands and their outcomes; what changed and why; design references (quote the controlling sentence for anything non-obvious); how each prior finding was resolved (keep every prior finding's disposition in each round, since the reviewer only remembers the last three rounds); rejected alternatives; known gaps.

## Pull Request Body Conventions

AI reviewers see only the diff and the PR body, so the body carries the design context:

1. **Summary**: what changed and why.
2. **Design references**: link the relevant sections of `docs/design.md`, and quote the controlling sentence for any non-obvious decision.
3. **What this PR does NOT do**: list intentional gaps so they aren't flagged as bugs.
4. **Test plan**: `make check` results, plus anything verified by hand.

## Testing

- New behavior comes with tests. Use `llms/testllm` fakes for model calls and `githubtest` for GitHub; tests never touch the network. The one exception is `TestLive`, which runs only when explicitly configured.
- Failing tests on a branch belong to that branch. We don't waive failures as "unrelated"; fix the code or fix the test.
