package market

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	return newTestAppWithConfig(t, Config{})
}

func newTestAppWithConfig(t *testing.T, override Config) *App {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		DatabasePath:    filepath.Join(root, "market.db"),
		ArtifactRoot:    filepath.Join(root, "artifacts"),
		PublicBaseURL:   "http://market.test",
		AdminToken:      "secret",
		ProxyToken:      "proxy-secret",
		MaxUploadBytes:  10 * 1024 * 1024,
		EnableLocalAuth: true,
	}
	if override.SSOJWTIssuer != "" {
		cfg.SSOJWTIssuer = override.SSOJWTIssuer
	}
	if override.SSOJWTPublicKeyFile != "" {
		cfg.SSOJWTPublicKeyFile = override.SSOJWTPublicKeyFile
	}
	if override.SSOJWTPublicKeyPEM != "" {
		cfg.SSOJWTPublicKeyPEM = override.SSOJWTPublicKeyPEM
	}
	if override.SSOJWTAudience != "" {
		cfg.SSOJWTAudience = override.SSOJWTAudience
	}
	if override.ArtifactStorage != "" {
		cfg.ArtifactStorage = override.ArtifactStorage
	}
	if override.S3Bucket != "" {
		cfg.S3Bucket = override.S3Bucket
	}
	if override.S3Region != "" {
		cfg.S3Region = override.S3Region
	}
	if override.S3Endpoint != "" {
		cfg.S3Endpoint = override.S3Endpoint
	}
	if override.OIDCLogoutURL != "" {
		cfg.OIDCLogoutURL = override.OIDCLogoutURL
	}
	if override.OIDCLogoutCallback != "" {
		cfg.OIDCLogoutCallback = override.OIDCLogoutCallback
	}
	app, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func TestOpenDefaultsToLocalArtifactStorage(t *testing.T) {
	app := newTestApp(t)
	if _, ok := app.artifactStorage.(*localArtifactStorage); !ok {
		t.Fatalf("artifact storage = %T, want *localArtifactStorage", app.artifactStorage)
	}
}

func TestRandomOIDCStateIsOpaqueHex(t *testing.T) {
	state, err := randomOIDCState()
	if err != nil {
		t.Fatalf("randomOIDCState() error = %v", err)
	}
	if len(state) != 64 {
		t.Fatalf("state length = %d, want 64", len(state))
	}
	for _, char := range state {
		if !strings.ContainsRune("0123456789abcdef", char) {
			t.Fatalf("state contains non-hex character %q", char)
		}
	}
	secondState, err := randomOIDCState()
	if err != nil {
		t.Fatalf("second randomOIDCState() error = %v", err)
	}
	if state == secondState {
		t.Fatal("two generated states must not match")
	}
}

func TestOIDCLogoutClearsLocalSessionAndRedirectsToIAM(t *testing.T) {
	app := newTestAppWithConfig(t, Config{
		OIDCLogoutURL:      "https://eiam.qiuer.net/auth/ssoLogout",
		OIDCLogoutCallback: "http://127.0.0.1:5173/",
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/logout", nil)
	req.AddCookie(&http.Cookie{Name: oidcSessionCookie, Value: "signed-session"})
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	wantLocation := "https://eiam.qiuer.net/auth/ssoLogout?callback=http%3A%2F%2F127.0.0.1%3A5173%2F"
	if rec.Header().Get("Location") != wantLocation {
		t.Fatalf("logout location = %q, want %q", rec.Header().Get("Location"), wantLocation)
	}
	cookies := rec.Result().Cookies()
	cleared := map[string]bool{}
	for _, cookie := range cookies {
		if cookie.MaxAge < 0 && cookie.Value == "" {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[oidcSessionCookie] || !cleared[oidcStateCookie] {
		t.Fatalf("logout cookies were not cleared: %+v", cookies)
	}
}

func TestOIDCLogoutURLValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"relative endpoint": {OIDCLogoutURL: "/auth/ssoLogout", OIDCLogoutCallback: "http://127.0.0.1:5173/"},
		"relative callback": {OIDCLogoutURL: "https://eiam.qiuer.net/auth/ssoLogout", OIDCLogoutCallback: "/"},
		"callback too long": {OIDCLogoutURL: "https://eiam.qiuer.net/auth/ssoLogout", OIDCLogoutCallback: "https://market.test/" + strings.Repeat("a", 110)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := oidcLogoutURL(cfg); err == nil {
				t.Fatal("oidcLogoutURL() error = nil, want validation error")
			}
		})
	}
}

func TestOIDCIdentityClaimsPreferHumanReadableValues(t *testing.T) {
	username, name := oidcIdentityClaims(map[string]any{
		"preferred_username": "agent-builder",
		"name":               "Agent Builder",
	}, "oidc-subject")
	if username != "agent-builder" || name != "Agent Builder" {
		t.Fatalf("identity = (%q, %q), want preferred username and name", username, name)
	}
	username, name = oidcIdentityClaims(map[string]any{"email": "builder@example.test"}, "oidc-subject")
	if username != "builder@example.test" || name != "builder@example.test" {
		t.Fatalf("fallback identity = (%q, %q), want email", username, name)
	}
}

func TestOIDCAdminUserIDsGrantAdminRole(t *testing.T) {
	if !oidcAdminUserID(map[string]any{"staffno": "129943", "roles": []any{}}, "129943, fallback-subject") {
		t.Fatal("staff number should be allowed")
	}
	if !oidcAdminUserID(map[string]any{"sub": "fallback-subject"}, "129943, fallback-subject") {
		t.Fatal("subject should be allowed")
	}
	if oidcAdminUserID(map[string]any{"staffno": "other-user"}, "129943, fallback-subject") {
		t.Fatal("unknown account should not be allowed")
	}
}

func TestNormalizePublishADPAllowsMissingManifest(t *testing.T) {
	app := newTestApp(t)
	for _, itemType := range []ItemType{TypeSkill, TypeCLITool} {
		req := PublishRequest{Type: itemType, ID: "optional-adp", Version: "1.0.0"}
		if err := normalizePublishADP(context.Background(), app.store, "http://market.test", &req, nil); err != nil {
			t.Fatalf("normalizePublishADP(%s) error = %v", itemType, err)
		}
		if req.ADPYAML != "" {
			t.Fatalf("normalizePublishADP(%s) adpYaml = %q, want empty", itemType, req.ADPYAML)
		}
	}
}

func TestRedactedOIDCClaimsRemovesSensitiveValues(t *testing.T) {
	claims := redactedOIDCClaims(map[string]any{
		"sub":            "user-123",
		"custom_token":   "should-not-log",
		"client_secret":  "should-not-log",
		"email":          "builder@example.test",
		"phone_number":   "+86-18000000000",
		"email_verified": true,
	})
	if claims["sub"] != "[redacted]" {
		t.Fatalf("sub = %#v, want [redacted]", claims["sub"])
	}
	if claims["custom_token"] != "[redacted]" || claims["client_secret"] != "[redacted]" || claims["email"] != "[redacted]" || claims["phone_number"] != "[redacted]" {
		t.Fatalf("sensitive claims = %#v, want redacted", claims)
	}
	if claims["email_verified"] != true {
		t.Fatalf("email_verified = %#v, want true", claims["email_verified"])
	}
}

func TestStoreUpsertOIDCUserUsesStaffNumberAndStableIdentity(t *testing.T) {
	app := newTestApp(t)
	first, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer:          "https://identity.example.test/oidc",
		Subject:         "053624",
		Username:        "zhengpuruo",
		DisplayName:     "Zheng Puruo",
		Email:           "zhengpuruo@example.test",
		ProviderAccount: "zhengpuruo",
		StaffNumber:     "129943",
	})
	if err != nil {
		t.Fatalf("UpsertOIDCUser() error = %v", err)
	}
	if first.ID != "129943" {
		t.Fatalf("user ID = %q, want staff number", first.ID)
	}
	if first.Username != "zhengpuruo" || first.DisplayName != "Zheng Puruo" || first.Role != "creator" {
		t.Fatalf("first user = %+v", first)
	}
	second, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer:      "https://identity.example.test/oidc",
		Subject:     "053624",
		Username:    "different-name",
		DisplayName: "Updated Display Name",
		Email:       "updated@example.test",
		StaffNumber: "129943",
		IsAdmin:     true,
	})
	if err != nil {
		t.Fatalf("second UpsertOIDCUser() error = %v", err)
	}
	if second.ID != first.ID || second.Username != "zhengpuruo" || second.DisplayName != "Updated Display Name" || second.Role != "admin" {
		t.Fatalf("second user = %+v", second)
	}
}

func TestStoreUpsertOIDCUserPreservesEmailVerificationWhenClaimIsMissing(t *testing.T) {
	app := newTestApp(t)
	first, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer: "https://identity.example.test/oidc", Subject: "email-verified-subject", Username: "email-verified", Email: "verified@example.test", EmailVerified: true, HasEmailVerified: true,
	})
	if err != nil {
		t.Fatalf("first UpsertOIDCUser() error = %v", err)
	}
	second, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer: "https://identity.example.test/oidc", Subject: "email-verified-subject", Email: "verified@example.test",
	})
	if err != nil {
		t.Fatalf("second UpsertOIDCUser() error = %v", err)
	}
	if first.ID != second.ID || !second.EmailVerified {
		t.Fatalf("second user = %+v, want preserved email verification", second)
	}
}

