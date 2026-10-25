# Proposal: settle findings by resolution; push-only reviews

Status: **proposal**, 2026-10-25. Agreed in outline with Dan; under review.
When it's accepted, the implementation PR folds it into `docs/design.md`
(§1, §2, §3, §4, §6, §7, §8, §10, §11, §12) and deletes this file.

## Why

Three problems showed up on PR #33:

1. **Queue churn.** Push runs and comment runs share one concurrency group
   per PR. GitHub keeps one pending run per group, so a burst of replies
   bumps out a waiting push run. The run that replaces it does the full
   40-minute pass if the head has none, under whatever name its event
   earned.
2. **The completion signal goes missing** (#34). A reply from an org
   member whose membership is private runs as `outside event`, so
   `review` never reports on that head.
3. **Replies are judged by a model.** A "won't fix" or "disagree" goes
   through the verifier, which can rebut it once and then escalate to
   `needs_human` (design §7 rule 4). Dan's development model is the
   opposite. The PR should get as close as possible to mergeable with
   agents alone, and a person acts only as the last safeguard, at the
   merge. What the agent and the person agree to fix or not fix can be
   decided anywhere, not necessarily on GitHub, and buggrit shouldn't
   relitigate it.

The one reason comment-triggered runs exist that still holds is the
**final state**. A PR has to be able to reach a green, mergeable state
without another push.

## The model

### A push runs the model; nothing else does

- **A push** (`pull_request`: opened, synchronize, reopened,
  ready_for_review) runs a **full pass**: the finder (whole PR on pass 1,
  incremental scope after that, design §7 rule 2) and the verifier over
  the findings it selects (below).
- **A sync run** runs no model. It re-reads the threads, recomputes the
  gate and refreshes the summary, in seconds. It's started by
  `workflow_dispatch` with `mode: sync`. The agent dispatches it after
  resolving threads, and a person can do the same from the Actions tab.
- `workflow_dispatch` with `mode: full` (the default) reruns a full pass
  at the current head, for example after a deadline-hit pass.
- **No comment or review triggers.** `pull_request_review_comment` and
  `pull_request_review` are removed. GitHub Actions has no trigger for
  resolving a thread (`pull_request_review_thread` is webhook-only), so
  those triggers never carried the event that matters. That's why sync is
  an explicit dispatch.

### Resolution settles a finding

The thread's resolution is the human-facing lever. Which person or agent
resolved it doesn't matter: an agent with a person's credentials counts
the same as the person, as design §1's trust model already says. What
matters is only **whether buggrit resolved it**. GraphQL's
`reviewThread.resolvedBy` says so. Checked live: it reports
`buggrit-bot[bot]` for threads buggrit resolved (maestro-llms#115,
buggrit#10) and the person's login for everything else.

**A thread that's resolved, and not by buggrit, is settled. That beats
every marker.** A `fixed` or `regressed` marker posted later doesn't
override it, and buggrit never re-resolves a thread that's already
resolved, so `resolvedBy` keeps the settler. This covers the normal flow
racing a pass: an agent resolves a thread after a push run selected it.
If the run then publishes `fixed`, the thread stays settled, and a later
change to its evidence files doesn't re-verify or reopen it.

| Finding's state (from markers) | Thread | Meaning | What buggrit does |
|---|---|---|---|
| open | unresolved | Outstanding | Verified on the next push. Marked `fixed` and resolved if the defect is gone. |
| any | resolved, `resolvedBy` not buggrit | **Settled.** Accepted as is, whatever the reason | Never verified or reopened. Not counted by the gate. Dedupe never raises it again. |
| fixed | resolved by buggrit | Fixed | Re-verified only if its evidence files change since its baseline (below). Reopened as `regressed` if the defect is back. |
| fixed | unresolved | A run killed before its resolve, or someone unresolved it | Repair resolves it (buggrit owns `fixed` threads). |
| open (regressed or reopened) | resolved by buggrit | A run killed between the `regressed` marker and the unresolve | Repair unresolves it. If GitHub refuses, it's listed for manual unresolve in the summary and the gate's, and left out of the gate's count (fail green). |
| any | resolved, `resolvedBy` unknown (null, such as a deleted account) | Can't tell | Not settled, so it's still verified, but never unresolved. Left out of the gate's count and listed for a person (fail green). |

- **Replies are never a state signal.** No rebuttals, no `needs_human`,
  no verifier `dismissed` verdict, and no `max_rebuttals`.
- **Removed: "a bare resolution with no reply while the issue is still
  present reopens the thread"** (design §6). This deliberately reverses
  "an agent resolving a thread doesn't prove the issue is fixed" (§1).
  Resolving no longer claims a fix; it settles the finding. The merge is
  where a person confirms they're comfortable with every resolution.
- **Unresolving a settled thread un-settles it.** It's open again, and the
  next push verifies it. Nothing extra is needed: replay reads the current
  `isResolved`.
- **buggrit owns the resolution of its `fixed` threads.** The repair pass
  (design §6) keeps a `fixed` thread resolved, which covers a run killed
  between the marker and the resolve. Unresolving a `fixed` thread is
  therefore not a reopen signal. Dan agreed to this for now.
- **Every pass records what it re-verified.** A `fixed` finding re-checked
  because its evidence files changed, and found still fixed, posts no
  reply (design §6, "never say the same thing twice"). Without replies,
  nothing would record that the check happened, and sync couldn't tell a
  successful re-verification from one the verifier never reached. So the
  pass marker gains `"verified":{"f-…":["path",…],…}`: each `fixed`
  finding re-verified as still fixed at that pass's head, with the
  `evidence_files` of that verdict. The verifier already has to return
  them for a `fixed` verdict, and the files can differ from the earlier
  ones if the fix has moved. It's posted with every pass, complete or
  not, in the review body that already carries the marker. A `fixed`
  finding's **evidence baseline** is its newest record among those whose
  commit is in the current head's history: its fix commit with the
  transition marker's evidence files, or the head of a pass that listed
  it, with the files listed there. Selection (design §6) and gate
  condition 2 both measure "evidence files changed" from that commit,
  over those files.
- **The verifier posts only on transitions:** `fixed` and `regressed`. A
  finding it judges still open gets no reply. With no rebuttal loop, a
  "still open" reply on every push is noise, and the gate already says the
  finding is outstanding.

### The gate is the merge signal

The `buggrit/findings` check (design §8, built in #32) is the merge
signal. **Whether it blocks is the repository's choice, not buggrit's:** a
repo makes it a required check, or not, and can give its maintainers a
bypass in the ruleset. Once it's required, GitHub blocks on anything but
success, including `in_progress` and a check that doesn't exist yet. So
buggrit's only job is to report honestly: `in_progress` while a pass runs,
then a conclusion.

**Principle (Dan, 2026-10-25): the gate is red only for findings someone
can settle.** A red gate always has a workaround that needs no bypass:
resolve the thread and dispatch sync. When buggrit can't do its job, the
gate **fails green**, and the check's summary says why. Nobody will adopt
a reviewer that blocks merges for its own failures. This reverses design
§8's fail-closed choice ("every gap fails toward not done").

The conclusion:

- **`failure`** if and only if some buggrit finding at or above `fail_on`
  is open, unresolved and unsettled, counted from a fresh read of the
  threads at write time.
- **`success`** otherwise. The summary lists anything that weakened it:
  - the pass at the head was incomplete (deadline), or there's no pass at
    the head because the run failed;
  - `fixed` findings whose evidence changed since their baseline (above)
    and that weren't re-verified at this head. They're selected again on
    the next push;
  - threads whose `resolvedBy` is unknown, or whose unresolve GitHub
    refused. They're left out of the count and listed for a person.

Who writes it:

| When | What |
|---|---|
| A push run starts | Sets the gate on its head to `in_progress` |
| A push run ends, however it ends (complete, incomplete, or an error buggrit caught) | Concludes it from the rule above |
| The binary crashes or the job times out | A final `if: always()` step runs sync, which concludes the gate by the rule above, with "not reviewed: the run failed" in the summary |
| A sync run | Asks the Actions API whether a buggrit push or `mode: full` run for the head is queued or in progress. **If one is**, it leaves the gate alone: that run owns it, and it creates none if the head has none yet. **If none is**, it creates the gate if it's missing, or concludes it if it isn't concluded, by the same rule as every writer. If the head has no pass, the summary says "not reviewed". |

The last row is what recovers every way a push run can end without
concluding the gate:
- it failed before creating the gate (token minting, binary install);
- its runner was lost, so not even `always()` ran. GitHub marks the run
  failed once the runner stops responding;
- it was cancelled by a newer push. That push's run owns the new head,
  and the old head no longer matters.

Even a recovered gate stays red while a blocking finding is open. A
crashed run never turns findings green.

Reading run status is an Actions concern, so it lives in `cmd/buggrit`
(the front end), not the engine (CLAUDE.md, "keep the review engine free
of GitHub Actions specifics"). The job's `GITHUB_TOKEN` needs
`actions: read`.

**Residual gap:** if buggrit can't start at all, for example because its
release is broken, then neither the `always()` step nor sync can run, and a
required gate stays unconcluded. The workaround is the repository's: a
ruleset bypass, or making the check optional until buggrit is fixed.
Nothing inside the workflow can do better without a hosted service.

`gate.accept_incomplete` isn't needed: an incomplete pass never fails the
gate on its own.

`review` remains the "this push's pass has finished" signal for an agent
waiting on `gh pr checks --watch`. The gate is what says "mergeable".

**Known gap: fork PRs.** The workflow skips them, and a fork's
`GITHUB_TOKEN` can't create check runs, so a repo that requires the gate
blocks fork PRs until a maintainer bypasses it. A repo that takes fork PRs
shouldn't require the gate, or should give maintainers a bypass.

### Concurrency

| Run | Group | `cancel-in-progress` |
|---|---|---|
| Push, or `mode: full` dispatch | `buggrit-<pr>` | true for `pull_request` (a push cancels the pass in flight), false for a dispatch |
| `mode: sync` dispatch | `buggrit-sync-<pr>` | true (latest wins) |

Sync never touches the push queue, so the churn can't happen. buggrit's
own comments trigger nothing, so the bot-event concurrency group, the
job-name expression and the `outside event` naming (design §3, §8) all go
away.

Both kinds of run write the gate from a fresh read of the threads, so
writes are idempotent. The races:

- **A sync run while a push run for the head is queued or in flight:**
  the Actions API shows it, so sync leaves the gate alone and creates
  none.
- **A sync run reads "no active run" just before a push run starts:** sync
  may conclude the gate, and then the push run sets it back to
  `in_progress` when it starts. The window is a few seconds, during which
  the gate shows the previous conclusion. Because the conclusion is
  computed from a fresh read of the threads, it can only be green if
  nothing blocking was open.
- **A push run's final write races a sync run:** a stale read can miss a
  resolution that happened after it, so the gate reports red. Another sync
  fixes it, so a workaround exists.
- **A person *unresolves* a thread between a run's last read and its
  write:** the gate reports green from the stale read until the next sync
  or push. That's the fail-green side, accepted under the principle.

**No database is needed.** The queue churn is on GitHub's side, and
correctness rests on fresh reads, not locks. The optional store (#28)
stays for telemetry and the ledger (#26, #30).

### What the agent does (CLAUDE.md step 9 for repos using buggrit)

1. Push, then wait for `review`.
2. For each buggrit thread:
   - **To fix:** fix it, push, and leave the thread open; buggrit
     verifies and resolves it.
   - **Not to fix:** resolve it. A reply is optional, and so is
     discussing it with the person somewhere else.
3. After resolving threads, dispatch sync
   (`gh workflow run buggrit.yml -f pr=<n> -f mode=sync`) and wait for
   the gate.
4. Stop when the gate is green. The person reviews the resolutions and
   merges.

## What goes away

- Reply and review triggers; the bot-event group and job-name expression;
  the `outside event` name and the outsider stop in `cmd/buggrit`
  (closes #34).
- Rebuttals, `needs_human`, the verifier's `dismissed` verdict, the
  `seen` field and "handled" replies, and `convergence.max_rebuttals`.
- The repair rule that unresolves an `open` thread someone resolved.
- **Replies in the verifier's input** (settled question 2). Without replies,
  outsiders on a public repo can't put text in front of the verifier.
  The finder still reads other reviewers' threads, filtered to the team
  (design §1), to avoid repeating them.

## Compatibility

- **Existing markers:** replay keeps reading the old kinds. `rebutted`
  and `reopened` mean open. `needs_human` means open, and since a
  `needs_human` thread is left unresolved it counts until resolved.
  `dismissed` and `settled` stay terminal.
- **Config:** strict decoding would make `convergence.max_rebuttals` an
  error in existing configs. For version 1 it's accepted, ignored, and
  warned about in the job summary.
- **Permissions:** the reusable workflow's job needs `actions: read` on
  `GITHUB_TOKEN` for sync's run check. A caller's `permissions` block caps
  what a reusable workflow gets, so the README snippet and each caller add
  it.
- **Old caller workflows** (morris, maestro-llms) still send comment and
  review events. The binary treats those events as sync, which keeps
  them cheap and model-free. They still share the push group until the
  caller drops the triggers, so the README snippet and each caller get
  updated in the release that ships this.

## Effect on open issues

- **#34:** closed by this, since nothing comment-triggered reports any
  more.
- **#32** (warn and gate): the fail-green gate above replaces its
  fail-closed conditions and its `needs_human` clause, and the sync mode
  belongs with it.
- **#30** (summary, ledger, converged line): the `awaiting human` status
  line goes away.
- **#31** (dedupe): "dismissed" becomes "dismissed or settled by
  resolution".
- **#24's** engine-side reply filtering mostly goes away with replies
  leaving the verifier. Fork-PR skipping stays.

## Questions settled with Dan

1. **Unresolving a `fixed` thread** doesn't reopen it. buggrit owns
   `fixed` threads for now.
2. **Replies leave the verifier's input.** Under this model a reply has no
   authority, so it can only be a hint. The hints that matter most, "fixed
   in abc123" and "the fix is in the caller", are claims the verifier has
   to check against the code anyway, and it has grep and evidence files
   for fixes made elsewhere. A reply can also only push toward `fixed`: an
   author who doesn't want to fix something just resolves the thread. So
   replies add the risk of an agent talking the verifier into a false
   `fixed`, plus an injection path on public repos that needs the team
   filter and permission lookup (#24). Dropping them removes both. Dan:
   leave them out for now, but reconsider admitting the **PR author's**
   replies only, for their value as rebuttals. What the models get as
   input (comments, other reviewers' threads, the PR body) deserves its
   own decision record.
3. **The stale-green race is accepted.** More broadly: if a race or gap
   can't be resolved, fail green (see "The gate is the merge signal").
4. **Gate condition 1 stays at the current head.**
