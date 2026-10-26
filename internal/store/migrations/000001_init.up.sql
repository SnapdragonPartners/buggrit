-- buggrit schema v1 (docs/design.md §6, "The optional database"). Applied by
-- `buggrit db migrate`; review runs never run DDL. Each up file records its
-- version in buggrit.schema_version as its last statement, so a run can
-- check the schema with the DML-only runtime role.

CREATE SCHEMA IF NOT EXISTS buggrit;

CREATE TABLE buggrit.schema_version (
    version    integer PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO buggrit.schema_version (version) VALUES (1);
