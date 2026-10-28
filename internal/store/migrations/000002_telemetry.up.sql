-- buggrit schema v2: run telemetry (docs/design.md §6, "Telemetry";
-- docs/telemetry.md). Raw data for comparing models and packing, not a
-- forensic record: a run that couldn't write everything says so in its
-- counts and complete flag.

-- One row per review run that captured telemetry.
CREATE TABLE buggrit.run (
    id             uuid PRIMARY KEY,
    started_at     timestamptz NOT NULL,
    ended_at       timestamptz,
    repo           text        NOT NULL,
    pr             integer     NOT NULL,
    head_sha       text        NOT NULL,
    dry_run        boolean     NOT NULL,
    -- label names an experiment or packing variant; '' for none.
    label          text        NOT NULL DEFAULT '',
    -- meta is what the run was: versions, effective models and limits,
    -- the workflow run. outcome is how it ended.
    meta           jsonb       NOT NULL,
    outcome        jsonb,
    -- Set when the run ends. complete means every emitted event was
    -- written; a run cut off before it ended keeps complete false.
    events_emitted integer,
    events_written integer,
    complete       boolean     NOT NULL DEFAULT false
);

CREATE INDEX run_repo_pr ON buggrit.run (repo, pr, started_at);
CREATE INDEX run_started_at ON buggrit.run (started_at);

-- One row per event, in emission order. session names the stage run
-- ('find', 'verify'); call numbers the session's model calls from 1. Both
-- are null for events outside them.
CREATE TABLE buggrit.event (
    run_id  uuid        NOT NULL REFERENCES buggrit.run (id) ON DELETE CASCADE,
    seq     integer     NOT NULL,
    at      timestamptz NOT NULL,
    kind    text        NOT NULL,
    session text,
    call    integer,
    payload jsonb       NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE INDEX event_kind ON buggrit.event (run_id, kind);
CREATE INDEX event_session_call ON buggrit.event (run_id, session, call);

INSERT INTO buggrit.schema_version (version) VALUES (2);
