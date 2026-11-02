DROP TABLE buggrit.extra_finding;
DROP TABLE buggrit.score;
DROP TABLE buggrit.experiment;
DROP TABLE buggrit.benchmark;
DELETE FROM buggrit.schema_version WHERE version = 3;
