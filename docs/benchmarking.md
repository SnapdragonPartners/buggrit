# Benchmarking

How to compare models, prompts and harness changes on a real pull request:
what an experiment is, how to run one, and how to read and correct its
results. [Design §12](design.md#benchmarks) ("Benchmarks") is the binding
summary; this is the runbook.

An **experiment** runs several **arms** (variants of buggrit) a few times
each against one **target**, a pull request replayed from two commits.
Each run is a fresh first pass, `buggrit review --dry-run`, on GitHub
Actions, or, for a model the runners can't reach, on your own machine
([Running one locally](#running-one-locally)). Everything except recall comes from the runs' telemetry
([telemetry.md](telemetry.md)). With a **benchmark**, the target's list
of known defects, a **judge** model scores each run's findings against
those defects, and the scores go in the database.

Every arm is judged by design §13's rule: wall clock to convergence
matters more than speed per pass, so the report puts recall and whether
the pass finished next to its latency.

## Arms

An arm is a buggrit ref, a config overlay and a name:

```json
{"name": "glm-medium", "ref": "main", "overlay": {"models": {"find": {"model": "glm-5.3"}}}}
```

- **`ref`** is a branch, tag or SHA in this repository, default `main`.
  Each arm builds its own binary from it. `prepare` resolves the ref to a
  commit once, and every run of the arm builds that commit, so a branch
  that moves mid-experiment doesn't split the arm across two builds.
  Rerunning an experiment resolves it afresh. **A prompt or harness
  variant is a ref:** push a branch with the change and name it. It needs
  no PR and is never merged. There's no runtime switch for prompts.
- **The ref must contain #49** (`216d2fa`), which records each run's
  identity; `prepare` refuses one that doesn't. Rebase an old branch on
  `main`.
- **Keep the branch** (or tag it, `git tag exp/<name> <sha> && git push
  origin exp/<name>`) while its results matter: a deleted branch's commit
  can eventually be garbage-collected, and the identity group's `commit`
  then names nothing you can check out.
- **`overlay`** changes the base config (by default this repository's
  `.github/buggrit.json`) and can set only:
  - `models.default` or `models.<stage>`: `provider`, `model`, `base_url`,
    `api_key_env`, `reasoning_effort`, `max_output_tokens`,
    `request_timeout`;
  - `limits.pass_deadline` and `limits.max_concurrency`.

  Objects merge key by key, so `{"models": {"find": {"model": "glm-5.3"}}}`
  keeps the base's provider, endpoint and key. Changing provider usually
  means setting `base_url` too (`""` for the provider's own). The merged
  config must validate, and must leave telemetry on.
- **`name`** becomes the runs' telemetry label, `<experiment>/<arm>`, so
  rerunning an experiment adds to the same groups.
- **Model keys.** The workflow gives each run `LUNAROUTE_API_KEY`,
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and `GOOGLE_GENAI_API_KEY`
  (organization secrets; the last is for `"provider": "google"`). An
  `api_key_env` naming anything else is empty in Actions.

Compare against the production arm (the dogfood config, `deepseek-4.1-flash`
at `medium`): it was chosen for a reason (design §13), so a new arm has to
beat it, not just do well.

## Benchmarks

A benchmark is a checked-in file, `bench/<name>.json`:

```json
{
  "name": "pr9-e619281",
  "repo": "SnapdragonPartners/buggrit",
  "base": "<full SHA>", "head": "<full SHA>",
  "title": "The PR's title", "body": "The PR's body",
  "defects": [
    {"id": "dry-run-order", "source": "buggrit", "description": "One line on the defect.",
     "locations": [{"path": "cmd/buggrit/review.go", "start": 276, "end": 294}]}
  ]
}
```

- `base` and `head` are full SHAs; the replay is `head` against `base`,
  with the title and body, so the model sees what the original PR's
  reviewers saw. The target can be in another repository the App is
  installed in.
- Each defect has a stable `id`, unique within the benchmark (scores are
  keyed by run and defect ID, and overrides select by it; lowercase
  letters, digits, `.`, `_` and `-`, starting with a letter or digit), who
  raised it, a
  one-line description, and where it is **at `head`**: a line range, or a
  path alone for the whole file. A defect that spans files (a class, or a
  doc that drifted from code) lists every place.
- **The file's sha256 identifies the benchmark** in the database, so
  editing it makes a new benchmark, and scores always refer to the defects
  they were judged against. Add a defect by editing the file; earlier
  scores stay with the old version.
- The first one, `bench/pr9-e619281.json`, is the #8 trial's set: #9 at
  `e619281`, with its 7 known defects and the 3 real extras filed as
  #13–#15.

## The experiment file

```json
{
  "name": "deepseek-vs-glm",
  "benchmark": "bench/pr9-e619281.json",
  "judge": {"provider": "anthropic", "model": "claude-opus-5-5", "api_key_env": "ANTHROPIC_API_KEY",
            "max_output_tokens": 16000},
  "runs": 3,
  "parallel": 2,
  "arms": [
    {"name": "deepseek-medium"},
    {"name": "glm-medium", "overlay": {"models": {"find": {"model": "glm-5.3"}}}}
  ]
}
```

- `name` and the arm names are lowercase letters, digits, `.`, `_` and
  `-`, starting with a letter or digit, without `..` or a trailing `.` or
  `.lock` (the experiment's name is in its replay's branch names). A
  benchmark's `name` follows the same rule.
- `benchmark` is a path in this repository; without one, give a `target`
  instead (`repo`, `base`, `head`, `title`, `body`), and the report covers
  latency and lists the findings side by side, unscored.
- **`judge` is required with a benchmark and has no default**: every
  experiment says what scored it. It's a model block like an arm's. Look
  for model diversity here rather than in the arms: a judge from another
  family (GLM, or Claude) doesn't grade its own kind.
- `runs` per arm, default 3: questions like "without losing recall" need
  enough runs to see past the noise, and two is thin. It must be 1 to 20,
  and arms × runs at most 256, the jobs a workflow's matrix allows. `parallel` is how
  many reviews run at once, default 2, since the LunaRoute key allows 2
  requests in flight. With `BUGGRIT_RATE_LIMITS` set, the shared rate
  limit holds across runs anyway (design §9).
- `base_config` replaces `.github/buggrit.json` as the base, a path in
  this repository.
- There's no `issue`: the report goes to the run's job summary, and
  everything it's built from is in the database, so it can be regenerated
  at any time (below). An experiment recorded while `issue` existed still
  regenerates; a new file naming it is rejected.

Check a file before spending model time on it:

```sh
go run ./cmd/buggrit-bench check my-experiment.json
```

It validates the experiment, reads the benchmark, merges every arm's
overlay and prints what would run. It opens nothing.

## Running one

Dispatch the bench workflow. For a model GitHub's runners can't reach,
`scripts/bench-local.sh` takes the same steps from your machine
([below](#running-one-locally)), with the same database and cleanup.

```sh
gh workflow run bench.yml -f experiment="$(cat my-experiment.json)"
```

Only `.github/workflows/bench.yml` has to be on `main`; it runs the
harness from `main` and each arm from the commit its ref names when the
experiment starts. Dispatch it from `main`
(the default): the database's identity provider admits it only from
there ([database.md](database.md)). One experiment runs at a time. The
jobs:

1. **prepare** validates the experiment, resolves every arm's ref to a
   commit and checks it contains #49, checks the database (it takes the
   workflow's login, is at v4, and lets it write the runs' telemetry and
   the scores: a review that can't use the database carries on without
   it, so a run would record nothing; and no experiment is recorded
   under this workflow run yet), and opens the replay: branches
   `bench/<experiment>/<run>-<attempt>/base` and `.../head` at the
   target's commits, and a **draft** PR between them. `buggrit.yml` skips
   drafts, and CI runs only on PRs to `main`, so nothing else reviews it.
2. **review**, one job per arm × run, at most `parallel` at once: build
   the commit prepare resolved the arm's ref to, write its merged config,
   and run `buggrit review --dry-run` on the replay with the arm's label.
   The database is required; a run that couldn't record would be wasted,
   so each job checks, with the arm's own `buggrit db status`, that its
   login works and the schemas are what the arm's binary needs (an arm's
   branch may add a migration). An incomplete pass is a result, not a
   failure. Each job's timeout is the arm's pass deadline plus 15
   minutes.
3. **report** reads the runs back by workflow run and attempt, has the
   judge score each run not yet scored, records the scores, and writes the
   report to the job summary. Its
   timeout grows with the runs (3 minutes each, plus 10, at most 6
   hours; `check` prints it). Each score is saved as it's made, so a
   report cut off keeps what it judged. A run the
   judge fails on is left unscored and fails the step; rerun the report
   locally (below) to score it.
4. **cleanup** closes the PR and deletes both branches, whatever happened.
   A branch GitHub refuses to delete (a protected one, say) fails the job
   rather than being left behind quietly.
   If the workflow was canceled before cleanup, delete them by hand:
   `git push origin --delete bench/<experiment>/<run>-<attempt>/head
   bench/<experiment>/<run>-<attempt>/base`, and close the PR.

Don't rerun failed jobs: cleanup has already closed the replay. Dispatch
the experiment again; its runs join the same groups as long as each arm's
ref still names the same commit, since a new dispatch resolves the refs
afresh. Rerunning the whole workflow is fine, since a new attempt gets its
own replay.

## Running one locally

GitHub's runners can't reach a model on your machine, such as one in a
local Ollama. `scripts/bench-local.sh` does what the workflow does, from
your machine and against the same database, so its runs, scores and
report are recorded as a dispatched experiment's are:

```sh
scripts/bench-local.sh my-experiment.json
```

It checks the experiment, then runs prepare under a made-up workflow run
ID: the time in Unix seconds, which is far below any real Actions run's
ID. Set `BENCH_RUN_ID` to choose one; prepare refuses an ID that already
recorded an experiment, since report would file the scores under that one. Next it builds each arm's commit (from a worktree of this checkout, so
the build info carries the commit), checks the database for it, and runs
every arm × run **one at a time** from a checkout of the target's head. It
ignores `parallel`, since a local model serves one request at a time. Then
it runs report and cleanup. Cleanup closes the replay however the script
ends after prepare. Ctrl-C during the reviews ends the one in progress,
skips the rest, and still reports; before they start, it just stops. The run ID, the logs and `report.md` are in the directory
it names (`BENCH_DIR`, a new or empty directory, default a temporary
one). Regenerate the report from your checkout with
`BUGGRIT_DATABASE_URL=... go run ./cmd/buggrit-bench report --workflow-run <that ID> --attempt 1 --no-judge`.

It needs `GITHUB_TOKEN` (yours can open the replay), every `api_key_env`
the arms and the judge name, `go`, `gh` and `jq`, and
`BUGGRIT_DATABASE_URL` through an Auth Proxy logged in as the operator
([database.md](database.md#migrations)). The runs outlast the hour an
access token is good for, so give the proxy credentials that refresh,
your application-default ones, not `--token`:

```sh
gcloud auth application-default login   # as the operator
cloud-sql-proxy --auto-iam-authn --port 5433 buggrit:us-east1:buggrit-db &
export BUGGRIT_DATABASE_URL='postgres://dan%40snapdragonpartners.com@127.0.0.1:5433/buggrit?sslmode=disable&options=-c%20role%3Dbuggrit_owner'
```

For a model in **Ollama**:

- **Set the context size in the model.** buggrit can't send Ollama's
  `num_ctx`, and Ollama silently cuts a prompt longer than its small
  default, so recall would measure the cut. The finder's opening on
  `pr9-e619281` alone is about 84k tokens, before any file it reads, so
  use the model's full context if it fits in memory (Qwen3-Coder 30B's
  256k takes about 25 GB of cache on top of its 19 GB of weights). Make a
  variant whose name records the setting:

  ```sh
  printf 'FROM qwen3-coder:30b\nPARAMETER num_ctx 262144\n' > Modelfile
  ollama create qwen3-coder-30b-256k -f Modelfile
  ```

- **The overlay** sets `models.default`, so every stage is local, and
  clears what the base's LunaRoute model sets and the Ollama provider
  can't take: the key, the endpoint (the default is `localhost:11434`),
  and `reasoning_effort` for a model without thinking, which Ollama
  refuses outright (`does not support thinking`). A fresh first pass
  calls only the finder, so overlaying `models.find` alone gives the same
  runs, but the other stages would keep LunaRoute's key, and the script
  requires every key an arm's config names. A local model takes minutes
  per turn, so give the pass a long deadline and each call a long
  timeout, so the run measures recall rather than the deadline:

  ```json
  {"name": "qwen-local", "overlay": {
    "models": {"default": {"provider": "ollama", "model": "qwen3-coder-30b-256k", "base_url": "",
                           "api_key_env": "", "reasoning_effort": "", "request_timeout": "30m"}},
    "limits": {"pass_deadline": "6h"}}}
  ```

What the first local model (Qwen3-Coder 30B at Q4, Ollama 0.33.2, a
128 GB Mac, #67) taught, for the next one:

- **Smoke-test it first** with one `buggrit review --dry-run --config`
  on a small PR. Check that it calls tools sensibly and that its
  request is accepted (a refused `think` fails on the first call). Pick a
  PR whose head buggrit hasn't completed a pass on, or the review skips
  it. Nothing needs the database for this.
- **Check the opening prompt's size against the context.** Each turn's
  log line gives its prompt tokens (`83453 in`). Keep the largest well
  under `num_ctx`: Ollama cuts silently, and the judge sees only
  findings, so a cut run just looks like a weak one. A file read adds
  roughly 6k tokens. A 128k context left too little room on
  `pr9-e619281`, so that run was stopped and rerun at 256k.
- **Budget memory for the cache, not just the weights.** `ollama ps`
  shows what a context costs: 32 GB at 128k and 45 GB at 256k for this
  model. Unload other models first (`ollama stop <name>`).
- **Expect a slow first turn.** Ollama processed the 84k-token opening
  at 120–180 tokens a second, slower as the context grew: 21 minutes.
  Later turns process only what's new: seconds for a short tool result,
  3–4 minutes for a file read. The review log is silent until a turn
  returns, so watch progress in Ollama's own log
  (`~/.ollama/logs/server.log`, its `prompt processing ... progress`
  lines).
- **Compare turns, not wall time, across runs.** Ollama keeps the
  processed prompt between runs, and every run's opening is identical,
  so only the first run's first turn pays for it (later ones took about
  20 seconds). The report's wall times aren't comparable across an
  arm's runs.
- **Read the turn counts before the recall.** A run that submits on its
  first turn without calling a tool ("complete" or "partial") is the
  model stopping early, not a harness failure. This model did that in 2
  of 3 runs.

## Reading the report

- **Arms**: each arm's identity groups, with its ref and the commit
  prepare resolved it to (`main @ 1234567`), the find model and effort,
  the prompt and tools hashes, and the commit the runs report building.
  A report covers one workflow run, in which every run of an arm builds
  one commit with one config, so an arm has one group. Two mean its runs
  differed, which shouldn't happen: find out why before reading its
  numbers. (Across dispatches, a ref that moved in between legitimately
  makes a new group; the Ref column shows which commit each built.)
- **Runs**: per run, wall time, turns, seconds per turn, tool calls per
  turn, tokens, how the pass ended (complete, a stop reason, cut off or
  error), findings (blocking in brackets), and with a benchmark, recall
  and extra findings judged real or false.
- **By arm**: averages within each identity group, side by side. This is
  the comparison. **Never average across groups** ([telemetry.md](telemetry.md#identity)).
- **Known defects**: ✓ found, ½ partial, · missed for each run, with the
  matches and the judge's reasoning under a fold.
- **Extra findings**: every finding that matched no defect, with the
  judge's draft verdict.
- **Findings by run**: each run's findings in full, folded.

Recall gives a defect 1 for found and ½ for partial ("right place, wrong
reading", as #8 scored it), over the benchmark's defect count.

## Reviewing the judge's verdicts

The judge's verdicts are **drafts**. Read the matches and the extras, and
override what's wrong. Connect read-write as the operator
([database.md](database.md#reading-the-data)) and set the override beside
the draft; the draft stays for the record:

```sql
-- A defect the judge missed, or credited wrongly: found, partial or missed.
UPDATE buggrit.score SET override_verdict = 'found', override_note = 'f-3 is the same bug, reported at its caller'
WHERE run_id = '<run uuid>' AND defect = 'dry-run-order';

-- An extra finding: real or false.
UPDATE buggrit.extra_finding SET override_verdict = 'false', override_note = 'incompleteReason already handles it'
WHERE run_id = '<run uuid>' AND finding = 2;
```

The report lists each run's ID under "Findings by run", and numbers its
findings. Then regenerate the report, overrides applied, without judging
anything again:

```sh
BUGGRIT_DATABASE_URL=... go run ./cmd/buggrit-bench report --workflow-run <id> --attempt <n> --no-judge
```

Without `--no-judge` it also scores any run the judge missed, which needs
the judge's key in the environment. `--out report.md` writes a file.

A real extra finding that's still in the code is worth an issue, as
#13–#15 were; if it was in the target, add it to the benchmark as a known
defect for later experiments (which makes a new benchmark version).

## The score tables

Migration 3 adds four tables to the `buggrit` schema, and migration 4 the
experiment's `commits`. Review runs don't use them; `buggrit-bench` needs
v4 (`buggrit db migrate`).

- **`benchmark`**: one row per benchmark file version, keyed by `hash`
  (the sha256 of its bytes), with its name, repository, base and head, and
  the file's content.
- **`experiment`**: one row per experiment run, keyed by `id` and unique
  by `(workflow_run, attempt)`: the experiment file as given (`spec`), its
  `benchmark` hash, its `replay` (repository, PR, branches) and its
  `commits`, each arm's ref mapped to the commit its runs built (`{}` for
  an experiment recorded before v4). Its runs are `buggrit.run` rows of
  that replay PR whose `meta.workflow_run` is that run and attempt.
- **`score`**: per run and defect, the judge's `verdict` (`found`,
  `partial` or `missed`), the matched `finding` (its number among the
  run's kept findings, null when missed), the judge's `reasoning`, and a
  person's `override_verdict` and `override_note`.
- **`extra_finding`**: per run, each finding that matched no defect: its
  number, path, line and title, the judge's `verdict` (`real` or `false`)
  and reasoning, and the override.

The effective verdict is `coalesce(override_verdict, verdict)`.
[telemetry.md](telemetry.md#reading-the-data) has the recall-by-identity-group
query, which averages within a group and a benchmark.

## The comparability rule

Compare **identity groups**, not arms or labels: a group is runs that
share the model settings, `mph`, `limits` and `review` settings, in one
repository and under one label ([telemetry.md](telemetry.md#identity)).
Average within a group and place the groups side by side. Averaging
across groups mixes different things into one number.

## Known gaps

- **The judge can't read the code.** It judges extra findings on their own
  reasoning, which is why its real/false verdicts are drafts.
- **Finder only, one fresh pass per run.** The verifier and dedupe stages
  aren't compared, and passes to convergence aren't measured; design §12
  step 8 extends this.
