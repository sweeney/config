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
	"github.com/sweeney/config/internal/testutil"
)

// --- fakes ---

// fakeConfigRepo is an in-memory domain.ConfigRepository suitable for unit
// testing business logic without a database.
type fakeConfigRepo struct {
	mu   sync.Mutex
	data map[string]*domain.ConfigNamespace

	// Audit recording is shared with the other suite's fake so the two
	// cannot disagree about what gets recorded; see internal/testutil.
	audit testutil.AuditLog
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
	r.audit.Record(domain.AuditEntry{
		Namespace:    ns.Name,
		Action:       domain.AuditActionCreate,
		NewReadRole:  ns.ReadRole,
		NewWriteRole: ns.WriteRole,
		Actor:        ns.UpdatedBy,
		At:           ns.CreatedAt,
	})
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
	r.audit.Record(domain.AuditEntry{
		Namespace:    name,
		Action:       domain.AuditActionACLChange,
		OldReadRole:  ns.ReadRole,
		OldWriteRole: ns.WriteRole,
		NewReadRole:  readRole,
		NewWriteRole: writeRole,
		Actor:        updatedBy,
		At:           at,
	})
	ns.ReadRole = readRole
	ns.WriteRole = writeRole
	ns.UpdatedBy = updatedBy
	ns.UpdatedAt = at
	return nil
}

func (r *fakeConfigRepo) Delete(name, deletedBy string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return domain.ErrNotFound
	}
	r.audit.Record(domain.AuditEntry{
		Namespace:    name,
		Action:       domain.AuditActionDelete,
		OldReadRole:  ns.ReadRole,
		OldWriteRole: ns.WriteRole,
		Actor:        deletedBy,
		At:           at,
	})
	delete(r.data, name)
	return nil
}

func (r *fakeConfigRepo) ListAudit(namespace string) ([]domain.AuditEntry, error) {
	return r.audit.List(namespace), nil
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

// anon is the synthetic caller the HTTP layer mints for a request that
// carried no Authorization header at all.
var anon = service.Caller{Sub: "", Role: domain.ConfigRolePublic}

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

	require.NoError(t, svc.UpdateACL(admin, "n", service.UpdateACLInput{ReadRole: "user", WriteRole: "admin"}))
	assert.Equal(t, 1, b.count())

	got, _ := svc.Get(admin, "n")
	assert.Equal(t, "user", got.ReadRole)

	err := svc.UpdateACL(user, "n", service.UpdateACLInput{ReadRole: "user", WriteRole: "user"})
	assert.ErrorIs(t, err, service.ErrConfigForbidden)
}

func TestUpdateACL_InvalidRole(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	err := svc.UpdateACL(admin, "n", service.UpdateACLInput{ReadRole: "root", WriteRole: "admin"})
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

// --- public read role ---

// seedNS is a small helper for the public-role tests, which care about the
// ACL rather than the document.
func seedNS(t *testing.T, repo *fakeConfigRepo, name, readRole, writeRole string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, repo.Create(&domain.ConfigNamespace{
		Name: name, ReadRole: readRole, WriteRole: writeRole,
		Document: []byte(`{"k":1}`), UpdatedAt: now, UpdatedBy: "seed", CreatedAt: now,
	}))
}

// TestGet_RoleMatrix enumerates every (namespace read_role, caller role)
// pair rather than spot-checking. This is the authorization primitive the
// whole feature rests on, and an exhaustive table is cheap at 3x3.
//
// A denied read must surface as not-found, never forbidden, so an anonymous
// caller cannot use the status code to discover which namespaces exist.
func TestGet_RoleMatrix(t *testing.T) {
	callers := map[string]service.Caller{"anonymous": anon, "user": user, "admin": admin}
	allowed := map[string]map[string]bool{
		//  namespace read_role -> caller -> may read
		"public": {"anonymous": true, "user": true, "admin": true},
		"user":   {"anonymous": false, "user": true, "admin": true},
		"admin":  {"anonymous": false, "user": false, "admin": true},
	}

	for nsRole, byCaller := range allowed {
		for callerName, want := range byCaller {
			t.Run(nsRole+"/"+callerName, func(t *testing.T) {
				svc, repo, _ := newConfigSvc(t)
				writeRole := nsRole
				if nsRole == "public" {
					writeRole = "user" // public is never a write role
				}
				seedNS(t, repo, "ns", nsRole, writeRole)

				got, err := svc.Get(callers[callerName], "ns")
				if want {
					require.NoError(t, err)
					assert.JSONEq(t, `{"k":1}`, string(got.Document))
					return
				}
				assert.ErrorIs(t, err, service.ErrConfigNamespaceNotFound,
					"a denied read must be indistinguishable from a missing namespace")
			})
		}
	}
}

