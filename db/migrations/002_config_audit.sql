-- Audit trail for namespace lifecycle and ACL changes.
--
-- There is deliberately NO foreign key to config_namespaces. PRAGMA
-- foreign_keys is ON, so a foreign key here would either cascade these rows
-- away when a namespace is deleted -- destroying the record of the deletion,
-- which is the single event most worth keeping -- or block the delete
-- outright. The trail has to outlive the thing it describes.
--
-- Document writes are not recorded and document bodies are never stored.
-- Every write already triggers a full-database upload to R2, so auditing
-- 64KB bodies would inflate both the database and every backup without
-- bound. This table records who changed a namespace's existence or its
-- access rules, not what was in it.
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
    CHECK (action IN ('create', 'acl_change', 'delete'))
);

CREATE INDEX IF NOT EXISTS idx_config_audit_namespace
    ON config_audit(namespace, at);
