# buggrit — Design

Status: **draft** · 2026-10-15

buggrit is an automated pull-request reviewer that runs as a GitHub Action,
posts findings as review threads (like Copilot code review), and on every push
or reply checks its earlier findings, then resolves, reopens or escalates them. It talks
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
                          Steps 2 and 3 run at once, sharing the pass deadline (§9).
            4. Reconcile  dedupe against history, group into classes, apply thresholds
            5. Publish    head-fenced: one review (N threads), replies, resolve/unresolve,
                          repairs, summary
```

**Every run reviews the PR as it is now, whatever triggered it.** GitHub
keeps only one pending run per concurrency group, so any run can replace a
pending one, including a comment-triggered run replacing a push-triggered
one. Every run therefore:
- looks up the PR's **current** head from the API before checking out, never
  the head in the event payload;
- picks its mode from state: a full pass if that head has no completed pass,
  otherwise reconcile only (step 2 plus publish, no finder and no review).
  A reconcile-only run selects findings to verify by the same rules as a
  full pass (§6). A completed pass at the head doesn't prove each thread's
  current state was judged against it: after a force-push back to an
  earlier head, for one, later transitions were decided elsewhere. Skipping
  findings no one has touched needs a per-finding record of the head each
  was last verified at, which belongs with the summary ledger (step 5).

This keeps an invariant: the last run to start in a PR's group saw the
latest head, so replacing a pending run never loses work.

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

The engine entry point is

```go
review.Review(ctx, PRRef, Workspace, review.GitHub, *review.Options) (*review.Result, error)
publish.Publish(ctx, publish.GitHub, PRRef, *review.Result) (*publish.Posted, error)
```

`Review` does steps 0–4 and returns what to post; `Publish` does step 5.
The thread state machine itself (§6) is pure: `internal/state` replays a
thread into a finding's state and decides each step from that state and the
verifier's verdict, so it's tested without models or GitHub.
`Workspace` is a read-only view of the checkout, and each `GitHub` is an
interface over the calls that step makes, so the engine can be tested
against fakes and reused by other front ends. A local front end would call
`Review` and print the result instead of publishing; `buggrit review
--dry-run` does that today.

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
  # Events caused by buggrit's own comments get a throwaway group, so they
  # never replace a pending run (§3 "Bot events").
  group: >-
    ${{ (github.event.comment.user.login == 'buggrit-bot[bot]' ||
         github.event.review.user.login == 'buggrit-bot[bot]')
        && format('buggrit-ignored-{0}', github.run_id)
        || format('buggrit-{0}', github.event.pull_request.number || inputs.pr) }}
  # pushes cancel an in-flight pass; comment-triggered runs wait their turn
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
permissions:
  contents: read
  pull-requests: write
  checks: write          # only if the gate check (§8) is enabled
jobs:
  review:
    # buggrit's own events get their own check name (§3 "Bot events")
    name: >-
      ${{ (github.event.comment.user.login == 'buggrit-bot[bot]' ||
           github.event.review.user.login == 'buggrit-bot[bot]')
          && 'ignored bot event' || 'review' }}
    # skip drafts, and skip events caused by buggrit's own comments
    if: >-
      !github.event.pull_request.draft &&
      github.event.comment.user.login != 'buggrit-bot[bot]' &&
      github.event.review.user.login != 'buggrit-bot[bot]'
    runs-on: [self-hosted]   # must be able to reach the model endpoint
    timeout-minutes: 45      # hard wall-clock ceiling; buggrit's own deadline is lower
    steps:
      - uses: actions/create-github-app-token@v1
        id: app
        with:
          app-id: ${{ vars.BUGGRIT_APP_ID }}
          private-key: ${{ secrets.BUGGRIT_APP_PRIVATE_KEY }}
      - id: head   # the PR's head now, not the event's (§2)
        run: |
          set -euo pipefail
          sha=$(gh pr view "$PR" --repo "$GITHUB_REPOSITORY" --json headRefOid -q .headRefOid)
          [ -n "$sha" ] || { echo "could not resolve PR head" >&2; exit 1; }
          echo "sha=$sha" >> "$GITHUB_OUTPUT"
        env:
          GH_TOKEN: ${{ steps.app.outputs.token }}
          PR: ${{ github.event.pull_request.number || inputs.pr }}
      - uses: actions/checkout@v4
        with:
          ref: ${{ steps.head.outputs.sha }}   # the PR head, not the merge commit
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
  refer to that commit. GitHub's synthetic merge commit is never used. The
  workflow looks up the current head before checking out, for every trigger
  (§2). At startup buggrit checks again. If the head has moved since the
  lookup, it exits without doing anything. That's safe: the push that moved
  it queued a newer run in the same group, and whichever run ends up pending
  will look up the head again.
- **Dogfooding:** this repo's own workflow (`.github/workflows/buggrit.yml`)
  builds buggrit from `main`, never from the PR, and skips with a notice
  when `main` has no config (as it did until PR #5 added one). Until build
  step 7 it builds from source. It has the triggers and the bot-event
  concurrency group and job name shown above.
- **Bot events:** with the App token, buggrit's own reviews and replies
  trigger workflow events. A job-level `if` would skip them, but the run
  would still enter the PR's concurrency group and replace a pending run
  queued by a human reply. So the concurrency group expression sends events
  authored by buggrit to a group of their own (§3 YAML), and the job-level
  `if` then skips them. A skipped job still files a check run under the
  job's name on the PR head, and the checks panel and `gh pr checks` show
  only the newest run per name. buggrit's events arrive while it is still
  publishing, so with a single name a skipped run would hide the real one:
  the PR would show `review` as skipped, and an agent waiting for the pass
  (§8 "Completion signal") could take it as finished. So the job's `name`
  is an expression with the same test. GitHub doesn't evaluate the name of
  a job its `if` skips, so these runs show the expression's text as their
  name rather than `ignored bot event`, and so does a draft's skipped run.
  That's untidy, but it's a different name from `review`, which is all the
  fix needs. Running the job for bot events just to get a tidy name would
  cost a runner start for every comment buggrit posts. The expressions name the login literally, so all three must match
  `identity.bot_login` (§4): a differently named App, or
  `github-actions[bot]` with the `GITHUB_TOKEN` fallback, needs them all
  edited. Otherwise buggrit's own comments count as human events.
- **Identity:** the supported setup is a small **GitHub App** (no hosting).
  The workflow mints an installation token with
  `actions/create-github-app-token`, so comments appear as `buggrit-bot[bot]`.
  (The App is `buggrit-bot` because GitHub reserves the name `buggrit` for
  the existing @buggrit account.)
  The built-in `GITHUB_TOKEN` also works as a fallback, with comments
  appearing as `github-actions[bot]`.
  **Resolving threads needs write access to repository contents.** The
  build-step-2 live check (`TestLive`, October 2026) measured each token
  kind:

  | Token | Post, reply, summary | Resolve / unresolve |
  |---|---|---|
  | Personal token (`repo` scope) | yes | yes |
  | App, Contents: Read | yes | no: "Resource not accessible by integration" |
  | App, Contents: Read and write | yes | yes |
  | Actions `GITHUB_TOKEN`, `contents: read` | yes | no, same error |

  So the App needs **Pull requests: Read and write**, **Contents: Read and
  write**, and, for the gate check, **Checks: Read and write**. The Actions
  `GITHUB_TOKEN` with `contents: write` is untested. Whenever resolving or
  unresolving is refused, buggrit keeps the state in markers, lists the
  affected threads in the summary as "resolve manually", and carries on.
- **App credentials** are set once at the org level: the variable
  `BUGGRIT_APP_ID` and the secret `BUGGRIT_APP_PRIVATE_KEY`, both visible to
  the repos buggrit runs in. The App's installation needs those repos too.
  Runs triggered by Dependabot see only Dependabot secrets, so reviewing
  Dependabot PRs also needs the key added there.
- **Own comments:** a comment is buggrit's only if it has the
  `buggrit:v1` marker **and** its author is buggrit's configured login.
  Other workflows share `github-actions[bot]`, so neither check works alone.
  Logins compare case-insensitively, in REST form (`buggrit-bot[bot]`). GraphQL
  reports a bot's login without the `[bot]` suffix, so the GitHub layer adds
  it back for `Bot` authors before anything compares logins.

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
      "api_key_env": "BUGGRIT_LLM_API_KEY",
      "request_timeout": "10m",
      "max_output_tokens": 0,
      "reasoning_effort": ""
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
  "gate": { "fail_on": "none" },
  "identity": { "bot_login": "buggrit-bot[bot]" }
}
```

