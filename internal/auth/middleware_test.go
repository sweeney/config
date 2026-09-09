package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonauth "github.com/sweeney/identity/common/auth"

	"github.com/sweeney/config/internal/auth"
)

// --- stubParser ---
// A TokenParser whose outcome the test dictates outright. RequireAuth's job is
// to translate what the parser reports into a status code, so the parser is the
// only thing that needs to vary.

type stubParser struct {
	userClaims *commonauth.TokenClaims
	userErr    error
	svcClaims  *commonauth.ServiceTokenClaims
	svcErr     error
}

func (p stubParser) Parse(context.Context, string) (*commonauth.TokenClaims, error) {
	return p.userClaims, p.userErr
}

func (p stubParser) ParseServiceToken(context.Context, string) (*commonauth.ServiceTokenClaims, error) {
	return p.svcClaims, p.svcErr
}

// fakeToken builds a token whose header carries typ. peekJWTTyp reads only the
// header to choose a branch, and stubParser never looks at the rest, so the
// body and signature can be junk.
func fakeToken(typ string) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": typ})
	return base64.RawURLEncoding.EncodeToString(hdr) + ".body.sig"
}

func userToken() string    { return fakeToken("JWT") }
func serviceToken() string { return fakeToken("at+jwt") }

func serve(parser auth.TokenParser, token string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	auth.RequireAuth(parser, next).ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body["error"]
}

// assertKeysUnavailable pins the whole point of the change: a token we could
// not check must not look like a token we checked and rejected.
func assertKeysUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"), "503 must tell the client when to come back")
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"), "503 must not invite re-authentication")
	assert.Equal(t, "keys_unavailable", errorCode(t, rec))
}

func assertUnauthorized(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Empty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, "unauthorized", errorCode(t, rec))
}

// The verifier wraps its sentinels rather than returning them bare, so every
// error case here is wrapped too — a mapping written with == would pass the
// bare form and still fail in production.
func wrapped(sentinel error) error {
	return fmt.Errorf("%w: jwks fetch failed: connection refused", sentinel)
}

func TestRequireAuth_UserToken(t *testing.T) {
	tests := []struct {
		name   string
		parser stubParser
		assert func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name:   "keys unavailable is our problem, not the caller's",
			parser: stubParser{userErr: wrapped(commonauth.ErrKeysUnavailable)},
			assert: assertKeysUnavailable,
		},
		{
			name:   "invalid token is the caller's problem",
			parser: stubParser{userErr: wrapped(commonauth.ErrTokenInvalid)},
			assert: assertUnauthorized,
		},
		{
			name:   "expired token is the caller's problem",
			parser: stubParser{userErr: wrapped(commonauth.ErrTokenExpired)},
			assert: assertUnauthorized,
		},
		{
			name:   "unclassified error stays 401",
			parser: stubParser{userErr: fmt.Errorf("something else entirely")},
			assert: assertUnauthorized,
		},
		{
			name: "disabled account is forbidden, not unauthorized",
			parser: stubParser{userClaims: &commonauth.TokenClaims{
				UserID: "u1", Role: commonauth.RoleUser, IsActive: false,
			}},
			assert: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Equal(t, "account_disabled", errorCode(t, rec))
			},
		},
		{
			name: "valid active token passes through",
			parser: stubParser{userClaims: &commonauth.TokenClaims{
				UserID: "u1", Role: commonauth.RoleUser, IsActive: true,
			}},
			assert: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusOK, rec.Code)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, serve(tc.parser, userToken()))
		})
	}
}

// Service tokens take a separate branch through RequireAuth, so every case
// above has to be proved again here rather than assumed.
func TestRequireAuth_ServiceToken(t *testing.T) {
	tests := []struct {
		name   string
		parser stubParser
		assert func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name:   "keys unavailable is our problem, not the caller's",
			parser: stubParser{svcErr: wrapped(commonauth.ErrKeysUnavailable)},
			assert: assertKeysUnavailable,
		},
		{
			name:   "invalid token is the caller's problem",
			parser: stubParser{svcErr: wrapped(commonauth.ErrTokenInvalid)},
			assert: assertUnauthorized,
		},
		{
			name:   "expired token is the caller's problem",
			parser: stubParser{svcErr: wrapped(commonauth.ErrTokenExpired)},
			assert: assertUnauthorized,
		},
		{
			// No error to inspect, so there is nothing to distinguish: this
			// path stays 401.
			name:   "nil claims with no error stays 401",
			parser: stubParser{},
			assert: assertUnauthorized,
		},
		{
			name:   "valid service token passes through",
			parser: stubParser{svcClaims: &commonauth.ServiceTokenClaims{ClientID: "svc"}},
			assert: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusOK, rec.Code)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, serve(tc.parser, serviceToken()))
		})
	}
}

func TestRequireAuth_MissingOrMalformedHeader(t *testing.T) {
	parser := stubParser{userClaims: &commonauth.TokenClaims{UserID: "u1", IsActive: true}}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, header := range []string{"", "Basic abc", "Bearer"} {
		t.Run(fmt.Sprintf("header %q", header), func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			auth.RequireAuth(parser, next).ServeHTTP(rec, req)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Empty(t, rec.Header().Get("Retry-After"))
		})
	}
}

