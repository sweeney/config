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
