package handler_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonauth "github.com/sweeney/identity/common/auth"

	"github.com/sweeney/config/internal/auth"
	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/handler"
	"github.com/sweeney/config/internal/service"
	"github.com/sweeney/config/internal/testutil"
)

// --- testIssuer ---
// A minimal in-process JWT issuer/verifier that implements commonauth.TokenParser.
// Used by handler tests so they have no dependency on identity's internal packages.

type userClaims struct {
	jwt.RegisteredClaims
	Username string          `json:"usr"`
	Role     commonauth.Role `json:"rol"`
	IsActive bool            `json:"act"`
}

type svcClaims struct {
	jwt.RegisteredClaims
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
}

type testIssuer struct {
	key    *ecdsa.PrivateKey
	issuer string
}

func newTestIssuer(t *testing.T, issuer string) *testIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &testIssuer{key: key, issuer: issuer}
}

func (ti *testIssuer) mint(c *commonauth.TokenClaims) string {
	claims := &userClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    ti.issuer,
			Subject:   c.UserID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Username: c.Username,
		Role:     c.Role,
		IsActive: c.IsActive,
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(ti.key)
	return tok
}

func (ti *testIssuer) mintService(c *commonauth.ServiceTokenClaims) string {
	claims := &svcClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    ti.issuer,
			Subject:   c.ClientID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		ClientID: c.ClientID,
		Scope:    c.Scope,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["typ"] = "at+jwt"
	tok, _ := t.SignedString(ti.key)
	return tok
}

func (ti *testIssuer) Parse(_ context.Context, tokenStr string) (*commonauth.TokenClaims, error) {
	c := &userClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		if typ, _ := t.Header["typ"].(string); typ == "at+jwt" {
			return nil, fmt.Errorf("service token not accepted as user token")
		}
		return &ti.key.PublicKey, nil
	}, jwt.WithIssuer(ti.issuer), jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{"ES256"}))
	if err != nil {
		return nil, commonauth.ErrTokenInvalid
	}
	return &commonauth.TokenClaims{
		UserID:   c.Subject,
		Username: c.Username,
		Role:     c.Role,
		IsActive: c.IsActive,
	}, nil
}

func (ti *testIssuer) ParseServiceToken(_ context.Context, tokenStr string) (*commonauth.ServiceTokenClaims, error) {
	c := &svcClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		if typ, _ := t.Header["typ"].(string); typ != "at+jwt" {
			return nil, fmt.Errorf("not a service token")
		}
		return &ti.key.PublicKey, nil
	}, jwt.WithIssuer(ti.issuer), jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{"ES256"}))
	if err != nil {
		return nil, commonauth.ErrTokenInvalid
	}
	return &commonauth.ServiceTokenClaims{ClientID: c.ClientID, Scope: c.Scope}, nil
}

// fakeRepo is an in-memory ConfigRepository — duplicated from the service
// tests rather than sharing to keep this test file self-contained.
type fakeRepo struct {
	mu   sync.Mutex
	data map[string]*domain.ConfigNamespace

	// Audit recording is shared with the other suite's fake so the two
	// cannot disagree about what gets recorded; see internal/testutil.
	audit testutil.AuditLog
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{data: map[string]*domain.ConfigNamespace{}}
}

func (r *fakeRepo) List() ([]domain.ConfigNamespaceSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.ConfigNamespaceSummary, 0, len(r.data))
	for _, ns := range r.data {
		out = append(out, domain.ConfigNamespaceSummary{
			Name: ns.Name, ReadRole: ns.ReadRole, WriteRole: ns.WriteRole,
			UpdatedAt: ns.UpdatedAt, CreatedAt: ns.CreatedAt,
		})
	}
	return out, nil
}
func (r *fakeRepo) GetACL(name string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return "", "", domain.ErrNotFound
	}
	return ns.ReadRole, ns.WriteRole, nil
}

