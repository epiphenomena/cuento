-- name: SelectOne :one
-- Schema-less round-trip: the migration baseline declares no tables yet
-- (p01.3). SELECT 1 exercises the generated query layer without depending on
-- business schema; real queries land alongside their tables in later phases.
SELECT 1 AS one;
