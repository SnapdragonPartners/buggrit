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
