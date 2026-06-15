package market

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	app, err := Open(context.Background(), Config{
		DatabasePath:   filepath.Join(root, "market.db"),
		ArtifactRoot:   filepath.Join(root, "artifacts"),
		PublicBaseURL:  "http://market.test",
		AdminToken:     "secret",
		ProxyToken:     "proxy-secret",
		MaxUploadBytes: 10 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
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
	if len(markets.Markets) != 7 {
		t.Fatalf("market count = %d, want 7: %+v", len(markets.Markets), markets.Markets)
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
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
		t.Fatalf("download status = %d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
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

	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/unpublish", strings.NewReader(`{"type":"skill","id":"favorite-demo"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d body=%s", rec.Code, rec.Body.String())
	}
	_ = favoriteRequest(t, handler, http.MethodPost, "/api/v1/skills/favorite-demo/favorite", "alice", http.StatusNotFound)
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
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("npm status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"latest":"1.0.0"`) || !strings.Contains(rec.Body.String(), `"@zenmind-skill/demo"`) {
		t.Fatalf("unexpected packument: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/npm/@zenmind-agent/assistant", nil)
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

func TestSevenMarketTypesPublishListResolveAndDownload(t *testing.T) {
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
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
			t.Fatalf("download %s status = %d location=%q body=%s", fixture.path, rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/website-apps/docs/resolve", nil)
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

func TestNormalizeItemTypeAliases(t *testing.T) {
	cases := map[string]ItemType{
		"agent":        TypeAgent,
		"agents":       TypeAgent,
		"智能体":          TypeAgent,
		"webapp":       TypeWebsiteApp,
		"webapps":      TypeWebsiteApp,
		"website-app":  TypeWebsiteApp,
		"website-apps": TypeWebsiteApp,
		"网站应用":         TypeWebsiteApp,
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

func setProxyUser(req *http.Request, userID string) {
	req.Header.Set("X-ZenMind-Market-Proxy-Token", "proxy-secret")
	req.Header.Set("X-ZenMind-User-ID", userID)
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
	rawMetadata, _ := json.Marshal(metadata)
	_ = writer.WriteField("metadata", string(rawMetadata))
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

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
