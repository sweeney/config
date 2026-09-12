//go:build integration

package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sweeney/config/db"
	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/store"
)

func openTestDB(t *testing.T) *db.Database {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestConfigStore_CreateAndGet(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)

	ns := &domain.ConfigNamespace{
		Name:      "houses",
		ReadRole:  "admin",
		WriteRole: "admin",
		Document:  []byte(`{"main":"Rivendell"}`),
		UpdatedAt: now,
		UpdatedBy: "user-123",
		CreatedAt: now,
	}
	require.NoError(t, s.Create(ns, domain.Actor{Sub: "user-123"}))

	got, err := s.Get("houses")
	require.NoError(t, err)
	assert.Equal(t, "houses", got.Name)
	assert.Equal(t, "admin", got.ReadRole)
	assert.Equal(t, "admin", got.WriteRole)
	assert.JSONEq(t, `{"main":"Rivendell"}`, string(got.Document))
	assert.Equal(t, "user-123", got.UpdatedBy)
	assert.True(t, got.UpdatedAt.Equal(now))
	assert.True(t, got.CreatedAt.Equal(now))
}

func TestConfigStore_Get_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	_, err := s.Get("missing")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_GetACL(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "u"}))

	readRole, writeRole, err := s.GetACL("prefs")
	require.NoError(t, err)
	assert.Equal(t, "user", readRole)
	assert.Equal(t, "admin", writeRole)
}

func TestConfigStore_GetACL_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	_, _, err := s.GetACL("missing")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_Create_Duplicate_ReturnsConflict(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	ns := &domain.ConfigNamespace{
		Name: "dup", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}
	require.NoError(t, s.Create(ns, domain.Actor{Sub: "user-123"}))
	assert.ErrorIs(t, s.Create(ns, domain.Actor{Sub: "user-123"}), domain.ErrConflict)
}

func TestConfigStore_Create_InvalidRole_RejectedByCheckConstraint(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	err := s.Create(&domain.ConfigNamespace{
		Name: "bad", ReadRole: "root", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "u"})
	assert.Error(t, err, "CHECK constraint on read_role must reject unknown roles")
}

func TestConfigStore_UpdateDocument(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "mqtt", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"topic":"/a"}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "u1"}))

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateDocument("mqtt", []byte(`{"topic":"/b"}`), domain.Actor{Sub: "u2"}, later))

	got, err := s.Get("mqtt")
	require.NoError(t, err)
	assert.JSONEq(t, `{"topic":"/b"}`, string(got.Document))
	assert.Equal(t, "u2", got.UpdatedBy)
	assert.True(t, got.UpdatedAt.Equal(later))
	assert.True(t, got.CreatedAt.Equal(now), "created_at must not move on document update")
}

func TestConfigStore_UpdateDocument_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	err := s.UpdateDocument("missing", []byte(`{}`), domain.Actor{Sub: "u"}, time.Now().UTC())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_UpdateACL(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "u"}))

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateACL("prefs", "user", "admin", domain.Actor{Sub: "admin-2"}, later, true))

	got, err := s.Get("prefs")
	require.NoError(t, err)
	assert.Equal(t, "user", got.ReadRole)
	assert.Equal(t, "admin", got.WriteRole)
	assert.True(t, got.UpdatedAt.Equal(later))
	assert.Equal(t, "admin-2", got.UpdatedBy, "UpdateACL must record the admin who changed the ACL")
}

func TestConfigStore_UpdateACL_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	err := s.UpdateACL("missing", "user", "admin", domain.Actor{Sub: "admin-1"}, time.Now().UTC(), true)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_Delete(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "temp", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "u"}))
	require.NoError(t, s.Delete("temp", domain.Actor{Sub: "deleter"}, now))

	_, err := s.Get("temp")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	assert.ErrorIs(t, s.Delete("temp", domain.Actor{Sub: "deleter"}, now), domain.ErrNotFound,
		"second delete must return not-found")
}

func TestConfigStore_List_Empty(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	list, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestConfigStore_List_Ordered(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		require.NoError(t, s.Create(&domain.ConfigNamespace{
			Name: name, ReadRole: "admin", WriteRole: "admin",
			Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
		}, domain.Actor{Sub: "u"}))
	}

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name)
	assert.Equal(t, "bravo", list[1].Name)
	assert.Equal(t, "charlie", list[2].Name)
}

