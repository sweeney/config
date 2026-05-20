package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/service"
)

// --- fakes ---

// fakeConfigRepo is an in-memory domain.ConfigRepository suitable for unit
// testing business logic without a database.
type fakeConfigRepo struct {
	mu   sync.Mutex
	data map[string]*domain.ConfigNamespace
}

func newFakeConfigRepo() *fakeConfigRepo {
	return &fakeConfigRepo{data: map[string]*domain.ConfigNamespace{}}
}

func (r *fakeConfigRepo) List() ([]domain.ConfigNamespaceSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.ConfigNamespaceSummary, 0, len(r.data))
	for _, ns := range r.data {
		out = append(out, domain.ConfigNamespaceSummary{
			Name:      ns.Name,
			ReadRole:  ns.ReadRole,
			WriteRole: ns.WriteRole,
			UpdatedAt: ns.UpdatedAt,
			CreatedAt: ns.CreatedAt,
		})
	}
	return out, nil
}

func (r *fakeConfigRepo) GetACL(name string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return "", "", domain.ErrNotFound
	}
	return ns.ReadRole, ns.WriteRole, nil
}

func (r *fakeConfigRepo) Get(name string) (*domain.ConfigNamespace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	copied := *ns
	return &copied, nil
}

func (r *fakeConfigRepo) Create(ns *domain.ConfigNamespace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.data[ns.Name]; exists {
		return domain.ErrConflict
	}
	copied := *ns
	r.data[ns.Name] = &copied
	return nil
}

func (r *fakeConfigRepo) UpdateDocument(name string, document []byte, updatedBy string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return domain.ErrNotFound
	}
	ns.Document = append(ns.Document[:0], document...)
	ns.UpdatedBy = updatedBy
	ns.UpdatedAt = at
	return nil
}

func (r *fakeConfigRepo) UpdateACL(name, readRole, writeRole, updatedBy string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return domain.ErrNotFound
	}
	ns.ReadRole = readRole
	ns.WriteRole = writeRole
	ns.UpdatedBy = updatedBy
	ns.UpdatedAt = at
	return nil
}

func (r *fakeConfigRepo) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[name]; !ok {
		return domain.ErrNotFound
	}
	delete(r.data, name)
	return nil
}

// fakeBackup records TriggerAsync calls so tests can assert on-write
// backup wiring without touching R2.
type fakeBackup struct {
	mu       sync.Mutex
	triggers int
}

func (b *fakeBackup) TriggerAsync() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.triggers++
}
func (b *fakeBackup) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.triggers
}

// --- helpers ---

func newConfigSvc(t *testing.T) (*service.ConfigService, *fakeConfigRepo, *fakeBackup) {
	t.Helper()
	repo := newFakeConfigRepo()
	b := &fakeBackup{}
	return service.NewConfigService(repo, b), repo, b
}

var admin = service.Caller{Sub: "admin-1", Role: domain.ConfigRoleAdmin}
var user = service.Caller{Sub: "user-1", Role: domain.ConfigRoleUser}

// --- CreateNamespace ---

func TestCreate_AdminCanCreate(t *testing.T) {
	svc, _, b := newConfigSvc(t)
	ns, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "houses", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, "houses", ns.Name)
	assert.Equal(t, "admin-1", ns.UpdatedBy)
	assert.Equal(t, 1, b.count())
}