func TestOpenKeepsCatalogAvailableWhenOIDCProviderIsUnavailable(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	app, err := Open(ctx, Config{
		DatabasePath: filepath.Join(root, "market.db"), ArtifactRoot: filepath.Join(root, "artifacts"), PublicBaseURL: "http://market.test",
		OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "client", OIDCClientSecret: "secret", OIDCSessionSecret: "session-secret",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if app.currentOIDCClient() != nil {
		t.Fatal("OIDC client should not initialize when discovery is unavailable")
	}
	response := httptest.NewRecorder()
	app.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health response = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestStoreUpsertOIDCUserRekeysLegacyUserToStaffNumber(t *testing.T) {
	app := newTestApp(t)
	legacy, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer:   "https://identity.example.test/oidc",
		Subject:  "legacy-subject",
		Username: "legacy-user",
	})
	if err != nil {
		t.Fatalf("legacy UpsertOIDCUser() error = %v", err)
	}
	if !strings.HasPrefix(legacy.ID, "usr_") {
		t.Fatalf("legacy user ID = %q, want generated ID", legacy.ID)
	}
	rekeyed, err := app.store.UpsertOIDCUser(context.Background(), oidcUserProfile{
		Issuer:      "https://identity.example.test/oidc",
		Subject:     "legacy-subject",
		StaffNumber: "10053624",
	})
	if err != nil {
		t.Fatalf("rekeyed UpsertOIDCUser() error = %v", err)
	}
	if rekeyed.ID != "10053624" {
		t.Fatalf("rekeyed user ID = %q, want staff number", rekeyed.ID)
	}
}

func TestOpenS3ArtifactStorageRequiresBucket(t *testing.T) {
	root := t.TempDir()
	_, err := Open(context.Background(), Config{
		DatabasePath:    filepath.Join(root, "market.db"),
		ArtifactRoot:    filepath.Join(root, "artifacts"),
		ArtifactStorage: "s3",
		PublicBaseURL:   "http://market.test",
		MaxUploadBytes:  10 * 1024 * 1024,
	})
	if err == nil || !strings.Contains(err.Error(), "S3 bucket") {
		t.Fatalf("Open() error = %v, want missing S3 bucket error", err)
	}
}

func TestS3ArtifactStorageBuildsPrefixedPresignedDownloadURL(t *testing.T) {
	storage, err := newS3ArtifactStorage(Config{
		S3Bucket:          "market-artifacts",
		S3Region:          "ap-guangzhou",
		S3Endpoint:        "https://market-artifacts.cos.ap-guangzhou.myqcloud.com",
		S3Prefix:          "market",
		S3AccessKeyID:     "test-access-key",
		S3SecretAccessKey: "test-secret-key",
		S3PresignTTL:      time.Minute,
	})
	if err != nil {
		t.Fatalf("newS3ArtifactStorage() error = %v", err)
	}
	objectID, err := storage.ObjectID("skill/demo/1.0.0/universal-abc.zip")
	if err != nil {
		t.Fatalf("ObjectID() error = %v", err)
	}
	if objectID != "market/skill/demo/1.0.0/universal-abc.zip" {
		t.Fatalf("ObjectID() = %q", objectID)
	}
	downloadURL, err := storage.PresignGet(context.Background(), objectID)
	if err != nil {
		t.Fatalf("PresignGet() error = %v", err)
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	if parsed.Host != "market-artifacts.cos.ap-guangzhou.myqcloud.com" || parsed.Path != "/market/skill/demo/1.0.0/universal-abc.zip" {
		t.Fatalf("presigned URL = %s", downloadURL)
	}
	for _, key := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if parsed.Query().Get(key) == "" {
			t.Fatalf("presigned URL missing %s: %s", key, downloadURL)
		}
	}
}

func TestPublishSkillAndPublicAPIs(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	archive := zipArchive(t, map[string]string{
		"demo/SKILL.md": "# Demo\n",
	})
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "demo",
		Name:        "Demo Skill",
		Version:     "1.0.0",
		Description: "A demo skill",
		Readme:      "# Demo Skill\n",
		Tags:        []string{"demo"},
		ArchiveType: "zip",
		Metadata:    map[string]string{"author": "zenmind"},
		Dependencies: []MarketDependency{{
			Kind:        DependencySandboxImage,
			Phase:       DependencyPhaseRuntime,
			Required:    true,
			ID:          "python",
			Version:     ">=1.0.0",
			InstallHint: "Install the Python sandbox image.",
		}},
	}, archive)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/markets", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET markets status = %d body=%s", rec.Code, rec.Body.String())
	}
	var markets MarketsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &markets); err != nil {
		t.Fatalf("decode markets: %v", err)
	}
	if len(markets.Markets) != 8 {
		t.Fatalf("market count = %d, want 8: %+v", len(markets.Markets), markets.Markets)
	}
	archiveTypesByType := map[string][]string{}
	var petMarket *MarketInfo
	for index := range markets.Markets {
		archiveTypesByType[markets.Markets[index].Type] = markets.Markets[index].ArchiveTypes
		if markets.Markets[index].Type == string(TypePet) {
			petMarket = &markets.Markets[index]
		}
	}
	if got := strings.Join(archiveTypesByType[string(TypeSkill)], ","); got != "zip" {
		t.Fatalf("skill archive types = %q, want zip", got)
	}
	if got := strings.Join(archiveTypesByType[string(TypePlugin)], ","); got != "zip" {
		t.Fatalf("plugin archive types = %q, want zip", got)
	}
	if got := strings.Join(archiveTypesByType[string(TypeSandboxImage)], ","); got != "zip,tar.gz" {
		t.Fatalf("sandbox-image archive types = %q, want zip,tar.gz", got)
	}
	if petMarket == nil || len(petMarket.ArchiveTypes) != 1 || petMarket.ArchiveTypes[0] != "zip" {
		t.Fatalf("pet archive types = %+v, want [zip]", petMarket)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills?page=1&limit=10", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET skills status = %d body=%s", rec.Code, rec.Body.String())
	}
	var skills MarketItemsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &skills); err != nil {
		t.Fatalf("decode skills: %v", err)
	}
	if skills.Market != "skill" || len(skills.Items) != 1 || skills.Items[0].ID != "demo" || skills.Items[0].Version != "1.0.0" {
		t.Fatalf("unexpected skills response: %+v", skills)
	}
	if len(skills.Items[0].Dependencies) != 1 || skills.Items[0].Dependencies[0].Kind != DependencySandboxImage {
		t.Fatalf("dependency did not round-trip: %+v", skills.Items[0].Dependencies)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/desktop/catalog", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog DesktopCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Items) != 1 || catalog.Items[0].Assets["universal"].SHA256 == "" {
		t.Fatalf("unexpected desktop catalog: %+v", catalog)
	}
	if catalog.Items[0].Readme == "" || catalog.Items[0].PublishedAt.IsZero() || catalog.Items[0].UpdatedAt.IsZero() {
		t.Fatalf("desktop catalog missing detail fields: %+v", catalog.Items[0])
	}
	if len(catalog.Items[0].Dependencies) != 1 || catalog.Items[0].Metadata["author"] != "zenmind" {
		t.Fatalf("desktop catalog missing protocol fields: %+v", catalog.Items[0])
	}
	universalPlatform, ok := catalog.Items[0].Platforms["universal"]
	if !ok || universalPlatform.Platform != "universal" || len(universalPlatform.Dependencies) != 1 || universalPlatform.Metadata["author"] != "zenmind" {
		t.Fatalf("desktop catalog missing universal platform: %+v", catalog.Items[0].Platforms)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/demo/versions", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions status = %d body=%s", rec.Code, rec.Body.String())
	}
	var versions VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(versions.Versions) != 1 || versions.Versions[0].Version != "1.0.0" || len(versions.Versions[0].Dependencies) != 1 {
		t.Fatalf("unexpected versions: %+v", versions)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/demo/resolve?platform=darwin-arm64", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Asset == nil || resolved.Asset.Platform != "universal" || resolved.Asset.SHA256 == "" {
		t.Fatalf("unexpected resolve: %+v", resolved)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/demo/download", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
		t.Fatalf("download status = %d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

func TestGetPublicDoesNotLoadUnrelatedItems(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "target-skill",
		Name:        "Target Skill",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"target-skill/SKILL.md": "# Target\n"}))
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "malformed-skill",
		Name:        "Malformed Skill",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"malformed-skill/SKILL.md": "# Malformed\n"}))

	if _, err := app.store.db.ExecContext(context.Background(), `UPDATE items SET metadata_json = '{' WHERE type = ? AND id = ?`, TypeSkill, "malformed-skill"); err != nil {
		t.Fatalf("corrupt unrelated item metadata: %v", err)
	}

	item, err := app.store.GetPublic(context.Background(), TypeSkill, "target-skill", "")
	if err != nil {
		t.Fatalf("GetPublic() error = %v, want target item without loading unrelated items", err)
	}
	if item.ID != "target-skill" {
		t.Fatalf("GetPublic() id = %q, want target-skill", item.ID)
	}
}

func TestAdminReviewsFiltersStatusBeforeLoadingItems(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:         TypeSkill,
		ID:           "pending-skill",
		Name:         "Pending Skill",
		Version:      "1.0.0",
		ArchiveType:  "zip",
		ReviewStatus: ReviewStatusPending,
	}, zipArchive(t, map[string]string{"pending-skill/SKILL.md": "# Pending\n"}))
	publishMultipart(t, handler, PublishRequest{
		Type:         TypeSkill,
		ID:           "rejected-skill",
		Name:         "Rejected Skill",
		Version:      "1.0.0",
		ArchiveType:  "zip",
		ReviewStatus: ReviewStatusRejected,
	}, zipArchive(t, map[string]string{"rejected-skill/SKILL.md": "# Rejected\n"}))

	if _, err := app.store.db.ExecContext(context.Background(), `UPDATE items SET metadata_json = '{' WHERE type = ? AND id = ?`, TypeSkill, "rejected-skill"); err != nil {
		t.Fatalf("corrupt rejected item metadata: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reviews?status=pending", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending reviews status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var response CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pending reviews: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "pending-skill" {
		t.Fatalf("pending reviews = %+v, want only pending-skill", response.Items)
	}
}

func TestPublicMarketResponsesDoNotExposeReviewFields(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "public-review-demo",
		Name:        "Public Review Demo",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"public-review-demo/SKILL.md": "# Demo\n"}))

	for _, path := range []string{
		"/api/v1/catalog",
		"/api/v1/desktop/catalog",
		"/api/v1/skills",
		"/api/v1/skills/public-review-demo",
		"/api/v1/skills/public-review-demo/versions",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, field := range []string{"reviewStatus", "reviewNote", "reviewedAt", "reviewedBy", "creatorId"} {
			if strings.Contains(body, field) {
				t.Fatalf("GET %s leaked %s in body=%s", path, field, body)
			}
		}
	}
}