// TestGet_AnonymousMissingNamespace pins the other half of the
// indistinguishability property: a namespace that does not exist and one the
// anonymous caller may not read must produce the same error.
func TestGet_AnonymousMissingNamespace(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "private", "user", "user")

	_, errPrivate := svc.Get(anon, "private")
	_, errMissing := svc.Get(anon, "nosuchns")
	assert.ErrorIs(t, errPrivate, service.ErrConfigNamespaceNotFound)
	assert.ErrorIs(t, errMissing, service.ErrConfigNamespaceNotFound)
	assert.Equal(t, errMissing, errPrivate, "the two cases must be the same error value")
}

func TestCreate_PublicReadRequiresConfirmation(t *testing.T) {
	svc, _, b := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "tariffs", ReadRole: "public", WriteRole: "admin", Document: []byte(`{}`),
	})
	assert.ErrorIs(t, err, service.ErrConfigPublicConfirmRequired)
	assert.Equal(t, 0, b.count(), "a rejected publish must not trigger a backup")
}

func TestCreate_PublicReadWrongConfirmation(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "tariffs", ReadRole: "public", WriteRole: "admin",
		Document: []byte(`{}`), ConfirmPublic: "something-else",
	})
	assert.ErrorIs(t, err, service.ErrConfigPublicConfirmRequired,
		"the confirmation must match this namespace, so a body cannot be replayed against another")
}

func TestCreate_PublicReadWithConfirmation(t *testing.T) {
	svc, _, b := newConfigSvc(t)
	ns, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "tariffs", ReadRole: "public", WriteRole: "admin",
		Document: []byte(`{}`), ConfirmPublic: "tariffs",
	})
	require.NoError(t, err)
	assert.Equal(t, "public", ns.ReadRole)
	assert.Equal(t, 1, b.count())
}

func TestCreate_PublicWriteRoleAlwaysRejected(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "tariffs", ReadRole: "public", WriteRole: "public",
		Document: []byte(`{}`), ConfirmPublic: "tariffs",
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole,
		"nothing is anonymously writable, confirmation or not")
}

// TestCreate_PublicReadPermitsUserWrite covers the loosened invariant:
// public is the weakest read requirement, so any write role satisfies
// writers-are-readers.
func TestCreate_PublicReadPermitsUserWrite(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.CreateNamespace(admin, service.CreateNamespaceInput{
		Name: "tariffs", ReadRole: "public", WriteRole: "user",
		Document: []byte(`{}`), ConfirmPublic: "tariffs",
	})
	require.NoError(t, err)
}

func TestUpdateACL_PublishRequiresConfirmation(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	err := svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "user",
	})
	assert.ErrorIs(t, err, service.ErrConfigPublicConfirmRequired)

	readRole, _, gerr := repo.GetACL("tariffs")
	require.NoError(t, gerr)
	assert.Equal(t, "user", readRole, "a rejected publish must not have changed the ACL")
}

func TestUpdateACL_PublishWithConfirmation(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	require.NoError(t, svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "user", ConfirmPublic: "tariffs",
	}))
	readRole, _, err := repo.GetACL("tariffs")
	require.NoError(t, err)
	assert.Equal(t, "public", readRole)
}

// TestUpdateACL_AlreadyPublicNeedsNoConfirmation: the guard exists to make
// the transition deliberate. A namespace that is already public is not
// transitioning, so an unrelated write-role edit must not demand it.
func TestUpdateACL_AlreadyPublicNeedsNoConfirmation(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "public", "user")

	require.NoError(t, svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "admin",
	}))
}

// TestUpdateACL_RevokePublic: unpublishing is an ordinary ACL edit and must
// never be gated behind a confirmation.
func TestUpdateACL_RevokePublic(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "public", "user")

	require.NoError(t, svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "user", WriteRole: "user",
	}))
	readRole, _, err := repo.GetACL("tariffs")
	require.NoError(t, err)
	assert.Equal(t, "user", readRole)
}

// TestUpdateACL_RevokeToAdminNeedsWriteRaised documents the sharp edge in
// revocation: read=admin with write=user violates writers-are-readers, so
// locking a public namespace all the way down means raising both.
func TestUpdateACL_RevokeToAdminNeedsWriteRaised(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "public", "user")

	err := svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "admin", WriteRole: "user",
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole)

	require.NoError(t, svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "admin", WriteRole: "admin",
	}))
}

func TestUpdateACL_PublicWriteRoleRejected(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	err := svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "public", ConfirmPublic: "tariffs",
	})
	assert.ErrorIs(t, err, service.ErrConfigInvalidRole)
}

// TestUpdateACL_ConfirmationOnMissingNamespace: not-found must win over the
// confirmation error, so the guard cannot be used to probe for existence.
func TestUpdateACL_ConfirmationOnMissingNamespace(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	err := svc.UpdateACL(admin, "nosuchns", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "user",
	})
	assert.ErrorIs(t, err, service.ErrConfigNamespaceNotFound)
}

