-- Audit trail for namespace lifecycle, access and content changes.
--
-- There is deliberately NO foreign key to config_namespaces. PRAGMA
-- foreign_keys is ON, so a foreign key here would either cascade these rows
-- away when a namespace is deleted -- destroying the record of the deletion,
-- which is the single event most worth keeping -- or block the delete
-- outright. The trail has to outlive the thing it describes.
--
-- Document bodies are never stored. A document write is recorded as an event
-- -- who wrote, and when -- but not what was written: every write already
-- ships the whole database to R2, so keeping 64KB bodies here would inflate
-- both the database and every backup without bound. To see what a document
-- used to contain, restore the backup from around that timestamp.
--
-- The action CHECK below lists document_write, but only new databases get it
-- from here: CREATE TABLE IF NOT EXISTS means a database that already has the
-- table does not re-run this statement. Those go through widenAuditActions in
-- db/schema.go, which rebuilds the table because SQLite cannot ALTER a CHECK.
-- Same split, and same reason, as the read_role widening in 001_init.sql.
--
CREATE TABLE IF NOT EXISTS config_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    namespace   TEXT NOT NULL,
    action      TEXT NOT NULL,
    old_read    TEXT,
    old_write   TEXT,
    new_read    TEXT,
    new_write   TEXT,
    actor       TEXT NOT NULL,
    at          TEXT NOT NULL,
    CHECK (action IN ('create', 'acl_change', 'delete', 'document_write')),
    -- The role columns are constrained to the same values config_namespaces
    -- allows. This is the artefact you reach for when reconstructing how a
    -- namespace came to be public, so it is the last place that should be
    -- able to hold a role the live table could not. NULL is permitted
    -- throughout: a create has no previous ACL and a delete no resulting one.
    CHECK (old_read  IS NULL OR old_read  IN ('admin', 'user', 'public')),
    CHECK (new_read  IS NULL OR new_read  IN ('admin', 'user', 'public')),
    CHECK (old_write IS NULL OR old_write IN ('admin', 'user')),
    CHECK (new_write IS NULL OR new_write IN ('admin', 'user'))
);

-- (namespace, id) rather than (namespace, at): ListAudit filters on
-- namespace and orders by id, so this lets the index supply the ordering
-- instead of filtering with it and then sorting. id is also the column the
-- ordering deliberately uses, because entries written within the same clock
-- tick must keep their true order.
CREATE INDEX IF NOT EXISTS idx_config_audit_namespace
    ON config_audit(namespace, id);