func TestPublishSkillPackageProfile(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "word-helper",
		Name:        "Word Helper",
		Version:     "1.0.0",
		Description: "Edit documents",
		ArchiveType: "zip",
		Skill: &SkillProfileSpec{
			Kind:     SkillKindSingle,
			Category: "document",
			Scenario: "productivity",
			Level:    "beginner",
		},
	}, zipArchive(t, map[string]string{"word/SKILL.md": "# Word\n"}))
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "excel-analyst",
		Name:        "Excel Analyst",
		Version:     "1.0.0",
		Description: "Analyze sheets",
		ArchiveType: "zip",
		Skill: &SkillProfileSpec{
			Kind:     SkillKindSingle,
			Category: "data",
			Scenario: "productivity",
			Level:    "intermediate",
		},
	}, zipArchive(t, map[string]string{"excel/SKILL.md": "# Excel\n"}))
	publishJSON(t, handler, "/api/v1/admin/skills/publish", PublishRequest{
		Type:        TypeSkill,
		ID:          "office-pack",
		Name:        "Office Pack",
		Version:     "1.0.0",
		Description: "Office skill package",
		Skill: &SkillProfileSpec{
			Kind:        SkillKindPackage,
			Category:    "office",
			Scenario:    "productivity",
			Level:       "beginner",
			PackageMode: SkillPackageModeCollection,
			Featured:    true,
			IncludedSkills: []SkillPackageItem{
				{ID: "word-helper"},
				{ID: "excel-analyst"},
			},
		},
	}, http.StatusOK)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/items/skill/office-pack", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET skill package status = %d body=%s", rec.Code, rec.Body.String())
	}
	var item PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode skill package: %v", err)
	}
	if item.Skill == nil || item.Skill.Kind != SkillKindPackage || item.Skill.Category != "office" || item.Skill.PackageMode != SkillPackageModeCollection || !item.Skill.Featured {
		t.Fatalf("unexpected skill profile: %+v", item.Skill)
	}
	if len(item.Skill.IncludedSkills) != 2 || item.Skill.IncludedSkills[0].ID != "word-helper" || item.Skill.IncludedSkills[0].Name != "Word Helper" {
		t.Fatalf("unexpected included skills: %+v", item.Skill.IncludedSkills)
	}
	if item.ADPInstallURL != "" || len(item.Assets) != 0 {
		t.Fatalf("collection package should not expose artifact install data: adp=%q assets=%+v", item.ADPInstallURL, item.Assets)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/office-pack/package/download", nil)
	authorizeMarketRequest(t, handler, req)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download skill package status = %d body=%s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("open skill package zip: %v", err)
	}
	entries := map[string]bool{}
	for _, file := range zr.File {
		entries[file.Name] = true
	}
	for _, name := range []string{
		"manifest.json",
		"skills/word-helper/word/SKILL.md",
		"skills/excel-analyst/excel/SKILL.md",
		"adp/word-helper.adp.yaml",
		"adp/excel-analyst.adp.yaml",
	} {
		if !entries[name] {
			t.Fatalf("skill package zip missing %s; entries=%+v", name, entries)
		}
	}
}

func TestPublicReadEndpointsRemainAnonymous(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "anonymous-demo",
		Name:        "Anonymous Demo",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"anonymous-demo/SKILL.md": "# Anonymous\n"}))

	cases := []struct {
		path       string
		wantStatus int
	}{
		{path: "/api/v1/catalog", wantStatus: http.StatusOK},
		{path: "/api/v1/desktop/catalog", wantStatus: http.StatusOK},
		{path: "/api/v1/skills", wantStatus: http.StatusOK},
		{path: "/api/v1/skills/anonymous-demo", wantStatus: http.StatusOK},
		{path: "/api/v1/skills/anonymous-demo/versions", wantStatus: http.StatusOK},
		{path: "/api/v1/skills/anonymous-demo/resolve", wantStatus: http.StatusUnauthorized},
		{path: "/api/v1/skills/anonymous-demo/download", wantStatus: http.StatusUnauthorized},
		{path: "/api/v1/skills/anonymous-demo/package/download", wantStatus: http.StatusUnauthorized},
		{path: "/api/v1/adp/skill/anonymous-demo", wantStatus: http.StatusUnauthorized},
		{path: "/npm/@zenmind-skill/anonymous-demo", wantStatus: http.StatusUnauthorized},
		{path: "/artifacts/skill/anonymous-demo/1.0.0/artifact.zip", wantStatus: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Fatalf("GET %s status = %d, want %d body=%s", tc.path, rec.Code, tc.wantStatus, rec.Body.String())
		}
	}
}

