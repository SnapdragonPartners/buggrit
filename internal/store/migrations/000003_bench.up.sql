-- buggrit schema v3: benchmark scores (docs/design.md §12, "Benchmarks";
-- docs/benchmarking.md). Recall builds up over time here instead of living
-- in issue comments. The judge's verdicts are drafts: a person's override
-- sits beside each, and the effective verdict is the override when set.

-- A benchmark file, by the sha256 of its bytes: editing the file makes a
-- new benchmark, so scores always refer to the defects they were judged
-- against.
CREATE TABLE buggrit.benchmark (
    hash       text        PRIMARY KEY,
    name       text        NOT NULL,
    repo       text        NOT NULL,
    base_sha   text        NOT NULL,
    head_sha   text        NOT NULL,
    content    jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One experiment run: the workflow run (and attempt) that ran its arms,
-- the experiment file as given, and the replay PR its runs reviewed. Its
-- runs are buggrit.run rows of that replay recorded by that workflow run.
CREATE TABLE buggrit.experiment (
    id           uuid        PRIMARY KEY,
    name         text        NOT NULL,
    workflow_run bigint      NOT NULL,
    attempt      integer     NOT NULL,
    spec         jsonb       NOT NULL,
    benchmark    text        REFERENCES buggrit.benchmark (hash),
    replay       jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workflow_run, attempt)
);

-- A run's verdict on one known defect. finding is the 1-based number of
-- the matching finding among the run's kept findings, null when missed.
CREATE TABLE buggrit.score (
    run_id           uuid    NOT NULL REFERENCES buggrit.run (id) ON DELETE CASCADE,
    experiment_id    uuid    NOT NULL REFERENCES buggrit.experiment (id) ON DELETE CASCADE,
    defect           text    NOT NULL,
    verdict          text    NOT NULL CHECK (verdict IN ('found', 'partial', 'missed')),
    finding          integer,
    reasoning        text    NOT NULL DEFAULT '',
    override_verdict text    CHECK (override_verdict IN ('found', 'partial', 'missed')),
    override_note    text    NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, defect)
);

-- A run's finding that matched no known defect, judged real or false.
CREATE TABLE buggrit.extra_finding (
    run_id           uuid    NOT NULL REFERENCES buggrit.run (id) ON DELETE CASCADE,
    experiment_id    uuid    NOT NULL REFERENCES buggrit.experiment (id) ON DELETE CASCADE,
    finding          integer NOT NULL,
    path             text    NOT NULL,
    line             integer NOT NULL,
    title            text    NOT NULL,
    verdict          text    NOT NULL CHECK (verdict IN ('real', 'false')),
    reasoning        text    NOT NULL DEFAULT '',
    override_verdict text    CHECK (override_verdict IN ('real', 'false')),
    override_note    text    NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, finding)
);

CREATE INDEX score_experiment ON buggrit.score (experiment_id);
CREATE INDEX extra_finding_experiment ON buggrit.extra_finding (experiment_id);

INSERT INTO buggrit.schema_version (version) VALUES (3);
