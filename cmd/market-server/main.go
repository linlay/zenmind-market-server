package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/linlay/zenmind-market-server/internal/market"
)

const defaultSSOJWTPublicKeyFile = "configs/jwt-public.pem"

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("load .env: %v", err)
	}

	cfg := market.Config{
		Addr:                envString("MARKET_ADDR", ":8088"),
		DatabasePath:        envString("MARKET_DB_PATH", "data/market.db"),
		ArtifactRoot:        envString("MARKET_ARTIFACT_ROOT", "data/artifacts"),
		ArtifactStorage:     envString("MARKET_ARTIFACT_STORAGE", "local"),
		S3Bucket:            strings.TrimSpace(os.Getenv("MARKET_S3_BUCKET")),
		S3Region:            envString("MARKET_S3_REGION", "us-east-1"),
		S3Endpoint:          strings.TrimSpace(os.Getenv("MARKET_S3_ENDPOINT")),
		S3Prefix:            strings.TrimSpace(os.Getenv("MARKET_S3_PREFIX")),
		S3AccessKeyID:       strings.TrimSpace(os.Getenv("MARKET_S3_ACCESS_KEY_ID")),
		S3SecretAccessKey:   strings.TrimSpace(os.Getenv("MARKET_S3_SECRET_ACCESS_KEY")),
		S3SessionToken:      strings.TrimSpace(os.Getenv("MARKET_S3_SESSION_TOKEN")),
		S3PresignTTL:        envDuration("MARKET_S3_PRESIGN_TTL", 5*time.Minute),
		PublicBaseURL:       envString("MARKET_PUBLIC_BASE_URL", "http://localhost:8088"),
		AdminToken:          strings.TrimSpace(os.Getenv("MARKET_ADMIN_TOKEN")),
		ProxyToken:          strings.TrimSpace(os.Getenv("MARKET_PROXY_TOKEN")),
		SSOJWTIssuer:        strings.TrimSpace(os.Getenv("SSO_JWT_ISSUER")),
		SSOJWTPublicKeyFile: envString("SSO_JWT_PUBLIC_KEY_FILE", defaultSSOJWTPublicKeyFile),
		SSOJWTPublicKeyPEM:  strings.TrimSpace(os.Getenv("SSO_JWT_PUBLIC_KEY_PEM")),
		SSOJWTAudience:      envString("SSO_JWT_AUDIENCE", "zenmind-market-server"),
		OIDCIssuer:          strings.TrimSpace(os.Getenv("MARKET_OIDC_ISSUER")),
		OIDCClientID:        strings.TrimSpace(os.Getenv("MARKET_OIDC_CLIENT_ID")),
		OIDCClientSecret:    strings.TrimSpace(os.Getenv("MARKET_OIDC_CLIENT_SECRET")),
		OIDCRedirectURL:     strings.TrimSpace(os.Getenv("MARKET_OIDC_REDIRECT_URL")),
		OIDCSessionSecret:   strings.TrimSpace(os.Getenv("MARKET_OIDC_SESSION_SECRET")),
		OIDCScopes:          envString("MARKET_OIDC_SCOPES", "openid profile email"),
		OIDCRoleClaim:       envString("MARKET_OIDC_ROLE_CLAIM", "roles"),
		OIDCAdminRole:       envString("MARKET_OIDC_ADMIN_ROLE", "market-admin"),
		OIDCSuccessRedirect: envString("MARKET_OIDC_SUCCESS_REDIRECT", "/"),
		OIDCDebugClaims:     envBool("MARKET_OIDC_DEBUG_CLAIMS", false),
		EnableLocalAuth:     envBool("MARKET_ENABLE_LOCAL_AUTH", false),
		MaxUploadBytes:      envInt64("MARKET_MAX_UPLOAD_BYTES", 512*1024*1024),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := market.Open(ctx, cfg)
	if err != nil {
		log.Fatalf("open market app: %v", err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("close app: %v", err)
		}
	}()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("market server listening on %s", cfg.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("shutdown server: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve market app: %v", err)
		}
	}
}

func envString(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