func TestVersionCanonicalizationAtAPIBoundaries(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	rec := publishMultipartRecordAt(t, handler, "/api/v1/admin/plugins/publish", PublishRequest{
		ID:          "calendar",
		Name:        "Calendar",
		Version:     "v1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"calendar","version":"v1.0.0"}`}))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("publish response did not return canonical version: %s", rec.Body.String())
	}

	publishMultipartAt(t, handler, "/api/v1/admin/pets/publish", PublishRequest{
		ID:          "spark",
		Name:        "Spark",
		Version:     "v2.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"spark/pet.json": `{"id":"spark","version":"2.0.0"}`, "spark/pet-idle.png": "png"}), http.StatusOK)

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/calendar", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("item status = %d body=%s", rec.Code, rec.Body.String())
	}
	var item PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	if item.Version != "1.0.0" {
		t.Fatalf("item version = %q, want 1.0.0", item.Version)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/calendar/versions", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions status = %d body=%s", rec.Code, rec.Body.String())
	}
	var versions VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(versions.Versions) != 1 || versions.Versions[0].Version != "1.0.0" {
		t.Fatalf("versions = %+v, want canonical 1.0.0", versions.Versions)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/calendar/resolve?version=v1.0.0", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Version != "1.0.0" || resolved.Asset == nil {
		t.Fatalf("resolve = %+v, want canonical version with asset", resolved)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/calendar/download?version=V1.0.0", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("download status = %d body=%s", rec.Code, rec.Body.String())
	}
	var recordedVersion string
	if err := app.store.db.QueryRowContext(context.Background(), `SELECT version FROM download_events WHERE item_type = ? AND item_id = ?`, TypePlugin, "calendar").Scan(&recordedVersion); err != nil {
		t.Fatalf("download event query: %v", err)
	}
	if recordedVersion != "1.0.0" {
		t.Fatalf("download event version = %q, want 1.0.0", recordedVersion)
	}

	rawUnpublish, _ := json.Marshal(map[string]string{"type": "plugin", "id": "calendar", "version": "v1.0.0"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", bytes.NewReader(rawUnpublish))
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCanonicalizeStoredVersionsMigratesLegacyRows(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := app.store.db.ExecContext(ctx, `INSERT INTO items (
		type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, website_kind, metadata_json, dependencies_json, protocol_json, published, published_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		TypeSkill, "legacy", "Legacy Skill", "Legacy description", "# Legacy", "v1.0.0", "", "", "", "{}", "[]", "{}", now, now)
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	_, err = app.store.db.ExecContext(ctx, `INSERT INTO versions (
		item_type, item_id, version, description, readme, metadata_json, dependencies_json, protocol_json, published, published_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		TypeSkill, "legacy", "1.0.0", "Canonical version", "# Canonical", "{}", "[]", "{}", now)
	if err != nil {
		t.Fatalf("insert canonical version: %v", err)
	}
	_, err = app.store.db.ExecContext(ctx, `INSERT INTO versions (
		item_type, item_id, version, description, readme, metadata_json, dependencies_json, protocol_json, published, published_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		TypeSkill, "legacy", "v1.0.0", "Legacy version", "# Legacy", "{}", "[]", "{}", now)
	if err != nil {
		t.Fatalf("insert legacy version: %v", err)
	}
	_, err = app.store.db.ExecContext(ctx, `INSERT INTO artifacts (
		item_type, item_id, version, platform_key, archive_type, asset_role, path, url, sha256, integrity, size_bytes, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		TypeSkill, "legacy", "v1.0.0", "universal", "zip", AssetRolePrimary, "/tmp/legacy.zip", "http://market.test/artifacts/skill/legacy/v1.0.0/universal.zip", "sha", "integrity", 12, now)
	if err != nil {
		t.Fatalf("insert legacy artifact: %v", err)
	}
	_, err = app.store.db.ExecContext(ctx, `INSERT INTO version_platforms (
		item_type, item_id, version, platform_key, os, arch, description, readme, min_desktop_version, metadata_json, dependencies_json, protocol_json, published, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		TypeSkill, "legacy", "v1.0.0", "universal", "", "", "Legacy platform", "", "", "{}", "[]", "{}", now, now)
	if err != nil {
		t.Fatalf("insert legacy platform: %v", err)
	}
	_, err = app.store.db.ExecContext(ctx, `INSERT INTO download_events (
		item_type, item_id, version, artifact_platform, user_agent, ip, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, TypeSkill, "legacy", "v1.0.0", "universal", "test", "127.0.0.1", now)
	if err != nil {
		t.Fatalf("insert legacy download: %v", err)
	}

	if err := app.store.canonicalizeStoredVersions(ctx); err != nil {
		t.Fatalf("canonicalizeStoredVersions: %v", err)
	}

	var legacyVersionRows int
	if err := app.store.db.QueryRowContext(ctx, `SELECT count(*) FROM versions WHERE item_type = ? AND item_id = ? AND version = ?`, TypeSkill, "legacy", "v1.0.0").Scan(&legacyVersionRows); err != nil {
		t.Fatalf("count legacy versions: %v", err)
	}
	if legacyVersionRows != 0 {
		t.Fatalf("legacy version rows = %d, want 0", legacyVersionRows)
	}

	item, err := app.store.GetPublic(ctx, TypeSkill, "legacy", "")
	if err != nil {
		t.Fatalf("GetPublic: %v", err)
	}
	public := publicItem(item)
	if public.Version != "1.0.0" {
		t.Fatalf("public version = %q, want 1.0.0", public.Version)
	}
	asset := public.Assets["universal"]
	if asset.URL != "http://market.test/artifacts/skill/legacy/v1.0.0/universal.zip" {
		t.Fatalf("asset URL = %q, want legacy path preserved", asset.URL)
	}
	if _, ok := public.Platforms["universal"]; !ok {
		t.Fatalf("platforms missing universal: %+v", public.Platforms)
	}
	artifact, err := app.store.GetArtifact(ctx, TypeSkill, "legacy", "v1.0.0", "universal")
	if err != nil {
		t.Fatalf("GetArtifact with legacy input: %v", err)
	}
	if artifact.Version != "1.0.0" {
		t.Fatalf("artifact version = %q, want 1.0.0", artifact.Version)
	}
	var eventVersion string
	if err := app.store.db.QueryRowContext(ctx, `SELECT version FROM download_events WHERE item_type = ? AND item_id = ?`, TypeSkill, "legacy").Scan(&eventVersion); err != nil {
		t.Fatalf("download event query: %v", err)
	}
	if eventVersion != "1.0.0" {
		t.Fatalf("download event version = %q, want 1.0.0", eventVersion)
	}
}

func TestPublicItemStatsAndDownloadCount(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	for _, version := range []string{"1.0.0", "2.0.0"} {
		publishMultipart(t, handler, PublishRequest{
			Type:        TypeSkill,
			ID:          "stats",
			Name:        "Stats Skill",
			Version:     version,
			Description: "Tracks stats",
			ArchiveType: "zip",
			Metadata:    map[string]string{"author": "ZenMind Labs"},
		}, zipArchive(t, map[string]string{"stats/SKILL.md": "# Stats " + version + "\n"}))
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog?type=skill", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Items) != 1 {
		t.Fatalf("catalog item count = %d, want 1: %+v", len(catalog.Items), catalog.Items)
	}
	item := catalog.Items[0]
	if item.Author != "ZenMind Labs" || item.CreatedAt.IsZero() || !item.CreatedAt.Equal(item.PublishedAt) {
		t.Fatalf("unexpected author/created fields: %+v", item)
	}
	if item.DownloadCount != 0 || item.FavoriteCount != 0 || item.Favorited {
		t.Fatalf("unexpected initial stats: %+v", item)
	}

	for _, version := range []string{"1.0.0", "2.0.0"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/stats/download?version="+version, nil)
		authorizeMarketRequest(t, handler, req)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
			t.Fatalf("download %s status = %d location=%q body=%s", version, rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/items/skill/stats", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	var detail PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.DownloadCount != 2 {
		t.Fatalf("detail downloadCount = %d, want 2: %+v", detail.DownloadCount, detail)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/stats/resolve", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Item.DownloadCount != 2 || resolved.Item.Author != "ZenMind Labs" || resolved.Item.CreatedAt.IsZero() {
		t.Fatalf("resolve item missing public stats: %+v", resolved.Item)
	}
}

func TestFavoriteItemsUseTrustedProxyUser(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "favorite-demo",
		Name:        "Favorite Demo",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"favorite-demo/SKILL.md": "# Favorite\n"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/favorite-demo/favorite", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("favorite without proxy status = %d, want 401 body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/skills/favorite-demo/favorite", nil)
	req.Header.Set("X-ZenMind-Market-Proxy-Token", "proxy-secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("favorite without user status = %d, want 401 body=%s", rec.Code, rec.Body.String())
	}

	aliceFavorite := favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusOK)
	if aliceFavorite.FavoriteCount != 1 || !aliceFavorite.Favorited {
		t.Fatalf("alice favorite response = %+v, want count 1 and favorited", aliceFavorite)
	}
	aliceFavorite = favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusOK)
	if aliceFavorite.FavoriteCount != 1 || !aliceFavorite.Favorited {
		t.Fatalf("repeat alice favorite response = %+v, want idempotent count 1", aliceFavorite)
	}
	bobFavorite := favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/favorite-demo/favorite", "bob", http.StatusOK)
	if bobFavorite.FavoriteCount != 2 || !bobFavorite.Favorited {
		t.Fatalf("bob favorite response = %+v, want count 2 and favorited", bobFavorite)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/items/skill/favorite-demo", nil)
	setProxyUser(req, "alice")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	var detail PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.FavoriteCount != 2 || !detail.Favorited {
		t.Fatalf("alice detail favorite state = %+v", detail)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/catalog?type=skill", nil)
	setProxyUser(req, "alice")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Items) != 1 || catalog.Items[0].FavoriteCount != 2 || !catalog.Items[0].Favorited {
		t.Fatalf("alice catalog favorite state = %+v", catalog.Items)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/favorite-demo/versions", nil)
	setProxyUser(req, "alice")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions status = %d body=%s", rec.Code, rec.Body.String())
	}
	var versions VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if versions.Item.FavoriteCount != 2 || !versions.Item.Favorited {
		t.Fatalf("versions item favorite state = %+v", versions.Item)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/items/skill/favorite-demo", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode anonymous detail: %v", err)
	}
	if detail.FavoriteCount != 2 || detail.Favorited {
		t.Fatalf("anonymous detail favorite state = %+v", detail)
	}

	aliceFavorite = favoriteRequest(t, handler, http.MethodDelete, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusOK)
	if aliceFavorite.FavoriteCount != 1 || aliceFavorite.Favorited {
		t.Fatalf("alice unfavorite response = %+v, want count 1 and not favorited", aliceFavorite)
	}
	aliceFavorite = favoriteRequest(t, handler, http.MethodDelete, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusOK)
	if aliceFavorite.FavoriteCount != 1 || aliceFavorite.Favorited {
		t.Fatalf("repeat alice unfavorite response = %+v, want idempotent count 1", aliceFavorite)
	}

	_ = favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/missing/favorite", "alice", http.StatusNotFound)
	_ = favoriteRequest(t, handler, http.MethodDelete, "/api/v1/skills/missing/favorite", "alice", http.StatusNotFound)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"favorite-demo","version":"1.0.0"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d body=%s", rec.Code, rec.Body.String())
	}
	_ = favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusNotFound)
}

func TestFavoriteItemsUseSSOJWTUser(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	app := newTestAppWithConfig(t, Config{
		SSOJWTIssuer:       "https://official.example.test",
		SSOJWTPublicKeyPEM: publicKeyPEM,
		SSOJWTAudience:     "zenmind-market-server",
	})
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "jwt-favorite-demo",
		Name:        "JWT Favorite Demo",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"jwt-favorite-demo/SKILL.md": "# Favorite\n"}))

	token := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "42",
		Email:    "jwt.user@example.test",
		Role:     "user",
		Scope:    "profile market tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/jwt-favorite-demo/favorite", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("favorite with JWT status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/items/skill/jwt-favorite-demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail with JWT status = %d body=%s", rec.Code, rec.Body.String())
	}
	var detail PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !detail.Favorited || detail.FavoriteCount != 1 {
		t.Fatalf("JWT viewer favorite state = %+v", detail)
	}

	noMarketScopeToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "43",
		Email:    "jwt.no-market@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/skills/jwt-favorite-demo/favorite", nil)
	req.Header.Set("Authorization", "Bearer "+noMarketScopeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("favorite without market scope status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLocalLoginSeparatesCreatorAndAdminReviewAccess(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type:         TypeSkill,
		ID:           "pending-demo",
		Name:         "Pending Demo",
		Version:      "1.0.0",
		ArchiveType:  "zip",
		ReviewStatus: ReviewStatusPending,
	}, zipArchive(t, map[string]string{"pending-demo/SKILL.md": "# Pending\n"}))
	publishMultipart(t, handler, PublishRequest{
		Type:         TypeSkill,
		ID:           "rejected-demo",
		Name:         "Rejected Demo",
		Version:      "1.0.0",
		ArchiveType:  "zip",
		ReviewStatus: ReviewStatusRejected,
	}, zipArchive(t, map[string]string{"rejected-demo/SKILL.md": "# Rejected\n"}))

	creatorToken := loginLocalUser(t, handler, "creator-a", "creator")
	rawPublish, _ := json.Marshal(PublishRequest{
		Type:         TypeWebsiteApp,
		ID:           "creator-submitted",
		Name:         "Creator Submitted",
		Version:      "1.0.0",
		WebsiteKind:  WebsiteKindExternal,
		Metadata:     map[string]string{"url": "https://example.test/app"},
		ReviewStatus: ReviewStatusApproved,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/creator/publish", bytes.NewReader(rawPublish))
	req.Header.Set("Authorization", "Bearer "+creatorToken)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("creator publish status = %d body=%s", rec.Code, rec.Body.String())
	}
	otherCreatorToken := loginLocalUser(t, handler, "creator-b", "creator")
	otherRawPublish, _ := json.Marshal(PublishRequest{
		Type:        TypeWebsiteApp,
		ID:          "other-submitted",
		Name:        "Other Submitted",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindExternal,
		Metadata:    map[string]string{"url": "https://example.test/other"},
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/creator/publish", bytes.NewReader(otherRawPublish))
	req.Header.Set("Authorization", "Bearer "+otherCreatorToken)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("other creator publish status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/creator/items", nil)
	req.Header.Set("Authorization", "Bearer "+creatorToken)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("creator items status = %d body=%s", rec.Code, rec.Body.String())
	}
	var creatorCatalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &creatorCatalog); err != nil {
		t.Fatalf("decode creator catalog: %v", err)
	}
	statuses := map[string]string{}
	for _, item := range creatorCatalog.Items {
		statuses[item.ID] = item.ReviewStatus
	}
	if len(creatorCatalog.Items) != 1 || statuses["creator-submitted"] != ReviewStatusPending {
		t.Fatalf("creator catalog statuses = %+v, want only creator-submitted pending", statuses)
	}
	if statuses["pending-demo"] != "" || statuses["rejected-demo"] != "" || statuses["other-submitted"] != "" {
		t.Fatalf("creator catalog leaked other items: %+v", statuses)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/reviews", nil)
	req.Header.Set("Authorization", "Bearer "+creatorToken)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("creator admin reviews status = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}

	adminToken := loginLocalUser(t, handler, "admin-a", "admin")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/reviews?status=pending", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reviews status = %d body=%s", rec.Code, rec.Body.String())
	}
	var reviewCatalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reviewCatalog); err != nil {
		t.Fatalf("decode admin reviews: %v", err)
	}
	adminPending := map[string]bool{}
	for _, item := range reviewCatalog.Items {
		adminPending[item.ID] = item.ReviewStatus == ReviewStatusPending
	}
	if len(reviewCatalog.Items) != 3 || !adminPending["pending-demo"] || !adminPending["creator-submitted"] || !adminPending["other-submitted"] {
		t.Fatalf("admin pending reviews = %+v, want all pending submissions", reviewCatalog.Items)
	}

	rawReview, _ := json.Marshal(map[string]string{"status": ReviewStatusApproved, "reviewedBy": "spoofed-admin"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/reviews/skill/pending-demo", bytes.NewReader(rawReview))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var reviewedItem PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &reviewedItem); err != nil {
		t.Fatalf("decode reviewed item: %v", err)
	}
	if reviewedItem.ReviewedBy != "admin-a" {
		t.Fatalf("reviewedBy = %q, want authenticated admin id", reviewedItem.ReviewedBy)
	}
}