// --- audit trail ---

func seedForAudit(t *testing.T, s *store.ConfigStore, name, readRole, writeRole string, at time.Time) {
	t.Helper()
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: name, ReadRole: readRole, WriteRole: writeRole,
		Document: []byte(`{}`), UpdatedAt: at, CreatedAt: at,
	}, domain.Actor{Sub: "creator"}))
}

func TestConfigStore_Create_WritesAuditEntry(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "prefs", "user", "user", now)

	entries, err := s.ListAudit("prefs")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, "prefs", e.Namespace)
	assert.Equal(t, domain.AuditActionCreate, e.Action)
	assert.Equal(t, "creator", e.Actor)
	assert.Equal(t, "user", e.NewReadRole)
	assert.Equal(t, "user", e.NewWriteRole)
	assert.Empty(t, e.OldReadRole, "create has no previous ACL")
	assert.Empty(t, e.OldWriteRole)
	assert.True(t, e.At.Equal(now))
}

func TestConfigStore_UpdateACL_WritesAuditEntryWithTransition(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "tariffs", "user", "user", now)

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateACL("tariffs", "public", "user", domain.Actor{Sub: "admin-1"}, later, true))

	entries, err := s.ListAudit("tariffs")
	require.NoError(t, err)
	require.Len(t, entries, 2, "create then acl_change")

	e := entries[1]
	assert.Equal(t, domain.AuditActionACLChange, e.Action)
	assert.Equal(t, "admin-1", e.Actor)
	assert.Equal(t, "user", e.OldReadRole, "the transition is the point of the record")
	assert.Equal(t, "user", e.OldWriteRole)
	assert.Equal(t, "public", e.NewReadRole)
	assert.Equal(t, "user", e.NewWriteRole)
	assert.True(t, e.At.Equal(later))
}

// TestConfigStore_Delete_AuditSurvivesTheNamespace is the reason config_audit
// carries no foreign key: with PRAGMA foreign_keys=ON a reference would
// either cascade this row away or block the delete, and the deletion is the
// event most worth keeping.
func TestConfigStore_Delete_AuditSurvivesTheNamespace(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "temp", "admin", "admin", now)

	later := now.Add(time.Minute)
	require.NoError(t, s.Delete("temp", domain.Actor{Sub: "admin-9"}, later))

	_, err := s.Get("temp")
	require.ErrorIs(t, err, domain.ErrNotFound, "namespace is gone")

	entries, aerr := s.ListAudit("temp")
	require.NoError(t, aerr)
	require.Len(t, entries, 2)

	e := entries[1]
	assert.Equal(t, domain.AuditActionDelete, e.Action)
	assert.Equal(t, "admin-9", e.Actor)
	assert.Equal(t, "admin", e.OldReadRole)
	assert.Equal(t, "admin", e.OldWriteRole)
	assert.Empty(t, e.NewReadRole, "a deleted namespace has no resulting ACL")
	assert.Empty(t, e.NewWriteRole)
}

// TestConfigStore_UpdateACL_AuditRollsBackWithFailedMutation is the
// atomicity guard, and the reason the store needed transactions at all. The
// audit row is written before the mutation, so a mutation that fails must
// take the audit row with it — otherwise the trail records a change that
// never happened, which is worse than having no trail because it will be
// believed.
//
// The role CHECK constraint is the failure injector: it is a real database
// error on the real schema, not a fault simulated by a fake.
func TestConfigStore_UpdateACL_AuditRollsBackWithFailedMutation(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "prefs", "user", "user", now)

	err := s.UpdateACL("prefs", "root", "user", domain.Actor{Sub: "admin-1"}, now.Add(time.Minute), true)
	require.Error(t, err, "an invalid role must be rejected by the CHECK constraint")

	readRole, writeRole, gerr := s.GetACL("prefs")
	require.NoError(t, gerr)
	assert.Equal(t, "user", readRole, "the ACL must be unchanged")
	assert.Equal(t, "user", writeRole)

	entries, aerr := s.ListAudit("prefs")
	require.NoError(t, aerr)
	assert.Len(t, entries, 1, "only the create — the failed change must leave no audit row")
}

