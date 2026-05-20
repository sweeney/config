package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment represents the deployment environment.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// ConfigSvcConfig holds runtime configuration for the config service, loaded
// from environment variables.
type ConfigSvcConfig struct {
	Env  Environment
	Port int

	DBPath string

	// IdentityIssuerURL is the base URL of the identity service used to fetch
	// JWKS from {IdentityIssuerURL}/.well-known/jwks.json.
	IdentityIssuerURL string

	// IdentityIssuer is the expected `iss` claim on incoming JWTs. Defaults
	// to IdentityIssuerURL if unset.
	IdentityIssuer string

	JWKSCacheTTL time.Duration

	BackupMinInterval time.Duration

	// RequiredAudience, when non-empty, forces the JWKS verifier to assert
	// the `aud` claim on every incoming token.
	RequiredAudience string

	// IdentityPublicURL is the URL the browser uses for OAuth flows. Defaults
	// to IdentityIssuerURL. Empty disables the admin UI.
	IdentityPublicURL string

	// OAuthClientID is the public OAuth client registered on identity for the
	// config admin SPA. Empty disables the admin UI.
	OAuthClientID string

	TrustProxy        string
	CORSOrigins       []string
	RateLimitDisabled bool

	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2BucketName      string
}

func LoadConfigSvc() (*ConfigSvcConfig, error) {
	cfg := &ConfigSvcConfig{
		Port:              8282,
		DBPath:            "config.db",
		BackupMinInterval: 30 * time.Second,
	}

	switch Environment(os.Getenv("IDENTITY_ENV")) {
	case EnvProduction:
		cfg.Env = EnvProduction
	default:
		cfg.Env = EnvDevelopment
	}

	var errs []error

	if v := os.Getenv("PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("PORT must be a valid integer, got %q", v))
		} else {
			cfg.Port = port
		}
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		cfg.DBPath = v
	}

	cfg.IdentityIssuerURL = os.Getenv("IDENTITY_ISSUER_URL")
	if cfg.IdentityIssuerURL == "" {
		if cfg.Env == EnvDevelopment {
			cfg.IdentityIssuerURL = "http://localhost:8181"
		} else {
			errs = append(errs, errors.New("IDENTITY_ISSUER_URL is required in production"))
		}
	}
	if cfg.Env == EnvProduction && cfg.IdentityIssuerURL != "" && !strings.HasPrefix(cfg.IdentityIssuerURL, "https://") {
		errs = append(errs, fmt.Errorf("IDENTITY_ISSUER_URL must be an https:// URL in production (got %q)", cfg.IdentityIssuerURL))
	}
	if cfg.IdentityIssuerURL != "" {
		if err := validateIssuerURL(cfg.IdentityIssuerURL); err != nil {
			errs = append(errs, fmt.Errorf("IDENTITY_ISSUER_URL: %w", err))
		}
	}

	cfg.IdentityIssuer = os.Getenv("IDENTITY_ISSUER")
	if cfg.IdentityIssuer == "" {
		cfg.IdentityIssuer = cfg.IdentityIssuerURL
	}

	cfg.RequiredAudience = os.Getenv("REQUIRED_AUDIENCE")

	cfg.IdentityPublicURL = os.Getenv("IDENTITY_PUBLIC_URL")
	if cfg.IdentityPublicURL == "" {
		cfg.IdentityPublicURL = cfg.IdentityIssuerURL
	}
	if cfg.IdentityPublicURL != "" {
		if err := validateIssuerURL(cfg.IdentityPublicURL); err != nil {
			errs = append(errs, fmt.Errorf("IDENTITY_PUBLIC_URL: %w", err))
		}
	}
	cfg.OAuthClientID = os.Getenv("OAUTH_CLIENT_ID")

	if v := os.Getenv("JWKS_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("JWKS_CACHE_TTL: %w", err))
		} else {
			cfg.JWKSCacheTTL = d
		}
	}

	if v := os.Getenv("BACKUP_MIN_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("BACKUP_MIN_INTERVAL: %w", err))
		} else {
			cfg.BackupMinInterval = d
		}
	}

	if v := os.Getenv("TRUST_PROXY"); v == "cloudflare" {
		cfg.TrustProxy = "cloudflare"
	}

	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				cfg.CORSOrigins = append(cfg.CORSOrigins, o)
			}
		}
	}

	if v := os.Getenv("RATE_LIMIT_DISABLED"); v == "1" || v == "true" {
		cfg.RateLimitDisabled = true
	}

	cfg.R2AccountID = os.Getenv("R2_ACCOUNT_ID")
	cfg.R2AccessKeyID = os.Getenv("R2_ACCESS_KEY_ID")
	cfg.R2SecretAccessKey = os.Getenv("R2_SECRET_ACCESS_KEY")
	cfg.R2BucketName = os.Getenv("R2_BUCKET_NAME")

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func validateIssuerURL(raw string) error {
	if strings.TrimSpace(raw) != raw {
		return errors.New("must not contain leading/trailing whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host is required")
	}
	if u.RawQuery != "" {
		return errors.New("must not contain a query string")
	}
	if u.Fragment != "" {
		return errors.New("must not contain a fragment")
	}
	p := strings.TrimSuffix(u.Path, "/")
	if p != "" {
		return fmt.Errorf("must not contain a path (got %q)", u.Path)
	}
	return nil
}

func (c *ConfigSvcConfig) R2Configured() bool {
	return c.R2AccountID != "" &&
		c.R2AccessKeyID != "" &&
		c.R2SecretAccessKey != "" &&
		c.R2BucketName != ""
}

func (c *ConfigSvcConfig) IsProduction() bool {
	return c.Env == EnvProduction
}
