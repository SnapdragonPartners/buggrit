ALTER TABLE buggrit.experiment DROP COLUMN commits;
DELETE FROM buggrit.schema_version WHERE version = 4;
