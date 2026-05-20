package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sweeney/config/internal/config"
)

func TestLoadConfigSvc_Defaults(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)

	assert.Equal(t, 8282, cfg.Port)
	assert.Equal(t, "config.db", cfg.DBPath)
	assert.Equal(t, "http://localhost:8181", cfg.IdentityIssuerURL)
	assert.Equal(t, "http://localhost:8181", cfg.IdentityIssuer)
	assert.Equal(t, "http://localhost:8181", cfg.IdentityPublicURL)
	assert.Equal(t, 30*time.Second, cfg.BackupMinInterval)
	assert.False(t, cfg.IsProduction())
}

func TestLoadConfigSvc_AllEnvSet(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("PORT", "9090")
	t.Setenv("DB_PATH", "/tmp/test.db")
	t.Setenv("IDENTITY_ISSUER_URL", "http://localhost:8181")
	t.Setenv("IDENTITY_ISSUER", "http://localhost:8181")
	t.Setenv("OAUTH_CLIENT_ID", "config-spa")
	t.Setenv("IDENTITY_PUBLIC_URL", "http://localhost:8181")
	t.Setenv("CORS_ORIGINS", "http://localhost:3000")
	t.Setenv("RATE_LIMIT_DISABLED", "1")
	t.Setenv("R2_ACCOUNT_ID", "acct123")
	t.Setenv("R2_ACCESS_KEY_ID", "key123")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret123")
	t.Setenv("R2_BUCKET_NAME", "my-bucket")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, "/tmp/test.db", cfg.DBPath)
	assert.Equal(t, "http://localhost:8181", cfg.IdentityIssuerURL)
	assert.Equal(t, "http://localhost:8181", cfg.IdentityIssuer)
	assert.Equal(t, "config-spa", cfg.OAuthClientID)
	assert.Equal(t, []string{"http://localhost:3000"}, cfg.CORSOrigins)
	assert.True(t, cfg.RateLimitDisabled)
	assert.True(t, cfg.R2Configured())
}

func TestLoadConfigSvc_InvalidPort(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("PORT", "notanumber")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORT")
}

func TestLoadConfigSvc_Production_MissingIssuerURL(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "production")
	t.Setenv("IDENTITY_ISSUER_URL", "")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IDENTITY_ISSUER_URL")
}

func TestLoadConfigSvc_Production_RequiresHTTPS(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "production")
	t.Setenv("IDENTITY_ISSUER_URL", "http://id.example.com")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestLoadConfigSvc_Production_HTTPSValid(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "production")
	t.Setenv("IDENTITY_ISSUER_URL", "https://id.example.com")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, "https://id.example.com", cfg.IdentityIssuerURL)
	assert.True(t, cfg.IsProduction())
}

func TestLoadConfigSvc_IdentityIssuer_DefaultsToIssuerURL(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("IDENTITY_ISSUER_URL", "http://localhost:8181")
	// IDENTITY_ISSUER not set

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, cfg.IdentityIssuerURL, cfg.IdentityIssuer)
}

func TestLoadConfigSvc_IdentityPublicURL_DefaultsToIssuerURL(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("IDENTITY_ISSUER_URL", "http://localhost:8181")
	// IDENTITY_PUBLIC_URL not set

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, cfg.IdentityIssuerURL, cfg.IdentityPublicURL)
}

func TestLoadConfigSvc_IssuerURL_WithQueryString(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("IDENTITY_ISSUER_URL", "http://localhost:8181?foo=bar")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query")
}

func TestLoadConfigSvc_IssuerURL_WithPath(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("IDENTITY_ISSUER_URL", "http://localhost:8181/some/path")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path")
}

func TestLoadConfigSvc_JWKSCacheTTL_Valid(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("JWKS_CACHE_TTL", "10m")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, cfg.JWKSCacheTTL)
}

func TestLoadConfigSvc_JWKSCacheTTL_Invalid(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("JWKS_CACHE_TTL", "notaduration")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWKS_CACHE_TTL")
}

func TestLoadConfigSvc_BackupMinInterval_Valid(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("BACKUP_MIN_INTERVAL", "5m")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.BackupMinInterval)
}

func TestLoadConfigSvc_BackupMinInterval_Invalid(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("BACKUP_MIN_INTERVAL", "notaduration")

	_, err := config.LoadConfigSvc()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BACKUP_MIN_INTERVAL")
}

func TestLoadConfigSvc_CORSOrigins_ParsedAndTrimmed(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("CORS_ORIGINS", "http://localhost:3000, http://localhost:4000 ,http://localhost:5000")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, []string{
		"http://localhost:3000",
		"http://localhost:4000",
		"http://localhost:5000",
	}, cfg.CORSOrigins)
}

func TestLoadConfigSvc_RateLimitDisabled_1(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("RATE_LIMIT_DISABLED", "1")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.True(t, cfg.RateLimitDisabled)
}

func TestLoadConfigSvc_RateLimitDisabled_true(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("RATE_LIMIT_DISABLED", "true")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.True(t, cfg.RateLimitDisabled)
}

func TestLoadConfigSvc_TrustProxy_Cloudflare(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("TRUST_PROXY", "cloudflare")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Equal(t, "cloudflare", cfg.TrustProxy)
}

func TestLoadConfigSvc_TrustProxy_UnknownValue_Ignored(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("TRUST_PROXY", "nginx")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.Empty(t, cfg.TrustProxy)
}

func TestConfigSvc_R2Configured(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("R2_ACCOUNT_ID", "acct")
	t.Setenv("R2_ACCESS_KEY_ID", "key")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret")
	t.Setenv("R2_BUCKET_NAME", "bucket")

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.True(t, cfg.R2Configured())
}

func TestConfigSvc_R2Configured_MissingField(t *testing.T) {
	t.Setenv("IDENTITY_ENV", "development")
	t.Setenv("R2_ACCOUNT_ID", "acct")
	t.Setenv("R2_ACCESS_KEY_ID", "key")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret")
	// R2_BUCKET_NAME not set

	cfg, err := config.LoadConfigSvc()
	require.NoError(t, err)
	assert.False(t, cfg.R2Configured())
}