// TestConfigStore_Create_AuditRollsBackOnConflict is the same guard on the
// create path: a duplicate name must not leave an orphan audit entry
// claiming the namespace was created twice.
func TestConfigStore_Create_AuditRollsBackOnConflict(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "dup", "user", "user", now)

	err := s.Create(&domain.ConfigNamespace{
		Name: "dup", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "creator2"})
	require.ErrorIs(t, err, domain.ErrConflict)

	entries, aerr := s.ListAudit("dup")
	require.NoError(t, aerr)
	assert.Len(t, entries, 1, "the rejected create must leave no audit row")
}

func TestConfigStore_ListAudit_ScopedAndOrdered(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "alpha", "user", "user", now)
	seedForAudit(t, s, "beta", "admin", "admin", now)
	require.NoError(t, s.UpdateACL("alpha", "public", "user", domain.Actor{Sub: "admin-1"}, now.Add(time.Minute), true))
	require.NoError(t, s.UpdateACL("alpha", "user", "user", domain.Actor{Sub: "admin-1"}, now.Add(2*time.Minute), true))

	entries, err := s.ListAudit("alpha")
	require.NoError(t, err)
	require.Len(t, entries, 3, "other namespaces must not appear")
	assert.Equal(t, domain.AuditActionCreate, entries[0].Action)
	assert.Equal(t, "public", entries[1].NewReadRole)
	assert.Equal(t, "user", entries[2].NewReadRole)
	assert.True(t, entries[0].At.Before(entries[2].At), "oldest first")
}

func TestConfigStore_ListAudit_UnknownNamespaceIsEmpty(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	entries, err := s.ListAudit("nosuchns")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestConfigStore_UpdateACL_RefusesUnconfirmedPublish: the publish guard is
// evaluated here, against the row the transaction is about to overwrite,
// rather than from an ACL the service read in an earlier round trip. That
// earlier arrangement left a window in which a concurrent revoke made a
// genuine publish look like an edit to something already public.
func TestConfigStore_UpdateACL_RefusesUnconfirmedPublish(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "tariffs", "user", "user", now)

	err := s.UpdateACL("tariffs", "public", "user", domain.Actor{Sub: "admin-1"}, now.Add(time.Minute), false)
	require.ErrorIs(t, err, domain.ErrPublishNotConfirmed)

	readRole, _, gerr := s.GetACL("tariffs")
	require.NoError(t, gerr)
	assert.Equal(t, "user", readRole, "the refused publish must not have written")

	entries, aerr := s.ListAudit("tariffs")
	require.NoError(t, aerr)
	assert.Len(t, entries, 1, "and must leave no audit row")
}

// TestConfigStore_UpdateACL_AlreadyPublicNeedsNoConfirmation: the guard is on
// the transition, so an unrelated edit to a namespace that is already public
// goes through without one.
func TestConfigStore_UpdateACL_AlreadyPublicNeedsNoConfirmation(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "tariffs", "public", "user", now)

	require.NoError(t, s.UpdateACL("tariffs", "public", "admin", domain.Actor{Sub: "admin-1"}, now.Add(time.Minute), false))
	_, writeRole, err := s.GetACL("tariffs")
	require.NoError(t, err)
	assert.Equal(t, "admin", writeRole)
}

// TestConfigStore_Audit_RejectsRolesTheLiveTableCouldNotHold: config_audit is
// the record you consult to reconstruct how a namespace became public, so it
// should not be able to hold a role config_namespaces would refuse.
func TestConfigStore_Audit_RejectsRolesTheLiveTableCouldNotHold(t *testing.T) {
	database := openTestDB(t)
	cases := map[string]string{
		"unknown read role": `INSERT INTO config_audit (namespace, action, new_read, actor, at) VALUES ('n','create','root','a','t')`,
		"public write role": `INSERT INTO config_audit (namespace, action, new_write, actor, at) VALUES ('n','create','public','a','t')`,
		"unknown old role":  `INSERT INTO config_audit (namespace, action, old_read, actor, at) VALUES ('n','delete','root','a','t')`,
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := database.DB().Exec(stmt)
			assert.Error(t, err, "the trail must not accept a role the live table would reject")
		})
	}

	// NULL stays legal: a create has no previous ACL, a delete no resulting one.
	_, err := database.DB().Exec(
		`INSERT INTO config_audit (namespace, action, new_read, new_write, actor, at)
		 VALUES ('n','create','public','user','a','t')`)
	assert.NoError(t, err)
	_, err = database.DB().Exec(
		`INSERT INTO config_audit (namespace, action, old_read, old_write, actor, at)
		 VALUES ('n','delete','admin','admin','a','t')`)
	assert.NoError(t, err)
}

