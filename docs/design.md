# buggrit — Design

Status: **draft** · 2026-10-15

buggrit is an automated pull-request reviewer that runs as a GitHub Action,
posts findings as review threads (like Copilot code review), and on every push
checks its earlier findings, then resolves, reopens or escalates them. It talks
to models through [`maestro-llms`](https://github.com/SnapdragonPartners/maestro-llms),
so it works with any provider that supports: Anthropic, OpenAI, Gemini, Ollama
and vLLM.

## 1. Goals and context

**Why build it:** cost. Copilot review is too expensive. buggrit has to work
well on self-hosted and flat-rate ("all-you-can-eat") models. With those,
**dollars per token barely matter**. What matters is:

1. **Convergence.** The PR reaches an agreed final state rather than looping
   through review → fix → review forever.
2. **Wall-clock time.** A pass finishes in a bounded, predictable time.

**Main reader: Claude Code (~90% of PRs).** Findings are read and acted on by
an agent that follows a repo workflow like the one in `morris/CLAUDE.md`: fix
or rebut every blocking finding, reply with the fixing commit, resolve the
thread, and check for new feedback. That shapes the design:

- Each comment must stand on its own and be actionable without the summary.
- An agent resolving a thread doesn't prove the issue is fixed. buggrit checks.
- An agent will act on every finding, so **precision beats recall**, where
  precision means *confidence*, not severity. Low-severity findings (for
  example documentation drift) are wanted on every pass. They have to be
  correct and clearly labelled non-blocking.
- An agent will loop for as long as the reviewer keeps producing new findings,
  so **convergence is a first-class requirement** (§7), not a tuning knob.

**Trust model: private, trusted repos (our own, at first).** PRs come from
trusted branches, not forks. Anyone with permission to comment on the repo,
whether a person or an agent using a person's credentials, counts as a
**human** for buggrit's purposes. The real integrity check is that **agents
never merge**: a person on the team approves and merges. So buggrit doesn't
try to be airtight against people who already have write access, which
matches Copilot's posture. We still take the cheap precautions in §10.

### Non-goals (for now)

- Hosting a GitHub App service. The engine is a library, so a `buggrit serve`
  front end can come later without a rewrite.
- Reviewing fork PRs (they get no secrets under `pull_request`, and we won't
  use `pull_request_target`).
- Running or building PR code. buggrit only reads.
- Approving PRs or posting `REQUEST_CHANGES` reviews.

## 2. Architecture

```
GitHub event
  pull_request: opened | synchronize | reopened | ready_for_review
  pull_request_review_comment: created     (replies, §3)
  pull_request_review: submitted
  workflow_dispatch                        (manual rerun)
  └─ Action job (composite action → prebuilt buggrit binary)
       └─ buggrit review
            0. Pick mode  full pass if HEAD has no completed pass yet; otherwise reconcile only
            1. Load       PR metadata, diff, checkout at HEAD, config (base branch), history
            2. Verify     open findings, findings with new replies, fixed findings whose
                          files changed, stale transitions (§6)
            3. Find       new findings (full pass only; scope from the completed baseline, §7)
            4. Reconcile  dedupe against history, group into classes, apply thresholds
            5. Publish    head-fenced: one review (N threads), replies, resolve/unresolve,
                          repairs, summary
```

**Mode comes from state, not from the event.** GitHub keeps only one pending
run per concurrency group, so a comment-triggered run can replace a pending
push-triggered one. Any run that finds HEAD without a completed pass
therefore does the full pass. A reconcile-only run (step 2 plus publish, no
finder) happens only when HEAD has already been fully reviewed.

**Publishing is fenced to the reviewed head.** All model work happens in
steps 2–4. Before publishing, and again before each state-changing mutation
(resolve, unresolve, transition reply), buggrit re-reads the PR head. If it
moved, the run stops publishing and exits, because the newer run owns the
PR. Workflow cancellation is asynchronous, so the fence is what keeps a
stale run from resolving threads on the evidence of an old commit. Anything
that slips through is caught next pass (§6, stale transitions).
Publishing is idempotent through the markers, and every pass repairs side
effects that are missing (§6), so a run killed partway through publishing
leaves nothing broken.

### Package layout

```
cmd/buggrit/          entrypoint: `review` (Action), later `local`, `serve`
internal/config/      load + validate .github/buggrit.json (base branch)
internal/llm/         config → llms.ChatClient (+ middleware, limiter, per-stage models)
internal/github/      REST + GraphQL client, diff parsing, line mapping
internal/state/       comment markers, finding ledger, thread state machine
internal/review/      engine: verify / find / reconcile, prompts, tools, output parsing
internal/publish/     render comments, post review, replies, resolve/unresolve, summary
```

The engine entry point looks roughly like:

```go
Review(ctx, PRRef, Workspace, GitHub, Config) (Result, error)
```

`Workspace` is a read-only view of the checkout and `GitHub` is an interface,
so the engine can be tested against fakes and reused by other front ends.

## 3. Trigger, packaging, permissions

```yaml
# .github/workflows/buggrit.yml in a consuming repo
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
  pull_request_review_comment:
    types: [created]
  pull_request_review:
    types: [submitted]
  workflow_dispatch:
    inputs: { pr: { required: true } }
concurrency:
  group: buggrit-${{ github.event.pull_request.number || inputs.pr }}
  # pushes cancel an in-flight pass; comment-triggered runs wait their turn
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
permissions:
  contents: read
  pull-requests: write
  checks: write          # only if the gate check (§8) is enabled
jobs:
  review:
    # skip drafts, and skip events caused by buggrit's own comments
    if: >-
      !github.event.pull_request.draft &&
      github.event.comment.user.login != 'buggrit[bot]' &&
      github.event.review.user.login != 'buggrit[bot]'
    runs-on: [self-hosted]   # must be able to reach the model endpoint
    timeout-minutes: 45      # hard wall-clock ceiling; buggrit's own deadline is lower
    steps:
      - uses: actions/create-github-app-token@v1
        id: app
        with:
          app-id: ${{ vars.BUGGRIT_APP_ID }}
          private-key: ${{ secrets.BUGGRIT_APP_PRIVATE_KEY }}
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.sha }}   # the PR head, not the merge commit
          fetch-depth: 0
          persist-credentials: false                       # keep tokens out of .git/config
      - uses: SnapdragonPartners/buggrit@v1
        env:
          GITHUB_TOKEN: ${{ steps.app.outputs.token }}
          BUGGRIT_LLM_API_KEY: ${{ secrets.BUGGRIT_LLM_API_KEY }}
```

The reusable workflow (below) hides all of this; a consuming repo only
passes secrets.

- **Packaging:** a composite action that downloads a prebuilt release binary
  for the runner's OS and architecture. It doesn't use a Docker container
  action, because self-hosted runners may have no Docker and macOS runners
  can't run container actions. We also ship a reusable workflow so a consuming
  repo needs only one `uses:` line.
- **Network:** self-hosted models usually require self-hosted runners, or an
  authenticated public endpoint.
- **Snapshot:** buggrit pins the whole pass to one head SHA, which it reads
  from the PR at startup. The checkout, the diff, line anchors and markers all
  refer to that commit. GitHub's synthetic merge commit is never used. At
  startup buggrit checks that the checkout's `HEAD` matches the PR's current
  head SHA, and exits without doing anything if not. For example, a
  comment-triggered run whose payload predates a newer push; that push's own
  run takes over. For `workflow_dispatch` the reusable workflow looks up the
  head SHA (`gh pr view`) before checking out.
- **Identity:** the supported setup is a small **GitHub App** (no hosting).
  The workflow mints an installation token with
  `actions/create-github-app-token`, so comments appear as `buggrit[bot]`.
  The built-in `GITHUB_TOKEN` also works as a fallback, with comments
  appearing as `github-actions[bot]`.
  GitHub documents that `resolveReviewThread` can fail with "Resource not
  accessible by integration" for the Actions `GITHUB_TOKEN`. That limitation
  applies to the Actions integration token. Claude Code's personal `gh`
  token, which resolves threads without trouble today, is a different kind
  of token. Build step 2 tests both early. If resolving or unresolving
  fails, buggrit keeps the state in markers, lists the affected threads in
  the summary as "resolve manually", and carries on.
- **Own comments:** a comment is buggrit's only if it has the
  `buggrit:v1` marker **and** its author is buggrit's configured login.
  Other workflows share `github-actions[bot]`, so neither check works alone.

## 4. Configuration

The config is read from the **base branch** (`.github/buggrit.json`), never
from the PR head, so a PR can't redirect `base_url` and leak the key. Secrets
come only from environment variables. Decoding is strict: unknown keys are an
error. Settings left out take defaults from code, and `version` is required.

```json
{
  "version": 1,
  "models": {
    "default": {
      "provider": "openai-compatible",
      "base_url": "https://…",
      "model": "qwen3-coder",
      "api_key_env": "BUGGRIT_LLM_API_KEY"
    },
    "verify": { "model": "glm-4.6" }
  },
  "limits": {
    "profile": "unlimited",
    "pass_deadline": "30m",
    "max_concurrency": 4,
    "max_tool_iterations": 200,
    "max_tokens_per_pass": 0
  },
  "review": {
    "min_severity": "P3",
    "min_confidence": "medium",
    "include": ["**"],
    "exclude": ["**/*.lock", "go.sum", "**/*.pb.go", "vendor/**", "**/*_gen.go"],
    "instruction_files": ["CLAUDE.md", "AGENTS.md", ".github/copilot-instructions.md"],
    "instructions": "Free-form repo-specific review guidance."
  },
  "convergence": {
    "max_rebuttals": 1,
    "warn_after_pass": 5
  },
  "gate": { "fail_on": "none" }
}
```

Notes:
- `models.default` is used for every stage. A per-stage entry (`find`,
  `verify`, `dedupe`) overrides only the fields it sets.
- `provider` is a buggrit name, not a `maestro-llms` package name:
  `anthropic | openai | google | ollama | openai-compatible`.
  `openai-compatible` (LunaRoute, vLLM, other Chat Completions endpoints) is
  currently backed by `maestro-llms`' `vllm` package. The mapping lives in
  one place in `internal/llm`, so a dedicated provider can replace it later
  without changing any config.
- `limits`: in the `unlimited` profile the token and iteration settings are
  sanity ceilings, and in `budget` they're hard budgets (§9). `0` means no
  ceiling.
- `review.min_severity` defaults to `P3`, so every severity is reported. The
  `min_confidence` filter is what controls noise.
- `gate.fail_on`: `none | P0 | P1` (§8).

## 5. The review engine

### Inputs the reviewer always gets
- PR title and **body**. Our PR bodies carry design context, ADR quotes, and
  "what this PR does NOT do", so the reviewer must not flag intentional gaps.
- Repo instruction files (`CLAUDE.md`, `AGENTS.md`, …) and `review.instructions`.
- The diff for the current scope (§7), with surrounding context.
- Existing threads from other reviewers (Copilot, humans) so it doesn't
  repeat findings they've already raised (§6).

### Every review uses tools
Each stage is a `toolloop` run. The tools are read-only and rooted at the
checkout:
- `read_file(path, start, end)`
- `grep(pattern, path_glob)`
- `list_dir`
- `get_diff(path)`
- `get_thread(id)`: earlier threads, during verification

The model can follow callers, read the ADRs and docs the PR body links to, and
check invariants across the codebase. This matters for documentation drift,
which can only be found by reading docs the diff didn't touch. The opening
prompt already includes the diff and instructions, so the model spends its
tool calls on investigation rather than orientation.

**Large PRs** are split into groups of related files (by package or
directory, sized with `llms.EstimateTextTokens`). Each group gets its own
agentic sub-review, and they run in parallel up to `max_concurrency`. Every
sub-review sees the PR-wide summary and can read any file, so it can still
find cross-cutting issues. A reconcile step merges and deduplicates their
results.

### Structured output
Each stage ends with a **terminal tool**: `submit_findings` (find) or
`submit_verdicts` (verify). If the arguments don't match the schema, buggrit
returns a tool error (`ToolResult{IsError: true}`) naming the problem, so the
model can correct itself and resubmit inside the same loop. A valid
submission ends the loop successfully.

`toolloop` in maestro-llms v0.7.1 can't do this: a successful tool call
always triggers another model call. The fix is tracked in
[maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53),
and buggrit depends on it. buggrit calls `toolloop` from one function in
`internal/review`, so the change is confined to one place. Finding schema:

```json
{
  "path": "internal/foo/bar.go", "start_line": 40, "line": 42, "side": "RIGHT",
  "severity": "P1", "category": "correctness", "confidence": "high",
  "title": "Nil map write when cache is cold",
  "problem": "...", "impact": "...", "fix": "...",
  "suggestion": "optional replacement text for the anchored lines",
  "class": "optional short name if this is one instance of a pattern",
  "instances": [{"path": "...", "line": 0}]
}
```

### Severity scale (same as the existing workflow)
- **P0:** data loss, corruption, critical security exposure, unusable result.
- **P1:** a concrete correctness, robustness, architectural or maintainability defect.
- **P2:** a real issue that doesn't block merge, such as documentation drift,
  missing tests for an edge case, misleading names or comments.
- **P3:** a minor nit.

P0 and P1 are **blocking**. P2 and P3 are **advisory**. They're reported on
every pass, labelled non-blocking in the comment, and never prevent the PR
from converging (§7).

Categories include: `correctness`, `security`, `concurrency`,
`error-handling`, `performance`, `api-contract`, `tests`, `docs-drift`,
`maintainability`, `style`. `docs-drift` gets explicit attention in the
prompt: it means docs, ADRs, help text, comments or config examples that
this change has made inaccurate.

## 6. History and state

### The thread is the source of truth
Every buggrit comment ends with a hidden marker:

```html
<!-- buggrit:v1 {"id":"f-7f3a2c","fp":"<fingerprint>","sev":"P1","pass":2,"sha":"abc123"} -->
```

Each buggrit reply in a thread carries a transition marker
(`{"id":"f-7f3a2c","to":"fixed","sha":"def456"}`). The state of a finding is
the result of replaying its thread. Replay only counts markers that pass the
own-comment check in §3; marker text in anyone else's comment is ignored.
The **summary comment** (one per PR, updated in place) is a *derived view*
that buggrit regenerates every pass. It's never read back as authoritative,
so it can't drift from the threads.

History is read with GraphQL `pullRequest.reviewThreads`, which gives
`isResolved`, `isOutdated`, the anchor, and all comments with authors. That
covers our findings plus human or agent replies.

Threads started by **other reviewers** (Copilot, humans) are also read.
buggrit doesn't manage them, but it treats them as already-raised issues:
the finder is told about them and deduplication compares against them. That
way buggrit doesn't repeat what another reviewer already said, which matters
while it runs alongside Copilot.

A database may come later, for metrics across PRs and model comparisons. For
now, comments are the only store.

### Finding state machine

```
open ──verify: fixed──────────────► fixed      (reply "fixed in <sha>", resolve)
 │
 ├─ reply judged a valid rebuttal ─► dismissed  (ack, resolve; never re-raised)
 ├─ reply judged invalid, rebuttals < max ─► open (reply with reasoning)
 ├─ reply judged invalid, rebuttals = max ─► needs_human (reply, leave open)
 └─ thread resolved, no reply, issue still present ─► open (unresolve + reply with evidence)

needs_human ── any later reply or resolution ─► dismissed (ack, resolve)
fixed ── defect is back (re-verify or dedupe match) ─► open (unresolve + "regressed in <sha>")
```

- **What gets verified each pass:**
  - every `open` and `needs_human` finding, including resolved threads,
    because resolving isn't proof;
  - every finding with a reply buggrit hasn't handled yet;
  - every `fixed` finding whose file changed since the fix commit;
  - every transition whose recorded `sha` isn't in the current head's history
    (§2 fencing, force-push).
- **Humans settle findings.** Under the trust model in §1, any reply or
  resolution by someone other than buggrit is a human action. That includes
  Claude Code acting with a person's credentials. Replies to `open` findings
  go through the rebuttal rules in §7. Once a finding is `needs_human`, the
  next reply or resolution from anyone settles it as `dismissed`, and buggrit
  doesn't argue. The one thing that never settles a finding is **a bare
  resolution with no reply while the issue is still present**. That reopens
  the thread, because it's the "resolved without fixing" case.
- **`dismissed` is terminal. `fixed` is terminal unless the defect comes
  back**: then the original thread reopens rather than a new thread being
  opened, so its history stays in one place.
- **Repair every pass.** After replay, buggrit compares each finding's state
  with the thread's actual `isResolved`, including terminal findings. If they
  disagree, it does the missing resolve or unresolve without reposting the
  transition reply. This repairs runs killed between "post marker" and
  "resolve".
- **Outdated anchors** (the code moved) are handled by giving the verifier the
  original snippet plus the current file, not by trusting line numbers.

### Review baseline
Two SHAs are recorded in the summary marker and in each pass's review body:
- `attempted_sha`: the head of the most recent pass, complete or not.
- `completed_sha`: the head of the most recent pass that covered its whole
  scope **and** finished publishing.

Incremental scope (§7) starts from `completed_sha`. An interrupted pass
(deadline, ceiling, cancellation, fence) doesn't advance it, so the next
pass covers what was missed. If `completed_sha` isn't an ancestor of HEAD
(force-push or rebase), the pass falls back to the full PR diff.

## 7. Convergence (the critical requirement)

Without these rules, the review → fix → review loop between Claude and
buggrit doesn't end. The rules, in order of importance:

1. **Report everything in one pass; don't drip-feed.** The worst failure we've
   seen is a reviewer finding one instance of a pattern per round (morris PR
   #355: six rounds). When the finder spots an issue, it must **sweep for every
   instance of that pattern in scope** and report them as one *class* finding:
   one thread anchored at the first instance, listing the rest, with the fix
   framed as "fix this at one point where all cases pass". This mirrors the
   "a repeated finding is a class" rule in our CLAUDE.md.
2. **Incremental scope.** Pass 1 reviews the whole PR. Later passes look for
   new findings **only in lines changed since `completed_sha`** (§6), using the
   whole PR as context. Fix commits can still produce findings, which is
   correct, because fixes can introduce regressions. Code that was already
   reviewed isn't re-reviewed by a model that just happened to notice
   something different this time.
3. **Stable identity and deduplication.** Every candidate is compared to all
   earlier findings, including fixed and dismissed ones. A deterministic
   fingerprint (path, category, normalized snippet) does the first pass and a
   short model call catches near-duplicates. What happens to a match depends
   on the earlier finding's state:
   - **dismissed:** the candidate is dropped. A dismissed finding is never
     raised again in any form.
   - **fixed:** the original thread reopens as a regression (§6).
   - **open, or another reviewer's thread:** the candidate is dropped.
4. **Bounded disputes.** At most `max_rebuttals` (default 1) buggrit replies
   per disputed finding, then `needs_human`. buggrit never argues in a loop,
   which matches "if Codex reaffirms, stop and bring it to Dan". For
   **advisory** findings (P2/P3), any reasoned reply ("deferred to #123",
   "intentional, see ADR 0042") is accepted as a dismissal without a rebuttal.
   The agent owns those decisions, as the morris workflow already says.
5. **Advisory findings never block convergence.** Thresholds don't tighten as
   passes add up: P2/P3 findings such as docs drift are still raised on the
   last pass. Instead, only open **blocking** findings and `needs_human` count
   toward convergence. Open advisory findings are listed but don't keep the
   PR from converging.
6. **A clear final state.** The summary always ends with a status line Claude
   can act on. One of:
   - `converged at abc123: 0 open blocking, 3 open advisory, 5 fixed, 1 dismissed`
   - `awaiting human at abc123: 2 needs_human, 0 open blocking, …`
   - `not converged at abc123: 2 open blocking, …`
   - `incomplete at abc123: deadline reached, …`

   `converged` requires zero open blocking findings, zero `needs_human`, and a
   completed pass at HEAD.
   It also carries a hidden JSON ledger, so the agent can fetch the whole
   state with one `gh api` call.
7. **Watch for non-convergence; don't suppress findings.** If a PR reaches
   `warn_after_pass` passes and still isn't converged, buggrit says so in the
   summary and lists the findings that keep reopening or appearing. That points
   a human at the cause (usually a missed class, or a dispute) instead of
   hiding findings.

The minimum metric to track per PR is passes until convergence. The eval
harness (§12) tracks it across models.

## 8. What the agent sees

### Finding comment (inline thread)

````markdown
**P1 · correctness** · confidence: high — Nil map write when cache is cold

`load()` returns before initializing `c.entries`, so the first `Put` after a
cold start panics.

**Impact:** any request that hits the cache before warm-up crashes the worker.

**Fix:** initialize `c.entries` in `New`, not in `load`.

```suggestion
	c := &Cache{entries: make(map[string]entry)}
```

Also applies to: `internal/cache/lru.go:88`, `internal/cache/ttl.go:31` (same class: *uninitialized map on constructor path*).

<sub>buggrit f-7f3a2c · blocking · Fix it and reply with the commit, or reply explaining why not. buggrit checks on the next push; resolving the thread without a fix will reopen it.</sub>
<!-- buggrit:v1 {...} -->
````

Findings that can't be anchored to a changed line are posted as file-level
review comments (`subject_type: file`) so they still get their own thread.

### Completion signal
- The job's check run tells the agent the pass is finished
  (`gh pr checks --watch`).
- Optional **gate check** (`gate.fail_on`): a separate `buggrit/findings`
  check run, created through the Checks API, whose conclusion is `failure`
  while blocking findings are open or need a human. The review itself is
  always `COMMENT`. The gate is off by default.
- Job summary (`$GITHUB_STEP_SUMMARY`): per-stage model, tokens, tool
  iterations, wall time. Useful for tuning even when tokens are free.

## 9. Limits: unlimited vs budget

There are two profiles that use the same knobs but treat them differently:

| | `unlimited` (default) | `budget` |
|---|---|---|
| Purpose | stop runaways and hangs | control spend |
| `max_tokens_per_pass` | optional sanity ceiling | hard budget |
| `max_tool_iterations` | high sanity ceiling (detects loops) | tight |
| On hitting a ceiling | publish what's done, flag the pass "incomplete: <reason>" | same |

**Wall clock is the real limit in both profiles.** `pass_deadline` is an
internal `context` deadline set below the job's `timeout-minutes`, so buggrit
always has time to publish partial results and say so. A pass that ends
without posting looks the same as "no findings" to an agent, which we can't
allow. An incomplete pass doesn't advance `completed_sha` (§6), so its
unreviewed scope is covered next time.

**Concurrency and lanes.** Every model call goes through a `Limiter` seam in
`internal/llm`. Today it's a semaphore of `max_concurrency`. Later it can
enforce lane limits (for example LunaRoute) or limits shared across runs,
reusing `maestro-llms`' rate-limit middleware where that fits. Retry, timeout
and circuit-breaker policy come from the `maestro-llms` recommended chain.

## 10. Security (proportionate to the trust model)

We take the precautions that are cheap. We deliberately don't defend against
people who already have write access to the repo (§1).

- **The model only reads.** Its tools are read-only and confined to the
  checkout: no symlink escape, no `..`, and no `.git/` (which can hold
  credentials and isn't needed for review, since diffs come from
  `get_diff`). It has no network or GitHub access. Everything posted goes
  through the terminal tool and our renderer.
- **No tokens on disk.** Checkout uses `persist-credentials: false`. The
  GitHub and LLM tokens exist only in buggrit's process environment, never
  in files the tools can read, and they're never echoed in logs or comments.
- **Config comes from the base branch**, so a PR can't redirect the model
  endpoint.
- No PR code is run, and `pull_request_target` is never used.
- Tokens get the minimum permissions listed in §3.

Not in scope: telling an agent apart from the person whose credentials it
uses, or stopping someone with write access from dismissing findings. The
merge gate is the control for those.

## 11. Testing

- Unit tests with `llms/testllm` fakes and an `httptest` fake GitHub (REST +
  GraphQL) that holds a PR with threads and replies.
- **State-machine tests** that script multi-pass scenarios:
  - fix, and false resolve (resolved without a reply while the issue remains);
  - valid and invalid rebuttal, `needs_human` settled by a reply or by a
    resolution;
  - a fixed finding that regresses;
  - force-push;
  - a pass interrupted by the deadline (the baseline must not advance);
  - a publish killed between the marker and the resolve (repaired next pass);
  - the head moving during publish (the fence stops it);
  - a comment-triggered run replacing a pending push-triggered run;
  - a forged marker in a non-buggrit comment (ignored).
- Golden tests for diff→line mapping and comment rendering.
- Dogfooding: buggrit reviews its own PRs from the first working milestone.

## 12. Build order

1. **Skeleton:** go module, Makefile, lint, CI, config loading and validation,
   `internal/llm` factory with limiter and per-stage models.
2. **GitHub layer:** PR fetch, diff parsing and line mapping, reading
   `reviewThreads`, posting reviews and replies, resolve/unresolve, markers,
   head fencing. All tested against the fake. Includes an early **live
   check** that resolving and unresolving threads works with both the App
   token and the Actions `GITHUB_TOKEN`.
3. **Agentic finder, end to end:** toolloop with the read-only tools,
   `submit_findings`, posting. First real reviews. Dogfood on this repo.
   Needs maestro-llms#53 (terminal tools).
4. **Verifier and thread lifecycle:** the state machine in §6, rebuttal judging.
5. **Convergence controls:** incremental scope, dedupe (including other
   reviewers' threads), class sweep, `needs_human`, the non-convergence warning,
   summary ledger, gate check.
6. **Large PRs:** splitting into parallel sub-reviews, then reconciling.
7. **Packaging:** release binaries, composite action, reusable workflow.
   Run in morris **alongside Copilot**, compare, then switch Copilot off.
8. **Eval harness:** PRs with planted bugs plus scripted fix and rebuttal
   agents. Measures precision, recall and passes to convergence per model.

Steps 1–2 don't depend on any maestro-llms work. Step 3 needs
[maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53)
(terminal tools). It also needs a working Qwen/GLM path,
[maestro-llms#54](https://github.com/SnapdragonPartners/maestro-llms/issues/54),
but any provider maestro-llms already supports can stand in until that's
done.

## 13. Decisions

- Module path `github.com/SnapdragonPartners/buggrit`.
- Config is JSON (`.github/buggrit.json`), using only the standard library.
- buggrit reads other reviewers' threads and deduplicates against them (§6).
- MVP models: **Qwen and GLM through LunaRoute**. Any `maestro-llms` changes
  needed to support them are tracked separately, in that repo.
- Every review uses tools. There's no tool-free mode.
- Comments are the only store for now. A database may come later.
- **LunaRoute** is OpenAI-compatible and is reached through
  `openai-compatible` (§4). Small fixes to routes or validation, if needed,
  are made in `maestro-llms`.
- **Local pre-push mode** (`buggrit local --base main`) is out of scope for
  MVP but kept cheap to add: the engine depends only on the `Workspace` and
  `GitHub` interfaces (§2), so a local front end would provide a git-diff
  workspace and print findings instead of posting them. Don't put anything
  Action-specific (env vars, event payload parsing) inside the engine.
- **Trust model** (§1): private, trusted repos. Anyone who can comment counts
  as human, including an agent using a person's credentials. Agents never
  merge, and that is the integrity control. Security work is limited to
  cheap precautions (§10).
- **Identity:** a GitHub App token (`buggrit[bot]`) is the supported setup,
  with the Actions `GITHUB_TOKEN` as a fallback (§3).
- **Terminal tools** are required from `toolloop`
  ([maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53)).
  LunaRoute verification is
  [maestro-llms#54](https://github.com/SnapdragonPartners/maestro-llms/issues/54).

No open questions remain.