func TestCreatorItemsUseStoredCreatorID(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	creatorToken := loginLocalUser(t, handler, "creator-owned", "creator")
	rawPublish, _ := json.Marshal(PublishRequest{
		Type:        TypeWebsiteApp,
		ID:          "creator-owned-app",
		Name:        "Creator Owned App",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindExternal,
		Metadata:    map[string]string{"url": "https://example.test/owned", "creatorId": "spoofed-creator"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/creator/publish", bytes.NewReader(rawPublish))
	req.Header.Set("Authorization", "Bearer "+creatorToken)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("creator publish status = %d body=%s", rec.Code, rec.Body.String())
	}
	var storedMetadata string
	if err := app.store.db.QueryRowContext(context.Background(), `SELECT metadata_json FROM items WHERE type = ? AND id = ?`, TypeWebsiteApp, "creator-owned-app").Scan(&storedMetadata); err != nil {
		t.Fatalf("read stored metadata: %v", err)
	}
	if strings.Contains(storedMetadata, "creatorId") {
		t.Fatalf("stored metadata contains reserved creatorId: %s", storedMetadata)
	}

	_, err := app.store.db.ExecContext(context.Background(), `UPDATE items SET metadata_json = ? WHERE type = ? AND id = ?`, `{"url":"https://example.test/owned"}`, TypeWebsiteApp, "creator-owned-app")
	if err != nil {
		t.Fatalf("clear item metadata creatorId: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/creator/items", nil)
	req.Header.Set("Authorization", "Bearer "+creatorToken)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("creator items status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode creator catalog: %v", err)
	}
	if len(catalog.Items) != 1 || catalog.Items[0].ID != "creator-owned-app" {
		t.Fatalf("creator catalog items = %+v, want creator-owned-app by stored creator id", catalog.Items)
	}
	if _, ok := catalog.Items[0].Metadata["creatorId"]; ok {
		t.Fatalf("public metadata leaked creatorId: %+v", catalog.Items[0].Metadata)
	}
}

func TestSSOJWTAdminAuthAndAudienceValidation(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	app := newTestAppWithConfig(t, Config{
		SSOJWTIssuer:       "https://official.example.test",
		SSOJWTPublicKeyPEM: publicKeyPEM,
		SSOJWTAudience:     "zenmind-market-server",
	})
	handler := app.Routes()

	adminToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Scope:    "profile market tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"missing","version":"1.0.0"}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin JWT status = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}

	wrongAudienceToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Scope:    "profile market tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"missing"}`))
	req.Header.Set("Authorization", "Bearer "+wrongAudienceToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience status = %d body=%s", rec.Code, rec.Body.String())
	}

	noMarketScopeToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"missing"}`))
	req.Header.Set("Authorization", "Bearer "+noMarketScopeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin without market scope status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCatalogHandlesConcurrentRequests(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	const itemCount = 10
	for i := 0; i < itemCount; i++ {
		id := fmt.Sprintf("demo-%02d", i)
		publishMultipart(t, handler, PublishRequest{
			Type:        TypeSkill,
			ID:          id,
			Name:        "Demo " + id,
			Version:     "1.0.0",
			Tags:        []string{"batch", id},
			ArchiveType: "zip",
		}, zipArchive(t, map[string]string{id + "/SKILL.md": "# " + id + "\n"}))
	}

	const concurrency = 16
	var wg sync.WaitGroup
	errs := make(chan string, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("request %d status = %d body=%s", index, rec.Code, rec.Body.String())
				return
			}
			var catalog CatalogResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
				errs <- fmt.Sprintf("request %d decode catalog: %v", index, err)
				return
			}
			if len(catalog.Items) != itemCount {
				errs <- fmt.Sprintf("request %d catalog item count = %d, want %d", index, len(catalog.Items), itemCount)
				return
			}
			for _, item := range catalog.Items {
				if len(item.Tags) == 0 {
					errs <- fmt.Sprintf("request %d item %s missing tags", index, item.ID)
					return
				}
				if item.Assets["universal"].SHA256 == "" {
					errs <- fmt.Sprintf("request %d item %s missing universal asset", index, item.ID)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestVersionsHandlesConcurrentRequests(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	for _, version := range []string{"1.0.0", "2.0.0"} {
		publishMultipart(t, handler, PublishRequest{
			Type:        TypeSkill,
			ID:          "versioned",
			Name:        "Versioned Skill",
			Version:     version,
			ArchiveType: "zip",
		}, zipArchive(t, map[string]string{"versioned/SKILL.md": "# Version " + version + "\n"}))
	}

	const concurrency = 16
	var wg sync.WaitGroup
	errs := make(chan string, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/versioned/versions", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("request %d status = %d body=%s", index, rec.Code, rec.Body.String())
				return
			}
			var response VersionsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				errs <- fmt.Sprintf("request %d decode versions: %v", index, err)
				return
			}
			if len(response.Versions) != 2 {
				errs <- fmt.Sprintf("request %d version count = %d, want 2", index, len(response.Versions))
				return
			}
			for _, version := range response.Versions {
				if version.Assets["universal"].SHA256 == "" {
					errs <- fmt.Sprintf("request %d version %s missing universal asset", index, version.Version)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestNpmPackument(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{Type: TypeSkill, ID: "demo", Name: "Demo", Version: "1.0.0", ArchiveType: "zip"}, zipArchive(t, map[string]string{"demo/SKILL.md": "# Demo\n"}))
	publishMultipart(t, handler, PublishRequest{Type: TypeAgent, ID: "assistant", Name: "Assistant", Version: "1.0.0", ArchiveType: "zip"}, zipArchive(t, map[string]string{"assistant/agent.yml": "name: Assistant\n"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/npm/@zenmind-skill/demo", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("npm status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"latest":"1.0.0"`) || !strings.Contains(rec.Body.String(), `"@zenmind-skill/demo"`) {
		t.Fatalf("unexpected packument: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/npm/@zenmind-agent/assistant", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent npm status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"@zenmind-agent/assistant"`) {
		t.Fatalf("unexpected agent packument: %s", rec.Body.String())
	}
}

func TestAdminAuthRejectsMissingToken(t *testing.T) {
	app := newTestApp(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"demo"}`))
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSandboxTemplateValidation(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	archive := zipArchive(t, map[string]string{
		"python/environment.json": `{"name":"python","image_repository":"python","image_tag":"3.12"}`,
		"python/files/Dockerfile": "FROM python:3.12\n",
	})
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSandboxImage,
		ID:          "python",
		Name:        "Python",
		Version:     "1.0.0",
		Description: "Python sandbox",
		ArchiveType: "zip",
	}, archive)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/desktop/catalog", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sandboxKind":"environment-template"`) {
		t.Fatalf("missing sandbox kind: %s", rec.Body.String())
	}
}

func TestEightMarketTypesPublishListResolveAndDownload(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	fixtures := []struct {
		path     string
		itemType ItemType
		id       string
		req      PublishRequest
		archive  []byte
	}{
		{
			path:     "skills",
			itemType: TypeSkill,
			id:       "shared-id",
			req: PublishRequest{
				ID:          "shared-id",
				Name:        "Shared Skill",
				Version:     "1.0.0",
				Description: "Skill with a shared id",
				ArchiveType: "zip",
			},
			archive: zipArchive(t, map[string]string{"skill/SKILL.md": "# Shared\n"}),
		},
		{
			path:     "plugins",
			itemType: TypePlugin,
			id:       "shared-id",
			req: PublishRequest{
				ID:          "shared-id",
				Name:        "Shared Plugin",
				Version:     "1.0.0",
				Description: "Plugin with a shared id",
				ArchiveType: "zip",
			},
			archive: zipArchive(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"shared-id","version":"1.0.0","scripts":{"deploy":"./deploy.sh","start":"./start.sh","stop":"./stop.sh"}}`}),
		},
		{
			path:     "agents",
			itemType: TypeAgent,
			id:       "planner",
			req: PublishRequest{
				ID:          "planner",
				Name:        "Planner Agent",
				Version:     "1.0.0",
				Description: "Task planning agent",
				ArchiveType: "zip",
				Dependencies: []MarketDependency{{
					Kind:      DependencyBuiltinService,
					Phase:     DependencyPhaseRuntime,
					Required:  true,
					ServiceID: "agent-platform",
				}, {
					Kind:     DependencySoftwarePackage,
					Phase:    DependencyPhaseRuntime,
					Required: true,
					ID:       "python-runtime",
					Version:  ">=3.12",
				}},
			},
			archive: zipArchive(t, map[string]string{"planner/agent.yml": "name: Planner\n"}),
		},
		{
			path:     "sandbox-images",
			itemType: TypeSandboxImage,
			id:       "python",
			req: PublishRequest{
				ID:          "python",
				Name:        "Python Sandbox",
				Version:     "1.0.0",
				Description: "Python environment template",
				ArchiveType: "zip",
				Dependencies: []MarketDependency{{
					Kind:      DependencyBuiltinService,
					Phase:     DependencyPhaseRuntime,
					Required:  true,
					ServiceID: "agent-container-hub",
				}},
			},
			archive: zipArchive(t, map[string]string{"python/environment.json": `{"name":"python","image_repository":"python","image_tag":"3.12"}`}),
		},
		{
			path:     "pets",
			itemType: TypePet,
			id:       "spark",
			req: PublishRequest{
				ID:          "spark",
				Name:        "Spark",
				Version:     "1.0.0",
				Description: "Animated pet",
				ArchiveType: "zip",
			},
			archive: zipArchive(t, map[string]string{"spark/pet.json": `{"id":"spark","version":"1.0.0"}`, "spark/pet-idle.png": "png"}),
		},
		{
			path:     "cli-tools",
			itemType: TypeCLITool,
			id:       "zmctl",
			req: PublishRequest{
				ID:          "zmctl",
				Name:        "ZenMind CLI",
				Version:     "1.0.0",
				Description: "CLI helper",
				ArchiveType: "zip",
				Install:     &MarketScriptSpec{Command: "brew install zmctl"},
				Detect:      &MarketDetectSpec{Commands: []string{"zmctl"}, VersionCommand: "zmctl --version"},
			},
			archive: zipArchive(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}),
		},
		{
			path:     "webapps",
			itemType: TypeWebsiteApp,
			id:       "docs",
			req: PublishRequest{
				ID:          "docs",
				Name:        "Docs",
				Version:     "1.0.0",
				Description: "Local docs website",
				WebsiteKind: WebsiteKindLocalApp,
				ArchiveType: "zip",
				Dependencies: []MarketDependency{{
					Kind:     DependencySystemRuntime,
					Phase:    DependencyPhaseRuntime,
					Required: true,
					Runtime:  "node",
				}},
			},
			archive: zipArchive(t, map[string]string{"docs/website.json": `{"id":"docs","version":"1.0.0"}`, "docs/index.html": "<h1>Docs</h1>"}),
		},
		{
			path:     "software-packages",
			itemType: TypeSoftwarePackage,
			id:       "python-runtime",
			req: PublishRequest{
				ID:          "python-runtime",
				Name:        "Python Runtime",
				Version:     "3.12.0",
				Description: "Portable Python runtime package",
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"python/bin/python": "#!/bin/sh\n"}),
		},
	}

	for _, fixture := range fixtures {
		publishMultipartAt(t, handler, "/api/v1/admin/"+fixture.path+"/publish", fixture.req, fixture.archive, http.StatusOK)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Items) != len(fixtures) {
		t.Fatalf("catalog item count = %d, want %d: %+v", len(catalog.Items), len(fixtures), catalog.Items)
	}
	seenTypes := map[string]bool{}
	for _, item := range catalog.Items {
		seenTypes[item.Type] = true
	}
	for _, fixture := range fixtures {
		if !seenTypes[string(fixture.itemType)] {
			t.Fatalf("catalog missing type %s: %+v", fixture.itemType, catalog.Items)
		}
	}

	for _, fixture := range fixtures {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/"+fixture.path, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s status = %d body=%s", fixture.path, rec.Code, rec.Body.String())
		}
		var list MarketItemsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list %s: %v", fixture.path, err)
		}
		if len(list.Items) != 1 || list.Items[0].Type != string(fixture.itemType) || list.Items[0].ID != fixture.id {
			t.Fatalf("unexpected list for %s: %+v", fixture.path, list)
		}

		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/items/"+string(fixture.itemType)+"/"+fixture.id, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("detail %s status = %d body=%s", fixture.path, rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/"+fixture.path+"/"+fixture.id+"/versions", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("versions %s status = %d body=%s", fixture.path, rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/"+fixture.path+"/"+fixture.id+"/resolve?platform=darwin-arm64", nil)
		authorizeMarketRequest(t, handler, req)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve %s status = %d body=%s", fixture.path, rec.Code, rec.Body.String())
		}
		var resolved ResolveResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
			t.Fatalf("decode resolve %s: %v", fixture.path, err)
		}
		if resolved.Item.Type != string(fixture.itemType) || resolved.Item.ID != fixture.id || resolved.Asset == nil || resolved.Asset.SHA256 == "" {
			t.Fatalf("unexpected resolve for %s: %+v", fixture.path, resolved)
		}
		if fixture.itemType == TypePet && (resolved.Asset.ArchiveType != "zip" || !strings.HasSuffix(resolved.Asset.URL, ".zip")) {
			t.Fatalf("pet resolve asset = %+v, want zip URL", resolved.Asset)
		}

		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/"+fixture.path+"/"+fixture.id+"/download", nil)
		authorizeMarketRequest(t, handler, req)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
			t.Fatalf("download %s status = %d location=%q body=%s", fixture.path, rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/website-apps/docs/resolve", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy website-apps resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var legacyResolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &legacyResolved); err != nil {
		t.Fatalf("decode legacy resolve: %v", err)
	}
	if legacyResolved.Item.Type != string(TypeWebsiteApp) || legacyResolved.Item.ID != "docs" {
		t.Fatalf("unexpected legacy resolve: %+v", legacyResolved)
	}
}

