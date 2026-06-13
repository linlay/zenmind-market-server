package market

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	archive := tarGz(t, map[string]string{
		"demo/SKILL.md": "# Demo\n",
	})
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSkill,
		ID:          "demo",
		Name:        "Demo Skill",
		Version:     "1.0.0",
		Description: "A demo skill",
		Tags:        []string{"demo"},
		ArchiveType: "tar.gz",
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
	if len(catalog.Items[0].Dependencies) != 1 || catalog.Items[0].Metadata["author"] != "zenmind" {
		t.Fatalf("desktop catalog missing protocol fields: %+v", catalog.Items[0])
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

func TestNpmPackument(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{Type: TypeSkill, ID: "demo", Name: "Demo", Version: "1.0.0", ArchiveType: "tar.gz"}, tarGz(t, map[string]string{"demo/SKILL.md": "# Demo\n"}))
	publishMultipart(t, handler, PublishRequest{Type: TypeAgent, ID: "assistant", Name: "Assistant", Version: "1.0.0", ArchiveType: "agent"}, tarGz(t, map[string]string{"assistant/agent.yml": "name: Assistant\n"}))

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
	archive := tarGz(t, map[string]string{
		"python/environment.json": `{"name":"python","image_repository":"python","image_tag":"3.12"}`,
		"python/files/Dockerfile": "FROM python:3.12\n",
	})
	publishMultipart(t, handler, PublishRequest{
		Type:        TypeSandboxImage,
		ID:          "python",
		Name:        "Python",
		Version:     "1.0.0",
		Description: "Python sandbox",
		ArchiveType: "sandbox-template",
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
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"skill/SKILL.md": "# Shared\n"}),
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
				ArchiveType: "tar.gz",
			},
			archive: tarGz(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"shared-id","version":"1.0.0","scripts":{"deploy":"./deploy.sh","start":"./start.sh","stop":"./stop.sh"}}`}),
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
				ArchiveType: "agent",
				Dependencies: []MarketDependency{{
					Kind:      DependencyBuiltinService,
					Phase:     DependencyPhaseRuntime,
					Required:  true,
					ServiceID: "agent-platform",
				}},
			},
			archive: tarGz(t, map[string]string{"planner/agent.yml": "name: Planner\n"}),
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
				ArchiveType: "sandbox-template",
				Dependencies: []MarketDependency{{
					Kind:      DependencyBuiltinService,
					Phase:     DependencyPhaseRuntime,
					Required:  true,
					ServiceID: "agent-container-hub",
				}},
			},
			archive: tarGz(t, map[string]string{"python/environment.json": `{"name":"python","image_repository":"python","image_tag":"3.12"}`}),
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
				ArchiveType: "pet",
			},
			archive: tarGz(t, map[string]string{"spark/pet.json": `{"id":"spark","version":"1.0.0"}`, "spark/pet-idle.png": "png"}),
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
				ArchiveType: "tar.gz",
				Install:     &MarketScriptSpec{Command: "brew install zmctl"},
				Detect:      &MarketDetectSpec{Commands: []string{"zmctl"}, VersionCommand: "zmctl --version"},
			},
			archive: tarGz(t, map[string]string{"bin/zmctl": "#!/bin/sh\n"}),
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
				ArchiveType: "website-app",
				Dependencies: []MarketDependency{{
					Kind:     DependencySystemRuntime,
					Phase:    DependencyPhaseRuntime,
					Required: true,
					Runtime:  "node",
				}},
			},
			archive: tarGz(t, map[string]string{"docs/website.json": `{"id":"docs","version":"1.0.0"}`, "docs/index.html": "<h1>Docs</h1>"}),
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
		ArchiveType: "agent",
	}, tarGz(t, map[string]string{"agent/readme.md": "missing manifest"}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/plugins/publish", PublishRequest{
		ID:          "expected-plugin",
		Version:     "1.0.0",
		ArchiveType: "tar.gz",
	}, tarGz(t, map[string]string{"plugin/manifest.json": `{"kind":"plugin","id":"other-plugin","version":"1.0.0"}`}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/pets/publish", PublishRequest{
		ID:          "bad-pet",
		Version:     "1.0.0",
		ArchiveType: "pet",
	}, tarGz(t, map[string]string{"pet/pet.json": `{"id":"bad-pet","version":"1.0.0"}`}), http.StatusBadRequest)

	publishMultipartAt(t, handler, "/api/v1/admin/webapps/publish", PublishRequest{
		ID:          "bad-local",
		Version:     "1.0.0",
		WebsiteKind: WebsiteKindLocalApp,
		ArchiveType: "website-app",
	}, tarGz(t, map[string]string{"app/index.html": "<h1>Missing manifest</h1>"}), http.StatusBadRequest)
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

func publishMultipart(t *testing.T, handler http.Handler, metadata PublishRequest, archive []byte) {
	t.Helper()
	publishMultipartAt(t, handler, "/api/v1/admin/publish", metadata, archive, http.StatusOK)
}

func publishMultipartAt(t *testing.T, handler http.Handler, path string, metadata PublishRequest, archive []byte, wantStatus int) {
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
	if rec.Code != wantStatus {
		t.Fatalf("publish %s status = %d, want %d body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
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
