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
	}, archive)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills?page=1&limit=10", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET skills status = %d body=%s", rec.Code, rec.Body.String())
	}
	var skills SkillAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &skills); err != nil {
		t.Fatalf("decode skills: %v", err)
	}
	if len(skills.Data) != 1 || skills.Data[0].Name != "demo" || skills.Data[0].LatestVersion != "1.0.0" {
		t.Fatalf("unexpected skills response: %+v", skills)
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
}

func TestNpmPackument(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	publishMultipart(t, handler, PublishRequest{Type: TypeSkill, ID: "demo", Name: "Demo", Version: "1.0.0", ArchiveType: "tar.gz"}, tarGz(t, map[string]string{"demo/SKILL.md": "# Demo\n"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/npm/@zenmind-skill/demo", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("npm status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"latest":"1.0.0"`) || !strings.Contains(rec.Body.String(), `"@zenmind-skill/demo"`) {
		t.Fatalf("unexpected packument: %s", rec.Body.String())
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
		Type:        TypeSandbox,
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

func publishMultipart(t *testing.T, handler http.Handler, metadata PublishRequest, archive []byte) {
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/publish", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d body=%s", rec.Code, rec.Body.String())
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
