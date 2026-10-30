# Run telemetry

With the optional database ([database.md](database.md)), every review run
records what it did: its inputs, what each stage's opening request carried,
every model call and provider attempt, every tool call, and what the pass
decided and published. It's raw data for comparing models and tuning
request packing ([design §6](design.md#6-history-and-state), "Telemetry"),
not a forensic record. A run that can't write every event still finishes,
says how many it lost in the database warning, and is marked incomplete.

## On, off, and labels

- **On whenever the database is configured.** Only an org or repository
  owner can set the database variables, so there's no separate opt-in.
- **Off for one repository** with `"telemetry": {"enabled": false}` in its
  `.github/buggrit.json`. Payloads include the PR description, the diff,
  every file the model read and the model's reasoning, so turn it off for
  any repository whose code shouldn't be in the database.
- **Labels:** set `BUGGRIT_TELEMETRY_LABEL` to name an experiment or a
  packing variant. It's stored in `run.label`. The reusable workflow and
  buggrit's own pass it from the Actions variable of that name, set on the
  repository (or the org, for every repository at once); a caller can't
  add `env` to a reusable workflow's job. A local
  `buggrit review --dry-run` with `BUGGRIT_DATABASE_URL` set records too,
  with `run.dry_run` set, so experiments stay apart from live reviews.

Runs record nothing until they've read the config and opened the
database. A run that stops before that, for example because the head
moved, isn't recorded.

## Schema

Both tables are in the `buggrit` schema (migration 2).

`run` has one row per run:
- `id`: a random UUID, which the job log prints ("recording telemetry as
  run …").
- `repo`, `pr`, `head_sha`, `dry_run` and `label`.
- `meta`: versions (`buggrit`, `maestro_llms`, `go`), `base_ref`, the
  effective `models` per stage (key variable names, never keys),
  `limits`, `review` thresholds, `shared_rate_limit`, `mph` (see
  [Identity](#identity)), and `workflow_run` (`id`, `attempt` and
  `event`) in Actions.
- `outcome`: `exit_code`, plus `status` (the status line), `stopped` or
  `error`.
- `events_emitted` and `events_written`, then `complete`, which is true
  only when the run ended and wrote everything. A run cut off, for example
  by a killed job, keeps `complete` false and has no `ended_at`.

`event` has one row per event, keyed by `(run_id, seq)` in emission order.
`session` is the stage run (`find` or `verify`), and `call` numbers its
model calls from 1. Both are null outside them. `kind` picks the payload;
each row names every key its payload can carry, with the larger nested
shapes under [Shared shapes](#shared-shapes), which the tests check:

| Kind | Session, call | Payload |
|---|---|---|
| `pass` | none | The inputs: `mode` (`full` or `reconcile`), `pass`, `finder` (whether the finder runs this pass), `scope`, `since`, `pr` (`number`, `title`, `body`, `base_ref`, `head_sha`), the whole `diff` (see below), `instructions`, `instruction_files`, `other_threads` (each with `path`, `line`, `author`, `body` and `resolved`), and the `checks` selected for the verifier, each with `id`, `state`, `severity` and `why` |
| `opening` | session, no call | What the opening message carried: its `bytes`; `description_bytes` and `description_truncated`; `instructions`, each instruction file's `path`, `bytes` and whether it was `truncated`, and those `instructions_omitted`; `review_instructions_bytes` and `review_instructions_truncated`; `other_threads` and how many were shown (`other_threads_shown`); `diff`, each file shown, with its `path`, `bytes` and whether it was `truncated` at the per-file cap; the files `deferred` to `get_diff`; and for the verifier, the `checks` shown, with the rest deferred to `get_thread` |
| `message` | session, call | One message the call added to the transcript: `pos` and `message` |
| `call` | session, call | One logical model call: `phase` (`investigate`, `nudge` or `wrapup`), `purpose`, `keep`, `messages`, `tools` (every tool offered), `tool_defs` (definitions first offered, or changed), `tool_choice`, `system` (on the first call), the request's `max_tokens` and `reasoning`, `latency_ms`, `error` and `error_kind`, and `response` (`message`, `stop_reason`, `usage`, `applied_reasoning`) |
| `attempt` | session, call | One provider attempt: `attempt` (maestro-llms' retry number), `provider`, `model`, `purpose`, `latency_ms`, `usage`, `error` and `error_kind` |
| `tool` | session, call | One tool call, under the call whose response asked for it: `iteration`, `id`, `name`, `parameters`, `result`, `is_error`, `stop` (the stage's terminal tool accepted it), `error` and `latency_ms` |
| `stage` | session, last call | How the stage ended: `iterations`, `submitted`, `wrapped_up`, `reason` and the summed `usage` |
| `result` | none | What the pass decided: the findings `submitted` (see below), those reconcile `kept` (each with `id`, `fp`, `path`, `line`, `severity` and `title`), `duplicates`, `below_threshold`, `verdicts` (by finding ID: `state`, `reason`, `evidence`), `transitions` (each with the finding's `id`, its `kind` (`fixed`, `regressed`, `reopened`, `rebutted`, `needs_human`, `dismissed` or `settled`), the `reason` and the thread resolution it `want`s: 1 to resolve, 2 to reopen, absent to keep), `unverified`, `complete`, `done`, `reason`, `summary` and `elapsed_ms`; `skipped` and `skip_reason` for a pass that didn't review; or the pass's `error` |
| `publish` | none | What publishing did: the pass's `review_id`, `threads`, `replies`, `resolved`, `unresolved`, `fell_back`, threads left for a person (`manual`: `id`, `url`, `resolve`, `new`) and the `error`, or `dry_run` |

buggrit's own keys are snake_case, nested ones included; the tests check
that every struct buggrit records gives each field one. Only maestro-llms
types (`message`, `usage`, `tool_choice` and the like) are encoded with
their Go field names: `{"Role": "user", "Content": [{"Type":
"text", "Text": "…"}]}`, and `usage` has `InputTokens`, `OutputTokens`,
`ReasoningTokens`, `CacheReadTokens` and so on. jsonb can't hold NUL, so a
NUL in a payload (a tool result read from a binary file, say) is stored as
U+FFFD.

### Shared shapes

Two nested shapes are too big for the table:

- **The diff** (`pass`'s `diff`): `files`, each with `old_path` (absent
  for an added file), `new_path` (absent for a deleted one), `status`
  (`added`, `modified`, `deleted` or `renamed`), `binary`, `mode_changed`
  and `hunks`. Each hunk has `old_start`, `old_lines`, `new_start`,
  `new_lines` and `lines`, and each line its `text`, its `kind` (`" "`,
  `"+"` or `"-"`) and its `old` and `new` line numbers, where it has them.
- **A finding** (`result`'s `submitted`), as the finder submitted it:
  `path`, `line`, `start_line`, `side`, `severity`, `category`,
  `confidence`, `title`, `problem`, `impact`, `fix`, `suggestion`, `class`
  and the `instances` of its class, each a `path` and `line`.

The tests check that this section and the table name every key, nested
ones included.

### Requests are deltas

A call's `message` events hold only what it added. buggrit never compacts,
so each request is the previous one's first `keep` messages, then its own.
That's usually a plain append. A nudge or a wrap-up can keep one fewer,
because it drops a dangling tool-call turn first. A request is rebuilt by
taking, for each position below its `messages` count, the message written
there by the latest call up to it (`telemetry.Rebuild` in Go):

```sql
-- Messages of one call's request, in order.
-- psql -v run=<uuid> -v session=find -v call=3 -f rebuild.sql
WITH c AS (
    SELECT (payload->>'messages')::int AS n
    FROM buggrit.event
    WHERE run_id = :'run' AND kind = 'call' AND session = :'session' AND call = :'call'
)
SELECT DISTINCT ON ((e.payload->>'pos')::int) (e.payload->>'pos')::int AS pos, e.payload->'message' AS message
FROM buggrit.event e, c
WHERE e.run_id = :'run' AND e.kind = 'message' AND e.session = :'session' AND e.call <= :'call'
  AND (e.payload->>'pos')::int < c.n
ORDER BY (e.payload->>'pos')::int, e.call DESC;
```

The system prompt is on the session's first `call` event, and the tools
offered are on each call, with each definition on the call that first
offered it. The time budget the stage appends to its opening message is
part of message 0.

## Identity

Each run records what it ran: its model/prompt/harness (MPH) identity,
the idea borrowed from maestro's benchmark runner. The model is
`meta.models`. `meta.mph` holds the rest:

- `stages`: for `find` and `verify`, a `prompt_hash` and a `tools_hash`.
  The prompt hash covers the stage's standing instructions: the system
  prompt, the closing instruction of the opening message, the nudge and
  the time-up turn. The tools hash covers every tool definition offered,
  in order: name, description and schema. Both are `sha256:` hashes of
  content, so they move only when that text does.
- `commit` and `modified`: the commit buggrit was built from and whether
  its tree had uncommitted changes, from the Go build info. A build
  without VCS information, such as `go run`, `-buildvcs=false` or a build
  outside a git checkout, records an empty `commit` and `modified` false.

The split says which kind of change a run carries. A prompt edit moves
`prompt_hash`, and a new or changed tool moves `tools_hash`. How the
opening message packs the pass's input is code, so a packing change
moves only `commit`.

**Average within an identity group, and compare groups side by side.**
A group is runs that share the model settings, `mph`, `limits` and
`review` thresholds, in one repository and under one label: a label
names an experiment's variant, and repositories differ in their code
and instructions. Averaging runs across groups mixes different things
into one number, while placing two groups' averages next to each other
is the comparison. Runs from before `mph` was recorded have none and
form their own group.

## Reading the data

Connect read-only, as in [database.md](database.md#reading-the-data),
and pass the run with `-v run=<uuid>`. Every query below runs in a
Postgres test, so it stays in step with the schema.

```sql
-- Recent runs.
SELECT id, started_at, repo, pr, left(head_sha, 7) AS head, label, dry_run,
       meta->'models'->'find'->>'model' AS find_model,
       outcome->>'status' AS status, complete, events_written, events_emitted
FROM buggrit.run
ORDER BY started_at DESC
LIMIT 20;
```

```sql
-- One run's events in order, as JSON lines (psql -At -v run=<uuid> > run.jsonl).
SELECT json_build_object('seq', seq, 'at', at, 'kind', kind, 'session', session, 'call', call, 'payload', payload)
FROM buggrit.event
WHERE run_id = :'run'
ORDER BY seq;
```

```sql
-- Turns per stage and phase, with tool calls per turn.
SELECT c.session, c.payload->>'phase' AS phase, count(*) AS turns,
       round(avg(t.tools), 2) AS tools_per_turn,
       sum((c.payload->'response'->'usage'->>'InputTokens')::int) AS input_tokens,
       sum(c.latency) AS model_ms
FROM (
    SELECT session, call, payload, (payload->>'latency_ms')::bigint AS latency
    FROM buggrit.event WHERE run_id = :'run' AND kind = 'call'
) c
LEFT JOIN LATERAL (
    SELECT count(*) AS tools FROM buggrit.event t
    WHERE t.run_id = :'run' AND t.kind = 'tool' AND t.session = c.session AND t.call = c.call
) t ON true
GROUP BY c.session, c.payload->>'phase'
ORDER BY c.session, phase;
```

```sql
-- Tool reads the opening could have carried: get_diff of a file the
-- opening already showed in full, and any read repeated in the session.
WITH shown AS (
    SELECT session, f->>'path' AS path
    FROM buggrit.event, jsonb_array_elements(payload->'diff') AS f
    WHERE run_id = :'run' AND kind = 'opening' AND NOT coalesce((f->>'truncated')::boolean, false)
), reads AS (
    SELECT session, call, payload->>'name' AS tool, payload->'parameters' AS params,
           payload->'parameters'->>'path' AS path
    FROM buggrit.event
    WHERE run_id = :'run' AND kind = 'tool' AND payload->>'name' IN ('read_file', 'get_diff', 'grep', 'list_dir')
)
SELECT r.session, r.call, r.tool, r.params,
       CASE WHEN r.tool = 'get_diff' AND s.path IS NOT NULL THEN 'shown in the opening' ELSE 'repeated' END AS why
FROM reads r
LEFT JOIN shown s ON s.session = r.session AND s.path = r.path
WHERE (r.tool = 'get_diff' AND s.path IS NOT NULL)
   OR (SELECT count(*) FROM reads o WHERE o.session = r.session AND o.tool = r.tool AND o.params = r.params AND o.call < r.call) > 0
ORDER BY r.session, r.call;
```

```sql
-- Compare identity groups across live runs: wall time, turns, tool
-- calls, tokens, completion and findings, by identity group (see
-- Identity): the find columns only label a group, and group tells apart
-- groups that label alike.
WITH r AS (
    SELECT id, repo, label, meta,
           jsonb_build_object('models', meta->'models', 'mph', meta->'mph',
                              'limits', meta->'limits', 'review', meta->'review') AS identity
    FROM buggrit.run
    WHERE complete AND NOT dry_run
)
SELECT r.repo, left(md5(r.identity::text), 8) AS "group",
       r.identity->'models'->'find'->>'model' AS find_model,
       r.identity->'models'->'find'->>'reasoning_effort' AS effort,
       left(r.identity->'mph'->'stages'->'find'->>'prompt_hash', 15) AS prompt,
       left(r.identity->'mph'->'stages'->'find'->>'tools_hash', 15) AS tools,
       left(r.identity->'mph'->>'commit', 7) AS commit, r.label,
       count(DISTINCT r.id) AS runs,
       round(avg((res.payload->>'elapsed_ms')::bigint) / 1000.0) AS avg_wall_s,
       round(avg(calls.n), 1) AS avg_calls,
       round(avg(tools.n), 1) AS avg_tool_calls,
       round(avg(calls.input_tokens)) AS avg_input_tokens,
       round(avg(calls.output_tokens)) AS avg_output_tokens,
       round(100.0 * avg(((res.payload->>'done')::boolean)::int), 0) AS pct_done,
       round(avg(jsonb_array_length(coalesce(res.payload->'kept', '[]'))), 2) AS avg_findings_kept
FROM r
JOIN buggrit.event res ON res.run_id = r.id AND res.kind = 'result'
LEFT JOIN LATERAL (
    SELECT count(*) AS n,
           sum((payload->'response'->'usage'->>'InputTokens')::int) AS input_tokens,
           sum((payload->'response'->'usage'->>'OutputTokens')::int) AS output_tokens
    FROM buggrit.event WHERE run_id = r.id AND kind = 'call'
) calls ON true
LEFT JOIN LATERAL (
    SELECT count(*) AS n FROM buggrit.event WHERE run_id = r.id AND kind = 'tool'
) tools ON true
GROUP BY r.repo, r.identity, r.label
ORDER BY r.repo, find_model, effort, prompt, tools, commit, r.label, "group";
```

```sql
-- Where a call's time went: provider attempts, and what they don't account
-- for (retry backoff, limiter waits, overhead).
SELECT c.session, c.call, (c.payload->>'latency_ms')::bigint AS call_ms,
       count(a.seq) AS attempts,
       coalesce(sum((a.payload->>'latency_ms')::bigint), 0) AS provider_ms,
       (c.payload->>'latency_ms')::bigint - coalesce(sum((a.payload->>'latency_ms')::bigint), 0) AS other_ms
FROM buggrit.event c
LEFT JOIN buggrit.event a ON a.run_id = c.run_id AND a.kind = 'attempt' AND a.session = c.session AND a.call = c.call
WHERE c.run_id = :'run' AND c.kind = 'call'
GROUP BY c.session, c.call, c.payload
ORDER BY c.session, c.call;
```

## Known gaps

- **Missing and zero look alike** in token usage: maestro-llms' `Usage`
  has no way to say a provider didn't report a count. Failed attempts carry
  no provider request ID, and an attempt's latency doesn't separate the
  network from the model. Both are tracked in
  [maestro-llms#112](https://github.com/SnapdragonPartners/maestro-llms/issues/112).
- **A call's settings are recorded before the stage's own**: a stage's
  `max_output_tokens` and `reasoning_effort` are applied below the
  recorder, so they're in `run.meta.models`, and each response's
  `applied_reasoning` says what the provider got.
- **No retention** yet: rows stay until someone deletes them
  (`DELETE FROM buggrit.run WHERE …` cascades to their events).
