# buggrit — Design

Status: **draft** · 2026-10-15

buggrit is an automated pull-request reviewer that runs as a GitHub Action,
posts findings as review threads (like Copilot code review), and on every push
checks its earlier findings, then resolves or reopens their threads. Resolving
a thread settles its finding. It talks
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
a finding and push, or decide not to and resolve its thread, and check for
new feedback. The PR should get as close as possible to mergeable with
agents alone. A person acts as the last safeguard, at the merge, where they
see every resolution. That shapes the design:

- Each comment must stand on its own and be actionable without the summary.
- buggrit checks fixes against the code; it doesn't judge decisions. What
  the agent and the person agree to fix or not fix can be decided anywhere,
  not necessarily on GitHub. Resolving a thread settles its finding, and
  buggrit doesn't relitigate it (§6).
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

**Public repositories.** On a public repo, anyone on GitHub can comment, so
"anyone who can comment" would mean everyone. There buggrit counts only the
**team**: commenters whose author association is `OWNER`, `MEMBER` or
`COLLABORATOR`, or, failing that, who have write or admin permission on the
repository (an org member whose membership is private can show up as
`CONTRIBUTOR`, or even `NONE`). Bots count too, since only an admin can
install an App. Everyone else's comments are left out before the finder
reads other reviewers' threads, and are listed in the job summary. No one's
replies reach the verifier (§6), and settling a finding needs a resolution,
which needs write access or authorship of the PR. On a private repo everyone
who can comment is on the team anyway, so buggrit drops nothing there; that
also spares private repos any doubt about how GitHub reports private
members. Comments start no runs, and the workflows skip fork PRs everywhere.
Agents never merging is still the integrity control.

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
  pull_request: opened | synchronize | reopened | ready_for_review    (full pass)
  workflow_dispatch                                                    (manual rerun)
  └─ Action job (composite action → prebuilt buggrit binary)
       └─ buggrit review
            0. Pick mode  a full pass if HEAD has no completed pass yet,
                          or else reconcile only
            1. Load       PR metadata, diff, checkout at HEAD, config (base branch), history
            2. Verify     open findings, fixed findings whose evidence changed (§6);
                          settled findings never
            3. Find       new findings (full pass only; scope from the completed baseline, §7)
                          Steps 2 and 3 run at once, sharing the pass deadline (§9).
            4. Reconcile  dedupe against history, group into classes, apply thresholds
            5. Publish    head-fenced: one review (N threads), replies, resolve/unresolve,
                          repairs, summary
```

**Only a push, or a manual rerun, runs buggrit.** A push runs a full pass. Rerunning that run,
for example after a pass that hit its deadline, does the same at the
current head, and so does a `workflow_dispatch`, though a dispatched run's
check shows on the branch it was dispatched on, not on the PR (§8). Comments and reviews start no runs: what
matters to buggrit is whether a thread is resolved, and resolving one needs
no run. It settles the finding at once (§6), and a repository that blocks
merges on findings does it through GitHub's own conversation-resolution
rule (§8, "Blocking merges"). Resolutions buggrit owns that a killed run
left wrong are repaired by the next push's run (§6, "Repair every pass").

**Every run reviews the PR as it is now.** Every run:
- looks up the PR's **current** head from the API before checking out, never
  the head in the event payload;
- picks its mode from state: a full pass if that
  head has no completed pass, otherwise reconcile only (step 2 plus publish,
  no finder and no review). A reconcile-only run, say after a force-push
  back to a reviewed head, selects findings to verify by the same rules as
  a full pass (§6).

This keeps an invariant: the last run to start in a PR's group saw the
latest head, so a pending run replaced by a newer one never loses work.

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
internal/store/       the optional database: open and check, migrations (§6)
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
`Workspace` is a read-only view of the checkout, and
each `GitHub` is an interface over the calls that step makes, so the engine
can be tested
against fakes and reused by other front ends. A local front end would call
`Review` and print the result instead of publishing; `buggrit review
--dry-run` does that today.

## 3. Trigger, packaging, permissions

```yaml
# .github/workflows/buggrit.yml in a consuming repo
name: buggrit
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
  workflow_dispatch:
    inputs:
      pr: { required: true }