func TestCreate_UserCannotCreate(t *testing.T) {
	svc, _, b := newConfigSvc(t)
	_, err := svc.CreateNamespace(user, service.CreateNamespaceInput{
		Name: "prefs", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`),
	})
	assert.ErrorIs(t, err, service.ErrConfigForbidden)
	assert.Equal(t, 0, b.count(), "failed auth must not trigger a backup")
}

func TestCreate_InvalidName(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	for _, bad := range []string{"", "UPPER", "has space", "!@#", strings.Repeat("a", 65)} {
		t.Run(bad, func(t *testing.T) {
			_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
				Name: bad, ReadRole: "admin", WriteRole: "admin", Document: []byte(`{}`),
			})
			assert.ErrorIs(t, err, service.ErrConfigInvalidName)
		})
	}
}

func TestCreate_InvalidRole(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "x", ReadRole: "root", WriteRole: "admin", Document: []byte(`{}`),
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole)
}

func TestCreate_InvalidDocument(t *testing.T) {
	svc, _, _ := newConfigSvc(t)

	cases := map[string][]byte{
		"empty":    nil,
		"array":    []byte(`[]`),
		"scalar":   []byte(`42`),
		"string":   []byte(`"hi"`),
		"garbage":  []byte(`{not json}`),
		"trailing": []byte(`{} extra`),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
				Name: "n", ReadRole: "admin", WriteRole: "admin", Document: doc,
			})
			assert.ErrorIs(t, err, service.ErrConfigInvalidDocument)
		})
	}
}

func TestCreate_DocumentTooLarge(t *testing.T) {
	svc, _, _ := newConfigSvc(t)

	big := make(map[string]string)
	for i := 0; i < 2_000; i++ {
		big[fmt.Sprintf("k%05d", i)] = strings.Repeat("v", 50)
	}
	doc, _ := json.Marshal(big)

	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "n", ReadRole: "admin", WriteRole: "admin", Document: doc,
	})
	assert.ErrorIs(t, err, service.ErrConfigDocumentTooLarge)
}

// TestCreate_RejectsWriteWithoutRead enforces the ACL invariant that every
// writer must also be a reader — otherwise a write-but-not-read role can
// turn PUT's byte-equality no-op detection into a read oracle for the
// document contents they are not allowed to see.
func TestCreate_RejectsWriteWithoutRead(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "n", ReadRole: "admin", WriteRole: "user", Document: []byte(`{}`),
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole,
		"read_role=admin + write_role=user must be rejected (writers-are-not-readers)")
}

// TestValidateDocument_DeepNestingRejected guards against the stack-
// exhaustion DOS surfaced in the red-team review: 128KB of nested braces
// must not reach the full json.Unmarshal recursion.
func TestValidateDocument_DeepNestingRejected(t *testing.T) {
	svc, _, _ := newConfigSvc(t)

	// Build a ~1000-deep nested object. Well over MaxConfigDocumentDepth
	// (64) but small enough to keep the test fast.
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString(`{"a":`)
	}
	sb.WriteString(`1`)
	for i := 0; i < 1000; i++ {
		sb.WriteString(`}`)
	}

	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "n", ReadRole: "admin", WriteRole: "admin", Document: []byte(sb.String()),
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidDocument,
		"pathological nesting must be rejected before reaching json.Unmarshal")
}

func TestCreate_DuplicateReturnsExists(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	in := service.CreateNamespaceInput{
		Name: "dup", ReadRole: "admin", WriteRole: "admin", Document: []byte(`{}`),
	}
	_, err := svc.CreateNamespace(admin, in)
	require.NoError(t, err)
	_, err = svc.CreateNamespace(admin, in)
	assert.ErrorIs(t, err, service.ErrConfigNamespaceExists)
}

// --- Get and role-gated reads ---

func TestGet_AdminReadsAll(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"a":1}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	got, err := svc.Get(admin, "secret")
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(got.Document))
}

func TestGet_UserBlockedFromAdminNamespaceReturns404(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	_, err := svc.Get(user, "secret")
	assert.ErrorIs(t, err, service.ErrConfigNamespaceNotFound,
		"role-deny on read must surface as not-found to avoid leaking existence")
}

func TestGet_UserCanReadUserNamespace(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{"theme":"dark"}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	got, err := svc.Get(user, "prefs")
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"dark"}`, string(got.Document))
}

func TestGet_NameValidation(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.Get(admin, "BAD NAME")
	assert.ErrorIs(t, err, service.ErrConfigInvalidName)
}

// --- ListVisible ---

func TestListVisible_FiltersByReadRole(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	for _, ns := range []domain.ConfigNamespace{
		{Name: "alpha-admin", ReadRole: "admin", WriteRole: "admin"},
		{Name: "beta-user", ReadRole: "user", WriteRole: "admin"},
		{Name: "gamma-admin", ReadRole: "admin", WriteRole: "admin"},
	} {
		ns := ns
		ns.Document = []byte(`{}`)
		ns.UpdatedAt, ns.CreatedAt = now, now
		ns.UpdatedBy = "u"
		require.NoError(t, repo.Create(&ns))
	}

	// Admin sees all three.
	adminList, err := svc.ListVisible(admin)
	require.NoError(t, err)
	assert.Len(t, adminList, 3)

	// User sees only the user-role ns.
	userList, err := svc.ListVisible(user)
	require.NoError(t, err)
	require.Len(t, userList, 1)
	assert.Equal(t, "beta-user", userList[0].Name)
}

// --- PutDocument ---

func TestPutDocument_Changes(t *testing.T) {
	svc, repo, b := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"a":1}`), UpdatedAt: now, UpdatedBy: "orig", CreatedAt: now,
	}))

	changed, err := svc.PutDocument(admin, "n", []byte(`{"a":2}`))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, b.count())

	got, err := svc.Get(admin, "n")
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":2}`, string(got.Document))
	assert.Equal(t, "admin-1", got.UpdatedBy)
}

