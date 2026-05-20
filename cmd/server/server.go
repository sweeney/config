package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	commonauth "github.com/sweeney/identity/common/auth"
	"github.com/sweeney/identity/common/backup"
	"github.com/sweeney/identity/common/ratelimit"
	configdb "github.com/sweeney/config/db"
	"github.com/sweeney/config/internal/config"
	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/handler"
	"github.com/sweeney/config/internal/service"
	"github.com/sweeney/config/internal/store"
)

const (
	eventBackupSuccess = "backup_success"
	eventBackupFailure = "backup_failure"
)

func runServer(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			printUsage()
			return nil
		case "--list-backups":
			return listBackups()
		case "--restore-backup":
			key := ""
			if len(args) > 1 {
				key = args[1]
			}
			return restoreBackup(key)
		default:
			printUsage()
			return fmt.Errorf("unknown flag: %s", args[0])
		}
	}
	return runConfigServer()
}

func printUsage() {
	fmt.Println("Usage: config-server [flags]")
	fmt.Println()
	fmt.Println("The config service stores structured configuration as")
	fmt.Println("named JSON documents with per-namespace read/write role ACLs.")
	fmt.Println("It validates JWTs against the identity service's JWKS endpoint.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  (none)                  Start the HTTP server")
	fmt.Println("  --list-backups          List available R2 backups")
	fmt.Println("  --restore-backup [key]  Restore the DB from an R2 backup")
	fmt.Println("  --help                  Show this help")
	fmt.Println()
	fmt.Println("Environment variables:")
	fmt.Println("  PORT                    Listen port (default 8282)")
	fmt.Println("  DB_PATH                 SQLite file path (default config.db)")
	fmt.Println("  IDENTITY_ENV            development | production")
	fmt.Println("  IDENTITY_ISSUER_URL     Base URL of identity service (for JWKS)")
	fmt.Println("  IDENTITY_ISSUER         Expected JWT iss claim (defaults to IDENTITY_ISSUER_URL)")
	fmt.Println("  JWKS_CACHE_TTL          How long to cache JWKS (e.g. 5m)")
	fmt.Println("  BACKUP_MIN_INTERVAL     Per-write backup cooldown (e.g. 30s)")
	fmt.Println("  TRUST_PROXY             'cloudflare' to honour CF-Connecting-IP")
	fmt.Println("  CORS_ORIGINS            Comma-separated allowed origins")
	fmt.Println("  RATE_LIMIT_DISABLED     Set to 1 to disable rate limiting")
	fmt.Println("  R2_*                    R2 credentials for backups")
}