concurrency:
  group: buggrit-${{ github.event.pull_request.number || inputs.pr }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
permissions:
  contents: read
  id-token: write # the optional database ("Database" below)
jobs:
  buggrit:
    uses: SnapdragonPartners/buggrit/.github/workflows/review.yml@v0
    secrets: inherit
    # with: { runs-on: self-hosted, timeout-minutes: 70, version: v0.1.0 }
```

The triggers and the concurrency group stay in the calling workflow,
because a run enters its group before any job starts. Everything else is
in the reusable workflow, `.github/workflows/review.yml`, whose one job:

- is named `review` (§8 "Completion signal"). Through a reusable workflow
  the check shows as `buggrit / review`, the caller's job name first. It
  runs only for pushes and dispatches, and skips drafts and fork PRs. A
  caller that still sends comment or review events gets a skipped run;
- mints the App installation token for the calling repo, and a second
  token that can only read buggrit's contents, for the release download
  ("Packaging" below);
- looks up the PR's head now, not the event's (§2), and checks it out with
  `fetch-depth: 0` and `persist-credentials: false`;
- connects to the optional database when the org has set it up
  ("Database" below);
- runs the composite action (`SnapdragonPartners/buggrit@v0`), passing the
  model key from the `LUNAROUTE_API_KEY` or `BUGGRIT_LLM_API_KEY` secret
  (the config's `api_key_env` names one of them);
- has inputs for `runs-on` (default `ubuntu-latest`), `timeout-minutes`
  (default 70, above the default `pass_deadline`, so buggrit always has time
  to publish) and `version`.

The App token is all it posts with. The reusable workflow declares no
permissions, so its job takes the caller's: `contents: read`, plus
`id-token: write` for the database. A called workflow that asked for
`id-token: write` would fail to start in a caller that doesn't grant it,
and the database is optional. A repo on the `GITHUB_TOKEN` fallback ("Identity"
below) writes its own job around the composite action instead, with
`pull-requests: write`.

- **Packaging:** a composite action (`action.yml`) that downloads a
  prebuilt release binary for the runner's OS and architecture, checks it
  against the release's `checksums.txt`, and runs `buggrit review`. It
  doesn't use a Docker container action, because self-hosted runners may
  have no Docker and macOS runners can't run container actions. The runner
  needs `gh`, `tar` and `sha256sum` or `shasum`, as GitHub's hosted runners
  have. The reusable workflow above wraps it, so a consuming repo needs only
  one `uses:` line.
  - **Releases:** pushing an immutable `vX.Y.Z` tag runs
    `.github/workflows/release.yml`. goreleaser builds linux and darwin, on
    amd64 and arm64, with `main.version` set to the tag, and uploads plain
    archives (`buggrit_<os>_<arch>.tar.gz`) and `checksums.txt`. Each tag's
    run has its own concurrency group, so overlapping tags never drop a
    release. Then every moving major tag (`v0`) moves to the newest published
    final release of its major, read fresh rather than taken from the run's
    tag, with each run reconciling every major. A patch to an older line never moves it backward, and the last
    run to get there sees every release published before it.
  - **Which binary:** the action installs the highest tag on the commit it
    was used at whose release is published with the runner's archive and
    `checksums.txt`, by SemVer precedence (a final release outranks its own
    pre-releases). A tag whose release is still building, or failed, is
    passed over. So `@v0` runs the release `v0` points at, and an
    action pinned by a release's SHA runs that release. A ref with no
    published release on its commit is an error. The `version` input overrides
    this. A reusable workflow can't learn the ref it was called at, so
    `review.yml` uses the action at `@v0`; a caller that pins the workflow
    by SHA should also pass `version` to pin the binary.
  - **Private repo:** while buggrit's repository is private, the action and
    the reusable workflow need its Actions access set to "accessible from
    repositories in the organization", and its release assets need an
    authenticated download. The App must be installed on buggrit for the
    download token. If the repository goes public, nothing else changes.
- **Network:** self-hosted models usually require self-hosted runners, or an
  authenticated public endpoint. A self-hosted runner needs Actions Runner
  v2.327.1 or later, because `actions/create-github-app-token@v3` runs on
  Node 24. Behind an HTTP proxy, set `NODE_USE_ENV_PROXY=1` alongside
  `HTTP_PROXY`/`HTTPS_PROXY`, since v3 dropped its own proxy handling.
- **Database:** when the org variable `BUGGRIT_CLOUDSQL_INSTANCE` is set,
  the job runs the composite action `.github/actions/database`, which
  authenticates to Google Cloud with `google-github-actions/auth`
  through Workload Identity Federation (no key; the variables
  `BUGGRIT_GCP_WIF_PROVIDER` and `BUGGRIT_GCP_SERVICE_ACCOUNT` name the
  provider and the service account). It then starts a pinned,
  checksum-verified Cloud SQL Auth Proxy with IAM database login on
  localhost, and passes its URL to buggrit as `BUGGRIT_DATABASE_URL`. If any
  of those steps fails, including a caller that doesn't grant `id-token:
  write`, the job passes the reason as `BUGGRIT_DATABASE_ERROR` instead,
  and the review runs without the database (§6, "The optional database").
  The job also passes the org variable `BUGGRIT_RATE_LIMITS` through, as
  the environment variable of the same name (§9, "Shared rate limit").
  None of this reaches the engine (§2). The dogfood workflow runs the same
  action from its checkout of `main`, so buggrit's own PRs use the
  database too, and catch a broken action before a release does.
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
  when `main` has no config (as it did until PR #5 added one). It keeps
  building from source after step 7, so it's the canary: each merge runs
  here before a release carries it anywhere else. It has the triggers, the
  concurrency group and the job name shown above.
- **Concurrency:** one group per PR, `buggrit-<pr>`. A push cancels the
  pass in flight (`cancel-in-progress` for `pull_request`); a dispatch
  waits its turn. GitHub keeps one pending run per group, and the last run
  to start sees the latest head (§2). buggrit's own comments trigger
  nothing.
- **Callers** written for the earlier comment and review triggers must
  drop them before the release that ships this model: a comment-triggered
  run in the group can replace a pending push run, and since the reusable
  job skips it, that push's pass would wait for the next push or dispatch.
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

  So the App needs **Pull requests: Read and write** and **Contents: Read and
  write**. The Actions
  `GITHUB_TOKEN` with `contents: write` is untested. Whenever resolving or
  unresolving is refused, buggrit keeps the state in markers, lists the
  affected threads in the pass's review body and the job summary as
  "resolve manually", and carries on.
  A rate limit isn't a refusal (§9): it fails the pass, and the next one
  repairs the thread.
  On a public repository buggrit also reads commenters' permission (§1),
  which GitHub documents as needing only **Metadata: Read**, a permission
  every App has. A refusal there fails the run, so a missing permission
  can't pass for "outside the team".
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
    "warn_after_pass": 5
  },
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
  currently backed by `maestro-llms`' `openaicompat` package. The mapping lives in
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
- `convergence.max_rebuttals` is no longer used, since buggrit doesn't judge
  replies (§6). Strict decoding would make it an error in existing version 1
  configs, so it's accepted, ignored, and warned about in the job log and
  the job summary.
- `identity.bot_login` is the login buggrit's own comments must be authored
  by (§3, "Own comments"). Change it when using `GITHUB_TOKEN`
  (`github-actions[bot]`) or a differently named App.
- `buggrit config validate [path]` checks a file and prints it with defaults
  applied.
- The optional database isn't in the config. It's the org's
  infrastructure rather than the repository's, and its URL can hold a
  credential, so a run gets it as `BUGGRIT_DATABASE_URL` in its environment
  (§3, "Database"; §6, "The optional database"). For the same reason, the
  rate limits shared across repositories are in the org variable
  `BUGGRIT_RATE_LIMITS`, not here (§9, "Shared rate limit").

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
- `get_thread(id)`: buggrit's own comments on a finding, during
  verification

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
  exactly one verdict, that it's `fixed` or `open`, and that a `fixed`
  verdict's `evidence_files` include the anchor file. At wrap-up it accepts verdicts for only some findings; the rest are
  verified on a later run.
- **Files outside the diff:** a finding about a file the diff doesn't touch,
  such as a stale doc, is anchored at the changed line that made it wrong.
- **No submission:** a model that answers in text, or returns an empty
  response (a reasoning model that spent its whole output budget thinking),
  instead of submitting gets one nudge. If it still doesn't submit, or the
  run hits a ceiling, the deadline, a context-window overflow or a model
  error, the pass is reported incomplete with that reason. It never comes back silently empty (§9).
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
(`{"id":"f-7f3a2c","to":"fixed","kind":"fixed","sha":"def456","evidence_files":[…]}`).
A finding's state is the result of replaying buggrit's markers in its
thread, together with the thread's resolution (below). Replay only counts
markers that pass the own-comment check in §3; marker text in anyone else's
comment is ignored. Nobody else's comments carry any state: **replies are
never a state signal.**

- `kind` says why the transition happened, since two kinds lead to the
  same state. buggrit posts two: `fixed` and `regressed` (the defect is
  back). Replay keeps reading the kinds earlier versions posted:
  `reopened`, `rebutted` and `needs_human` mean open (a `needs_human`
  thread was left unresolved, so it stays outstanding until someone
  resolves it), and `dismissed` and `settled` are terminal. Earlier
  versions' `seen` and `left_resolved` fields are ignored.
The **summary** is the body of each pass's review, a *derived view* that
buggrit writes fresh every pass. Only its pass marker is read back
("Review baseline", below), so it can't drift from the threads.

History is read with GraphQL `pullRequest.reviewThreads`, which gives
`isResolved`, `resolvedBy`, `isOutdated`, the anchor, and all comments with
authors and their `authorAssociation`. `resolvedBy` is normalized to the
REST login form like a comment's author (§3, "Own comments"); checked live,
it reports `buggrit-bot[bot]` for a thread buggrit resolved.

Threads started by **other reviewers** (Copilot, humans) are also read.
buggrit doesn't manage them, but it treats them as already-raised issues:
the finder is told about them and deduplication compares against them. That
way buggrit doesn't repeat what another reviewer already said, which matters
while it runs alongside Copilot. On a public repository the comments of
people outside the team are dropped first (§1). When a comment's
association doesn't place its author on the team, buggrit reads the
author's permission once per run (`GET
/repos/{owner}/{repo}/collaborators/{login}/permission`). A 404 (no such
user) leaves the author outside. Any other error fails the run rather than
drop what may be a teammate's thread: a 403 is about the token, not the
author, and a rate limit buggrit can't wait out is temporary.

### The optional database

The threads are the only review state. buggrit also has an optional
Postgres database, for what a PR can't hold: a rate limit shared across
repositories, and run telemetry for comparing models. A review never
depends on it.

- **Optional, inferred:** a run uses the database when
  `BUGGRIT_DATABASE_URL` is set, and not otherwise.
- **Degrades with a warning:** when the database is configured but can't be
  used (no connection, failed authentication, a schema older than the
  binary needs, or a failed setup step in the workflow), the run goes on
  without it. It says why in one comment on the PR, edited in place and
  removed once the database works again or is no longer configured, and in
  the job summary. The warning carries the failure message as is, except
  that the database URL and any password in it are masked (§10). Problems
  with the shared rate limit are database problems too: an invalid
  `BUGGRIT_RATE_LIMITS`, a bucket that can't be set up, and, added at the
  end of the run, a bucket that failed or a lease lost mid-run (§9,
  "Shared rate limit"). The
  comment has a marker with the ID `database` and passes the own-comment
  check like any other (§3). Failing to post it is logged, not fatal.
- **Migrations are an explicit command:** `buggrit db migrate`, run by an
  operator as the role that owns the schemas; `buggrit db status` shows
  where each stands. Review runs never run DDL, and their role has DML
  only. buggrit's tables are in the `buggrit` schema, and maestro-llms'
  rate-limit tables in `bucket`, from `bucket/postgres.Migrations()`. Each
  schema has its own golang-migrate table in it. A run checks each schema
  is at least the version its binary needs, so an older pinned binary keeps
  working after a migration, and `db migrate` leaves a newer schema alone.
- **A seam, not a dependency:** the engine never sees the database. The
  front end opens it and hands the engine small interfaces, such as a
  limiter or a telemetry sink. The SQL stays generic where it can, so
  another backend stays cheap.

### Resolution settles a finding

The thread's resolution is the human-facing lever. Which person or agent
resolved it doesn't matter: an agent with a person's credentials counts the
same as the person (§1). What matters is only **whether buggrit resolved
it**, which `resolvedBy` says. **A thread that's resolved, and not by
buggrit, is settled. That beats every marker.** A settled finding is never
verified or reopened, and dedupe never raises it again (§7 rule 3).
Resolving a thread doesn't claim a fix; it says the finding is accepted as
is, for whatever reason. The merge is where a person confirms they're
comfortable with every resolution, which is why buggrit doesn't relitigate
it.

| Finding's state (from markers) | Thread | Meaning | What buggrit does |
|---|---|---|---|
| open | unresolved | Outstanding | Verified on the next push. Marked `fixed` and resolved if the defect is gone. |
| any | resolved, `resolvedBy` not buggrit | **Settled**, whatever the reason | Never verified, reopened or counted again. |
| fixed | resolved by buggrit | Fixed | Re-verified only if its evidence files change since its baseline (below). Reopened as `regressed` if the defect is back. |
| fixed | unresolved | A run killed before its resolve, or someone unresolved it | Repair resolves it: buggrit owns the resolution of its `fixed` threads, so unresolving one doesn't reopen the finding. |
| open (regressed) | resolved by buggrit | A run killed between the `regressed` marker and the unresolve | Repair unresolves it. If GitHub refuses, it's listed for a person to reopen. |
| any | resolved, `resolvedBy` unknown (null, such as a deleted account) | Can't tell | Not settled, so it's still verified, but never unresolved. An open one is listed for a person, who resolves it again to settle it or reopens it. |

- **Unresolving a settled thread un-settles it.** The finding is open again,
  and the next push verifies it. Replay reads the current resolution, so
  nothing else is needed.
- **buggrit never resolves a thread that's already resolved, or undoes a
  resolution it didn't make**, so `resolvedBy` keeps the settler. Before
  each resolve or unresolve it reads the thread's resolution again, and
  unresolves only a thread still resolved by buggrit. If that read fails,
  the run fails rather than change the thread blind. That covers the normal race: an agent resolves a thread after a
  push run selected it, and the run then judges it fixed. buggrit posts its
  `fixed` reply, but the thread stays the agent's, so the finding stays
  settled, and a later change to its evidence files doesn't re-verify or
  reopen it.

### Finding state machine

```
open ──verify: fixed──────────────► fixed      (reply "fixed in <sha>", resolve)
fixed ── defect is back (re-verify) ─► open    (reply "regressed in <sha>", unresolve)

any state ── resolved by someone other than buggrit ─► settled   (left alone)
settled ── unresolved ─► its marker state again
```

- **What gets verified each pass:**
  - every `open` finding that isn't settled. A finding still open gets no
    reply: the thread already says it's outstanding, and with no dispute
    to answer, a "still open" reply on every push would be noise;
  - every `fixed` finding that isn't settled, where **any of its evidence
    files** changed since its evidence baseline. A `fixed` verdict from the
    verifier must list the files it relied on (`evidence_files`: the anchor
    file plus wherever the fix actually lives, such as a caller that now
    initializes the state). So undoing a fix in another file still triggers
    re-verification;
  - every `fixed` finding none of whose baseline candidates is still in the
    current head's history (force-push or rebase), or whose changes since
    can't be read. Re-verifying is always safe.

  Settled and `dismissed` findings are never verified.

  **Evidence baseline.** A `fixed` finding re-checked because its evidence
  files changed, and found still fixed, posts no reply. So that a later run
  knows the check happened, every pass marker records the `fixed` findings
  it re-verified as still fixed at its head, with the `evidence_files` of
  that verdict: `"verified":{"f-…":["path",…],…}`. The files can differ from
  the earlier ones if the fix has moved. It's posted with every pass,
  complete or not. A reconcile-only run that re-verifies a finding posts a
  short review whose marker carries the same record and no pass status, so
  it isn't counted as a pass. A `fixed` finding's **evidence baseline** is
  its newest record among those whose commit is in the current head's
  history: its latest fix, with the transition marker's evidence files, or
  the head of a later review that listed it, with the files listed there.
  Records from before the latest fix belong to an earlier fix that has
  since regressed. Selection measures "evidence files changed" from that
  commit, over those files.

  **Why a late transition from an older head is safe.** Suppose run A passes
  the publish fence, then head B is pushed before A's mutation lands. Each
  transition type is still handled:
  - **fixed:** recorded with fix SHA A and its evidence files. If B touches
    any of them, the finding is re-verified at B. If B touches none, the
    files the verdict relied on are unchanged, so it still holds at B.
  - **regressed:** never suppresses anything, and the finding is
    re-evaluated by B's run anyway.

  So the fence is an optimization that avoids pointless mutations, and
  correctness comes from the rules above.
- **`fixed` is terminal unless the defect comes back**: then the original
  thread reopens rather than a new thread being opened, so its history stays
  in one place. Earlier versions' `dismissed` is terminal.
- **Repair every pass.** After replay, buggrit compares each finding's state
  with the thread's actual resolution, and does the missing resolve or
  unresolve without posting anything. This repairs runs killed between
  "post marker" and "resolve" or "unresolve". `fixed` threads, and earlier
  versions' `dismissed` ones, should be resolved, and an `open` finding's
  thread that buggrit resolved should be open. A settled thread is left alone, and so is a thread whose resolver
  GitHub doesn't report. Each transition posts its
  reply first and leaves the resolution to repair, so a run killed between
  the two leaves the marker for the next run. GitHub refuses by token, not
  by thread, so after any refusal buggrit reads the thread's current
  resolution. A thread already as wanted counts as neither refused nor
  needing manual action.
- **Never say the same thing twice.** buggrit posts only transitions. A
  finding still open, or still fixed, gets no reply.
- **Outdated anchors** (the code moved) are handled by giving the verifier the
  original snippet (the root comment's `diffHunk`, read with the thread) plus
  the current file, not by trusting line numbers.
- **Verdicts carry evidence.** `submit_verdicts` returns, for each finding:
  the state (`fixed` or `open`), a short reason that's posted as buggrit's
  reply when the state changes, and for `fixed`, a required non-empty
  `evidence_files` list that includes the anchor file. The verifier runs as
  its own agentic stage (§5) with `get_thread` added to the read-only
  tools. It's given every selected finding with its anchor, its text and
  the original snippet, within the 300 KiB opening, like the finder's. A
  finding that doesn't fit is named, for `get_thread`.
- **Replies don't reach the verifier.** It sees buggrit's own comments on a
  finding and nobody else's. The hints a reply could give, such as "fixed
  in abc123" or "the fix is in the caller", are claims the verifier has to
  check against the code anyway, and it has grep and evidence files for
  fixes made elsewhere. A reply could only push toward `fixed`, since an
  author who doesn't want to fix something resolves the thread, so replies
  would add the risk of an agent talking the verifier into a false `fixed`,
  plus an injection path on public repositories. Admitting the PR author's
  replies, for their value as rebuttals, may be reconsidered; what the
  models get as input deserves its own decision record
  ([#35](https://github.com/SnapdragonPartners/buggrit/issues/35)).

### Review baseline
Two SHAs come from the pass markers in the reviews' bodies:
- `attempted_sha`: the head of the most recent pass, complete or not.
- `completed_sha`: the head of the most recent pass that covered its whole
  scope **and** finished publishing.

Each pass's review body ends with a **pass marker**
(`{"id":"pass-3","pass":3,"sha":"…","status":"complete"}`, or `"incomplete"`,
plus `verified` above when the pass re-verified fixed findings).
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
   earlier findings, including fixed and settled ones. A deterministic
   fingerprint (path, category, normalized snippet) does the first pass and a
   short model call catches near-duplicates. What happens to a match depends
   on the earlier finding's state:
   - **settled by resolution, or dismissed:** the candidate is dropped. A
     settled finding is never raised again in any form.
   - **fixed:** the original thread reopens as a regression (§6).
   - **open, or another reviewer's thread:** the candidate is dropped.
4. **No disputes.** buggrit never argues. A finding the author won't fix is
   settled by resolving its thread (§6), with or without a reply, and the
   agent owns that decision, as the morris workflow already says; the
   person who merges sees it. buggrit judges only the code: whether a fix
   is in, and whether a fixed defect has come back. Earlier versions
   rebutted replies up to `max_rebuttals` times and then escalated to
   `needs_human`; that loop, and its wait on a person, are gone.
5. **Advisory findings never block convergence.** Thresholds don't tighten as
   passes add up: P2/P3 findings such as docs drift are still raised on the
   last pass. Instead, only open, unsettled **blocking** findings count toward
   convergence. Open advisory findings are listed but don't keep the
   PR from converging.
6. **A clear final state.** The summary always ends with a status line Claude
   can act on. One of:
   - `converged at abc123: 0 open blocking, 3 open advisory, 5 fixed, 2 settled`
   - `not converged at abc123: 2 open blocking, …`
   - `incomplete at abc123: deadline reached, …`

   `converged` requires zero open, unsettled blocking findings and a
   completed pass at HEAD.
   It also carries a hidden JSON ledger, so the agent can fetch the whole
   state with one `gh api` call.

   Not built yet: today's status line is `complete at abc123: …` or
   `incomplete at abc123: …`, which reports what the pass did rather than
   these totals. The `converged` line and the ledger land in build step 5.
7. **Watch for non-convergence; don't suppress findings.** If a PR reaches
   `warn_after_pass` passes and still isn't converged, buggrit says so in the
   summary and lists the findings that keep reopening or appearing. That points
   a human at the cause (usually a missed class, or a fix that keeps
   regressing) instead of hiding findings.

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

<sub>buggrit f-7f3a2c · blocking · To fix it, push the fix and leave this thread open: buggrit checks on the next push and resolves it. Not fixing it? Resolve the thread; that settles it, and buggrit won't raise it again.</sub>
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

An advisory finding's footer says `advisory, non-blocking` in place of
`blocking`, with the same advice. A comment is capped below GitHub's
65,536-character limit.

### Transition replies

Each transition is a reply in the finding's thread, carrying its marker
(§6). The reply opens with the outcome in bold and the reviewed head:
"**Fixed** at `def4567`." or "**Regressed** at `def4567`, so this thread is
reopened.". The verifier's reason follows. A `fixed` reply also names the
evidence files buggrit will watch. When GitHub refuses a resolve or
unresolve (§3), the pass's review body lists the threads to change by hand,
and so does the job summary; so do open findings whose resolver GitHub
doesn't report (§6). A reconcile-only run posts no pass review. If
a transition it posted has its resolution refused, or it re-verified fixed
findings (§6, "Evidence baseline"), it posts a short review with the list
and the record. A repair that keeps being refused on later runs is listed
only in the job summary, so that review isn't repeated on every run.

**Fingerprint** (§7 rule 3): a hash of the path, the category, and the
anchored lines with whitespace normalized (head lines from the checkout,
deleted lines from the diff), so the same issue on a moved line still
matches. A file-level finding has no anchored lines and uses its normalized
title. The finding ID is `f-` plus the fingerprint's first six hex digits.

### Completion signal
- The job's `review` check run tells the agent a push's pass is finished
  (`gh pr checks --watch`). In a repo on the reusable workflow it shows as
  `buggrit / review` (§3). It says the pass finished, not that the PR is
  mergeable; the PR's open threads say that.

### Blocking merges

buggrit has no merge check of its own, and the review itself is always
`COMMENT`. A repository that wants findings to block merges turns on
**require conversation resolution** in the branch's ruleset. Under the
thread model (§6), an unresolved buggrit thread is a finding nobody has
fixed or settled: buggrit resolves a thread once it verifies the fix, the
author resolves one to settle it, and buggrit unresolves one when a fixed
defect comes back. So "every conversation resolved" means "no open
findings", and GitHub enforces it live: resolving the last thread unblocks
the merge, with no run.

The equivalence holds while buggrit can change resolutions, and needs a
person in three cases:

- **An open thread resolved where buggrit can't see by whom**, or **a
  regressed finding whose unresolve GitHub refused** (§6). The finding is
  open but its thread stays resolved, so the rule passes until a person
  reopens the thread or resolves it again to settle it.
- **A finding buggrit resolves whose resolve GitHub refused**: a fixed
  one, or an earlier version's `dismissed` one (§6). GitHub refuses a
  token without Contents write (§3, "Identity"). The thread stays open, so
  the rule blocks the merge although nothing is left to fix. Resolving it
  by hand clears it.

A pass review lists each such thread for a person. A run that posts no pass
review lists a refusal again only in its job summary (§8, "Transition
replies").

The trade-offs, accepted for the simplicity:

- **Every thread counts**, advisory findings and other reviewers' threads
  included, not only blocking ones. Settling an advisory finding takes one
  resolve.
- **It doesn't know a pass is running**, so a PR can be merged before
  buggrit posts its findings. A repository that wants to wait also makes
  `buggrit / review` a required check. That check fails when the pass is
  incomplete or the run fails, and rerunning that run clears it: a rerun
  keeps the PR event, so its check lands on the PR, and it looks up the
  current head (§2). A dispatch doesn't clear it, since a dispatched run's
  check belongs to the branch it was dispatched on. A skipped run also
  reports as `buggrit / review`, and GitHub counts a skipped check as
  passing. So for a fork PR, which buggrit skips (§1), the required check
  passes with no review; a repository that takes fork PRs decides
  separately how to hold them. A caller that still sends comment or review
  events (§3, "Callers") gets skipped runs too, and has to drop those
  triggers before it relies on the check.

An earlier design had a separate `buggrit/findings` check run, with a
severity threshold, an `in_progress` state while a pass ran, fail-green
rules and model-free sync runs to refresh it. It was dropped before it
shipped: the conversation-resolution rule gives nearly the same signal
without any of that (§13).

### Other outputs
- Job summary (`$GITHUB_STEP_SUMMARY`): per-stage model, tokens, tool
  iterations, wall time. Useful for tuning even when tokens are free.
- `buggrit review` exits 0 for a complete pass, a skipped one
  (the head already has a complete pass and no earlier finding needs
  attention, the head moved, or there's no config on the base branch), and
  1 for an incomplete pass, earlier findings left unverified, or an error,
  so an incomplete pass shows on the PR's checks.

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

**Concurrency.** Every model call goes through a `Limiter` seam in
`internal/llm`: a semaphore of `max_concurrency` for the run, and for a
stage that shares a rate limit across runs (below), a lease on its bucket
too. Retry, timeout and circuit-breaker policy come from the `maestro-llms`
recommended chain.

### Shared rate limit

Each run's `max_concurrency` bounds only that run, but every repository's
runs use the same credentials. LunaRoute's key, for example, allows two
requests in flight across all of them. So when the database is available
(§6, "The optional database"), calls also take a lease on a bucket shared
by every run that uses the same endpoint and key: maestro-llms'
`bucket/postgres`, adapted by `ratelimit.FromBucket` (maestro-llms
ADR-0025).

- **The buckets are org settings.** The org variable
  `BUGGRIT_RATE_LIMITS` names them, a JSON object of
  `{"<bucket>": {"base_url", "api_key_env", "max_concurrency",
  "tokens_per_minute", "max_wait"}}`. It's a variable rather than a secret,
  so it can be read and edited in place. It holds no key: `api_key_env` is
  the name of the variable with the key, as in the config (§4). It's
  decoded as strictly as the config, and needs a `base_url` and at least
  one limit.
- **Each stage finds its bucket** by its effective model's `base_url` and
  `api_key_env` (a trailing slash doesn't matter). Stages on one endpoint
  share a bucket; a stage whose endpoint isn't listed has only the run's
  own limit, which the job log says. Two buckets matching one stage is an
  error.
- **The variable wins.** At startup each run sets every bucket it uses to
  the variable's limits (`Ensure` with overwrite), so a limit is changed by
  editing the variable, and every repository's next run applies it. The
  runtime role's DML is enough for that.
- **Both limits apply.** A call takes a slot of its run's
  `max_concurrency` first, then a lease, so it never holds a shared slot,
  which other repositories are waiting for, while it waits for one of its
  own.
- **A run never hangs on the bucket.** A lease wait is bounded by the
  bucket's `max_wait`, a quarter of `pass_deadline` by default. When the
  wait runs out, or the backend fails, the run uses only its own limit from
  then on, and says why in the database warning (§6). A lease lost while a
  call held it (its renewal failed, so the limit may have been exceeded
  once) goes in the warning too, but the run keeps using the bucket. A call
  whose own context ends while waiting fails as it would without the
  bucket. Leases are renewed for at most the longest stage's
  `request_timeout` plus a minute (at least maestro-llms' default of 15
  minutes), so a call that runs to its timeout keeps its lease.
- **Queueing shows.** The job summary says how long the run waited for
  leases in total, so time spent behind other repositories' runs isn't
  mistaken for model time.

GitHub calls retry rate limits (429; 403 with `Retry-After` or an exhausted
`X-RateLimit-Remaining`; or a 403 whose message says "rate limit", as the
secondary limit's sometimes does alone) for every method, waiting up to 60
seconds. A rate limit is never a refusal: a throttled resolve fails the
pass rather than being listed as "resolve manually" (§3). Longer waits
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
- **On public repositories**, only the team's threads reach the finder (§1
  "Public repositories"), nobody's replies reach the verifier (§6), comments
  start no runs, and fork PRs are skipped. An outsider can't settle a
  finding, since that takes resolving its thread, or put text in front of
  the models.
- **Database credentials** are keyless. A run gets a short-lived Google
  Cloud token through Workload Identity Federation, and the provider's
  attribute condition admits only SnapdragonPartners' repositories running
  buggrit's reusable workflow (`job_workflow_ref` on `review.yml`) or
  buggrit's own dogfood workflow (`workflow_ref` on `buggrit.yml`). A PR to buggrit could change the
  dogfood workflow, but only the team can open one that gets secrets (§1). The
  Auth Proxy logs in as the service account's IAM database user, whose
  role can read and write rows but not change the schema. The token file
  `google-github-actions/auth` writes is in the job's workspace, outside
  the PR checkout the model's tools are confined to.
- Tokens get the minimum permissions listed in §3. The App's Contents
  write access, needed only to resolve threads, could push code; it's
  acceptable because the token lives only in buggrit's process and the
  model's tools can't reach it.

Not in scope: telling an agent apart from the person whose credentials it
uses, or stopping someone with write access from settling findings. The
person who approves the merge, and sees every resolution, is the control
for those.

## 11. Testing

- Unit tests with `llms/testllm` fakes and an `httptest` fake GitHub (REST +
  GraphQL) that holds a PR with threads and replies.
- **State-machine tests** that script multi-pass scenarios:
  - fix; a thread resolved by a person (settled, then unsettled by
    unresolving it); a reply (changes nothing); a resolver GitHub doesn't
    report;
  - a thread settled while the run that judged it fixed was still going;
  - a fixed finding that regresses, and a run killed between its regressed
    reply and its unresolve (repaired by the next run);
  - a re-verification that moves the evidence baseline;
  - force-push;
  - a pass interrupted by the deadline (the baseline must not advance);
  - a publish killed between the marker and the resolve (repaired next pass);
  - the head moving during publish (the fence stops it);
  - a forged marker in a non-buggrit comment (ignored).
- Golden tests for diff→line mapping and comment rendering.
- **Database:** unit tests with fakes cover the degrade paths (the warning
  posted, edited, removed, and failing to post; the shared rate limit's
  backend failing, a lease wait running out, and a lost lease). The
  Postgres tests (migrate, schema checks, the DML-only role, and two runs
  sharing one bucket that never have more than its limit in flight) run against a real server
  when `BUGGRIT_POSTGRES_DSN` is set; CI runs them against a service
  container. The proxy path is checked live by hand.
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
4. **Verifier and thread lifecycle:** the state machine in §6.
5. **Convergence controls:** dedupe (including other reviewers' threads),
   class sweep, the non-convergence warning, the `converged` status line and
   summary ledger (§7 rule 6).
6. **Large PRs:** splitting into parallel sub-reviews, then reconciling.
   GitHub won't render very large diffs (`ErrDiffTooLarge`); for those the
   diff comes from the checkout (`git diff <merge-base>..<head>`) instead.
7. **Packaging:** release binaries, composite action, reusable workflow.
   Run in morris **alongside Copilot**, compare, then switch Copilot off.
8. **Eval harness:** PRs with planted bugs plus scripted agents that fix
   findings or settle them. Measures precision, recall and passes to convergence per model.

**Status (October 2026):** steps 1–4 are done (PRs #2–#5, #9 and #16), and
buggrit reviews its own PRs. Step 7's packaging (#12) was pulled ahead of
steps 5 and 6, so other SnapdragonPartners repos can pilot buggrit on a
pinned release instead of copying the dogfood workflow, which changes with
every merge to `main`. The release workflow, the composite action and the
reusable workflow (§3) are in; running in morris alongside Copilot is
still to come. A wide rollout waits for step 5; the rate limit shared
across repos is in (#29, §9). Step 4 took two PRs. #9 added the verifier, the
§6 state machine, transitions, and resolve, unresolve and repair. #16 added
reply-triggered runs. Dogfooding then showed comment-triggered runs bumping
pending push runs out of the queue, the completion signal going missing
for private org members ([#34](https://github.com/SnapdragonPartners/buggrit/issues/34)),
and a model relitigating the author's decisions, so the next PR replaced
replies and rebuttals with settling by resolution and push-only runs
(§2, §6). The PR after that dropped the planned gate check and the sync
runs that refreshed it: a repository blocks merges on findings by
requiring conversation resolution (§8, "Blocking merges").
Reopening a fixed finding that comes back as
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
v0.9.0). Since then, buggrit is on maestro-llms v0.14.0, which brought the
empty-response and deadline outcomes and the unlimited-iteration setting
(v0.10.0), per-stage reasoning control (§4,
[#86](https://github.com/SnapdragonPartners/maestro-llms/issues/86), v0.11.0),
the context-overflow outcome and the `openaicompat` rename (v0.13.0), and
toolloop compaction (v0.14.0), which buggrit doesn't use yet. buggrit
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
- **The threads are the only review state.** An optional Postgres
  database (October 2026) holds what a PR can't: the cross-repo rate limit
  and telemetry. It's reached through the Cloud SQL Auth Proxy with
  federated credentials and IAM login, uses golang-migrate, sqlc and pgx,
  and degrades to a warning (§6, "The optional database").
- **LunaRoute** is OpenAI-compatible and is reached through
  `openai-compatible` (§4). Small fixes to routes or validation, if needed,
  are made in `maestro-llms`.
- **Local pre-push mode** (`buggrit local --base main`) is out of scope for
  MVP but kept cheap to add: the engine depends only on the `Workspace` and
  `GitHub` interfaces (§2), so a local front end would provide a git-diff
  workspace and print findings instead of posting them. Don't put anything
  Action-specific (env vars, event payload parsing) inside the engine.
- **Trust model** (§1): trusted repos. On a private repo anyone who can
  comment counts as human, including an agent using a person's
  credentials; on a public repo only the team does (association, or else
  write access), and fork PRs are skipped. Agents never merge, and that is
  the integrity control. Security work is limited to
  cheap precautions (§10).
- **Settle by resolution** (October 2026): a thread resolved by anyone but
  buggrit settles its finding, and replies carry no state and don't reach
  the verifier. Only a push (or a dispatch) runs buggrit (§2, §6).
  Unresolving a `fixed` thread doesn't reopen it, since buggrit owns those
  threads.
- **No gate check** (October 2026): buggrit adds no merge check of its
  own. A repository that wants findings to block merges requires
  conversation resolution, which GitHub enforces live, with no run (§8,
  "Blocking merges"). A designed `buggrit/findings` gate, with a severity
  threshold, fail-green rules and model-free sync runs to refresh it, was
  dropped before it shipped: it gave nearly the same signal at a much
  higher cost in moving parts.
- **Identity:** a GitHub App token (`buggrit-bot[bot]`) is the supported setup,
  with the Actions `GITHUB_TOKEN` as a fallback (§3).
- **Terminal tools** use `toolloop`'s terminal-tool support
  ([maestro-llms#53](https://github.com/SnapdragonPartners/maestro-llms/issues/53),
  shipped in v0.8.0). LunaRoute was verified in
  [maestro-llms#54](https://github.com/SnapdragonPartners/maestro-llms/issues/54)
  (v0.8.1).

No open questions remain.