func TestExternalWebsiteAndCLIJsonPublish(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishJSON(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:      "ripgrep",
		Name:    "ripgrep",
		Version: "1.0.0",
		Install: &MarketScriptSpec{
			ScriptURL: "https://example.invalid/install.sh",
			SHA256:    "abc123",
		},
		Detect: &MarketDetectSpec{Commands: []string{"rg"}, VersionCommand: "rg --version"},
	}, http.StatusOK)

	publishJSON(t, handler, "/api/v1/admin/webapps/publish", PublishRequest{
		ID:          "external-docs",
		Name:        "External Docs",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindExternal,
		Metadata:    map[string]string{"url": "https://example.com/docs"},
		Dependencies: []MarketDependency{{
			Kind:       DependencyDesktopCapability,
			Phase:      DependencyPhaseRuntime,
			Required:   true,
			Capability: "webview",
		}},
	}, http.StatusOK)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scriptUrl":"https://example.invalid/install.sh"`) || !strings.Contains(rec.Body.String(), `"websiteKind":"external"`) {
		t.Fatalf("catalog missing JSON-only protocol fields: %s", rec.Body.String())
	}
}

func TestVersionPlatformsCarryPlatformSpecificProtocol(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishMultipartAt(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:          "zmctl",
		Name:        "ZenMind CLI",
		Version:     "1.0.0",
		Description: "Global CLI description",
		ArchiveType: "zip",
		Platform: &MarketPlatformSpec{
			Key:               "darwin-arm64",
			OS:                "darwin",
			Arch:              "arm64",
			Description:       "macOS Apple Silicon build",
			MinDesktopVersion: "1.2.0",
			Metadata:          map[string]string{"packageManager": "homebrew"},
			Dependencies: []MarketDependency{{
				Kind:     DependencySystemRuntime,
				Phase:    DependencyPhaseInstall,
				Required: true,
				Runtime:  "homebrew",
			}},
			Install: &MarketScriptSpec{Command: "brew install zmctl"},
			Detect:  &MarketDetectSpec{Commands: []string{"zmctl"}, VersionCommand: "zmctl --version"},
		},
	}, zipArchive(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}), http.StatusOK)

	publishMultipartAt(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:          "zmctl",
		Name:        "ZenMind CLI",
		Version:     "1.0.0",
		Description: "Global CLI description",
		ArchiveType: "zip",
		Platform: &MarketPlatformSpec{
			Key:               "linux-amd64",
			OS:                "linux",
			Arch:              "amd64",
			Description:       "Linux x64 build",
			MinDesktopVersion: "1.1.0",
			Metadata:          map[string]string{"packageManager": "apt"},
			Dependencies: []MarketDependency{{
				Kind:     DependencySystemCommand,
				Phase:    DependencyPhaseInstall,
				Required: true,
				Command:  "apt-get",
			}},
			Install: &MarketScriptSpec{Command: "sudo apt-get install zmctl"},
			Detect:  &MarketDetectSpec{Commands: []string{"zmctl"}, VersionCommand: "zmctl --version"},
		},
	}, zipArchive(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}), http.StatusOK)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog?type=cli-tool", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var catalog CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Items) != 1 {
		t.Fatalf("catalog item count = %d, want 1: %+v", len(catalog.Items), catalog.Items)
	}
	darwin := catalog.Items[0].Platforms["darwin-arm64"]
	linux := catalog.Items[0].Platforms["linux-amd64"]
	if darwin.Install == nil || darwin.Install.Command != "brew install zmctl" || darwin.Metadata["packageManager"] != "homebrew" || darwin.MinDesktopVersion != "1.2.0" {
		t.Fatalf("darwin platform did not round-trip: %+v", darwin)
	}
	if linux.Install == nil || linux.Install.Command != "sudo apt-get install zmctl" || linux.Metadata["packageManager"] != "apt" || linux.MinDesktopVersion != "1.1.0" {
		t.Fatalf("linux platform did not round-trip: %+v", linux)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cli-tools/zmctl/resolve?platform=darwin-arm64", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Asset == nil || resolved.Asset.Platform != "darwin-arm64" || resolved.PlatformSpec == nil || resolved.PlatformSpec.Install == nil || resolved.PlatformSpec.Install.Command != "brew install zmctl" {
		t.Fatalf("unexpected darwin resolve: %+v", resolved)
	}
}

func TestResolvePlatformSpecFallsBackWithoutArtifact(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishJSON(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:      "json-cli",
		Name:    "JSON CLI",
		Version: "1.0.0",
		Platform: &MarketPlatformSpec{
			Key:     "universal",
			Install: &MarketScriptSpec{Command: "install universal"},
		},
	}, http.StatusOK)
	publishJSON(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:      "json-cli",
		Name:    "JSON CLI",
		Version: "1.0.0",
		Platform: &MarketPlatformSpec{
			Key:     "linux",
			Install: &MarketScriptSpec{Command: "install linux"},
		},
	}, http.StatusOK)
	publishJSON(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:      "json-cli",
		Name:    "JSON CLI",
		Version: "1.0.0",
		Platform: &MarketPlatformSpec{
			Key:               "linux-amd64",
			OS:                "linux",
			Arch:              "amd64",
			MinDesktopVersion: "1.3.0",
			Install:           &MarketScriptSpec{Command: "install linux amd64"},
			Detect:            &MarketDetectSpec{Commands: []string{"json-cli"}, VersionCommand: "json-cli --version"},
		},
	}, http.StatusOK)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli-tools/json-cli/resolve?platform=linux-amd64-apt", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Asset != nil {
		t.Fatalf("json-only resolve returned asset: %+v", resolved.Asset)
	}
	if resolved.Platform != "linux-amd64" || resolved.PlatformSpec == nil || resolved.PlatformSpec.Install == nil || resolved.PlatformSpec.Install.Command != "install linux amd64" {
		t.Fatalf("unexpected linux-amd64 fallback resolve: %+v", resolved)
	}
	if resolved.PlatformSpec.MinDesktopVersion != "1.3.0" || resolved.PlatformSpec.Detect == nil || resolved.PlatformSpec.Detect.VersionCommand != "json-cli --version" {
		t.Fatalf("platform spec missing detail: %+v", resolved.PlatformSpec)
	}
}