// --- actor username ---

// TestConfigStore_Audit_RecordsActorUsername: the sub stays the stable key,
// the username is the human label as it was at the time. Both are kept.
func TestConfigStore_Audit_RecordsActorUsername(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "tariffs", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "sub-1", Username: "alice"}))
	require.NoError(t, s.UpdateACL("tariffs", "public", "user",
		domain.Actor{Sub: "sub-2", Username: "bob"}, now.Add(time.Minute), true))
	require.NoError(t, s.Delete("tariffs",
		domain.Actor{Sub: "sub-3", Username: "carol"}, now.Add(2*time.Minute)))

	entries, err := s.ListAudit("tariffs")
	require.NoError(t, err)
	require.Len(t, entries, 3)

	assert.Equal(t, "sub-1", entries[0].Actor)
	assert.Equal(t, "alice", entries[0].ActorUsername)
	assert.Equal(t, "sub-2", entries[1].Actor)
	assert.Equal(t, "bob", entries[1].ActorUsername, "the publisher is who you look for first")
	assert.Equal(t, "sub-3", entries[2].Actor)
	assert.Equal(t, "carol", entries[2].ActorUsername)
}

// TestConfigStore_Audit_UsernameIsOptional: rows written before the column
// existed, and any caller without a username claim, leave it empty rather
// than inventing one. Readers fall back to the sub.
func TestConfigStore_Audit_UsernameIsOptional(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "legacy", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "sub-only"}))
	entries, err := s.ListAudit("legacy")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "sub-only", entries[0].Actor)
	assert.Empty(t, entries[0].ActorUsername)
}

// TestConfigStore_Audit_UsernameSurvivesReopen guards the migration itself:
// ALTER TABLE ADD COLUMN re-runs on every boot under common/db's ledger-less
// runner, and must not disturb rows already written.
func TestConfigStore_Audit_UsernameSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.db")

	first, err := db.Open(path)
	require.NoError(t, err)
	s1 := store.NewConfigStore(first)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s1.Create(&domain.ConfigNamespace{
		Name: "tariffs", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "sub-1", Username: "alice"}))
	require.NoError(t, first.Close())

	second, err := db.Open(path)
	require.NoError(t, err, "re-running ADD COLUMN must be harmless")
	defer second.Close()

	entries, err := store.NewConfigStore(second).ListAudit("tariffs")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "alice", entries[0].ActorUsername, "the recorded username survives a reopen")
}

// --- who last wrote the document ---

// TestConfigStore_RecordsWhoLastWroteTheDocument: document writes are not
// audited by design, so this column is the only record of who last edited a
// namespace's contents. It must follow every write path, not just create.
func TestConfigStore_RecordsWhoLastWroteTheDocument(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "sub-1", Username: "alice"}))

	got, err := s.Get("prefs")
	require.NoError(t, err)
	assert.Equal(t, "sub-1", got.UpdatedBy)
	assert.Equal(t, "alice", got.UpdatedByUsername, "create records the writer")

	require.NoError(t, s.UpdateDocument("prefs", []byte(`{"k":1}`),
		domain.Actor{Sub: "sub-2", Username: "bob"}, now.Add(time.Minute)))
	got, err = s.Get("prefs")
	require.NoError(t, err)
	assert.Equal(t, "sub-2", got.UpdatedBy)
	assert.Equal(t, "bob", got.UpdatedByUsername, "a document write moves it on")

	require.NoError(t, s.UpdateACL("prefs", "public", "user",
		domain.Actor{Sub: "sub-3", Username: "carol"}, now.Add(2*time.Minute), true))
	got, err = s.Get("prefs")
	require.NoError(t, err)
	assert.Equal(t, "carol", got.UpdatedByUsername, "so does an ACL change")
}

