-- buggrit schema v4: the commits an experiment's arms built (#60;
-- docs/benchmarking.md). The bench workflow's prepare job resolves each
-- arm's ref to a commit once, and every run of the arm builds that commit;
-- commits maps each ref, as the experiment file gives it, to that commit's
-- SHA. Experiments recorded before v4 have none.
ALTER TABLE buggrit.experiment ADD COLUMN commits jsonb NOT NULL DEFAULT '{}';

INSERT INTO buggrit.schema_version (version) VALUES (4);