func (r *fakeRepo) Get(name string) (*domain.ConfigNamespace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.data[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c := *ns
	return &c, nil
}
func (r *fakeRepo) Create(ns *domain.ConfigNamespace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[ns.Name]; ok {
		return domain.ErrConflict
	}
	c := *ns
	r.data[ns.Name] = &c
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
func (r *fakeRepo) UpdateDocument(name string, document []byte, updatedBy string, at time.Time) error {
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
func (r *fakeRepo) UpdateACL(name, rRole, wRole, updatedBy string, at time.Time) error {
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
		NewReadRole:  rRole,
		NewWriteRole: wRole,
		Actor:        updatedBy,
		At:           at,
	})
	ns.ReadRole, ns.WriteRole, ns.UpdatedBy, ns.UpdatedAt = rRole, wRole, updatedBy, at
	return nil
}
func (r *fakeRepo) Delete(name, deletedBy string, at time.Time) error {
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

func (r *fakeRepo) ListAudit(namespace string) ([]domain.AuditEntry, error) {
	return r.audit.List(namespace), nil
}

// seed installs a namespace as if it had been created through the API.
// It routes through Create rather than writing the map directly so the fake
// records the same audit entry the real store would — otherwise a seeded
// namespace has a history the production code path would never produce.
func (r *fakeRepo) seed(name, readRole, writeRole, document string) {
	now := time.Now()
	_ = r.Create(&domain.ConfigNamespace{
		Name: name, ReadRole: readRole, WriteRole: writeRole,
		Document: []byte(document), CreatedAt: now, UpdatedAt: now,
		UpdatedBy: "seed",
	})
}

type fakeBackup struct{}

func (fakeBackup) TriggerAsync() {}

type harness struct {
	t        *testing.T
	issuer   *testIssuer
	repo     *fakeRepo
	srv      *httptest.Server
	adminTok string
	userTok  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	iss := newTestIssuer(t, "https://test")
	repo := newFakeRepo()
	svc := service.NewConfigService(repo, fakeBackup{})
	router := handler.NewRouter(handler.Deps{
		Service:  svc,
		Verifier: iss,
		Version:  "test",
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	mint := func(role commonauth.Role, sub string) string {
		return iss.mint(&commonauth.TokenClaims{
			UserID: sub, Username: sub, Role: role, IsActive: true,
		})
	}
	return &harness{
		t:        t,
		issuer:   iss,
		repo:     repo,
		srv:      srv,
		adminTok: mint(commonauth.RoleAdmin, "admin-1"),
		userTok:  mint(commonauth.RoleUser, "user-1"),
	}
}

// do sends an HTTP request with an optional Bearer token and a JSON body.
func (h *harness) do(method, path, token string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var buf io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			buf = strings.NewReader(v)
		case []byte:
			buf = bytes.NewReader(v)
		default:
			b, err := json.Marshal(v)
			require.NoError(h.t, err)
			buf = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, h.srv.URL+path, buf)
	require.NoError(h.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// --- Auth boundary ---

func TestHealthz_Unauth(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/healthz", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"status":"ok"`)
}

func TestList_MissingAuth_ReturnsUnauthorized(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/config", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// --- Service token access ---
//
// Service tokens are accepted and treated as user role. They can read
// user-readable namespaces but cannot write, create, delete, or change ACLs.
// Admin-only namespaces are invisible to them (404, not 403).

func svcTok(h *harness) string {
	return h.issuer.mintService(&commonauth.ServiceTokenClaims{ClientID: "svc-client"})
}

func TestServiceToken_List_Returns200(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/config", svcTok(h), nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"service tokens must be accepted with user-level access")
}

func TestServiceToken_List_SeesOnlyUserNamespaces(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("visible", "user", "admin", `{}`)
	h.repo.seed("hidden", "admin", "admin", `{}`)

	_, body := h.do("GET", "/api/v1/config", svcTok(h), nil)
	assert.Contains(t, string(body), "visible")
	assert.NotContains(t, string(body), "hidden",
		"admin-only namespaces must not appear in service token list")
}

func TestServiceToken_Get_UserNamespace_Returns200(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("pub", "user", "admin", `{"k":"v"}`)

	resp, body := h.do("GET", "/api/v1/config/pub", svcTok(h), nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"k"`)
}

func TestServiceToken_Get_AdminNamespace_Returns404(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("secret", "admin", "admin", `{"k":"v"}`)

	resp, _ := h.do("GET", "/api/v1/config/secret", svcTok(h), nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"existence of admin-only namespace must not be leaked via 403")
}

func TestServiceToken_Put_AdminWriteUserRead_Returns403(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("mqtt", "user", "admin", `{"k":"v"}`)

	// Service token can read mqtt (read_role=user) but write_role=admin.
	resp, _ := h.do("PUT", "/api/v1/config/mqtt", svcTok(h), map[string]any{"k": "hijack"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"service token must not be able to write to admin-write namespaces it can read")
}

func TestServiceToken_Put_AdminReadNamespace_Returns404(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("secret", "admin", "admin", `{"k":"v"}`)

	resp, _ := h.do("PUT", "/api/v1/config/secret", svcTok(h), map[string]any{"k": "hijack"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"existence of admin-only namespace must not be leaked during write attempt")
}

func TestServiceToken_Create_Returns403(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/config/namespaces", svcTok(h), map[string]any{
		"name": "new", "read_role": "user", "write_role": "user",
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"service token must not be able to create namespaces (admin-only)")
}

func TestServiceToken_Delete_Returns403(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("pub", "user", "user", `{}`)

	resp, _ := h.do("DELETE", "/api/v1/config/pub", svcTok(h), nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"service token must not be able to delete namespaces (admin-only)")
}

func TestServiceToken_PatchACL_Returns403(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("pub", "user", "admin", `{}`)

	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/pub", svcTok(h), map[string]any{
		"read_role": "user", "write_role": "user",
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"service token must not be able to escalate its own access via ACL patch")
}

func TestServiceToken_InvalidToken_Returns401(t *testing.T) {
	h := newHarness(t)
	// Forge a token with a bad signature but correct typ header.
	badTok := h.issuer.mintService(&commonauth.ServiceTokenClaims{ClientID: "evil"})
	// Corrupt the signature segment.
	parts := strings.Split(badTok, ".")
	require.Len(t, parts, 3)
	parts[2] = "invalidsignature"
	resp, _ := h.do("GET", "/api/v1/config", strings.Join(parts, "."), nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"tampered service token must be rejected with 401")
}

// --- Create ---

func TestCreate_AdminSucceeds(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/config/namespaces", h.adminTok, map[string]any{
		"name":       "houses",
		"read_role":  "admin",
		"write_role": "admin",
		"document":   json.RawMessage(`{"main":"Rivendell"}`),
	})
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestCreate_UserForbidden(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/config/namespaces", h.userTok, map[string]any{
		"name": "prefs", "read_role": "user", "write_role": "user",
		"document": json.RawMessage(`{}`),
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestCreate_InvalidName(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/api/v1/config/namespaces", h.adminTok, map[string]any{
		"name": "BAD NAME", "read_role": "admin", "write_role": "admin",
		"document": json.RawMessage(`{}`),
	})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "invalid_name")
}

func TestCreate_Duplicate_Conflict(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{
		"name": "dup", "read_role": "admin", "write_role": "admin",
		"document": json.RawMessage(`{}`),
	}
	resp, _ := h.do("POST", "/api/v1/config/namespaces", h.adminTok, body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/config/namespaces", h.adminTok, body)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

// --- Get / List with role gating ---

func TestGet_UserBlockedFromAdminNamespace_Returns404(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, _ := h.do("GET", "/api/v1/config/secret", h.userTok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"user must see 404 not 403, to avoid leaking namespace existence")
}

func TestGet_UserCanReadUserNamespace(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{"theme":"dark"}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, body := h.do("GET", "/api/v1/config/prefs", h.userTok, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"theme":"dark"}`, string(body))
}

func TestList_FiltersByVisibility(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	for _, ns := range []domain.ConfigNamespace{
		{Name: "hidden", ReadRole: "admin", WriteRole: "admin"},
		{Name: "visible", ReadRole: "user", WriteRole: "admin"},
	} {
		n := ns
		n.Document = []byte(`{}`)
		n.UpdatedAt, n.CreatedAt = now, now
		n.UpdatedBy = "u"
		require.NoError(t, h.repo.Create(&n))
	}

	resp, body := h.do("GET", "/api/v1/config", h.userTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out, 1)
	assert.Equal(t, "visible", out[0]["name"])
}

// --- PUT ---

func TestPut_WriteRoleEnforced(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "shared", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	// User can READ but not WRITE → 403 (since they can read it, we don't pretend it's 404)
	resp, _ := h.do("PUT", "/api/v1/config/shared", h.userTok, json.RawMessage(`{"x":1}`))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Admin can write → 200
	resp, _ = h.do("PUT", "/api/v1/config/shared", h.adminTok, json.RawMessage(`{"x":1}`))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// GET returns updated document
	resp, body := h.do("GET", "/api/v1/config/shared", h.userTok, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"x":1}`, string(body))
}

func TestPut_UserBlockedFromInvisibleNamespace_Returns404(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	resp, _ := h.do("PUT", "/api/v1/config/secret", h.userTok, json.RawMessage(`{"x":1}`))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"invisible-to-user namespace must 404, not 403, on write attempt")
}

func TestPut_InvalidDocument_Returns400(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, body := h.do("PUT", "/api/v1/config/n", h.adminTok, "[]")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "invalid_document")
}

func TestPut_NoOp_ReturnsChangedFalse(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{"a":1}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, body := h.do("PUT", "/api/v1/config/n", h.adminTok, json.RawMessage(`{"a":1}`))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, false, out["changed"])
}

func TestPut_OversizedBody_Returns413(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	// Build a JSON object larger than the handler's 128KB body cap.
	big := &strings.Builder{}
	big.WriteString(`{"k":"`)
	for i := 0; i < 200_000; i++ {
		big.WriteByte('x')
	}
	big.WriteString(`"}`)
	resp, _ := h.do("PUT", "/api/v1/config/n", h.adminTok, big.String())
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

// --- ACL response headers ---

func TestGet_ReturnsACLHeaders(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "prefs", ReadRole: "user", WriteRole: "admin",
		Document: []byte(`{"x":1}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, _ := h.do("GET", "/api/v1/config/prefs", h.adminTok, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "user", resp.Header.Get("X-Read-Role"))
	assert.Equal(t, "admin", resp.Header.Get("X-Write-Role"))
}

func TestGet_ACLHeaders_MatchStoredACL(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "sec", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, _ := h.do("GET", "/api/v1/config/sec", h.adminTok, nil)
	assert.Equal(t, "admin", resp.Header.Get("X-Read-Role"))
	assert.Equal(t, "admin", resp.Header.Get("X-Write-Role"))
}

func TestPatchACL_ReturnsACLHeaders(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/n", h.adminTok,
		map[string]string{"read_role": "user", "write_role": "admin"})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "user", resp.Header.Get("X-Read-Role"))
	assert.Equal(t, "admin", resp.Header.Get("X-Write-Role"))
}

func TestPatchACL_GetHeadersMatchPatchHeaders(t *testing.T) {
	// Regression: PATCH headers must reflect what's actually stored, not just
	// echo the request. A subsequent GET must agree with the PATCH response.
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	patchResp, _ := h.do("PATCH", "/api/v1/config/namespaces/n", h.adminTok,
		map[string]string{"read_role": "user", "write_role": "admin"})
	require.Equal(t, http.StatusOK, patchResp.StatusCode)

	getResp, _ := h.do("GET", "/api/v1/config/n", h.adminTok, nil)
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	assert.Equal(t, patchResp.Header.Get("X-Read-Role"), getResp.Header.Get("X-Read-Role"))
	assert.Equal(t, patchResp.Header.Get("X-Write-Role"), getResp.Header.Get("X-Write-Role"))
}

func TestGet_NoACLHeadersOn404_ReadDenied(t *testing.T) {
	// A user that lacks read_role sees 404. The ACL headers must be absent so
	// header presence cannot be used as an existence oracle.
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "secret", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))
	resp, _ := h.do("GET", "/api/v1/config/secret", h.userTok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("X-Read-Role"))
	assert.Empty(t, resp.Header.Get("X-Write-Role"))
}

func TestGet_NoACLHeadersOn404_Missing(t *testing.T) {
	// A truly missing namespace must also return no ACL headers.
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/config/nonexistent", h.adminTok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("X-Read-Role"))
	assert.Empty(t, resp.Header.Get("X-Write-Role"))
}

// --- PATCH ACL ---

func TestPatchACL_AdminOnly(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "admin", WriteRole: "admin",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/n", h.userTok,
		map[string]string{"read_role": "user", "write_role": "admin"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp, _ = h.do("PATCH", "/api/v1/config/namespaces/n", h.adminTok,
		map[string]string{"read_role": "user", "write_role": "admin"})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- DELETE ---

func TestDelete_AdminOnly(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.Create(&domain.ConfigNamespace{
		Name: "n", ReadRole: "user", WriteRole: "user",
		Document: []byte(`{}`), UpdatedAt: now, UpdatedBy: "u", CreatedAt: now,
	}))

	resp, _ := h.do("DELETE", "/api/v1/config/n", h.userTok, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"user must not delete even namespaces they can write")

	resp, _ = h.do("DELETE", "/api/v1/config/n", h.adminTok, nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, _ = h.do("GET", "/api/v1/config/n", h.adminTok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- Malformed requests ---

func TestCreate_MalformedJSON_Returns400(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/api/v1/config/namespaces", h.adminTok, "{not json}")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "invalid_request")
}

// --- SPA bundle ---

// newHarnessWithSPA spins up a router with the admin-UI bundle mounted.
func newHarnessWithSPA(t *testing.T) *harness {
	t.Helper()
	iss := newTestIssuer(t, "https://test")
	repo := newFakeRepo()
	svc := service.NewConfigService(repo, fakeBackup{})
	router := handler.NewRouter(handler.Deps{
		Service:           svc,
		Verifier:          iss,
		Version:           "test",
		IdentityPublicURL: "https://id.example.com",
		OAuthClientID:     "config-spa",
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	mint := func(role commonauth.Role, sub string) string {
		return iss.mint(&commonauth.TokenClaims{UserID: sub, Username: sub, Role: role, IsActive: true})
	}
	return &harness{
		t: t, issuer: iss, repo: repo, srv: srv,
		adminTok: mint(commonauth.RoleAdmin, "admin-1"),
		userTok:  mint(commonauth.RoleUser, "user-1"),
	}
}

func TestSPA_IndexServedAtRoot(t *testing.T) {
	h := newHarnessWithSPA(t)
	resp, body := h.do("GET", "/", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, string(body), "<title>Config Admin</title>")
	assert.Contains(t, string(body), "/static/app.js")
}

func TestSPA_StaticAssetsServed(t *testing.T) {
	h := newHarnessWithSPA(t)
	for _, path := range []string{"/static/app.js", "/static/style.css", "/static/auth.js", "/static/api.js", "/static/editor.js"} {
		t.Run(path, func(t *testing.T) {
			resp, body := h.do("GET", path, "", nil)
			assert.Equal(t, http.StatusOK, resp.StatusCode, "expected %s to be served", path)
			assert.NotEmpty(t, body)
		})
	}
}

func TestSPA_BootstrapConfigJSON(t *testing.T) {
	h := newHarnessWithSPA(t)
	resp, body := h.do("GET", "/spa-config.json", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "json")
	var cfg map[string]string
	require.NoError(t, json.Unmarshal(body, &cfg))
	assert.Equal(t, "https://id.example.com", cfg["identity_url"],
		"SPA must learn the identity URL via /spa-config.json so the OAuth flow can target it")
	assert.Equal(t, "config-spa", cfg["client_id"])
}

func TestSPA_NotMountedWhenDepsEmpty(t *testing.T) {
	// When IdentityPublicURL or OAuthClientID is unset, the SPA bundle
	// is intentionally not mounted — config stays a pure API server.
	h := newHarness(t)
	for _, path := range []string{"/", "/static/app.js", "/spa-config.json"} {
		t.Run(path, func(t *testing.T) {
			resp, _ := h.do("GET", path, "", nil)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"%s must 404 when SPA is disabled", path)
		})
	}
}

// TestSPA_DirectoryListingRefused asserts that http.FileServer's default
// "render an HTML index for a directory request" behaviour is suppressed.
func TestSPA_DirectoryListingRefused(t *testing.T) {
	h := newHarnessWithSPA(t)
	for _, path := range []string{"/static/", "/static/somedir/"} {
		t.Run(path, func(t *testing.T) {
			resp, body := h.do("GET", path, "", nil)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"%s must 404 (no directory listing)", path)
			assert.NotContains(t, string(body), "<a href=",
				"response body must not contain an HTML directory index")
		})
	}
}

// TestSPA_IndexCarriesAssetVersion verifies the cache-busting wiring:
// every /static/* URL in the rendered index.html should carry a ?v=…
// query so a deploy reliably invalidates browser caches.
func TestSPA_IndexCarriesAssetVersion(t *testing.T) {
	h := newHarnessWithSPA(t)
	resp, body := h.do("GET", "/", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	for _, asset := range []string{"app.js", "auth.js", "api.js", "editor.js", "style.css"} {
		assert.Regexp(t, `/static/`+asset+`\?v=\w+`, string(body),
			"%s must be referenced with a ?v=… cache-buster", asset)
	}
}

// --- OpenAPI ---

func TestOpenAPI_JSON_Unauth(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/openapi.json", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "json")
	var v map[string]any
	require.NoError(t, json.Unmarshal(body, &v), "openapi.json must decode")
	assert.Equal(t, "3.0.3", v["openapi"])
	assert.Contains(t, v["info"].(map[string]any)["title"], "Config")
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"), "spec must be CORS-fetchable by browser tooling")
}

func TestOpenAPI_YAML_Unauth(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/openapi.yaml", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "yaml")
	assert.Contains(t, string(body), "openapi: 3.0.3")
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"), "spec must be CORS-fetchable by browser tooling")
}

// documentedPaths mirrors the API routes registered in NewRouter that are
// expected to appear in the OpenAPI spec's `paths`. Keep this in sync with
// NewRouter — TestOpenAPI_PathCoverage fails on drift. Non-API routes
// (/openapi.json, /openapi.yaml, and SPA assets) are intentionally excluded.
var documentedPaths = []string{
	"/healthz",
	"/api/v1/config",
	"/api/v1/config/{ns}",
	"/api/v1/config/namespaces",
	"/api/v1/config/namespaces/{ns}",
}

// TestOpenAPI_PathCoverage cross-references the spec's paths against the routes
// the server documents, so adding/removing an endpoint without updating the
// spec (or vice versa) is a test failure.
func TestOpenAPI_PathCoverage(t *testing.T) {
	h := newHarness(t)
	_, body := h.do("GET", "/openapi.json", "", nil)

	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(body, &doc), "openapi.json must decode")

	specPaths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		specPaths = append(specPaths, p)
	}
	assert.ElementsMatch(t, documentedPaths, specPaths,
		"openapi spec paths and documentedPaths have drifted — update documentedPaths in this file or spec/openapi.yaml")
}

// Sanity: we don't leak a stack trace in errors.
func TestErrors_NoInternalLeaks(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/api/v1/config/missing", h.adminTok, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	var out map[string]string
	require.NoError(t, json.Unmarshal(body, &out))
	assert.NotContains(t, out["message"], "sql",
		"error envelope must not leak database error text")
	assert.NotContains(t, out["message"], "panic",
		"error envelope must not leak internal state")
	_ = fmt.Sprint
}

// --- JWKS outage ---

// keysUnavailableIssuer mints tokens the same way testIssuer does, but reports
// every verification as an identity outage.
type keysUnavailableIssuer struct{ *testIssuer }

func (keysUnavailableIssuer) Parse(context.Context, string) (*commonauth.TokenClaims, error) {
	return nil, fmt.Errorf("%w: connection refused", commonauth.ErrKeysUnavailable)
}

func (keysUnavailableIssuer) ParseServiceToken(context.Context, string) (*commonauth.ServiceTokenClaims, error) {
	return nil, fmt.Errorf("%w: connection refused", commonauth.ErrKeysUnavailable)
}

// A JWKS outage must reach the client as "retry shortly", not "your session is
// dead" — through the real router, not just the middleware in isolation.
func TestProtectedRoute_KeysUnavailable_Returns503(t *testing.T) {
	iss := newTestIssuer(t, "https://test")
	repo := newFakeRepo()
	svc := service.NewConfigService(repo, fakeBackup{})
	router := handler.NewRouter(handler.Deps{
		Service:  svc,
		Verifier: keysUnavailableIssuer{iss},
		Version:  "test",
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	tokens := map[string]string{
		"user token": iss.mint(&commonauth.TokenClaims{
			UserID: "u1", Username: "u1", Role: commonauth.RoleUser, IsActive: true,
		}),
		"service token": iss.mintService(&commonauth.ServiceTokenClaims{ClientID: "svc"}),
	}

	for name, tok := range tokens {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+"/api/v1/config", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close() //nolint:errcheck

			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.NotEmpty(t, resp.Header.Get("Retry-After"))
			assert.Empty(t, resp.Header.Get("WWW-Authenticate"),
				"an outage must not tell clients to re-authenticate")
		})
	}
}

// /healthz stays green through a JWKS outage. Deliberate: deploy.sh gates
// deploys on this endpoint, and anything that restarts config on an unhealthy
// signal would discard the cached keys that let it ride the outage out. The
// jwks counters in the response body are the signal to alert on instead.
func TestHealthz_StaysOKDuringKeysOutage(t *testing.T) {
	iss := newTestIssuer(t, "https://test")
	svc := service.NewConfigService(newFakeRepo(), fakeBackup{})
	router := handler.NewRouter(handler.Deps{
		Service:  svc,
		Verifier: keysUnavailableIssuer{iss},
		Version:  "test",
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- anonymous reads of public namespaces ---
//
// GET of a single namespace is the only optionally-authenticated route. The
// list endpoint deliberately stays authenticated: knowing a namespace's name
// is the price of reading it anonymously, and nothing advertises the names.

// outageParser stands in for a verifier that cannot reach identity's JWKS.
type outageParser struct{}

func (outageParser) Parse(context.Context, string) (*commonauth.TokenClaims, error) {
	return nil, commonauth.ErrKeysUnavailable
}

func (outageParser) ParseServiceToken(context.Context, string) (*commonauth.ServiceTokenClaims, error) {
	return nil, commonauth.ErrKeysUnavailable
}

func newHarnessWithParser(t *testing.T, parser auth.TokenParser) *harness {
	t.Helper()
	repo := newFakeRepo()
	svc := service.NewConfigService(repo, fakeBackup{})
	srv := httptest.NewServer(handler.NewRouter(handler.Deps{
		Service: svc, Verifier: parser, Version: "test",
	}))
	t.Cleanup(srv.Close)
	return &harness{t: t, repo: repo, srv: srv}
}

func TestGet_Anonymous_PublicNamespace_Returns200(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{"unit":0.24}`)

	resp, body := h.do("GET", "/api/v1/config/tariffs", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"unit":0.24}`, string(body))
}

func TestGet_Anonymous_PrivateNamespace_Returns404(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("internal", "user", "user", `{"secret":true}`)

	resp, body := h.do("GET", "/api/v1/config/internal", "", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a private namespace must be not-found to an anonymous caller, never 401")
	assert.NotContains(t, string(body), "secret")
}

// TestGet_Anonymous_PrivateAndMissingAreIndistinguishable is the property
// that makes the obscurity model hold: an anonymous caller brute-forcing
// names must not be able to tell a private namespace from one that does not
// exist.
func TestGet_Anonymous_PrivateAndMissingAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("internal", "user", "user", `{}`)

	privateResp, privateBody := h.do("GET", "/api/v1/config/internal", "", nil)
	missingResp, missingBody := h.do("GET", "/api/v1/config/nosuchns", "", nil)

	assert.Equal(t, missingResp.StatusCode, privateResp.StatusCode)
	assert.Equal(t, string(missingBody), string(privateBody))
	assert.Empty(t, privateResp.Header.Get("X-Read-Role"),
		"ACL headers must not leak on a denied read")
	assert.Equal(t, missingResp.Header.Get("Cache-Control"), privateResp.Header.Get("Cache-Control"))
}

// TestGet_BrokenTokenOnPublicNamespaceReturns401: a presented token that does
// not verify is never silently downgraded to anonymous, even where anonymous
// would have succeeded.
func TestGet_BrokenTokenOnPublicNamespaceReturns401(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/tariffs", "not-a-real-token", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestGet_Anonymous_SurvivesIdentityOutage: with no token presented there is
// nothing to verify, so a public namespace stays readable while identity is
// unreachable. Public namespaces are the outage-resilient read path.
func TestGet_Anonymous_SurvivesIdentityOutage(t *testing.T) {
	h := newHarnessWithParser(t, outageParser{})
	h.repo.seed("tariffs", "public", "user", `{"unit":0.24}`)

	resp, body := h.do("GET", "/api/v1/config/tariffs", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"an anonymous public read must not depend on identity being up")
	assert.JSONEq(t, `{"unit":0.24}`, string(body))
}

func TestGet_TokenPresentedDuringIdentityOutage_Returns503(t *testing.T) {
	h := newHarnessWithParser(t, outageParser{})
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/tariffs", "some-token", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "30", resp.Header.Get("Retry-After"))
}

func TestGet_AuthenticatedUserCanAlsoReadPublic(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/tariffs", h.userTok, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestGet_CacheHeaders: the TTL on a public namespace is also the revoke
// latency — flipping read_role back does not purge a CDN or a browser cache
// — so it is deliberately short, and Vary stays on both because the same URL
// answers differently with and without a token.
func TestGet_CacheHeaders(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)
	h.repo.seed("internal", "user", "user", `{}`)

	pub, _ := h.do("GET", "/api/v1/config/tariffs", "", nil)
	assert.Equal(t, "public, max-age=60", pub.Header.Get("Cache-Control"))
	assert.Equal(t, "Authorization", pub.Header.Get("Vary"))

	priv, _ := h.do("GET", "/api/v1/config/internal", h.userTok, nil)
	assert.Equal(t, "private, no-store", priv.Header.Get("Cache-Control"))
	assert.Equal(t, "Authorization", priv.Header.Get("Vary"))
}

// TestAnonymous_EveryOtherRouteStillRequiresAToken guards the blast radius:
// only the single-namespace GET became optionally authenticated.
func TestAnonymous_EveryOtherRouteStillRequiresAToken(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	cases := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/config", nil},
		{"PUT", "/api/v1/config/tariffs", map[string]any{"k": 1}},
		{"DELETE", "/api/v1/config/tariffs", nil},
		{"POST", "/api/v1/config/namespaces", map[string]any{"name": "x", "read_role": "user", "write_role": "user"}},
		{"PATCH", "/api/v1/config/namespaces/tariffs", map[string]any{"read_role": "user", "write_role": "user"}},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, _ := h.do(c.method, c.path, "", c.body)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
				"a public read role must not open any route but the namespace GET")
		})
	}
}

// --- publish confirmation ---

func TestPatchACL_PublishWithoutConfirmation_Returns400(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)

	resp, body := h.do("PATCH", "/api/v1/config/namespaces/tariffs", h.adminTok,
		map[string]any{"read_role": "public", "write_role": "user"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var env map[string]string
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "confirm_required", env["error"])

	readRole, _, err := h.repo.GetACL("tariffs")
	require.NoError(t, err)
	assert.Equal(t, "user", readRole, "the ACL must be untouched")
}

func TestPatchACL_PublishWithConfirmation_Succeeds(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)

	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/tariffs", h.adminTok,
		map[string]any{"read_role": "public", "write_role": "user", "confirm_public": "tariffs"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "public", resp.Header.Get("X-Read-Role"))

	anon, _ := h.do("GET", "/api/v1/config/tariffs", "", nil)
	assert.Equal(t, http.StatusOK, anon.StatusCode, "publishing must actually open the read path")
}

func TestPatchACL_PublishWithWrongConfirmation_Returns400(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)

	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/tariffs", h.adminTok,
		map[string]any{"read_role": "public", "write_role": "user", "confirm_public": "some-other-ns"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestPatchACL_RevokeNeedsNoConfirmation: unpublishing is an ordinary edit,
// and the anonymous read must stop immediately at the origin.
func TestPatchACL_RevokeNeedsNoConfirmation(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/tariffs", h.adminTok,
		map[string]any{"read_role": "user", "write_role": "user"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	anon, _ := h.do("GET", "/api/v1/config/tariffs", "", nil)
	assert.Equal(t, http.StatusNotFound, anon.StatusCode)
}

func TestCreate_PublicWithoutConfirmation_Returns400(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/api/v1/config/namespaces", h.adminTok, map[string]any{
		"name": "tariffs", "read_role": "public", "write_role": "user",
		"document": map[string]any{},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var env map[string]string
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "confirm_required", env["error"])
}

func TestCreate_PublicWithConfirmation_Returns201(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/config/namespaces", h.adminTok, map[string]any{
		"name": "tariffs", "read_role": "public", "write_role": "user",
		"document": map[string]any{}, "confirm_public": "tariffs",
	})
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestCreate_PublicWriteRole_Returns400(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/api/v1/config/namespaces", h.adminTok, map[string]any{
		"name": "tariffs", "read_role": "public", "write_role": "public",
		"document": map[string]any{}, "confirm_public": "tariffs",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var env map[string]string
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "invalid_role", env["error"])
}

// --- CORS on published namespaces ---

// TestGet_PublicNamespaceAllowsAnyOrigin: a published document is meant to be
// fetched from anywhere. Restricting browser origins on it is friction rather
// than protection — anything server-side ignores CORS entirely — so a public
// namespace answers any origin, as /openapi.json already does.
func TestGet_PublicNamespaceAllowsAnyOrigin(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/tariffs", "", nil)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
}

// TestGet_PrivateNamespaceDoesNotAllowAnyOrigin: the wildcard is scoped to
// what was deliberately published. A private namespace keeps whatever the
// origin allow-list decided.
func TestGet_PrivateNamespaceDoesNotAllowAnyOrigin(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("internal", "user", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/internal", h.userTok, nil)
	assert.NotEqual(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
}

// TestGet_VaryPreservesUpstreamValues: the CORS middleware sets Vary: Origin
// before the handler runs. The handler used to Set Vary outright, silently
// dropping it — which with a shared cache and max-age on public namespaces
// means a cached response could be served to an origin it was not built for.
func TestGet_VaryPreservesUpstreamValues(t *testing.T) {
	repo := newFakeRepo()
	repo.seed("tariffs", "public", "user", `{}`)
	router := handler.NewRouter(handler.Deps{
		Service:  service.NewConfigService(repo, fakeBackup{}),
		Verifier: newTestIssuer(t, "https://test"),
		Version:  "test",
	})
	// Stand in for the securityHeaders middleware in cmd/server.
	withVary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin")
		router.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(withVary)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/config/tariffs")
	require.NoError(t, err)
	defer resp.Body.Close()

	vary := resp.Header.Values("Vary")
	joined := strings.Join(vary, ", ")
	assert.Contains(t, joined, "Origin", "the handler must not drop an upstream Vary")
	assert.Contains(t, joined, "Authorization")
}

// --- audit endpoint ---

func auditEntries(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestAudit_AdminSeesHistory(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)
	resp, _ := h.do("PATCH", "/api/v1/config/namespaces/tariffs", h.adminTok,
		map[string]any{"read_role": "public", "write_role": "user", "confirm_public": "tariffs"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := h.do("GET", "/api/v1/config/namespaces/tariffs/audit", h.adminTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	entries := auditEntries(t, body)
	require.Len(t, entries, 2, "seeded create, then the publish")
	assert.Equal(t, "acl_change", entries[1]["action"])
	assert.Equal(t, "user", entries[1]["old_read_role"])
	assert.Equal(t, "public", entries[1]["new_read_role"])
	assert.Equal(t, "admin-1", entries[1]["actor"])
	assert.NotEmpty(t, entries[1]["at"])
	assert.NotContains(t, entries[0], "old_read_role", "a create has no previous ACL")
}

func TestAudit_NotCacheable(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/namespaces/tariffs/audit", h.adminTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"),
		"the history of a public namespace is not itself public")
	assert.NotEqual(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
}

func TestAudit_RequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/namespaces/tariffs/audit", h.userTok, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestAudit_ServiceTokenForbidden(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "user", "user", `{}`)

	resp, _ := h.do("GET", "/api/v1/config/namespaces/tariffs/audit", svcTok(h), nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"service tokens are pinned to user role and never read the trail")
}

// TestAudit_AnonymousRejectedEvenForPublicNamespace: publishing a document
// does not publish its history.
func TestAudit_AnonymousRejectedEvenForPublicNamespace(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("tariffs", "public", "user", `{}`)

	pub, _ := h.do("GET", "/api/v1/config/tariffs", "", nil)
	require.Equal(t, http.StatusOK, pub.StatusCode, "the document itself is anonymous-readable")

	resp, _ := h.do("GET", "/api/v1/config/namespaces/tariffs/audit", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAudit_DeletedNamespaceKeepsItsHistory(t *testing.T) {
	h := newHarness(t)
	h.repo.seed("temp", "user", "user", `{}`)
	resp, _ := h.do("DELETE", "/api/v1/config/temp", h.adminTok, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body := h.do("GET", "/api/v1/config/namespaces/temp/audit", h.adminTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	entries := auditEntries(t, body)
	require.Len(t, entries, 2)
	assert.Equal(t, "delete", entries[1]["action"])
	assert.Equal(t, "admin-1", entries[1]["actor"])
}

func TestAudit_UnknownNamespaceReturnsEmptyArray(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/api/v1/config/namespaces/nosuchns/audit", h.adminTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `[]`, string(body), "never null — clients iterate it")
}
