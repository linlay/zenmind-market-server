package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type App struct {
	cfg   Config
	store *Store
}

func Open(ctx context.Context, cfg Config) (*App, error) {
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 512 * 1024 * 1024
	}
	if err := os.MkdirAll(cfg.ArtifactRoot, 0o755); err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, store: store}, nil
}

func (a *App) Close() error {
	return a.store.Close()
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/v1/markets", a.handleMarkets)
	mux.HandleFunc("GET /api/v1/catalog", a.handleCatalog)
	mux.HandleFunc("GET /api/v1/desktop/catalog", a.handleDesktopCatalog)
	mux.HandleFunc("GET /api/v1/items/{type}/{id}", a.handleItem)
	for _, route := range marketRouteDefinitions() {
		route := route
		mux.HandleFunc("GET /api/v1/"+route.Path, func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketList(w, r, route.Type)
		})
		mux.HandleFunc("GET /api/v1/"+route.Path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketItem(w, r, route.Type)
		})
		mux.HandleFunc("GET /api/v1/"+route.Path+"/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketVersions(w, r, route.Type)
		})
		mux.HandleFunc("GET /api/v1/"+route.Path+"/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketResolve(w, r, route.Type)
		})
		mux.HandleFunc("GET /api/v1/"+route.Path+"/{id}/download", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketDownload(w, r, route.Type)
		})
		mux.HandleFunc("POST /api/v1/"+route.Path+"/{id}/favorite", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketFavorite(w, r, route.Type, true)
		})
		mux.HandleFunc("DELETE /api/v1/"+route.Path+"/{id}/favorite", func(w http.ResponseWriter, r *http.Request) {
			a.handleMarketFavorite(w, r, route.Type, false)
		})
		mux.HandleFunc("POST /api/v1/admin/"+route.Path+"/publish", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
			a.handleTypedPublish(w, r, route.Type)
		}))
	}
	mux.HandleFunc("POST /api/v1/admin/publish", a.requireAdmin(a.handlePublish))
	mux.HandleFunc("POST /api/v1/admin/unpublish", a.requireAdmin(a.handleUnpublish))
	mux.HandleFunc("/npm/", a.handleNPM)
	mux.HandleFunc("/artifacts/", a.handleArtifacts)
	return securityHeaders(mux)
}

func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleMarkets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, MarketsResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Markets: marketInfos()})
}

func (a *App) handleCatalog(w http.ResponseWriter, r *http.Request) {
	var itemType ItemType
	if rawType := strings.TrimSpace(r.URL.Query().Get("type")); rawType != "" {
		normalized, err := normalizeItemType(rawType)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
			return
		}
		itemType = normalized
	}
	items, err := a.store.ListPublic(r.Context(), itemType, a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	result := publicItems(items)
	sortPublicItems(result)
	writeJSON(w, http.StatusOK, CatalogResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Items: result})
}