func TestPluginValidatorAcceptsPluginAPIVersionManifest(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishMultipartAt(t, handler, "/api/v1/admin/plugins/publish", PublishRequest{
		ID:          "calendar",
		Version:     "v0.1.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"calendar/manifest.json": `{"pluginApiVersion":1,"id":"calendar","version":"v0.1.0"}`}), http.StatusOK)
}

func TestArchiveTypeContractRejectsNonZipInstallAssets(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	cases := []struct {
		name    string
		path    string
		req     PublishRequest
		archive []byte
	}{
		{
			name: "skill tar.gz",
			path: "/api/v1/admin/skills/publish",
			req: PublishRequest{
				ID:          "legacy-skill",
				Version:     "1.0.0",
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"skill/SKILL.md": "# Legacy\n"}),
		},
		{
			name: "plugin tar.gz",
			path: "/api/v1/admin/plugins/publish",
			req: PublishRequest{
				ID:          "legacy-plugin",
				Version:     "1.0.0",
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"legacy-plugin","version":"1.0.0"}`}),
		},
		{
			name: "agent semantic archive",
			path: "/api/v1/admin/agents/publish",
			req: PublishRequest{
				ID:          "legacy-agent",
				Version:     "1.0.0",
				ArchiveType: "agent",
			},
			archive: tarGz(t, map[string]string{"agent/agent.yml": "name: Legacy\n"}),
		},
		{
			name: "sandbox template semantic archive",
			path: "/api/v1/admin/sandbox-images/publish",
			req: PublishRequest{
				ID:          "legacy-sandbox",
				Version:     "1.0.0",
				ArchiveType: "sandbox-template",
			},
			archive: tarGz(t, map[string]string{"sandbox/environment.json": `{"name":"legacy","image_repository":"python","image_tag":"3.12"}`}),
		},
		{
			name: "pet tar.gz",
			path: "/api/v1/admin/pets/publish",
			req: PublishRequest{
				ID:          "legacy-pet",
				Version:     "1.0.0",
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"pet/pet.json": `{"id":"legacy-pet","version":"1.0.0"}`, "pet/pet-idle.png": "png"}),
		},
		{
			name: "cli tar.gz",
			path: "/api/v1/admin/cli-tools/publish",
			req: PublishRequest{
				ID:          "legacy-cli",
				Version:     "1.0.0",
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"bin/legacy": "#!/bin/sh\n"}),
		},
		{
			name: "website semantic archive",
			path: "/api/v1/admin/webapps/publish",
			req: PublishRequest{
				ID:          "legacy-webapp",
				Version:     "1.0.0",
				WebsiteKind: WebsiteKindLocalApp,
				ArchiveType: "website-app",
			},
			archive: tarGz(t, map[string]string{"app/website.json": `{"id":"legacy-webapp","version":"1.0.0"}`}),
		},
	}

	for _, tc := range cases {
		rec := publishMultipartRecordAt(t, handler, tc.path, tc.req, tc.archive)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":"invalid_artifact"`) {
			t.Fatalf("%s error = %s, want invalid_artifact", tc.name, rec.Body.String())
		}
	}
}

func TestContainerImageArtifactAcceptsTarGzAlias(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishMultipartAt(t, handler, "/api/v1/admin/sandbox-images/publish", PublishRequest{
		ID:          "runtime-image",
		Name:        "Runtime Image",
		Version:     "1.0.0",
		SandboxKind: SandboxKindContainerImage,
		PlatformKey: "darwin-arm64",
		ArchiveType: "container-image",
	}, tarGz(t, map[string]string{"image/manifest.json": `{"image":"runtime:1.0.0"}`}), http.StatusOK)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandbox-images/runtime-image/resolve?platform=darwin-arm64", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Asset == nil || resolved.Asset.ArchiveType != "tar.gz" || !strings.HasSuffix(resolved.Asset.URL, ".tar.gz") {
		t.Fatalf("container image asset = %+v, want tar.gz URL", resolved.Asset)
	}
	if resolved.Platform != "darwin-arm64" {
		t.Fatalf("resolved platform = %q, want darwin-arm64", resolved.Platform)
	}
}

func TestValidatorsRejectInvalidProtocolsAndArtifacts(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishJSON(t, handler, "/api/v1/admin/skills/publish", PublishRequest{
		ID:      "bad-skill",
		Version: "1.0.0",
		Install: &MarketScriptSpec{
			Command: "curl https://example.invalid/install.sh | sh",
		},
	}, http.StatusBadRequest)

	publishJSON(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:      "bad-cli",
		Version: "1.0.0",
		Install: &MarketScriptSpec{
			ScriptURL: "https://example.invalid/install.sh",
		},
	}, http.StatusBadRequest)

	publishJSON(t, handler, "/api/v1/admin/webapps/publish", PublishRequest{
		ID:          "bad-external",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindExternal,
	}, http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/agents/publish", PublishRequest{
		ID:          "bad-agent",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"agent/readme.md": "missing manifest"}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/plugins/publish", PublishRequest{
		ID:          "expected-plugin",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"other-plugin","version":"1.0.0"}`}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/pets/publish", PublishRequest{
		ID:          "bad-pet",
		Version:     "1.0.0",
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"pet/pet.json": `{"id":"bad-pet","version":"1.0.0"}`}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/webapps/publish", PublishRequest{
		ID:          "bad-local",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindLocalApp,
		ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"app/index.html": "<h1>Missing manifest</h1>"}), http.StatusBadRequest)
}

func TestADPManifestPublishNormalizeAndEndpoint(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishMultipartAt(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:          "zmctl",
		Name:        "ZenMind CLI",
		Version:     "1.0.0",
		Description: "CLI",
		ArchiveType: "zip",
		Platform: &MarketPlatformSpec{
			Key:  "darwin-arm64",
			OS:   "darwin",
			Arch: "arm64",
		},
	}, zipArchive(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}), http.StatusOK)

	publishMultipartAt(t, handler, "/api/v1/admin/cli-tools/publish", PublishRequest{
		ID:          "zmctl",
		Name:        "ZenMind CLI",
		Version:     "1.0.0",
		Description: "CLI",
		ArchiveType: "zip",
		Platform: &MarketPlatformSpec{
			Key:  "linux-amd64",
			OS:   "linux",
			Arch: "amd64",
		},
	}, zipArchive(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}), http.StatusOK)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/adp/cli-tool/zmctl?version=1.0.0", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("adp status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"macos-arm64:",
		"linux-x64:",
		"/api/v1/cli-tools/zmctl/download?version=1.0.0&platform=darwin-arm64",
		"/api/v1/cli-tools/zmctl/download?version=1.0.0&platform=linux-amd64",
		"- linux-x64",
		"- macos-arm64",
		"sha256:",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("adp manifest missing %q:\n%s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/catalog?type=cli-tool", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"adpInstallUrl":"/api/v1/adp/cli-tool/zmctl"`) {
		t.Fatalf("catalog missing adpInstallUrl: %s", rec.Body.String())
	}
}

func TestUnpublishLatestVersionFallsBackAndBlocksUnpublishedArtifacts(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	archive := zipArchive(t, map[string]string{"demo/SKILL.md": "# Demo\n"})
	for _, version := range []string{"1.2.0", "1.10.0", "2.0.0"} {
		publishMultipart(t, handler, PublishRequest{
			Type: TypeSkill, ID: "rollback-demo", Name: "Rollback Demo", Version: version, ArchiveType: "zip",
		}, archive)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/rollback-demo/resolve?version=2.0.0", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	var latest ResolveResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &latest) != nil || latest.Asset == nil {
		t.Fatalf("latest resolve status=%d response=%s", rec.Code, rec.Body.String())
	}
	directArtifactPath := strings.TrimPrefix(latest.Asset.URL, "http://market.test")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, directArtifactPath, nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("published artifact status=%d body=%s", rec.Code, rec.Body.String())
	}

	unpublish := func(version string, wantStatus int) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"rollback-demo","version":"`+version+`"}`))
		req.Header.Set("Authorization", "Bearer secret")
		handler.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("unpublish %s status = %d, want %d: %s", version, rec.Code, wantStatus, rec.Body.String())
		}
	}

	// A historical version is never eligible for the lifecycle action.
	unpublish("1.2.0", http.StatusBadRequest)
	unpublish("2.0.0", http.StatusOK)

	var storedLatest string
	if err := app.store.db.QueryRow(`SELECT latest_version FROM items WHERE type = ? AND id = ?`, TypeSkill, "rollback-demo").Scan(&storedLatest); err != nil {
		t.Fatalf("query latest version after fallback: %v", err)
	}
	if storedLatest != "1.10.0" {
		t.Fatalf("stored latest version after fallback = %q, want 1.10.0", storedLatest)
	}
	for version, wantPublished := range map[string]int{"1.2.0": 1, "1.10.0": 1, "2.0.0": 0} {
		var published int
		if err := app.store.db.QueryRow(`SELECT published FROM versions WHERE item_type = ? AND item_id = ? AND version = ?`, TypeSkill, "rollback-demo", version).Scan(&published); err != nil {
			t.Fatalf("query published state for %s: %v", version, err)
		}
		if published != wantPublished {
			t.Fatalf("published state for %s = %d, want %d", version, published, wantPublished)
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/rollback-demo", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback item status = %d: %s", rec.Code, rec.Body.String())
	}
	var item PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode fallback item: %v", err)
	}
	if item.Version != "1.10.0" {
		t.Fatalf("fallback version = %q, want 1.10.0", item.Version)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/rollback-demo/resolve", nil)
	authorizeMarketRequest(t, handler, req)
	handler.ServeHTTP(rec, req)
	var resolved ResolveResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resolved) != nil || resolved.Version != "1.10.0" || resolved.Asset == nil {
		t.Fatalf("fallback resolve status=%d response=%s", rec.Code, rec.Body.String())
	}

	for _, endpoint := range []string{
		"/api/v1/skills/rollback-demo/resolve?version=2.0.0",
		"/api/v1/skills/rollback-demo/download?version=2.0.0",
		"/api/v1/adp/skill/rollback-demo?version=2.0.0",
		directArtifactPath,
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, endpoint, nil)
		authorizeMarketRequest(t, handler, req)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unpublished endpoint %s status = %d, want 404: %s", endpoint, rec.Code, rec.Body.String())
		}
	}

	// Once the last public version is gone, the whole item disappears.
	unpublish("1.10.0", http.StatusOK)
	unpublish("1.2.0", http.StatusOK)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/rollback-demo", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("last-version item status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestADPRejectsManagedPathOutsideADPHome(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	publishMultipartAt(t, handler, "/api/v1/admin/skills/publish", PublishRequest{
		ID:          "bad-skill-path",
		Name:        "Bad Skill Path",
		Version:     "1.0.0",
		ArchiveType: "zip",
		ADPYAML: `schema: "0.1"
name: bad-skill-path-adp
packages:
  - id: bad-skill-path
    version: "1.0.0"
    x-zenmind-artifact: primary
    x-adp-managed-paths:
      - "/tmp/bad-skill-path"
    hooks:
      post:
        - sh: "echo install"
          os: [linux, macos]
