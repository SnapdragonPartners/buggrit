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
| open (regressed or reopened) | resolved by buggrit | A run killed between the `regressed` marker and the unresolve | Repair unresolves it. If GitHub refuses, it's listed for manual unresolve and counts as open for the gate. |
| any | resolved, `resolvedBy` unknown (null, such as a deleted account) | Can't tell | Not settled and not unresolved. Counts as open for the gate and is listed in the summary, failing toward "not done" like design §6's unknown resolution state. |

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

The `buggrit/findings` check (design §8, built in #32) is the one to make
required. It's `success` when all of these hold:

1. **The current head has a complete pass.** A pass at an earlier head
   doesn't count, so a push after green needs its own pass. An
   incremental pass with nothing in scope (say, a merge from the base
   branch) completes without the finder, in seconds.
2. **No fixed finding is left unverified.** That means a `fixed`,
   unsettled finding whose evidence files changed since its evidence
   baseline (above). Sync computes this without a model, from the pass
   markers and the compare API, by the same selection rules as design §6.
3. **No open, unresolved buggrit finding at or above `fail_on`.**

Otherwise it's `failure`. A push run sets it to `in_progress` when it
starts. **While pass latency is bad**, `gate.accept_incomplete: true`
(default `false`) lets an incomplete pass at the head satisfy condition 1.
It's a temporary switch, not the rule, because "incomplete" means part of
the change wasn't reviewed. The real fix is latency (#19).

`review` remains the "this push's pass has finished" signal for an agent
waiting on `gh pr checks --watch`. The gate is what says "mergeable".

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
writes are idempotent. Every race is covered:

- **A sync run while a push run is in flight at a newer head:** sync looks
  up the current head, finds no complete pass there, and **leaves the gate
  untouched**. It can't turn a running review green.
- **A push run's final write races a sync run:** the push run reads the
  threads again just before writing. A stale read misses a resolution
  that happened after it, so it reports `failure`, which is the safe
  side. Another sync corrects it.
- **One race lands on the unsafe side:** a person *unresolves* a thread
  between the push run's last read and its write. The gate then reports
  green from the stale read until the next sync or push. Unresolving
  isn't part of the agent flow, so this is accepted (settled question 3).

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
  warned about in the job summary. `gate.accept_incomplete` is new.
- **Old caller workflows** (morris, maestro-llms) still send comment and
  review events. The binary treats those events as sync, which keeps
  them cheap and model-free. They still share the push group until the
  caller drops the triggers, so the README snippet and each caller get
  updated in the release that ships this.

## Effect on open issues

- **#34:** closed by this, since nothing comment-triggered reports any
  more.
- **#32** (warn and gate): the gate conditions above replace its
  `needs_human` clause, and the sync mode belongs with it.
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
   filter and permission lookup (#24). Dropping them removes both.
3. **The stale-green race is accepted** (see Concurrency).
4. **Gate condition 1 stays at the current head.**