func TestPutDocument_NoOpReturnsFalseAndNoBackup(t *testing.T) {
	svc, repo, b := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"a":1}`), UpdatedAt: now, UpdatedBy: "orig", CreatedAt: now,
	}))

	changed, err := svc.PutDocument(admin, "n", []byte(`{ "a" : 1 }`))
	require.NoError(t, err)
	assert.False(t, changed, "byte-identical (after compaction) put should be a no-op")
	assert.Equal(t, 0, b.count(), "no-op put must not trigger a backup")
}

func TestPutDocument_UserBlockedFromAdminWrite(t *testing.T) {
	svc, repo, b := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "shared", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	// User can see (read_role=user) but cannot write → ErrConfigForbidden.
	_, err := svc.PutDocument(user, "shared", []byte(`{"x":1}`))
	assert.ErrorIs(t, err, service.ErrConfigForbidden)
	assert.Equal(t, 0, b.count())
}

func TestPutDocument_UserBlockedFromUnreadableNamespace(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	// User cannot read AND cannot write → 404 (no existence leak)
	_, err := svc.PutDocument(user, "secret", []byte(`{"x":1}`))
	assert.ErrorIs(t, err, service.ErrConfigNamespaceNotFound)
}

func TestPutDocument_InvalidDocument(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	_, err := svc.PutDocument(admin, "n", []byte(`[]`))
	assert.ErrorIs(t, err, service.ErrConfigInvalidDocument)
}

func TestPutDocument_NotFound(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.PutDocument(admin, "missing", []byte(`{}`))
	assert.ErrorIs(t, err, service.ErrConfigNamespaceNotFound)
}

// --- UpdateACL ---

func TestUpdateACL_AdminOnly(t *testing.T) {
	svc, repo, b := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	require.NoError(t, svc.UpdateACL(admin, "n", "user", "admin"))
	assert.Equal(t, 1, b.count())

	got, _ := svc.Get(admin, "n")
	assert.Equal(t, "user", got.ReadRole)

	err := svc.UpdateACL(user, "n", "user", "user")
	assert.ErrorIs(t, err, service.ErrConfigForbidden)
}

func TestUpdateACL_InvalidRole(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	err := svc.UpdateACL(admin, "n", "root", "admin")
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole)
}

// --- Delete ---

func TestDelete_AdminOnly(t *testing.T) {
	svc, repo, b := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	err := svc.Delete(user, "n")
	assert.ErrorIs(t, err, service.ErrConfigForbidden,
		"user must not be able to delete even user-writable namespaces")

	require.NoError(t, svc.Delete(admin, "n"))
	assert.Equal(t, 1, b.count())

	_, err = svc.Get(admin, "n")
	assert.True(t, errors.Is(err, service.ErrConfigNamespaceNotFound))
}
