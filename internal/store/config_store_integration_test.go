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
	require.NoError(t, s.Create(ns))

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
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

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
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}
	require.NoError(t, s.Create(ns))
	assert.ErrorIs(t, s.Create(ns), domain.ErrConflict)
}

func TestConfigStore_Create_InvalidRole_RejectedByCheckConstraint(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	err := s.Create(&domain.ConfigNamespace{
		Name: "bad", ReadRole: "root", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	})
	assert.Error(t, err, "CHECK constraint on read_role must reject unknown roles")
}

func TestConfigStore_UpdateDocument(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "mqtt", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"topic":"/a"}`), UpdatedAt: now, UpdatedBy: "u1", CreatedAt: now,
	}))

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateDocument("mqtt", []byte(`{"topic":"/b"}`), "u2", later))

	got, err := s.Get("mqtt")
	require.NoError(t, err)
	assert.JSONEq(t, `{"topic":"/b"}`, string(got.Document))
	assert.Equal(t, "u2", got.UpdatedBy)
	assert.True(t, got.UpdatedAt.Equal(later))
	assert.True(t, got.CreatedAt.Equal(now), "created_at must not move on document update")
}

func TestConfigStore_UpdateDocument_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	err := s.UpdateDocument("missing", []byte(`{}`), "u", time.Now().UTC())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_UpdateACL(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	later := now.Add(time.Minute)
	require.NoError(t, s.UpdateACL("prefs", "user", "admin", "admin-2", later))

	got, err := s.Get("prefs")
	require.NoError(t, err)
	assert.Equal(t, "user", got.ReadRole)
	assert.Equal(t, "admin", got.WriteRole)
	assert.True(t, got.UpdatedAt.Equal(later))
	assert.Equal(t, "admin-2", got.UpdatedBy, "UpdateACL must record the admin who changed the ACL")
}

func TestConfigStore_UpdateACL_NotFound(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	err := s.UpdateACL("missing", "user", "admin", "admin-1", time.Now().UTC())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConfigStore_Delete(t *testing.T) {
	s := store.NewConfigStore(openTestDB(t))
	now := time.Now().UTC()
	require.NoError(t, s.Create(&domain.ConfigNamespace{
		Name: "temp", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	require.NoError(t, s.Delete("temp", "deleter", now))

	_, err := s.Get("temp")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	assert.ErrorIs(t, s.Delete("temp", "deleter", now), domain.ErrNotFound,
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
			Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
		}))
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
		Document: []byte(`{}`), UpdatedAt: at, UpdatedBy: "creator", CreatedAt: at,
	}))
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
	require.NoError(t, s.UpdateACL("tariffs", "public", "user", "admin-1", later))

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
	require.NoError(t, s.Delete("temp", "admin-9", later))

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

	err := s.UpdateACL("prefs", "root", "user", "admin-1", now.Add(time.Minute))
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
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "creator2", CreatedAt: now,
	})
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
	require.NoError(t, s.UpdateACL("alpha", "public", "user", "admin-1", now.Add(time.Minute)))
	require.NoError(t, s.UpdateACL("alpha", "user", "user", "admin-1", now.Add(2*time.Minute)))

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