func (a *App) handleDesktopCatalog(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListPublic(r.Context(), "", a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	response := DesktopCatalogResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC()}
	for _, item := range items {
		public := publicItem(item)
		response.Items = append(response.Items, DesktopCatalogItem{
			ID:                public.ID,
			Type:              public.Type,
			Name:              public.Name,
			Version:           public.Version,
			Description:       public.Description,
			Readme:            public.Readme,
			Tags:              public.Tags,
			Author:            public.Author,
			MinDesktopVersion: public.MinDesktopVersion,
			SandboxKind:       public.SandboxKind,
			WebsiteKind:       public.WebsiteKind,
			Assets:            public.Assets,
			Platforms:         public.Platforms,
			Dependencies:      public.Dependencies,
			Metadata:          public.Metadata,
			Install:           public.Install,
			Uninstall:         public.Uninstall,
			Detect:            public.Detect,
			CreatedAt:         public.CreatedAt,
			PublishedAt:       public.PublishedAt,
			UpdatedAt:         public.UpdatedAt,
			DownloadCount:     public.DownloadCount,
			FavoriteCount:     public.FavoriteCount,
			Favorited:         public.Favorited,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleItem(w http.ResponseWriter, r *http.Request) {
	itemType, err := normalizeItemType(r.PathValue("type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
		return
	}
	item, err := a.store.GetPublic(r.Context(), itemType, sanitizeSlug(r.PathValue("id")), a.viewerUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, publicItem(item))
}

func (a *App) handleMarketList(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	page := intQuery(r, "page", 1)
	limit := intQuery(r, "limit", 100)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	items, err := a.store.ListPublic(r.Context(), itemType, a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	result := publicItems(items)
	sortPublicItems(result)
	total := len(result)
	start, end := pageWindow(page, limit, total)
	writeJSON(w, http.StatusOK, MarketItemsResponse{
		SchemaVersion: 1,
		Market:        string(itemType),
		GeneratedAt:   time.Now().UTC(),
		Items:         result[start:end],
		Pagination:    Pagination{Page: page, Limit: limit, Total: total},
	})
}

func (a *App) handleMarketItem(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	item, err := a.store.GetPublic(r.Context(), itemType, sanitizeSlug(r.PathValue("id")), a.viewerUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, publicItem(item))
}

func (a *App) handleMarketVersions(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	id := sanitizeSlug(r.PathValue("id"))
	item, err := a.store.GetPublic(r.Context(), itemType, id, a.viewerUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	versions, err := a.store.ListVersions(r.Context(), itemType, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, VersionsResponse{SchemaVersion: 1, Item: publicItem(item), Versions: versions})
}

func (a *App) handleMarketResolve(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	id := sanitizeSlug(r.PathValue("id"))
	item, err := a.store.GetPublic(r.Context(), itemType, id, a.viewerUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	version := canonicalVersion(r.URL.Query().Get("version"))
	if version == "" {
		version = item.LatestVersion
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	platformSpec, platformErr := a.store.GetPlatform(r.Context(), itemType, id, version, platform)
	if platformErr != nil && !errors.Is(platformErr, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "store_error", platformErr.Error())
		return
	}
	artifact, err := a.store.GetArtifact(r.Context(), itemType, id, version, platform)
	if errors.Is(err, sql.ErrNoRows) {
		response := ResolveResponse{SchemaVersion: 1, Item: publicItem(item), Version: version, Platform: platform}
		if platformErr == nil {
			response.Platform = platformSpec.Platform
			response.PlatformSpec = &platformSpec
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	asset := PublicAsset{
		URL:         artifact.URL,
		SHA256:      artifact.SHA256,
		Integrity:   artifact.Integrity,
		SizeBytes:   artifact.SizeBytes,
		ArchiveType: artifact.ArchiveType,
		Platform:    artifact.PlatformKey,
		Role:        artifact.AssetRole,
	}
	response := ResolveResponse{SchemaVersion: 1, Item: publicItem(item), Version: artifact.Version, Platform: artifact.PlatformKey, Asset: &asset}
	if platformErr == nil {
		response.PlatformSpec = &platformSpec
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleMarketDownload(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	a.downloadArtifact(w, r, itemType, sanitizeSlug(r.PathValue("id")), r.URL.Query().Get("version"), r.URL.Query().Get("platform"))
}

func (a *App) handleMarketFavorite(w http.ResponseWriter, r *http.Request, itemType ItemType, favorite bool) {
	id := sanitizeSlug(r.PathValue("id"))
	userID := a.viewerUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "trusted proxy user required")
		return
	}
	var err error
	if favorite {
		err = a.store.FavoriteItem(r.Context(), itemType, id, userID)
	} else {
		err = a.store.UnfavoriteItem(r.Context(), itemType, id, userID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	item, err := a.store.GetPublic(r.Context(), itemType, id, userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, publicItem(item))
}

func (a *App) handlePublish(w http.ResponseWriter, r *http.Request) {
	a.handlePublishWithType(w, r, "")
}

func (a *App) handleTypedPublish(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	a.handlePublishWithType(w, r, itemType)
}

func (a *App) handlePublishWithType(w http.ResponseWriter, r *http.Request, forcedType ItemType) {
	var req PublishRequest
	var artifact *storedArtifact
	contentType := r.Header.Get("content-type")
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadBytes)
		if err := r.ParseMultipartForm(a.cfg.MaxUploadBytes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_multipart", err.Error())
			return
		}
		if err := json.Unmarshal([]byte(r.FormValue("metadata")), &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
			return
		}
		if err := forcePublishType(&req, forcedType); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
			return
		}
		if err := validatePublishRequest(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
			return
		}
		file, header, err := r.FormFile("artifact")
		if err == nil {
			defer file.Close()
			artifact, err = saveAndValidateArtifact(a.cfg.ArtifactRoot, a.cfg.PublicBaseURL, req, file, header)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_artifact", err.Error())
				return
			}
		}
	} else {
		if err := json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if err := forcePublishType(&req, forcedType); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
			return
		}
		if err := validatePublishRequest(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
			return
		}
	}
	if err := validateArtifactRequirement(req, artifact != nil); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_artifact", err.Error())
		return
	}
	if err := validateArchiveTypeContract(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_artifact", err.Error())
		return
	}
	if err := a.store.Publish(r.Context(), req, artifact); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": map[string]any{"type": req.Type, "id": req.ID, "version": req.Version}})
}

func (a *App) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	itemType, err := normalizeItemType(req.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
		return
	}
	if err := a.store.Unpublish(r.Context(), itemType, sanitizeSlug(req.ID), canonicalVersion(req.Version)); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleNPM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	rawName := strings.TrimPrefix(r.URL.Path, "/npm/")
	name, _ := url.PathUnescape(rawName)
	itemType, id, err := parseNpmPackageName(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	item, err := a.store.GetPublic(r.Context(), itemType, id, "")
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "package not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, npmPackument(item))
}

func (a *App) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	relative := strings.TrimPrefix(r.URL.Path, "/artifacts/")
	if relative == "" || strings.Contains(relative, "..") {
		writeError(w, http.StatusBadRequest, "invalid_path", "invalid artifact path")
		return
	}
	fullPath := filepath.Join(a.cfg.ArtifactRoot, filepath.FromSlash(relative))
	root, _ := filepath.Abs(a.cfg.ArtifactRoot)
	resolved, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "invalid_path", "invalid artifact path")
		return
	}
	http.ServeFile(w, r, resolved)
}

func (a *App) downloadArtifact(w http.ResponseWriter, r *http.Request, itemType ItemType, id, version, platform string) {
	version = canonicalVersion(version)
	artifact, err := a.store.GetArtifact(r.Context(), itemType, id, version, platform)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "artifact not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	a.store.RecordDownload(r.Context(), itemType, id, artifact.Version, artifact.PlatformKey, r.UserAgent(), requestIP(r))
	http.Redirect(w, r, artifact.URL, http.StatusFound)
}

func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authorizedAdmin(r) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "admin token required")
	}
}

