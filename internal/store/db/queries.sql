-- name: SchemaVersion :one
SELECT coalesce(max(version), 0)::integer AS version FROM buggrit.schema_version;
