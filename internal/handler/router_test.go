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

	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/handler"
	"github.com/sweeney/config/internal/service"
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
	ns.ReadRole, ns.WriteRole, ns.UpdatedBy, ns.UpdatedAt = rRole, wRole, updatedBy, at
	return nil
}
func (r *fakeRepo) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[name]; !ok {
		return domain.ErrNotFound
	}
	delete(r.data, name)
	return nil
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

func TestList_ServiceToken_Rejected(t *testing.T) {
	h := newHarness(t)
	svcTok := h.issuer.mintService(&commonauth.ServiceTokenClaims{
		ClientID: "svc", Audience: "config",
	})
	resp, _ := h.do("GET", "/api/v1/config", svcTok, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"config v1 rejects service tokens; user tokens only")
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