// TestConfigStore_List_CarriesWhoLastWrote: the list is where this is
// surfaced, so the summary has to carry it.
func TestConfigStore_List_CarriesWhoLastWrote(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "sub-1", Username: "alice"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "sub-1", list[0].UpdatedBy)
	assert.Equal(t, "alice", list[0].UpdatedByUsername)
}

// TestConfigStore_UsernameOptionalOnNamespaces: a caller with no username —
// a service token writing a document — leaves it unset rather than blank.
func TestConfigStore_UsernameOptionalOnNamespaces(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "svc", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, CreatedAt: now,
	}, domain.Actor{Sub: "client-abc"}))

	got, err := s.Get("svc")
	require.NoError(t, err)
	assert.Equal(t, "client-abc", got.UpdatedBy)
	assert.Empty(t, got.UpdatedByUsername)
}

// --- document writes in the trail ---

// TestConfigStore_UpdateDocument_RecordsAnEvent: the trail now answers "was
// this changed, by whom, when" for contents as well as access. It still
// stores no body — every write already ships the whole database to R2, so
// keeping 64KB documents here would inflate both.
func TestConfigStore_UpdateDocument_RecordsAnEvent(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "prefs", "user", "user", now)

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateDocument("prefs", []byte(`{"k":2}`),
		domain.Actor{Sub: "sub-9", Username: "dave"}, later))

	entries, err := s.ListAudit("prefs")
	require.NoError(t, err)
	require.Len(t, entries, 2, "create, then the document write")

	e := entries[1]
	assert.Equal(t, domain.AuditActionDocumentWrite, e.Action)
	assert.Equal(t, "sub-9", e.Actor)
	assert.Equal(t, "dave", e.ActorUsername)
	assert.True(t, e.At.Equal(later))
	assert.Empty(t, e.OldReadRole, "a document write moves no roles")
	assert.Empty(t, e.NewReadRole)
	assert.Empty(t, e.OldWriteRole)
	assert.Empty(t, e.NewWriteRole)
}

// TestConfigStore_UpdateDocument_AuditRollsBackWithFailedWrite: same
// atomicity guarantee as the other write paths. The audit row goes in before
// the mutation, so a write that fails takes it with it.
func TestConfigStore_UpdateDocument_AuditRollsBackWithFailedWrite(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)

	err := s.UpdateDocument("nosuchns", []byte(`{}`), domain.Actor{Sub: "sub-1"}, now)
	require.ErrorIs(t, err, domain.ErrNotFound)

	entries, aerr := s.ListAudit("nosuchns")
	require.NoError(t, aerr)
	assert.Empty(t, entries, "a write that hit nothing must leave no trace")
}

// TestConfigStore_DocumentWrites_InterleaveWithACLChanges: both kinds of
// change share one ordered history, which is the point of recording them
// together rather than in separate places.
func TestConfigStore_DocumentWrites_InterleaveWithACLChanges(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	seedForAudit(t, s, "prefs", "user", "user", now)

	require.NoError(t, s.UpdateDocument("prefs", []byte(`{"k":1}`),
		domain.Actor{Sub: "a"}, now.Add(time.Minute)))
	require.NoError(t, s.UpdateACL("prefs", "public", "user",
		domain.Actor{Sub: "b"}, now.Add(2*time.Minute), true))
	require.NoError(t, s.UpdateDocument("prefs", []byte(`{"k":2}`),
		domain.Actor{Sub: "c"}, now.Add(3*time.Minute)))

	entries, err := s.ListAudit("prefs")
	require.NoError(t, err)
	require.Len(t, entries, 4)
	got := []string{entries[0].Action, entries[1].Action, entries[2].Action, entries[3].Action}
	assert.Equal(t, []string{
		domain.AuditActionCreate,
		domain.AuditActionDocumentWrite,
		domain.AuditActionACLChange,
		domain.AuditActionDocumentWrite,
	}, got, "one history, in the order things happened")
}