`,
	}, zipArchive(t, map[string]string{"bad/SKILL.md": "# Bad\n"}), http.StatusBadRequest)
}

func TestADPRejectsLegacyHookSyntax(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	legacyCommand := "comm" + "and"
	scalarHook := "- " + `"echo install"`

	for name, hookLine := range map[string]string{
		"bad-hook-command": "        - " + legacyCommand + `: "echo install"`,
		"bad-hook-scalar":  "        " + scalarHook,
	} {
		publishMultipartAt(t, handler, "/api/v1/admin/skills/publish", PublishRequest{
			ID:          name,
			Name:        "Bad Hook",
			Version:     "1.0.0",
			ArchiveType: "zip",
			ADPYAML: strings.Join([]string{
				`schema: "0.1"`,
				"name: " + name + "-adp",
				"packages:",
				"  - id: " + name,
				`    version: "1.0.0"`,
				"    x-zenmind-artifact: primary",
				"    x-adp-managed-paths:",
				`      - "${ADP_HOME}/zenmind/skills/` + name + `/1.0.0"`,
				"    hooks:",
				"      post:",
				hookLine,
				"",
			}, "\n"),
		}, zipArchive(t, map[string]string{"bad/SKILL.md": "# Bad\n"}), http.StatusBadRequest)
	}
}

func TestNormalizeItemTypeAliases(t *testing.T) {
	cases := map[string]ItemType{
		"agent":             TypeAgent,
		"agents":            TypeAgent,
		"智能体":               TypeAgent,
		"webapp":            TypeWebsiteApp,
		"webapps":           TypeWebsiteApp,
		"website-app":       TypeWebsiteApp,
		"website-apps":      TypeWebsiteApp,
		"网站应用":              TypeWebsiteApp,
		"software-package":  TypeSoftwarePackage,
		"software-packages": TypeSoftwarePackage,
		"软件依赖包":             TypeSoftwarePackage,
	}
	for input, want := range cases {
		got, err := normalizeItemType(input)
		if err != nil {
			t.Fatalf("normalizeItemType(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeItemType(%q) = %q, want %q", input, got, want)
		}
	}
}

func favoriteRequest(t *testing.T, handler http.Handler, method, path, userID string, wantStatus int) PublicItem {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	setProxyUser(req, userID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		return PublicItem{}
	}
	var item PublicItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode favorite response: %v", err)
	}
	return item
}

func loginLocalUser(t *testing.T, handler http.Handler, userID, role string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"userId": userID, "role": role})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local login status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token string `json:"token"`
		User  struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode local login: %v", err)
	}
	if response.Token == "" || response.User.ID != userID || response.User.Role != role {
		t.Fatalf("local login response = %+v", response)
	}
	return response.Token
}

func authorizeMarketRequest(t *testing.T, handler http.Handler, req *http.Request) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+loginLocalUser(t, handler, "download-user", "creator"))
}

func setProxyUser(req *http.Request, userID string) {
	req.Header.Set("X-ZenMind-Market-Proxy-Token", "proxy-secret")
	req.Header.Set("X-ZenMind-User-ID", userID)
}

type testSSOJWTClaims struct {
	Issuer   string
	Audience string
	UserID   string
	Email    string
	Role     string
	Scope    string
	Expires  time.Time
}

func testSSOJWTKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyDER,
	})
	return privateKey, string(publicKeyPEM)
}

func signTestSSOJWT(t *testing.T, privateKey *rsa.PrivateKey, claims testSSOJWTClaims) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	claimsJSON, _ := json.Marshal(map[string]any{
		"iss":     claims.Issuer,
		"sub":     "user:" + claims.UserID,
		"aud":     claims.Audience,
		"iat":     time.Now().Unix(),
		"exp":     claims.Expires.Unix(),
		"jti":     "test-jti",
		"user_id": claims.UserID,
		"email":   claims.Email,
		"role":    claims.Role,
		"scope":   claims.Scope,
	})
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signedValue := headerPart + "." + payloadPart
	digest := sha256.Sum256([]byte(signedValue))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signedValue + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func publishMultipart(t *testing.T, handler http.Handler, metadata PublishRequest, archive []byte) {
	t.Helper()
	publishMultipartAt(t, handler, "/api/v1/admin/publish", metadata, archive, http.StatusOK)
}

func publishMultipartAt(t *testing.T, handler http.Handler, path string, metadata PublishRequest, archive []byte, wantStatus int) {
	t.Helper()
	rec := publishMultipartRecordAt(t, handler, path, metadata, archive)
	if rec.Code != wantStatus {
		t.Fatalf("publish %s status = %d, want %d body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
}

func publishMultipartRecordAt(t *testing.T, handler http.Handler, path string, metadata PublishRequest, archive []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if metadata.ADPYAML == "" {
		metadata.ADPYAML = testADPYAML(inferTestPublishType(path, metadata.Type), metadata.ID, metadata.Version, true)
	}
	rawMetadata, _ := json.Marshal(metadata)
	_ = writer.WriteField("metadata", string(rawMetadata))
	if metadata.ADPYAML != "" {
		adpPart, err := writer.CreateFormFile("adp", "adp.yaml")
		if err != nil {
			t.Fatalf("CreateFormFile adp: %v", err)
		}
		if _, err := adpPart.Write([]byte(metadata.ADPYAML)); err != nil {
			t.Fatalf("write adp part: %v", err)
		}
	}
	part, err := writer.CreateFormFile("artifact", "artifact.tar.gz")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func publishJSON(t *testing.T, handler http.Handler, path string, metadata PublishRequest, wantStatus int) {
	t.Helper()
	if metadata.ADPYAML == "" {
		metadata.ADPYAML = testADPYAML(inferTestPublishType(path, metadata.Type), metadata.ID, metadata.Version, false)
	}
	rawMetadata, _ := json.Marshal(metadata)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(rawMetadata))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("publish JSON %s status = %d, want %d body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
}

func inferTestPublishType(path string, itemType ItemType) ItemType {
	if itemType != "" {
		return itemType
	}
	switch {
	case strings.Contains(path, "/skills/"):
		return TypeSkill
	case strings.Contains(path, "/cli-tools/"):
		return TypeCLITool
	}
	normalized, err := normalizeItemType(path)
	if err == nil {
		return normalized
	}
	return ""
}

func testADPYAML(itemType ItemType, id string, version string, hasArtifact bool) string {
	if itemType != TypeSkill && itemType != TypeCLITool {
		return ""
	}
	id = sanitizeSlug(id)
	version = canonicalVersion(version)
	if id == "" {
		id = "test"
	}
	if version == "" {
		version = "1.0.0"
	}
	if !hasArtifact {
		return fmt.Sprintf("schema: \"0.1\"\nname: %s-adp\npackages:\n  - %s\n", id, id)
	}
	if itemType == TypeSkill {
		return fmt.Sprintf(`schema: "0.1"
name: %s-adp
packages:
  - id: %s
    version: "%s"
    x-zenmind-artifact: primary
    x-adp-managed-paths:
      - "${ADP_HOME}/zenmind/skills/%s/%s"
    hooks:
      post:
        - sh: "mkdir -p ${ADP_HOME}/zenmind/skills/%s/%s && cp -R ${ADP_PKG_DIR}/. ${ADP_HOME}/zenmind/skills/%s/%s"
          os: [linux, macos]
        - pwsh: "New-Item -ItemType Directory -Force -Path ${ADP_HOME}/zenmind/skills/%s/%s; Copy-Item -Recurse -Force ${ADP_PKG_DIR}/* ${ADP_HOME}/zenmind/skills/%s/%s"
          os: [windows]
`, id, id, version, id, version, id, version, id, version, id, version, id, version)
	}
	return fmt.Sprintf(`schema: "0.1"
name: %s-adp
packages:
  - id: %s
    version: "%s"
    x-zenmind-artifact: primary
    expose:
      %s: "bin/%s"
`, id, id, version, id, id)
}

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := io.Copy(w, bytes.NewReader([]byte(content))); err != nil {
			t.Fatalf("zip copy: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		payload := []byte(content)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload))}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := io.Copy(tw, bytes.NewReader(payload)); err != nil {
			t.Fatalf("copy: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestCommentLifecycleAndModeration(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{
		Type: TypeSkill, ID: "commented", Name: "Commented Skill", Version: "1.0.0", Description: "comment target", ArchiveType: "zip",
	}, zipArchive(t, map[string]string{"commented/SKILL.md": "# Commented\n"}))

	request := func(method, path, userID string, body any, admin bool) *httptest.ResponseRecorder {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			reader = bytes.NewReader(data)
		}
		req := httptest.NewRequest(method, path, reader)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if admin {
			req.Header.Set("Authorization", "Bearer secret")
		} else if userID != "" {
			setProxyUser(req, userID)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := request(http.MethodPost, "/api/v1/skills/commented/comments", "", map[string]string{"sentiment": "positive", "content": "anonymous comment"}, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create status = %d body=%s", rec.Code, rec.Body.String())
	}

	created := make([]ItemComment, 0, 2)
	for _, input := range []map[string]string{
		{"sentiment": "positive", "content": "This component works very well."},
		{"sentiment": "negative", "content": "This component needs more documentation."},
	} {
		rec := request(http.MethodPost, "/api/v1/skills/commented/comments", "alice", input, false)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
		}
		var comment ItemComment
		if err := json.Unmarshal(rec.Body.Bytes(), &comment); err != nil {
			t.Fatalf("decode created comment: %v", err)
		}
		created = append(created, comment)
	}

	rec := request(http.MethodGet, "/api/v1/skills/commented/comments", "", nil, false)
	var public CommentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode public comments: %v", err)
	}
	if rec.Code != http.StatusOK || public.Summary.Total != 2 || public.Summary.Positive != 1 || public.Summary.Negative != 1 || public.Summary.PositiveRate != 50 {
		t.Fatalf("public comments = status %d response %+v", rec.Code, public)
	}

	rec = request(http.MethodPatch, fmt.Sprintf("/api/v1/skills/commented/comments/%d", created[0].ID), "bob", map[string]string{"sentiment": "negative", "content": "Someone else's edited comment."}, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other user update status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = request(http.MethodPost, fmt.Sprintf("/api/v1/admin/comments/%d/moderate", created[0].ID), "", map[string]string{"status": "hidden"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("hide status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = request(http.MethodGet, "/api/v1/skills/commented/comments", "", nil, false)
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode comments after hide: %v", err)
	}
	if public.Summary.Total != 1 || public.Summary.Positive != 0 || public.Summary.Negative != 1 || public.Summary.PositiveRate != 0 || len(public.Comments) != 1 {
		t.Fatalf("hidden comment included in public stats: %+v", public)
	}

	rec = request(http.MethodGet, "/api/v1/admin/comments", "", nil, true)
	var adminResponse struct {
		Comments []ItemComment `json:"comments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &adminResponse); err != nil || len(adminResponse.Comments) != 2 {
		t.Fatalf("admin comments response = %s err=%v", rec.Body.String(), err)
	}

	rec = request(http.MethodPost, fmt.Sprintf("/api/v1/admin/comments/%d/moderate", created[0].ID), "", map[string]string{"status": "visible"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = request(http.MethodDelete, fmt.Sprintf("/api/v1/skills/commented/comments/%d", created[1].ID), "alice", nil, false)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = request(http.MethodGet, "/api/v1/skills/commented/comments", "alice", nil, false)
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode final comments: %v", err)
	}
	if public.Summary.Total != 1 || public.Summary.Positive != 1 || len(public.Comments) != 1 || !public.Comments[0].Mine {
		t.Fatalf("final comments = %+v", public)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