func (a *App) authorizedAdmin(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("authorization"))
	if a.cfg.AdminToken != "" && strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer")), a.cfg.AdminToken) {
		return true
	}
	if a.cfg.ProxyToken == "" || r.Header.Get("X-ZenMind-Market-Proxy-Token") != a.cfg.ProxyToken {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(r.Header.Get("X-ZenMind-User-Role")))
	return role == "admin"
}

func (a *App) viewerUserID(r *http.Request) string {
	if a.cfg.ProxyToken == "" || r.Header.Get("X-ZenMind-Market-Proxy-Token") != a.cfg.ProxyToken {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-ZenMind-User-ID"))
}

func intQuery(r *http.Request, key string, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func pageWindow(page, limit, total int) (int, int) {
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return start, end
}

func forcePublishType(req *PublishRequest, forcedType ItemType) error {
	if forcedType == "" {
		return nil
	}
	if strings.TrimSpace(string(req.Type)) == "" {
		req.Type = forcedType
		return nil
	}
	itemType, err := normalizeItemType(string(req.Type))
	if err != nil {
		return err
	}
	if itemType != forcedType {
		return errors.New("publish type does not match endpoint")
	}
	req.Type = forcedType
	return nil
}

type marketRoute struct {
	Type          ItemType
	Path          string
	IncludeInInfo bool
}

func marketRouteDefinitions() []marketRoute {
	return []marketRoute{
		{Type: TypeSkill, Path: "skills", IncludeInInfo: true},
		{Type: TypePlugin, Path: "plugins", IncludeInInfo: true},
		{Type: TypeAgent, Path: "agents", IncludeInInfo: true},
		{Type: TypeSandboxImage, Path: "sandbox-images", IncludeInInfo: true},
		{Type: TypePet, Path: "pets", IncludeInInfo: true},
		{Type: TypeCLITool, Path: "cli-tools", IncludeInInfo: true},
		{Type: TypeWebsiteApp, Path: "webapps", IncludeInInfo: true},
		{Type: TypeWebsiteApp, Path: "website-apps"},
	}
}

func marketInfos() []MarketInfo {
	archiveTypes := map[ItemType][]string{
		TypeSkill:        {"zip"},
		TypePlugin:       {"zip"},
		TypeAgent:        {"zip"},
		TypeSandboxImage: {"zip", "tar.gz"},
		TypePet:          {"zip"},
		TypeCLITool:      {"zip"},
		TypeWebsiteApp:   {"zip"},
	}
	names := map[ItemType]string{
		TypeSkill:        "Skill Market",
		TypePlugin:       "Plugin Market",
		TypeAgent:        "Agents Market",
		TypeSandboxImage: "Sandbox Image Market",
		TypePet:          "Pet Market",
		TypeCLITool:      "CLI Tool Market",
		TypeWebsiteApp:   "WebApps Market",
	}
	routes := marketRouteDefinitions()
	result := make([]MarketInfo, 0, len(routes))
	for _, route := range routes {
		if !route.IncludeInInfo {
			continue
		}
		result = append(result, MarketInfo{
			Type:                  string(route.Type),
			Route:                 "/api/v1/" + route.Path,
			Name:                  names[route.Type],
			DesktopManagedInstall: route.Type != TypeCLITool,
			AllowsMarketScripts:   route.Type == TypeCLITool,
			ArchiveTypes:          archiveTypes[route.Type],
			DependencyKinds:       allowedDependencyKinds[route.Type],
		})
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-content-type-options", "nosniff")
		w.Header().Set("referrer-policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func requestIP(r *http.Request) string {
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("x-forwarded-for"), ",")[0])
	if forwarded != "" {
		return forwarded
	}
	return r.RemoteAddr
}