func runConfigServer() error {
	cfg, err := config.LoadConfigSvc()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	database, err := configdb.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	repo := store.NewConfigStore(database)

	var backupMgr domain.BackupService
	if cfg.R2Configured() {
		uploader, err := backup.NewR2Uploader(backup.R2Config{
			AccountID:       cfg.R2AccountID,
			AccessKeyID:     cfg.R2AccessKeyID,
			SecretAccessKey: cfg.R2SecretAccessKey,
			BucketName:      cfg.R2BucketName,
		})
		if err != nil {
			return fmt.Errorf("r2 uploader: %w", err)
		}
		mgr := backup.NewManager(backup.Config{
			DBPath:      cfg.DBPath,
			BucketName:  cfg.R2BucketName,
			Env:         string(cfg.Env),
			ServiceName: "config",
			MinInterval: cfg.BackupMinInterval,
		}, uploader, logBackupEvent)
		backupMgr = mgr
	} else {
		log.Println("warning: R2 backup not configured — backups disabled")
		backupMgr = &backup.NoopManager{}
	}

	verifier, err := commonauth.NewJWKSVerifier(commonauth.JWKSVerifierConfig{
		IssuerURL:        cfg.IdentityIssuerURL,
		Issuer:           cfg.IdentityIssuer,
		CacheTTL:         cfg.JWKSCacheTTL,
		RequiredAudience: cfg.RequiredAudience,
	})
	if err != nil {
		return fmt.Errorf("jwks verifier: %w", err)
	}
	if cfg.RequiredAudience == "" {
		log.Printf("config: verifying tokens via JWKS at %s/.well-known/jwks.json (expected iss=%s)",
			cfg.IdentityIssuerURL, cfg.IdentityIssuer)
	} else {
		log.Printf("config: verifying tokens via JWKS at %s/.well-known/jwks.json (expected iss=%s, aud=%s)",
			cfg.IdentityIssuerURL, cfg.IdentityIssuer, cfg.RequiredAudience)
	}

	svc := service.NewConfigService(repo, backupMgr)

	router := handler.NewRouter(handler.Deps{
		Service:           svc,
		Verifier:          verifier,
		Version:           version,
		IdentityPublicURL: cfg.IdentityPublicURL,
		OAuthClientID:     cfg.OAuthClientID,
	})
	if cfg.OAuthClientID != "" && cfg.IdentityPublicURL != "" {
		log.Printf("config: admin UI mounted at /; oauth client_id=%s, identity public url=%s",
			cfg.OAuthClientID, cfg.IdentityPublicURL)
	} else {
		log.Println("config: admin UI disabled (set OAUTH_CLIENT_ID and IDENTITY_PUBLIC_URL to enable)")
	}

	if cfg.Env == config.EnvDevelopment && len(cfg.CORSOrigins) == 0 {
		log.Println("WARNING: development mode + empty CORS_ORIGINS → ANY http://localhost:* origin is allowed; set IDENTITY_ENV=production or list explicit origins for non-dev hosts")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if m, ok := backupMgr.(*backup.Manager); ok {
		m.Start(ctx)
	}

	if cfg.RateLimitDisabled && cfg.IsProduction() {
		log.Println("WARNING: RATE_LIMIT_DISABLED ignored in production")
		cfg.RateLimitDisabled = false
	}
	var httpHandler http.Handler = router
	if !cfg.RateLimitDisabled {
		limiter := ratelimit.NewLimiter(5.0, 20, cfg.TrustProxy)
		httpHandler = limiter.Middleware(router)
		log.Println("rate limiting enabled")
	} else {
		log.Println("rate limiting disabled (RATE_LIMIT_DISABLED)")
	}

	httpHandler = securityHeaders(httpHandler, cfg.CORSOrigins, cfg.IdentityPublicURL, cfg.Env == config.EnvDevelopment)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      httpHandler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("config: listening on :%d [%s]", cfg.Port, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	var exitErr error
	select {
	case <-stop:
		log.Println("config: shutting down...")
	case err := <-errCh:
		log.Printf("config: listen error: %v", err)
		exitErr = err
	}
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && exitErr == nil {
		exitErr = err
	}
	return exitErr
}

func logBackupEvent(success bool, detail string) {
	outcome := eventBackupSuccess
	if !success {
		outcome = eventBackupFailure
	}
	if detail != "" {
		log.Printf("audit: %s user=system detail=%s", outcome, detail)
	} else {
		log.Printf("audit: %s user=system", outcome)
	}
}

func originAllowed(origin string, allowed map[string]bool, devMode bool) bool {
	if allowed[origin] {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	if u.Host != "localhost" && !strings.HasPrefix(u.Host, "localhost:") {
		return false
	}
	if allowed["http://localhost"] {
		return true
	}
	return devMode && len(allowed) == 0
}

func securityHeaders(next http.Handler, corsOrigins []string, identityURL string, devMode bool) http.Handler {
	allowed := make(map[string]bool, len(corsOrigins))
	for _, o := range corsOrigins {
		allowed[o] = true
	}

	spaCSP := "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'"
	if identityURL != "" {
		spaCSP += " " + identityURL
	}
	spaCSP += "; form-action 'self'"
	if identityURL != "" {
		spaCSP += " " + identityURL
	}
	spaCSP += "; base-uri 'self'; frame-ancestors 'none'"

	const apiCSP = "default-src 'none'; frame-ancestors 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")

		if isSPAPath(r.URL.Path) {
			w.Header().Set("Content-Security-Policy", spaCSP)
		} else {
			w.Header().Set("Content-Security-Policy", apiCSP)
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin != "" && originAllowed(origin, allowed, devMode) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Expose-Headers", "X-Read-Role, X-Write-Role")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isSPAPath(p string) bool {
	return p == "/" || p == "/spa-config.json" || strings.HasPrefix(p, "/static/")
}