// TestListVisible_AnonymousSeesOnlyPublic documents the service contract.
// The HTTP list route is authenticated, so this path is not reachable
// anonymously today — the test guards the behaviour if that ever changes.
func TestListVisible_AnonymousSeesOnlyPublic(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "open", "public", "user")
	seedNS(t, repo, "internal", "user", "user")
	seedNS(t, repo, "secret", "admin", "admin")

	list, err := svc.ListVisible(anon)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "open", list[0].Name)
}

// TestPutDocument_AnonymousCannotWrite: the write path has no anonymous
// entry point at the HTTP layer, but the service must refuse regardless.
func TestPutDocument_AnonymousCannotWrite(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "open", "public", "user")

	_, err := svc.PutDocument(anon, "open", []byte(`{"k":2}`))
	assert.ErrorIs(t, err, service.ErrConfigForbidden,
		"a public namespace is readable without a token, never writable")
}

// --- audit ---
//
// The service does not record audit entries itself — the store does, inside
// the transaction that performs the mutation. What the service decides is
// *who* is recorded, so that is what these assert. Rollback behaviour is
// tested against real SQLite in internal/store's integration suite, where it
// is real; a fake cannot meaningfully assert it.

func TestDelete_AuditRecordsTheCaller(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "temp", "admin", "admin")

	require.NoError(t, svc.Delete(admin, "temp"))

	entries, err := repo.ListAudit("temp")
	require.NoError(t, err)
	require.Len(t, entries, 2, "create then delete")
	assert.Equal(t, domain.AuditActionDelete, entries[1].Action)
	assert.Equal(t, admin.Sub, entries[1].Actor,
		"the deleting admin must be recorded, not the namespace's last writer")
	assert.False(t, entries[1].At.IsZero(), "the service supplies the clock")
}

func TestUpdateACL_AuditRecordsPublishTransition(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	require.NoError(t, svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "user", ConfirmPublic: "tariffs",
	}))

	entries, err := repo.ListAudit("tariffs")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, domain.AuditActionACLChange, entries[1].Action)
	assert.Equal(t, admin.Sub, entries[1].Actor)
	assert.Equal(t, "user", entries[1].OldReadRole)
	assert.Equal(t, "public", entries[1].NewReadRole,
		"publishing is the transition the trail exists to record")
}

// TestUpdateACL_RejectedPublishLeavesNoAuditEntry: the confirmation is
// checked before the repository is touched, so a rejected publish must not
// appear in the history at all.
func TestUpdateACL_RejectedPublishLeavesNoAuditEntry(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	err := svc.UpdateACL(admin, "tariffs", service.UpdateACLInput{
		ReadRole: "public", WriteRole: "user",
	})
	require.ErrorIs(t, err, service.ErrConfigPublicConfirmRequired)

	entries, aerr := repo.ListAudit("tariffs")
	require.NoError(t, aerr)
	assert.Len(t, entries, 1, "only the create")
}

// --- reading the audit trail ---

func TestListAudit_AdminOnly(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "user", "user")

	for name, caller := range map[string]service.Caller{"user": user, "anonymous": anon} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.ListAudit(caller, "tariffs")
			assert.ErrorIs(t, err, service.ErrConfigForbidden,
				"the trail says who changed what and confirms the namespace exists")
		})
	}

	entries, err := svc.ListAudit(admin, "tariffs")
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestListAudit_ReadRoleDoesNotGrantIt: a public namespace is world-readable,
// but its history is not. Read access to a document says nothing about who
// may see who has been changing its access rules.
func TestListAudit_ReadRoleDoesNotGrantIt(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "tariffs", "public", "user")

	_, err := svc.ListAudit(anon, "tariffs")
	assert.ErrorIs(t, err, service.ErrConfigForbidden)
}

// TestListAudit_SurvivesDeletion is the case the trail exists for: "what
// happened to the namespace that is no longer here". A not-found here would
// destroy exactly the answer being asked for.
func TestListAudit_SurvivesDeletion(t *testing.T) {
	svc, repo, _ := newConfigSvc(t)
	seedNS(t, repo, "temp", "user", "user")
	require.NoError(t, svc.Delete(admin, "temp"))

	entries, err := svc.ListAudit(admin, "temp")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, domain.AuditActionDelete, entries[1].Action)
}

func TestListAudit_UnknownNamespaceIsEmptyNotNotFound(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	entries, err := svc.ListAudit(admin, "nosuchns")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestListAudit_InvalidName(t *testing.T) {
	svc, _, _ := newConfigSvc(t)
	_, err := svc.ListAudit(admin, "BAD NAME")
	assert.ErrorIs(t, err, service.ErrConfigInvalidName)
}