Notes:
- `models.default` is used for every stage. A per-stage entry (`find`,
  `verify`, `dedupe`) overrides only the fields it sets. A field that is
  present counts as set even when it's `""` or `0`; for example,
  `"base_url": ""` switches a stage to its provider's default endpoint.
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
- `request_timeout` bounds one model-call attempt; retries each get their
  own. `max_output_tokens` of `0` means the provider's default.
- `api_key_env` is required for `anthropic`, `openai` and `google`, and
  optional for `openai-compatible` (those endpoints may run without auth).
  `ollama` rejects it: maestro-llms' Ollama provider has no authentication
  option, and a key that would be silently ignored is worse than an error.
- `max_output_tokens` caps every request a stage makes. A caller's smaller
  limit is kept.
- `reasoning_effort` asks a reasoning model to bound its thinking:
  `none | minimal | low | medium | high | xhigh | max`, or `""` (the
  default) for the model's own default. It's set per stage like any other
  model field, and sent as maestro-llms' `ChatRequest.Reasoning`. Each
  provider maps a level it supports and omits the rest, so an unsupported
  level falls back to the model default rather than failing. The job log
  says, once per stage, what the provider actually received. A model that
  rejects a level its provider supports fails the call with the provider's
  error. buggrit sets no default, since the right level depends on the
  model. [buggrit#8](https://github.com/SnapdragonPartners/buggrit/issues/8)
  measured it for dogfooding (§13).
- `gate.fail_on`: `none | P0 | P1` (§8). With `P0` or `P1`, the gate also
  fails until the head has a complete, fully verified pass.
- `identity.bot_login` is the login buggrit's own comments must be authored
  by (§3, "Own comments"). Change it when using `GITHUB_TOKEN`
  (`github-actions[bot]`) or a differently named App.
- `buggrit config validate [path]` checks a file and prints it with defaults
  applied.

## 5. The review engine

### Inputs the reviewer always gets
- PR title and **body**. Our PR bodies carry design context, ADR quotes, and
  "what this PR does NOT do", so the reviewer must not flag intentional gaps.
- Repo instruction files (`CLAUDE.md`, `AGENTS.md`, …) and `review.instructions`.
- The diff for the current scope (§7), with surrounding context.
- Existing threads from other reviewers (Copilot, humans) so it doesn't
  repeat findings they've already raised (§6).

The opening message is bounded (300 KiB). Each section before the diff has
its own share, and the diff fills the rest. Whatever doesn't fit is named, so
the model fetches it with `read_file` or `get_diff`. The tools stream files,
so a large file costs time, not memory.

### Every review uses tools
Each stage is a `toolloop` run. The tools are read-only and rooted at the
checkout:
- `read_file(path, start_line, end_line)`
- `grep(pattern, path_glob, ignore_case)`
- `list_dir(path)`
- `get_diff(path, offset)`: one file's diff with head-side line numbers, or,
  with no path, the list of changed files. `offset` continues output that
  was truncated.
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

This uses `toolloop`'s terminal tools, added in maestro-llms v0.8.0
([maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53),
its ADR-0017). buggrit calls `toolloop` from one function in
`internal/review`.

- **Validation:** `submit_findings` checks every finding: enums, required
  fields, that the path is a changed, in-scope file, and that every line
  exists at the head. It reports all problems at once, so one resubmission
  can fix them. `submit_verdicts` checks that every listed finding has
  exactly one verdict, that a `fixed` verdict's `evidence_files` include the
  anchor file, and that `dismissed` is used only on a finding with a new
  reply. At wrap-up it accepts verdicts for only some findings; the rest are
  verified on a later run.
- **Files outside the diff:** a finding about a file the diff doesn't touch,
  such as a stale doc, is anchored at the changed line that made it wrong.
- **No submission:** a model that answers in text, or returns an empty
  response (a reasoning model that spent its whole output budget thinking),
  instead of submitting gets one nudge. If it still doesn't submit, or the
  run hits a ceiling, the deadline or a model error, the pass is reported
  incomplete with that reason. It never comes back silently empty (§9).
- **"No ceiling":** `max_tool_iterations: 0` is passed to toolloop as
  `toolloop.NoIterationLimit`, since toolloop reads 0 as its own default
  of 8. The deadline bounds the run (maestro-llms v0.10.0,
  [ADR-0019](https://github.com/SnapdragonPartners/maestro-llms/blob/v0.10.0/docs/adr/0019-toolloop-deadline-and-no-iteration-limit.md)).

`submit_findings` takes `{"summary", "coverage", "findings": [...]}`.
`coverage` is required: `complete` if the model reviewed the pass's whole
scope (the whole change, or on a later pass the lines changed since
`completed_sha`, §7 rule 2), `partial` if it didn't (it says what it missed in `summary`). A partial
submission's findings are posted, but the pass is incomplete, so the next
run reviews that head again (§9).

Finding schema:

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
(`{"id":"f-7f3a2c","to":"fixed","kind":"fixed","sha":"def456","seen":1234,"evidence_files":[…]}`).
The state of a finding is the result of replaying its thread. Replay only
counts markers that pass the own-comment check in §3; marker text in anyone
else's comment is ignored, and the comment counts as a human's.

- `kind` says why the transition happened, since several lead to the same
  state: `fixed`, `regressed`, `reopened` (resolved without a fix),
  `rebutted` (a reply judged invalid), `needs_human`, `dismissed` (a reply
  judged valid, or any reply to an advisory finding), and `settled` (a
  human's reply or resolution after `needs_human`). Rebuttals are counted
  by kind.
- `seen` is the database ID of the newest human comment in the thread when
  the transition was decided. A human comment is **handled** once a
  transition's `seen` covers it. A reply that lands while buggrit is still
  deciding, even before buggrit's reply is posted, stays unhandled for the
  next run.
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
  - every `fixed` finding where **any of its evidence files** changed since
    the fix commit. A `fixed` verdict from the verifier must list the files
    it relied on (`evidence_files`: the anchor file plus wherever the fix
    actually lives, such as a caller that now initializes the state). The
    list is stored in the transition marker. So undoing a fix in another
    file still triggers re-verification;
  - every `fixed` finding whose fix commit isn't in the current head's
    history (force-push or rebase), or whose changes since can't be read.
    Re-verifying is always safe. `dismissed` doesn't depend on the code,
    and `open` and `needs_human` findings are verified anyway.

  A `needs_human` finding that a human has replied to or resolved is
  settled as `dismissed` without a model. A `dismissed` finding is never
  verified again.

  **Why a late transition from an older head is safe.** Suppose run A passes
  the publish fence, then head B is pushed before A's mutation lands. Each
  transition type is still handled:
  - **fixed:** recorded with fix SHA A and its evidence files. If B touches
    any of them, the finding is re-verified at B. If B touches none, the
    files the verdict relied on are unchanged, so it still holds at B.
  - **dismissed:** a judgment about a reply or a human decision, not about
    the code. Landing late doesn't make it wrong.
  - **reopen, rebuttal reply, needs_human:** never suppress anything, and
    are re-evaluated by B's run anyway.

  So the fence is an optimization that avoids pointless mutations, and
  correctness comes from the rules above.
- **Humans settle findings.** Under the trust model in §1, any reply or
  resolution by someone other than buggrit is a human action. That includes
  Claude Code acting with a person's credentials. Replies to `open` findings
  go through the rebuttal rules in §7. Once a finding is `needs_human`, the
  next reply or resolution from anyone settles it as `dismissed`, and buggrit
  doesn't argue. Only a reply posted after the `needs_human` marker counts.
  One that landed while buggrit was deciding answered the earlier state; it
  stays unhandled, but it settles nothing. The one thing that never settles a finding is **a bare
  resolution with no reply while the issue is still present**. That reopens
  the thread, because it's the "resolved without fixing" case.
- **`dismissed` is terminal. `fixed` is terminal unless the defect comes
  back**: then the original thread reopens rather than a new thread being
  opened, so its history stays in one place.
- **Repair every pass.** After replay, buggrit compares each finding's state
  with the thread's actual `isResolved`, including terminal findings. If they
  disagree, it does the missing resolve or unresolve without reposting the
  transition reply. This repairs runs killed between "post marker" and
  "resolve". `fixed` and `dismissed` threads should be resolved, and
  `needs_human` ones left open. A `needs_human` thread GitHub wouldn't
  unresolve is retried every pass. An open thread someone resolved goes to
  the verifier first, since that's the "resolved without fixing" case.
- **A needs_human thread is unresolved before its marker lands.** Replay
  reads a resolution under a `needs_human` marker as a human settling the
  finding, so the order matters there. The unresolve is sent even if the
  thread looked open when it was read, since someone may have resolved it
  while the verifier ran. A run killed between the two then leaves no
  marker, and the next run decides again. Every other transition
  posts its reply first and leaves the resolution to repair. If GitHub
  refuses the unresolve (§3) and the thread is still resolved, the marker
  records `"left_resolved":true`, and only a reply settles that finding.
  GitHub refuses by token, not by thread, so after any refusal buggrit
  reads the thread's current resolution. A thread already as wanted counts
  as neither refused nor needing manual action.
- **Never say the same thing twice.** If buggrit's latest transition already
  says a finding is open (`reopened`, `regressed` or `rebutted`) and no
  human has commented since, a resolved thread is just unresolved, with no
  new reply. That covers both a run killed before its unresolve and someone
  resolving the thread again without a word. A still-fixed finding gets no
  reply unless someone replied to it. If someone did, a fresh `fixed`
  transition answers, and it moves the evidence baseline to the current
  head.
- **Outdated anchors** (the code moved) are handled by giving the verifier the
  original snippet (the root comment's `diffHunk`, read with the thread) plus
  the current file, not by trusting line numbers.
- **Verdicts carry evidence.** `submit_verdicts` returns, for each finding:
  the state (`fixed`, `open`, or `dismissed` for a reply that holds up), a
  short reason that's posted as buggrit's reply, and for `fixed`, a required
  non-empty `evidence_files` list that includes the anchor file. The
  verifier runs as its own agentic stage (§5) with `get_thread` added to
  the read-only tools. It's given every selected finding with its anchor,
  its text, the original snippet and its unhandled replies.

### Review baseline
Two SHAs are recorded in the summary marker and in each pass's review body:
- `attempted_sha`: the head of the most recent pass, complete or not.
- `completed_sha`: the head of the most recent pass that covered its whole
  scope **and** finished publishing.

Each pass's review body ends with a **pass marker**
(`{"id":"pass-3","pass":3,"sha":"…","status":"complete"}`, or `"incomplete"`).
The status follows the finder: `complete` means the pass searched its whole
scope for new findings. Verification doesn't affect it, because re-running
the finder over a scope it already searched is the drip-feed that
incremental scope (§7 rule 2) exists to stop. Nothing was posted on the
threads of findings the verifier didn't reach, so the next run, at the same
head or a new one, selects them again. In the meantime, the pass's status
line says `incomplete` and the run exits 1 (§7 rule 6, §8).
The review is the last thing a pass posts, after every finding thread, so
`complete` also means "finished publishing". A head with a complete pass
gets no new full pass (§2, step 0). A pass that stops partway leaves no
complete marker; the next run redoes it, and dedupe against the fingerprints
of the threads that did land keeps it from posting them twice.

Incremental scope (§7) starts from `completed_sha`. An interrupted pass
(deadline, ceiling, cancellation, fence) doesn't advance it, so the next
pass covers what was missed. If `completed_sha` isn't an ancestor of HEAD
(force-push or rebase), GitHub no longer has it, or GitHub won't diff the
changes since it, the pass falls back to the full PR diff.

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

   "Changed since `completed_sha`" comes from GitHub's compare API
   (`completed_sha...HEAD`), read against the PR's own diff:
   - A line is in scope if the changes since `completed_sha` add it **and**
     the PR's diff adds it. A merge from the base branch therefore brings
     nothing into scope.
   - A run of lines that is only deleted (not replaced) puts its head-side
     neighbors in scope, since that's where a finding about the deletion is
     anchored, but only where the PR's diff also deletes next to them. That
     keeps a merge's deletions out.
   - A change with no head-side lines to anchor on (a file emptied or
     deleted, a binary file, a mode change, a pure rename) puts the file in
     scope for file-level and deleted-line findings. A change with no text
     hunks counts only if the PR's diff makes the same kind of change, again
     to keep a merge's changes out.
   - Only a finding's **anchor** must be in scope, checked as it will be
     published: a range GitHub would narrow to its last line is judged by
     that line, and a line that would fall back to a file-level comment is
     out of scope. A file-level finding, or one on a deleted line, needs
     only its file to have changed. A class finding's other instances may
     be anywhere (rule 1).

   The opening message lists the lines in scope, and `submit_findings`
   rejects an anchor outside them, naming the lines it would accept. If
   nothing reviewable changed (say, only a merge from the base branch), the
   pass completes without running the finder. Each pass's review body
   states its scope.
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
   The agent owns those decisions, as the morris workflow already says. The
   verifier judges whether a reply gives such a reason. A claim of a fix
   that the code disproves doesn't, and it gets the usual rebuttal. An
   advisory finding never goes to `needs_human`, which would hold up
   convergence (rule 5). Once its rebuttals are used up, the author's word
   stands and it's dismissed.
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

<sub>buggrit f-7f3a2c · blocking · Fix it and reply with the commit, or reply explaining why not. buggrit checks on the next push or reply; resolving the thread without a fix will reopen it.</sub>
<!-- buggrit:v1 {...} -->
````

Findings that can't be anchored to a changed line are posted as file-level
review comments (`subject_type: file`) so they still get their own thread.
GitHub's create-review call can't carry file-level comments, so they're
posted one by one, before the review that carries the pass marker (§6). A
range that crosses hunks is narrowed to its last line, and a line outside
every hunk falls back to file level. A suggestion is posted only when the
anchor still covers exactly the lines it replaces. If GitHub rejects a line
anchor anyway, the review call posts nothing, and its findings are posted at
file level instead.

An advisory finding's footer instead offers dismissal: "Fix it and reply
with the commit, or reply with a reason, such as deferred or intentional, to
dismiss it." (§7 rule 4). A comment is capped below GitHub's 65,536-character
limit.

### Transition replies

Each transition is a reply in the finding's thread, carrying its marker
(§6). The reply opens with the outcome in bold and the reviewed head:
"**Fixed** at `def4567`.", "**Still present** at `def4567`, so this thread is
reopened.", "**Regressed** at …", "**Still open** at …" (a rebuttal),
"**Needs a human.**", "**Dismissed.**". The verifier's reason follows. A
`fixed` reply also names the evidence files buggrit will watch. When GitHub
refuses a resolve or unresolve (§3), the pass's review body lists the
threads to change by hand, and so does the job summary. A reconcile-only
run posts no pass review. If a transition it posted has its resolution
refused, it posts a short review with the list. A repair that keeps being
refused on later runs is listed only in the job summary, so that review
isn't repeated on every run.

**Fingerprint** (§7 rule 3): a hash of the path, the category, and the
anchored lines with whitespace normalized (head lines from the checkout,
deleted lines from the diff), so the same issue on a moved line still
matches. A file-level finding has no anchored lines and uses its normalized
title. The finding ID is `f-` plus the fingerprint's first six hex digits.

### Completion signal
- The job's `review` check run tells the agent the pass is finished
  (`gh pr checks --watch`). Runs skipped for buggrit's own events report
  under another name, so they don't stand in for it (§3 "Bot events").
- Optional **gate check** (`gate.fail_on`): a separate `buggrit/findings`
  check run on the head, created through the Checks API. It is the check
  to make required, rather than `review`, because GitHub counts a skipped
  job as passing. Its conclusion is `success` only when all three hold:
  - the head has a complete pass marker (§6 "Review baseline"). A run
    that skips because the head moved or the base branch has no config
    doesn't create one. A reconcile-only run at a head that already has
    one does count;
  - no earlier finding is left unverified;
  - no finding at or above `fail_on` is open or `needs_human`.

  Otherwise it is `failure`. With `fail_on: P1` this is the same condition
  as §7 rule 6's `converged`. Today's status line (`complete at …`) reports
  what a pass did, not those totals. The `converged` line, the summary
  ledger and the gate all land in build step 5 (§12). The gate reflects
  whether the review finished, not
  only what it found: a pass that hits the deadline with no blocking
  findings posted yet stays `failure`. A run sets it to `in_progress` when
  it starts and concludes it before exiting. A run killed before that leaves
  it `in_progress`, which still blocks a required check. A new head has no
  gate until its first run. So every gap fails toward "not done", the same
  choice §6 makes for a thread whose state is unknown. The cost is that a
  PR whose passes keep hitting the deadline stays blocked until someone
  reruns the pass or overrides the check. The review itself is always
  `COMMENT`. The gate is off by default.
- Job summary (`$GITHUB_STEP_SUMMARY`): per-stage model, tokens, tool
  iterations, wall time. Useful for tuning even when tokens are free.
- `buggrit review` exits 0 for a complete pass, a skipped one (the head
  already has a complete pass and no earlier finding needs attention, the
  head moved, or there's no config on the base branch), and 1 for an
  incomplete pass, earlier findings left unverified, or an error, so an
  incomplete pass shows on the PR's checks.

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

**Both stages share the deadline.** In a full pass the verifier and the
finder run at once, each with the whole pass deadline and the same wrap-up
reserve. Running them together is an optimization, not a requirement. Every
model call goes through the limiter, so with one free lane (another buggrit
process may hold the rest) the stages take turns and each gets less done.
Whichever stage runs out of time reports itself incomplete under the usual
rules: the finder's unsearched scope is searched again (§6), and unverified
findings are checked again.

**Wrap-up.** Each stage investigates until the deadline less a reserve (a
quarter of the time that applies, which is `pass_deadline` or a caller's
sooner deadline: between one and ten minutes, and never more than half). If
it hasn't submitted by then, one more turn tells the model time is up and
forces its terminal tool with what it has. The finder's findings are
posted, and the pass is still marked incomplete. The verifier's verdicts
are applied, and the findings it left out stay unverified. Without this, a
slow model (GLM-5.3 can reason for minutes on one turn) would lose a whole
pass's work at the deadline. Before continuing a transcript (the nudge, or
the wrap-up), the stage drops a trailing assistant turn whose tool calls
have no results because the investigation was canceled mid-tool, which
maestro-llms would reject. An empty turn never reaches the transcript:
toolloop reports it as an empty response and doesn't append it
([maestro-llms#85](https://github.com/SnapdragonPartners/maestro-llms/issues/85),
v0.10.0).

**The model knows the clock.** The opening message states the time budget
and when to submit, and every tool result ends with
`[time: 14 min of 40 min used; submit by 30 min]`. Wall clock, not turns,
is what bounds a pass, and one turn can take seconds or minutes. Because
time pressure could make a model submit before it has covered everything,
`submit_findings` asks for `coverage`, and a `partial` review leaves the
pass incomplete instead of recording a complete pass over partial work.

**Batched tool calls.** The tools return in milliseconds; a pass's time is
the model's reasoning between calls, which dogfooding measured at about a
minute per turn late in a pass. The system prompt therefore tells the model
to request everything it knows it needs in one turn, and why: fewer, fuller
turns leave more of the budget for reasoning.

**Concurrency and lanes.** Every model call goes through a `Limiter` seam in
`internal/llm`. Today it's a semaphore of `max_concurrency`. Later it can
enforce lane limits (for example LunaRoute) or limits shared across runs,
reusing `maestro-llms`' rate-limit middleware where that fits. Retry, timeout
and circuit-breaker policy come from the `maestro-llms` recommended chain.

GitHub calls retry rate limits (429, or 403 with `Retry-After` or an
exhausted quota) for every method, waiting up to 60 seconds. Longer waits
fail the call, so the pass reports itself incomplete. Server errors and
transport failures are retried only for reads, including GraphQL queries.
A mutation that fails that way may still have landed, and the next pass's
replay and repair (§6) finish the job without risking a duplicate.

## 10. Security (proportionate to the trust model)

We take the precautions that are cheap. We deliberately don't defend against
people who already have write access to the repo (§1).

- **The model only reads.** Its tools are read-only and confined to the
  checkout: no `..`, no symlinks followed, and no `.git/` (which can hold
  credentials and isn't needed for review, since diffs come from
  `get_diff`). A path through a symlink is refused, and the refusal names
  the link's target so the model can ask for that path, where the same
  rules apply. That closes links that point into `.git/`, which `os.Root`
  alone would allow. The one exception is a configured instruction file in
  the opening message: a link such as `AGENTS.md -> CLAUDE.md` is followed
  one hop, and its target passes the same path rules. It has no network or GitHub access. Everything posted goes
  through the terminal tool and our renderer.
- **No tokens on disk.** Checkout uses `persist-credentials: false`. The
  GitHub and LLM tokens exist only in buggrit's process environment, never
  in files the tools can read, and they're never echoed in logs or comments.
- **Config comes from the base branch**, so a PR can't redirect the model
  endpoint.
- No PR code is run, and `pull_request_target` is never used.
- Tokens get the minimum permissions listed in §3. The App's Contents
  write access, needed only to resolve threads, could push code; it's
  acceptable because the token lives only in buggrit's process and the
  model's tools can't reach it.

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
   token and the Actions `GITHUB_TOKEN`: `TestLive`, run by
   `.github/workflows/livecheck.yml` when a PR is labeled
   `buggrit-livecheck`.
3. **Agentic finder, end to end:** toolloop with the read-only tools,
   `submit_findings`, posting. First real reviews. Dogfood on this repo.
4. **Verifier and thread lifecycle:** the state machine in §6, rebuttal judging.
5. **Convergence controls:** dedupe (including other
   reviewers' threads), class sweep, `needs_human`, the non-convergence warning,
   summary ledger, gate check.
6. **Large PRs:** splitting into parallel sub-reviews, then reconciling.
   GitHub won't render very large diffs (`ErrDiffTooLarge`); for those the
   diff comes from the checkout (`git diff <merge-base>..<head>`) instead.
7. **Packaging:** release binaries, composite action, reusable workflow.
   Run in morris **alongside Copilot**, compare, then switch Copilot off.
8. **Eval harness:** PRs with planted bugs plus scripted fix and rebuttal
   agents. Measures precision, recall and passes to convergence per model.

**Status (October 2026):** steps 1–4 are done (PRs #2–#5, #9 and #16), and
buggrit reviews its own PRs. Step 4 took two PRs. #9 added the verifier, the
§6 state machine, transitions, and resolve, unresolve and repair. #16 added
the reply-triggered runs and the bot-event concurrency group (§3) to the
dogfood workflow. Reopening a fixed finding that comes back as
a fingerprint match (§7 rule 3) stays with dedupe in step 5. Until then such
a candidate is dropped as already raised. The verifier still reopens a
regression whose evidence files changed. Incremental scope (§7 rule 2) was pulled forward from
step 5, because dogfooding showed full re-reviews drip-feeding new advisory
findings into text earlier passes had already seen. The maestro-llms work step 3 needed has shipped:
terminal tools
([#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53),
v0.8.0), the LunaRoute path
([#54](https://github.com/SnapdragonPartners/maestro-llms/issues/54),
v0.8.1), and the toolloop preallocation fix
([#73](https://github.com/SnapdragonPartners/maestro-llms/issues/73),
v0.9.0). Since then, buggrit is on maestro-llms v0.11.0, which brought the
empty-response and deadline outcomes and the unlimited-iteration setting
(v0.10.0), and per-stage reasoning control (§4,
[#86](https://github.com/SnapdragonPartners/maestro-llms/issues/86)). buggrit
still drops a canceled run's dangling tool-call turn before the wrap-up
(§9); that isn't toolloop's to fix.

## 13. Decisions

- Module path `github.com/SnapdragonPartners/buggrit`.
- Config is JSON (`.github/buggrit.json`), using only the standard library.
- buggrit reads other reviewers' threads and deduplicates against them (§6).
- MVP models: **Qwen and GLM through LunaRoute**. Any `maestro-llms` changes
  needed to support them are tracked separately, in that repo.
- **Dogfooding uses `deepseek-4.1-flash`** through LunaRoute. In October
  2026 trials (dry runs of PR #4, a 126 KB diff, 30-minute deadline), it
  submitted 4 real findings in 24 minutes, at 1–2 minutes per turn. GLM-5.3
  and `glm-5.3-flash` sometimes reasoned for 5–12 minutes on one turn, often
  spending their whole output budget, and never submitted. What matters is
  wall clock to convergence: every extra pass also costs a repair cycle and
  a CI run, so a slower pass that finds everything beats fast drip-feeding.
  Reasoning control is `reasoning_effort` (§4). Dogfooding sets `medium`
  on `models.default`, from the
  [buggrit#8](https://github.com/SnapdragonPartners/buggrit/issues/8) trial
  (two replays of PR #9 per arm): `deepseek-4.1-flash` at `medium` took
  31–40 minutes with the best recall and no false findings; `glm-5.3` at
  `medium` had similar recall in 38–40 minutes with one false P1; `glm-5.3`
  at `low` took 4–7 minutes but found far less, and half of its extras were
  false.
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
- **Identity:** a GitHub App token (`buggrit-bot[bot]`) is the supported setup,
  with the Actions `GITHUB_TOKEN` as a fallback (§3).
- **Terminal tools** use `toolloop`'s terminal-tool support
  ([maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53),
  shipped in v0.8.0). LunaRoute was verified in
  [maestro-llms#54](https://github.com/SnapdragonPartners/maestro-llms/issues/54)
  (v0.8.1).

No open questions remain.
