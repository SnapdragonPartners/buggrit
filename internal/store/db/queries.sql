-- name: SchemaVersion :one
SELECT coalesce(max(version), 0)::integer AS version FROM buggrit.schema_version;

-- name: StartRun :exec
INSERT INTO buggrit.run (id, started_at, repo, pr, head_sha, dry_run, label, meta)
VALUES (@id, @started_at, @repo, @pr, @head_sha, @dry_run, @label, @meta)
ON CONFLICT (id) DO NOTHING;

-- name: InsertEvents :exec
-- The arrays are one event per index (set-returning functions in a select
-- list advance together). Events already written, by run and seq, are
-- skipped, so a retried batch is harmless. An empty session or a call of 0
-- is stored as null.
INSERT INTO buggrit.event (run_id, seq, at, kind, session, call, payload)
SELECT @run_id::uuid,
       unnest(@seqs::integer[]),
       unnest(@ats::timestamptz[]),
       unnest(@kinds::text[]),
       nullif(unnest(@sessions::text[]), ''),
       nullif(unnest(@calls::integer[]), 0),
       unnest(@payloads::text[])::jsonb
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: FinishRun :exec
UPDATE buggrit.run
SET ended_at = @ended_at, outcome = @outcome, events_emitted = @events_emitted,
    events_written = @events_written, complete = @complete
WHERE id = @id;

-- name: SaveBenchmark :exec
INSERT INTO buggrit.benchmark (hash, name, repo, base_sha, head_sha, content)
VALUES (@hash, @name, @repo, @base_sha, @head_sha, @content)
ON CONFLICT (hash) DO NOTHING;

-- name: SaveExperiment :one
-- Saving a workflow run again keeps the first row and returns its ID.
WITH ins AS (
    INSERT INTO buggrit.experiment (id, name, workflow_run, attempt, spec, benchmark, replay)
    VALUES (@id, @name, @workflow_run, @attempt, @spec, sqlc.narg('benchmark'), @replay)
    ON CONFLICT (workflow_run, attempt) DO NOTHING
    RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM buggrit.experiment WHERE workflow_run = @workflow_run AND attempt = @attempt
LIMIT 1;

-- name: LoadExperiment :one
SELECT e.id, e.spec, e.replay, e.benchmark, b.content AS benchmark_content
FROM buggrit.experiment e
LEFT JOIN buggrit.benchmark b ON b.hash = e.benchmark
WHERE e.workflow_run = @workflow_run AND e.attempt = @attempt;

-- name: BenchRuns :many
-- The dry runs of a replay PR recorded by one workflow run and attempt,
-- with their finder's totals. group is the identity group's short hash,
-- computed as docs/telemetry.md's comparison query does.
SELECT r.id, r.label, r.started_at, r.ended_at, r.complete, r.outcome,
       jsonb_build_object('models', r.meta->'models', 'mph', r.meta->'mph',
                          'limits', r.meta->'limits', 'review', r.meta->'review') AS identity,
       left(md5(jsonb_build_object('models', r.meta->'models', 'mph', r.meta->'mph',
                                   'limits', r.meta->'limits', 'review', r.meta->'review')::text), 8)::text AS identity_group,
       res.payload AS result,
       st.payload AS stage,
       c.calls::integer AS calls,
       c.input_tokens::bigint AS input_tokens,
       c.output_tokens::bigint AS output_tokens,
       c.reasoning_tokens::bigint AS reasoning_tokens,
       c.cached_tokens::bigint AS cached_tokens,
       t.tools::integer AS tools
FROM buggrit.run r
LEFT JOIN buggrit.event res ON res.run_id = r.id AND res.kind = 'result'
LEFT JOIN buggrit.event st ON st.run_id = r.id AND st.kind = 'stage' AND st.session = 'find'
LEFT JOIN LATERAL (
    SELECT count(*) AS calls,
           coalesce(sum((payload->'response'->'usage'->>'InputTokens')::bigint), 0) AS input_tokens,
           coalesce(sum((payload->'response'->'usage'->>'OutputTokens')::bigint), 0) AS output_tokens,
           coalesce(sum((payload->'response'->'usage'->>'ReasoningTokens')::bigint), 0) AS reasoning_tokens,
           coalesce(sum((payload->'response'->'usage'->>'CacheReadTokens')::bigint), 0) AS cached_tokens
    FROM buggrit.event
    WHERE run_id = r.id AND kind = 'call' AND session = 'find'
) c ON true
LEFT JOIN LATERAL (
    SELECT count(*) AS tools FROM buggrit.event
    WHERE run_id = r.id AND kind = 'tool' AND session = 'find'
) t ON true
WHERE r.repo = @repo AND r.pr = @pr AND r.dry_run
  AND r.meta->'workflow_run'->>'id' = @workflow_run::text
  AND r.meta->'workflow_run'->>'attempt' = @attempt::text
ORDER BY r.label, r.started_at;

-- name: LockRun :exec
-- Serializes judgments of one run: two reports can't both save one.
SELECT id FROM buggrit.run WHERE id = @run_id FOR NO KEY UPDATE;

-- name: RunScored :one
SELECT EXISTS (SELECT 1 FROM buggrit.score WHERE run_id = @run_id) AS scored;

-- name: InsertScore :exec
INSERT INTO buggrit.score (run_id, experiment_id, defect, verdict, finding, reasoning)
VALUES (@run_id, @experiment_id, @defect, @verdict, sqlc.narg('finding'), @reasoning)
ON CONFLICT (run_id, defect) DO NOTHING;

-- name: InsertExtraFinding :exec
INSERT INTO buggrit.extra_finding (run_id, experiment_id, finding, path, line, title, verdict, reasoning)
VALUES (@run_id, @experiment_id, @finding, @path, @line, @title, @verdict, @reasoning)
ON CONFLICT (run_id, finding) DO NOTHING;

-- name: Scores :many
SELECT run_id, defect, verdict, coalesce(finding, 0)::integer AS finding, reasoning,
       coalesce(override_verdict, '')::text AS override_verdict, override_note
FROM buggrit.score
WHERE experiment_id = @experiment_id
ORDER BY run_id, defect;

-- name: ExtraFindings :many
SELECT run_id, finding, verdict, reasoning,
       coalesce(override_verdict, '')::text AS override_verdict, override_note
FROM buggrit.extra_finding
WHERE experiment_id = @experiment_id
ORDER BY run_id, finding;

-- name: BenchPrivileges :one
-- What a benchmark needs to write: the review runs' telemetry, and the
-- harness's experiment and scores.
SELECT has_table_privilege('buggrit.run', 'INSERT') AND has_table_privilege('buggrit.run', 'UPDATE')
         AND has_table_privilege('buggrit.event', 'INSERT') AS telemetry,
       has_table_privilege('buggrit.benchmark', 'INSERT') AND has_table_privilege('buggrit.experiment', 'INSERT')
         AND has_table_privilege('buggrit.score', 'INSERT') AND has_table_privilege('buggrit.extra_finding', 'INSERT') AS scores;
