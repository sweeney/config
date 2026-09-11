package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// securityHeaders answers OPTIONS itself, before the router runs, so the
// wildcard the namespace handler sets on a public GET never reaches a
// preflight. That made "fetchable from anywhere" true only for simple
// requests: a client that sets Content-Type on the GET, or sends any custom
// header, triggers a preflight that then fails.
//
// A permissive preflight discloses nothing. It says only which method and
// headers may be attempted; the GET itself still enforces the ACL, and a
// namespace the caller may not read still answers 404.

func preflight(t *testing.T, origin, path, reqHeaders string, corsOrigins []string) *http.Response {
	t.Helper()
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("preflight must not reach the router")
	}), corsOrigins, "", false)
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	if reqHeaders != "" {
		req.Header.Set("Access-Control-Request-Headers", reqHeaders)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestPreflight_NamespaceGetFromUnknownOriginIsAllowed(t *testing.T) {
	resp := preflight(t, "https://unknown.example", "/api/v1/config/tariffs", "content-type", nil)

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "GET")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Content-Type")
	assert.Contains(t, resp.Header.Get("Access-Control-Expose-Headers"), "X-Read-Role")
}

// The wildcard exists for anonymous reads. A cross-origin request carrying a
// token still goes through the configured allow-list, so it is deliberately
// not advertised here.
func TestPreflight_WildcardDoesNotAdvertiseAuthorization(t *testing.T) {
	resp := preflight(t, "https://unknown.example", "/api/v1/config/tariffs", "authorization", nil)
	assert.NotContains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Authorization")
}

// Only the single-namespace GET route. Everything else requires a token and
// keeps the allow-list.
func TestPreflight_OtherRoutesUnchangedFromUnknownOrigin(t *testing.T) {
	for _, path := range []string{"/api/v1/config", "/api/v1/config/namespaces/tariffs", "/api/v1/config/namespaces/tariffs/audit"} {
		t.Run(path, func(t *testing.T) {
			resp := preflight(t, "https://unknown.example", path, "content-type", nil)
			assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
				"an unknown origin gets no CORS grant on an authenticated route")
		})
	}
}

func TestPreflight_AllowListedOriginKeepsFullGrant(t *testing.T) {
	resp := preflight(t, "https://allowed.example", "/api/v1/config/tariffs", "content-type",
		[]string{"https://allowed.example"})

	assert.Equal(t, "https://allowed.example", resp.Header.Get("Access-Control-Allow-Origin"),
		"an allow-listed origin is still answered by name, not with the wildcard")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "PUT")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Authorization")
}

func TestIsNamespaceGetPath(t *testing.T) {
	cases := map[string]bool{
		"/api/v1/config/tariffs":                  true,
		"/api/v1/config/namespaces":               true, // a legal namespace name
		"/api/v1/config":                          false,
		"/api/v1/config/":                         false,
		"/api/v1/config/namespaces/tariffs":       false,
		"/api/v1/config/namespaces/tariffs/audit": false,
		"/healthz": false,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, want, isNamespaceGetPath(path))
		})
	}
}