// --- real verifier ---

// TestRequireAuth_RealVerifier_DeadJWKS proves the mapping against a genuine
// JWKSVerifier rather than against a belief about what it returns: the stub
// tests above would still pass if common classified an unreachable JWKS as
// something other than ErrKeysUnavailable.
func TestRequireAuth_RealVerifier_DeadJWKS(t *testing.T) {
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "identity is down", http.StatusInternalServerError)
	}))
	t.Cleanup(jwks.Close)

	verifier, err := commonauth.NewJWKSVerifier(commonauth.JWKSVerifierConfig{
		IssuerURL: jwks.URL,
		Issuer:    jwks.URL,
	})
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	mint := func(typ string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{
			Issuer:    jwks.URL,
			Subject:   "u1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		})
		tok.Header["kid"] = "test-key"
		tok.Header["typ"] = typ
		s, signErr := tok.SignedString(key)
		require.NoError(t, signErr)
		return s
	}

	t.Run("user token", func(t *testing.T) {
		assertKeysUnavailable(t, serve(verifier, mint("JWT")))
	})
	t.Run("service token", func(t *testing.T) {
		assertKeysUnavailable(t, serve(verifier, mint("at+jwt")))
	})
}

// --- OptionalAuth ---
//
// OptionalAuth exists for the one route that may be served anonymously: a
// GET of a namespace whose read_role is public. The ACL lives in the
// database, behind the service, so whether a token is required cannot be
// known until after the lookup — which means the middleware has to let an
// unauthenticated request through and leave the decision to the service.
//
// The critical property is that this is NOT a general weakening: a token
// that is present but does not verify is still rejected. Only the total
// absence of an Authorization header is treated as anonymous.

func serveOptional(parser auth.TokenParser, token string) (*httptest.ResponseRecorder, bool, bool) {
	var reached, sawClaims bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		sawClaims = auth.ClaimsFromContext(r.Context()) != nil ||
			auth.ServiceClaimsFromContext(r.Context()) != nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/ns", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	auth.OptionalAuth(parser, next).ServeHTTP(rec, req)
	return rec, reached, sawClaims
}

func TestOptionalAuth_NoHeaderProceedsAnonymously(t *testing.T) {
	rec, reached, sawClaims := serveOptional(stubParser{}, "")
	assert.True(t, reached, "a request with no Authorization header must reach the handler")
	assert.False(t, sawClaims, "an anonymous request must carry no claims")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestOptionalAuth_ValidTokenStillInjectsClaims(t *testing.T) {
	parser := stubParser{userClaims: &commonauth.TokenClaims{
		UserID: "u1", Role: commonauth.RoleUser, IsActive: true,
	}}
	_, reached, sawClaims := serveOptional(parser, userToken())
	assert.True(t, reached)
	assert.True(t, sawClaims, "a valid token must still be identified")
}

// TestOptionalAuth_InvalidTokenIsStillRejected is the load-bearing one. A
// presented-but-broken token must never be silently downgraded to anonymous:
// that would serve public namespaces to a client whose token has expired,
// hiding the breakage until it hit a private one.
func TestOptionalAuth_InvalidTokenIsStillRejected(t *testing.T) {
	rec, reached, _ := serveOptional(stubParser{userErr: fmt.Errorf("bad token")}, userToken())
	assert.False(t, reached, "a broken token must not reach the handler as anonymous")
	assertUnauthorized(t, rec)
}

// TestOptionalAuth_KeysUnavailableStillReturns503 keeps the identity-outage
// contract: if a token was presented and we could not check it, that is our
// problem to report, not a verdict on the caller.
func TestOptionalAuth_KeysUnavailableStillReturns503(t *testing.T) {
	rec, reached, _ := serveOptional(
		stubParser{userErr: wrapped(commonauth.ErrKeysUnavailable)}, userToken())
	assert.False(t, reached)
	assertKeysUnavailable(t, rec)
}

// TestOptionalAuth_NoHeaderDuringOutage: with no token presented there is
// nothing to verify, so an identity outage does not block an anonymous read.
// Public namespaces stay readable while identity is down.
func TestOptionalAuth_NoHeaderDuringOutage(t *testing.T) {
	rec, reached, _ := serveOptional(
		stubParser{userErr: wrapped(commonauth.ErrKeysUnavailable)}, "")
	assert.True(t, reached, "an anonymous read must survive an identity outage")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestOptionalAuth_MalformedHeaderIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/ns", nil)
	req.Header.Set("Authorization", "Basic abc123")
	reached := false
	auth.OptionalAuth(stubParser{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(rec, req)

	assert.False(t, reached, "a malformed credential is still a presented credential")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestOptionalAuth_ServiceTokenAccepted(t *testing.T) {
	parser := stubParser{svcClaims: &commonauth.ServiceTokenClaims{ClientID: "svc-1"}}
	_, reached, sawClaims := serveOptional(parser, serviceToken())
	assert.True(t, reached)
	assert.True(t, sawClaims)
}
