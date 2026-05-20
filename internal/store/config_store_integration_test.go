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
	require.NoError(t, s.Delete("temp"))

	_, err := s.Get("temp")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	assert.ErrorIs(t, s.Delete("temp"), domain.ErrNotFound, "second delete must return not-found")
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
