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

**At first, it runs only on our own repos.** PRs come from trusted branches,
not forks. We still treat PR content as untrusted prompt input (§10), because
that costs us almost nothing.

### Non-goals (for now)

- Hosting a GitHub App service. The engine is a library, so a `buggrit serve`
  front end can come later without a rewrite.
- Reviewing fork PRs (they get no secrets under `pull_request`, and we won't
  use `pull_request_target`).
- Running or building PR code. buggrit only reads.
- Approving PRs or posting `REQUEST_CHANGES` reviews.

## 2. Architecture

```
GitHub event (pull_request: opened | synchronize | reopened | ready_for_review)
  └─ Action job (composite action → prebuilt buggrit binary)
       └─ buggrit review
            1. Load      PR metadata, diff, checkout, config (base branch), history
            2. Verify    every earlier buggrit finding against HEAD
            3. Find      new findings (scope depends on pass number, §7)
            4. Reconcile dedupe against history, group into classes, apply thresholds
            5. Publish   one review (N threads) + thread replies/resolutions + summary
```

All model work happens in steps 2–4. Step 5 is short and mostly API calls, so
when a newer push cancels the run (`concurrency: cancel-in-progress`), the
cancellation almost always lands before anything is posted. Publishing is
idempotent through the markers (§6), so a run cancelled partway through
publishing is repaired by the next pass.

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
concurrency:
  group: buggrit-${{ github.event.pull_request.number }}
  cancel-in-progress: true
permissions:
  contents: read
  pull-requests: write
  checks: write          # only if the gate check (§8) is enabled
jobs:
  review:
    if: ${{ !github.event.pull_request.draft }}
    runs-on: [self-hosted]   # must be able to reach the model endpoint
    timeout-minutes: 45      # hard wall-clock ceiling; buggrit's own deadline is lower
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: SnapdragonPartners/buggrit@v1
        env:
          BUGGRIT_LLM_API_KEY: ${{ secrets.BUGGRIT_LLM_API_KEY }}
```

- **Packaging:** a composite action that downloads a prebuilt release binary
  for the runner's OS and architecture. It doesn't use a Docker container
  action, because self-hosted runners may have no Docker and macOS runners
  can't run container actions. We also ship a reusable workflow so a consuming
  repo needs only one `uses:` line.
- **Network:** self-hosted models usually require self-hosted runners, or an
  authenticated public endpoint.
- **Identity:** comments appear as `github-actions[bot]`. buggrit recognizes
  its own comments by marker (§6), not by author, because other workflows
  share that identity.

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
Findings come back through a `submit_findings` tool call that also ends the
loop. If the arguments don't match the schema, buggrit returns a tool error
(`ToolResult{IsError: true}`) naming the problem, so the model can correct
itself and resubmit inside the same loop. Finding schema:

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
the result of replaying its thread. The **summary comment** (one per PR,
updated in place) is a *derived view* that buggrit regenerates every pass.
It's never read back as authoritative, so it can't drift from the threads.

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
            ┌──────────── verify: still present ─────────────┐
            ▼                                                │
open ──verify: fixed──► fixed (reply "fixed in <sha>", resolve thread)
 │                                                           │
 ├─ reply judged a valid rebuttal ─► dismissed (ack, resolve; never re-raised)
 ├─ reply judged invalid, rebuttals < max ─► open (reply with reasoning)
 ├─ reply judged invalid, rebuttals = max ─► needs_human (reply, leave open, flag in summary)
 └─ thread resolved but issue still present ─► open (unresolve + reply with evidence)
```

- **Verify every non-terminal finding each pass**, including ones whose
  threads are resolved. Resolving isn't proof.
- `fixed` and `dismissed` are terminal. `needs_human` stays put until a human
  resolves the thread or replies. A human's resolution or reply is final:
  buggrit acknowledges it and doesn't argue.
- Outdated anchors (the code moved) are handled by giving the verifier the
  original snippet plus the current file, not by trusting line numbers.

### Last reviewed SHA
Recorded in the summary marker and in each pass's review body. Used for the
incremental scope (§7). If it isn't an ancestor of HEAD (force-push or
rebase), the pass falls back to the full PR diff.

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
   new findings **only in lines changed since `last_reviewed_sha`**, using the
   whole PR as context. Fix commits can still produce findings, which is
   correct, because fixes can introduce regressions. Code that was already
   reviewed isn't re-reviewed by a model that just happened to notice
   something different this time.
3. **Stable identity and deduplication.** Every candidate is compared to all
   earlier findings, including fixed and dismissed ones. A deterministic
   fingerprint (path, category, normalized snippet) does the first pass and a
   short model call catches near-duplicates. **A dismissed finding is never
   raised again** in any form.
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
   can act on, for example:
   `converged at abc123: 0 open blocking, 3 open advisory, 2 needs_human, 5 fixed, 1 dismissed`.
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
allow.

**Concurrency and lanes.** Every model call goes through a `Limiter` seam in
`internal/llm`. Today it's a semaphore of `max_concurrency`. Later it can
enforce lane limits (for example LunaRoute) or limits shared across runs,
reusing `maestro-llms`' rate-limit middleware where that fits. Retry, timeout
and circuit-breaker policy come from the `maestro-llms` recommended chain.

## 10. Security

- PR content (diff, body, comments, files) is **untrusted prompt input**. The
  model can't affect anything outside its review output: its tools are
  read-only and confined to the checkout (no symlink escape, no `..`), and it
  has no network or GitHub access. Everything posted goes through
  `submit_findings` and our renderer, so the worst a prompt injection can do
  is produce bad comments.
- Config and model endpoints come from the base branch. API keys come from
  secrets through the environment and are never echoed in logs or comments.
- No PR code is run, and `pull_request_target` is never used.
- `GITHUB_TOKEN` gets the minimum permissions listed in §3.

## 11. Testing

- Unit tests with `llms/testllm` fakes and an `httptest` fake GitHub (REST +
  GraphQL) that holds a PR with threads and replies.
- **State-machine tests** that script multi-pass scenarios: fix, false
  resolve, valid and invalid rebuttal, force-push, cancelled publish.
- Golden tests for diff→line mapping and comment rendering.
- Dogfooding: buggrit reviews its own PRs from the first working milestone.

## 12. Build order

1. **Skeleton:** go module, Makefile, lint, CI, config loading and validation,
   `internal/llm` factory with limiter and per-stage models.
2. **GitHub layer:** PR fetch, diff parsing and line mapping, reading
   `reviewThreads`, posting reviews and replies, resolve/unresolve, markers.
   All tested against the fake.
3. **Agentic finder, end to end:** toolloop with the read-only tools,
   `submit_findings`, posting. First real reviews. Dogfood on this repo.
4. **Verifier and thread lifecycle:** the state machine in §6, rebuttal judging.
5. **Convergence controls:** incremental scope, dedupe (including other
   reviewers' threads), class sweep, `needs_human`, the non-convergence warning,
   summary ledger, gate check.
6. **Large PRs:** splitting into parallel sub-reviews, then reconciling.
7. **Packaging:** release binaries, composite action, reusable workflow.
   Run in morris **alongside Copilot**, compare, then switch Copilot off.
8. **Eval harness:** PRs with planted bugs plus scripted fix and rebuttal
   agents. Measures precision, recall and passes to convergence per model.

Steps 1–2 don't depend on the LunaRoute work. Step 3 needs a working
Qwen/GLM path, which can be stubbed with any provider `maestro-llms` already
supports until that lands.

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

No open questions remain.
