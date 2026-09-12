-- Config service schema.
--
-- read_role admits 'public' here so a new database is created at the right
-- shape and never runs the rebuild in db/schema.go. Widening this statement
-- is safe with common/db's ledger-less runner precisely because it is
-- CREATE TABLE IF NOT EXISTS: databases that already have the table do not
-- re-run it, so they still reach the rebuild. write_role stays narrow --
-- nothing is ever anonymously writable.
--
-- One row per namespace. Each namespace is a JSON document with a
-- per-namespace ACL (read_role / write_role). The entire document is
-- replaced on PUT; there are no per-key rows.

CREATE TABLE IF NOT EXISTS config_namespaces (
    name        TEXT PRIMARY KEY,
    read_role   TEXT NOT NULL,
    write_role  TEXT NOT NULL,
    document    TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    CHECK (read_role IN ('admin', 'user', 'public')),
    CHECK (write_role IN ('admin', 'user'))
);

CREATE INDEX IF NOT EXISTS idx_config_namespaces_read_role
    ON config_namespaces(read_role);
