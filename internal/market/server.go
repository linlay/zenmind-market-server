package market

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	cfg             Config
	store           *Store
	artifactStorage artifactStorage
	ssoJWT          *ssoJWTVerifier
	oidc            *oidcClient
	oidcMu          sync.Mutex
}

func Open(ctx context.Context, cfg Config) (*App, error) {
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 512 * 1024 * 1024
	}
	storage, err := newArtifactStorage(cfg)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	ssoJWT, err := newSSOJWTVerifier(cfg)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	oidc, err := newOIDCClient(ctx, cfg)
	if err != nil {
		// The catalog should remain available while a remote OIDC provider recovers.
		log.Printf("OIDC initialization deferred: %v", err)
	}
	return &App{cfg: cfg, store: store, artifactStorage: storage, ssoJWT: ssoJWT, oidc: oidc}, nil
}

func (a *App) ensureOIDCClient(ctx context.Context) (*oidcClient, error) {
	if strings.TrimSpace(a.cfg.OIDCIssuer) == "" {
		return nil, nil
	}
	a.oidcMu.Lock()
	defer a.oidcMu.Unlock()
	if a.oidc != nil {
		return a.oidc, nil
	}
	client, err := newOIDCClient(ctx, a.cfg)
	if err != nil {
		return nil, err
	}
	a.oidc = client
	return client, nil
}

func (a *App) currentOIDCClient() *oidcClient {
	a.oidcMu.Lock()
	defer a.oidcMu.Unlock()
	return a.oidc
}

func (a *App) Close() error {
	return a.store.Close()
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	if a.cfg.EnableLocalAuth {
		mux.HandleFunc("POST /api/v1/auth/login", a.handleLocalLogin)
	}
	mux.HandleFunc("GET /api/v1/auth/me", a.handleAuthMe)
	mux.HandleFunc("GET /api/v1/me/favorites", a.handleMyFavorites)
	mux.HandleFunc("GET /api/v1/auth/oidc/login", a.handleOIDCLogin)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", a.handleOIDCCallback)
	mux.HandleFunc("POST /api/v1/auth/oidc/logout", a.handleOIDCLogout)
	mux.HandleFunc("GET /api/v1/markets", a.handleMarkets)
	mux.HandleFunc("GET /api/v1/catalog", a.handleCatalog)
	mux.HandleFunc("GET /api/v1/desktop/catalog", a.handleDesktopCatalog)
	mux.HandleFunc("GET /api/v1/creator/items", a.handleCreatorItems)
	mux.HandleFunc("POST /api/v1/creator/publish", a.handleCreatorPublish)
	mux.HandleFunc("GET /api/v1/items/{type}/{id}", a.handleItem)
	mux.HandleFunc("GET /api/v1/adp/{type}/{id}", a.handleADPManifest)
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
		if route.Type == TypeSkill {
			mux.HandleFunc("GET /api/v1/"+route.Path+"/{id}/package/download", a.handleSkillPackageDownload)
		}
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
	mux.HandleFunc("GET /api/v1/admin/reviews", a.requireAdmin(a.handleAdminReviews))
	mux.HandleFunc("POST /api/v1/admin/reviews/{type}/{id}", a.requireAdmin(a.handleAdminReviewUpdate))
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

func (a *App) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"userId"`
		Name   string `json:"name"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	userID := sanitizeSlug(req.UserID)
	if userID == "" {
		userID = "local-creator"
	}
	role := normalizeLocalRole(req.Role)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = userID
	}
	user := localUser{ID: userID, Name: name, Role: role}
	writeJSON(w, http.StatusOK, map[string]any{"token": encodeLocalUserToken(user), "user": user})
}

func (a *App) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if user, ok := a.oidcUserFromRequest(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"user": user})
		return
	}
	if user, ok := a.localUserFromRequest(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"user": user})
		return
	}
	if a.cfg.ProxyToken != "" && r.Header.Get("X-ZenMind-Market-Proxy-Token") == a.cfg.ProxyToken {
		userID := strings.TrimSpace(r.Header.Get("X-ZenMind-User-ID"))
		if userID != "" {
			role := normalizeLocalRole(r.Header.Get("X-ZenMind-User-Role"))
			writeJSON(w, http.StatusOK, map[string]any{"user": localUser{ID: userID, Name: userID, Role: role}})
			return
		}
	}
	if principal, ok := a.ssoJWT.principalFromRequest(r); ok && principal.HasScope("market") {
		writeJSON(w, http.StatusOK, map[string]any{"user": localUser{ID: principal.UserID, Name: principal.UserID, Role: normalizeLocalRole(principal.Role)}})
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
}

func (a *App) handleMyFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.authorizedMarketUser(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListFavorites(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	result := marketItems(items)
	sortPublicItems(result)
	writeJSON(w, http.StatusOK, CatalogResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Items: result})
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
	result := marketItems(items)
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
		public := marketItem(item)
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
			Skill:             public.Skill,
			ADPInstallURL:     public.ADPInstallURL,
			ReviewStatus:      public.ReviewStatus,
			ReviewNote:        public.ReviewNote,
			ReviewedAt:        public.ReviewedAt,
			ReviewedBy:        public.ReviewedBy,
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

func (a *App) handleCreatorItems(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.authorizedMarketUser(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListCreator(r.Context(), userID, a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	result := publicItems(items)
	sortPublicItems(result)
	writeJSON(w, http.StatusOK, CatalogResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Items: result})
}

func (a *App) handleAdminReviews(w http.ResponseWriter, r *http.Request) {
	var itemType ItemType
	if rawType := strings.TrimSpace(r.URL.Query().Get("type")); rawType != "" {
		normalized, err := normalizeItemType(rawType)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
			return
		}
		itemType = normalized
	}
	status := normalizeReviewStatus(r.URL.Query().Get("status"), "")
	items, err := a.store.ListAdmin(r.Context(), itemType, status, a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	result := publicItems(items)
	sortPublicItems(result)
	writeJSON(w, http.StatusOK, CatalogResponse{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Items: result})
}

func (a *App) handleAdminReviewUpdate(w http.ResponseWriter, r *http.Request) {
	itemType, err := normalizeItemType(r.PathValue("type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
		return
	}
	id := sanitizeSlug(r.PathValue("id"))
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := a.store.UpdateReview(r.Context(), itemType, id, req.Status, req.Note, a.reviewerID(r)); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market item not found")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_review", err.Error())
		return
	}
	item, err := a.store.ListAdmin(r.Context(), itemType, "", a.viewerUserID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	for _, candidate := range item {
		if candidate.ID == id {
			writeJSON(w, http.StatusOK, publicItem(candidate))
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "market item not found")
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
	writeJSON(w, http.StatusOK, marketItem(item))
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
	result := marketItems(items)
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
	writeJSON(w, http.StatusOK, marketItem(item))
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
	writeJSON(w, http.StatusOK, VersionsResponse{SchemaVersion: 1, Item: marketItem(item), Versions: versions})
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
	requestedVersion := canonicalVersion(r.URL.Query().Get("version"))
	version := requestedVersion
	if version == "" {
		version = item.LatestVersion
	} else if err := a.store.EnsurePublishedVersion(r.Context(), itemType, id, version); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "market version not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	platformSpec, platformErr := a.store.GetPlatform(r.Context(), itemType, id, version, platform)
	if platformErr != nil && !errors.Is(platformErr, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "store_error", platformErr.Error())
		return
	}
	artifact, err := a.store.GetArtifact(r.Context(), itemType, id, version, platform)
	if errors.Is(err, sql.ErrNoRows) {
		response := ResolveResponse{SchemaVersion: 1, Item: marketItem(item), Version: version, Platform: platform}
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
	response := ResolveResponse{SchemaVersion: 1, Item: marketItem(item), Version: artifact.Version, Platform: artifact.PlatformKey, Asset: &asset}
	if platformErr == nil {
		response.PlatformSpec = &platformSpec
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleMarketDownload(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	a.downloadArtifact(w, r, itemType, sanitizeSlug(r.PathValue("id")), r.URL.Query().Get("version"), r.URL.Query().Get("platform"))
}

func (a *App) handleSkillPackageDownload(w http.ResponseWriter, r *http.Request) {
	id := sanitizeSlug(r.PathValue("id"))
	item, err := a.store.GetPublic(r.Context(), TypeSkill, id, a.viewerUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "skill package not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if item.Skill == nil || item.Skill.Kind != SkillKindPackage {
		writeError(w, http.StatusBadRequest, "not_skill_package", "skill is not a package")
		return
	}
	if len(item.Skill.IncludedSkills) == 0 {
		writeError(w, http.StatusBadRequest, "empty_skill_package", "skill package has no included skills")
		return
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	filename := item.ID + "-" + item.LatestVersion + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	manifest := map[string]any{
		"schemaVersion": 1,
		"type":          "skill-package",
		"id":            item.ID,
		"name":          item.Name,
		"version":       item.LatestVersion,
		"generatedAt":   time.Now().UTC(),
		"skills":        []map[string]any{},
	}
	manifestSkills := manifest["skills"].([]map[string]any)
	for _, included := range item.Skill.IncludedSkills {
		skill, err := a.store.GetPublic(r.Context(), TypeSkill, included.ID, a.viewerUserID(r))
		if errors.Is(err, sql.ErrNoRows) {
			writeZipError(zw, "errors/"+included.ID+".txt", "included skill not found: "+included.ID+"\n")
			continue
		}
		if err != nil {
			writeZipError(zw, "errors/"+included.ID+".txt", err.Error()+"\n")
			continue
		}
		artifact, err := a.store.GetArtifact(r.Context(), TypeSkill, included.ID, "", platform)
		if err != nil {
			writeZipError(zw, "errors/"+included.ID+".txt", "artifact not found: "+included.ID+"\n")
			continue
		}
		skillPath := "skills/" + included.ID + "/"
		if artifact.ArchiveType != "zip" {
			writeZipError(zw, "errors/"+included.ID+".txt", "skill package can only expand zip artifacts: "+included.ID+"\n")
			continue
		}
		if err := writeExtractedZipFromPath(zw, skillPath, artifact.Path); err != nil {
			writeZipError(zw, "errors/"+included.ID+".txt", err.Error()+"\n")
			continue
		}
		adpYAML, err := a.store.GetADPYAML(r.Context(), TypeSkill, included.ID, artifact.Version)
		if err == nil {
			if err := writeZipText(zw, "adp/"+included.ID+".adp.yaml", adpYAML); err != nil {
				writeZipError(zw, "errors/"+included.ID+"-adp.txt", err.Error()+"\n")
			}
		}
		a.store.RecordDownload(r.Context(), TypeSkill, included.ID, artifact.Version, artifact.PlatformKey, r.UserAgent(), requestIP(r))
		manifestSkills = append(manifestSkills, map[string]any{
			"id":        skill.ID,
			"name":      skill.Name,
			"version":   skill.LatestVersion,
			"path":      skillPath,
			"adp":       "adp/" + included.ID + ".adp.yaml",
			"sha256":    artifact.SHA256,
			"platform":  artifact.PlatformKey,
			"optional":  included.Optional,
			"sortOrder": included.SortOrder,
		})
	}
	manifest["skills"] = manifestSkills
	manifestFile, err := zw.Create("manifest.json")
	if err == nil {
		_ = json.NewEncoder(manifestFile).Encode(manifest)
	}
}

func (a *App) handleMarketFavorite(w http.ResponseWriter, r *http.Request, itemType ItemType, favorite bool) {
	id := sanitizeSlug(r.PathValue("id"))
	userID, ok := a.authorizedMarketUser(w, r)
	if !ok {
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
	writeJSON(w, http.StatusOK, marketItem(item))
}

func (a *App) handlePublish(w http.ResponseWriter, r *http.Request) {
	a.handlePublishWithType(w, r, "")
}

func (a *App) handleTypedPublish(w http.ResponseWriter, r *http.Request, itemType ItemType) {
	a.handlePublishWithType(w, r, itemType)
}

func (a *App) handlePublishWithType(w http.ResponseWriter, r *http.Request, forcedType ItemType) {
	a.handlePublishWithOptions(w, r, forcedType, "", "")
}

func (a *App) handleCreatorPublish(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.authorizedMarketUser(w, r)
	if !ok {
		return
	}
	a.handlePublishWithOptions(w, r, "", ReviewStatusPending, userID)
}

func (a *App) handlePublishWithOptions(w http.ResponseWriter, r *http.Request, forcedType ItemType, forcedReviewStatus string, creatorID string) {
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
		if adpYAML, err := readOptionalFormFile(r, "adp", 1024*1024); err == nil {
			req.ADPYAML = adpYAML
		} else if !errors.Is(err, http.ErrMissingFile) {
			writeError(w, http.StatusBadRequest, "invalid_adp", err.Error())
			return
		}
		if err := validatePublishRequest(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
			return
		}
		imageFile, imageHeader, imageErr := r.FormFile("image")
		if imageErr == nil {
			defer imageFile.Close()
			imageURL, err := saveMarketImage(r.Context(), a.artifactStorage, a.cfg.PublicBaseURL, req, imageFile, imageHeader)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_image", err.Error())
				return
			}
			if req.Metadata == nil {
				req.Metadata = map[string]string{}
			}
			req.Metadata["icon"] = imageURL
			req.Metadata["screenshot"] = imageURL
		} else if !errors.Is(imageErr, http.ErrMissingFile) {
			writeError(w, http.StatusBadRequest, "invalid_image", imageErr.Error())
			return
		}
		file, header, err := r.FormFile("artifact")
		if err == nil {
			defer file.Close()
			artifact, err = saveAndValidateArtifact(r.Context(), a.artifactStorage, a.cfg.PublicBaseURL, req, file, header)
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
	if forcedReviewStatus != "" {
		req.ReviewStatus = forcedReviewStatus
	}
	if creatorID != "" {
		req.CreatorID = creatorID
	}
	if err := normalizePublishADP(r.Context(), a.store, a.cfg.PublicBaseURL, &req, artifact); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_adp", err.Error())
		return
	}
	if err := a.store.Publish(r.Context(), req, artifact); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": map[string]any{"type": req.Type, "id": req.ID, "version": req.Version}})
}

func (a *App) handleADPManifest(w http.ResponseWriter, r *http.Request) {
	itemType, err := normalizeItemType(r.PathValue("type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
		return
	}
	if itemType != TypeCLITool && itemType != TypeSkill {
		writeError(w, http.StatusBadRequest, "invalid_type", "ADP manifests are available only for cli-tool and skill")
		return
	}
	yamlText, err := a.store.GetADPYAML(r.Context(), itemType, sanitizeSlug(r.PathValue("id")), r.URL.Query().Get("version"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "adp.yaml not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, yamlText)
}

func readOptionalFormFile(r *http.Request, field string, limit int64) (string, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%s exceeds %d bytes", field, limit)
	}
	return string(data), nil
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
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "market item or version not found")
			return
		}
		if errors.Is(err, errUnpublishNotLatest) || err.Error() == "version is required" {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
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
	objectID, err := a.artifactStorage.ObjectID(relative)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "invalid artifact path")
		return
	}
	if !strings.HasPrefix(relative, "media/") {
		if err := a.store.EnsurePublishedArtifactPath(r.Context(), objectID); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "artifact not found")
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
	}
	if localPath, ok := a.artifactStorage.LocalPath(objectID); ok {
		http.ServeFile(w, r, localPath)
		return
	}
	downloadURL, err := a.artifactStorage.PresignGet(r.Context(), objectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	http.Redirect(w, r, downloadURL, http.StatusFound)
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

func writeZipFileFromPath(zw *zip.Writer, name string, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}

func writeExtractedZipFromPath(zw *zip.Writer, prefix string, filePath string) error {
	source, err := zip.OpenReader(filePath)
	if err != nil {
		return err
	}
	defer source.Close()
	prefix = strings.Trim(path.Clean(strings.TrimSpace(prefix)), "/")
	if prefix == "." || prefix == "" {
		return errors.New("zip prefix is required")
	}
	for _, file := range source.File {
		name := strings.TrimSpace(filepath.ToSlash(file.Name))
		cleanName := path.Clean(name)
		if cleanName == "." || strings.HasPrefix(cleanName, "../") || path.IsAbs(cleanName) {
			return fmt.Errorf("unsafe zip entry %q", file.Name)
		}
		targetName := prefix + "/" + cleanName
		if file.FileInfo().IsDir() {
			if !strings.HasSuffix(targetName, "/") {
				targetName += "/"
			}
			header := &zip.FileHeader{
				Name:     targetName,
				Method:   zip.Store,
				Modified: file.Modified,
			}
			if _, err := zw.CreateHeader(header); err != nil {
				return err
			}
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		header := &zip.FileHeader{
			Name:     targetName,
			Method:   zip.Deflate,
			Modified: file.Modified,
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			reader.Close()
			return err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			reader.Close()
			return err
		}
		if err := reader.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writeZipText(zw *zip.Writer, name string, text string) error {
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(entry, text)
	return err
}

func writeZipError(zw *zip.Writer, name string, text string) {
	_ = writeZipText(zw, name, text)
}

func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := a.authorizedAdmin(r)
		if result.OK {
			next(w, r)
			return
		}
		writeError(w, result.Status, result.Code, result.Message)
	}
}

type authResult struct {
	OK      bool
	Status  int
	Code    string
	Message string
}

type localUser struct {
	ID       string `json:"id"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

func allowAuth() authResult {
	return authResult{OK: true}
}

func denyAuth(status int, code, message string) authResult {
	return authResult{Status: status, Code: code, Message: message}
}

func (a *App) authorizedAdmin(r *http.Request) authResult {
	auth := strings.TrimSpace(r.Header.Get("authorization"))
	if a.cfg.AdminToken != "" && strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer")), a.cfg.AdminToken) {
		return allowAuth()
	}
	if user, ok := a.localUserFromRequest(r); ok {
		if user.Role == "admin" {
			return allowAuth()
		}
		return denyAuth(http.StatusForbidden, "forbidden", "admin role required")
	}
	if user, ok := a.oidcUserFromRequest(r); ok {
		if user.Role == "admin" {
			return allowAuth()
		}
		return denyAuth(http.StatusForbidden, "forbidden", "admin role required")
	}
	if a.cfg.ProxyToken != "" && r.Header.Get("X-ZenMind-Market-Proxy-Token") == a.cfg.ProxyToken {
		role := strings.ToLower(strings.TrimSpace(r.Header.Get("X-ZenMind-User-Role")))
		if role == "admin" {
			return allowAuth()
		}
	}
	if auth == "" {
		return denyAuth(http.StatusUnauthorized, "unauthorized", "admin token required")
	}
	if !bearerHeaderLooksLikeJWT(auth) {
		return denyAuth(http.StatusUnauthorized, "unauthorized", "invalid bearer token")
	}
	principal, err := a.ssoJWT.verifyBearerHeader(auth)
	if err != nil {
		return jwtAuthError(err)
	}
	if principal.Role != "admin" {
		return denyAuth(http.StatusForbidden, "forbidden", "admin role required")
	}
	if !principal.HasScope("market") {
		return denyAuth(http.StatusForbidden, "forbidden", "market scope required")
	}
	return allowAuth()
}

func (a *App) viewerUserID(r *http.Request) string {
	if user, ok := a.oidcUserFromRequest(r); ok {
		return user.ID
	}
	if user, ok := a.localUserFromRequest(r); ok {
		return user.ID
	}
	if a.cfg.ProxyToken != "" && r.Header.Get("X-ZenMind-Market-Proxy-Token") == a.cfg.ProxyToken {
		return strings.TrimSpace(r.Header.Get("X-ZenMind-User-ID"))
	}
	if principal, ok := a.ssoJWT.principalFromRequest(r); ok && principal.HasScope("market") {
		return principal.UserID
	}
	return ""
}

func (a *App) reviewerID(r *http.Request) string {
	if user, ok := a.oidcUserFromRequest(r); ok {
		return user.ID
	}
	if user, ok := a.localUserFromRequest(r); ok {
		return user.ID
	}
	if a.cfg.ProxyToken != "" && r.Header.Get("X-ZenMind-Market-Proxy-Token") == a.cfg.ProxyToken {
		userID := strings.TrimSpace(r.Header.Get("X-ZenMind-User-ID"))
		if userID != "" {
			return userID
		}
	}
	if principal, ok := a.ssoJWT.principalFromRequest(r); ok && principal.HasScope("market") {
		return principal.UserID
	}
	return "admin"
}

func (a *App) authorizedMarketUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if user, ok := a.oidcUserFromRequest(r); ok {
		return user.ID, true
	}
	if user, ok := a.localUserFromRequest(r); ok {
		return user.ID, true
	}
	if a.cfg.ProxyToken != "" && r.Header.Get("X-ZenMind-Market-Proxy-Token") == a.cfg.ProxyToken {
		userID := strings.TrimSpace(r.Header.Get("X-ZenMind-User-ID"))
		if userID != "" {
			return userID, true
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "trusted proxy user required")
		return "", false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "official JWT required")
		return "", false
	}
	principal, err := a.ssoJWT.verifyBearerHeader(auth)
	if err != nil {
		result := jwtAuthError(err)
		writeError(w, result.Status, result.Code, result.Message)
		return "", false
	}
	if !principal.HasScope("market") {
		writeError(w, http.StatusForbidden, "forbidden", "market scope required")
		return "", false
	}
	return principal.UserID, true
}

func (a *App) localUserFromRequest(r *http.Request) (localUser, bool) {
	if !a.cfg.EnableLocalAuth {
		return localUser{}, false
	}
	return decodeLocalUserToken(bearerToken(r.Header.Get("Authorization")))
}

func encodeLocalUserToken(user localUser) string {
	raw, _ := json.Marshal(user)
	return "local." + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeLocalUserToken(token string) (localUser, bool) {
	if !strings.HasPrefix(token, "local.") {
		return localUser{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "local."))
	if err != nil {
		return localUser{}, false
	}
	var user localUser
	if err := json.Unmarshal(raw, &user); err != nil {
		return localUser{}, false
	}
	user.ID = strings.TrimSpace(user.ID)
	user.Role = normalizeLocalRole(user.Role)
	user.Name = strings.TrimSpace(user.Name)
	if user.ID == "" {
		return localUser{}, false
	}
	if user.Name == "" {
		user.Name = user.ID
	}
	return user, true
}

func normalizeLocalRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin", "administrator":
		return "admin"
	default:
		return "creator"
	}
}

func jwtAuthError(err error) authResult {
	if errors.Is(err, errSSOJWTNotConfigured) {
		return denyAuth(http.StatusServiceUnavailable, "sso_jwt_not_configured", "official JWT verifier is not configured")
	}
	if errors.Is(err, errBearerTokenMissing) {
		return denyAuth(http.StatusUnauthorized, "unauthorized", "official JWT required")
	}
	return denyAuth(http.StatusUnauthorized, "unauthorized", "invalid bearer token")
}

func bearerHeaderLooksLikeJWT(header string) bool {
	token := bearerToken(header)
	return strings.Count(token, ".") == 2
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
		{Type: TypeSoftwarePackage, Path: "software-packages", IncludeInInfo: true},
	}
}

func marketInfos() []MarketInfo {
	archiveTypes := map[ItemType][]string{
		TypeSkill:           {"zip"},
		TypePlugin:          {"zip"},
		TypeAgent:           {"zip"},
		TypeSandboxImage:    {"zip", "tar.gz"},
		TypePet:             {"zip"},
		TypeCLITool:         {"zip"},
		TypeWebsiteApp:      {"zip"},
		TypeSoftwarePackage: {"zip", "tar.gz"},
	}
	names := map[ItemType]string{
		TypeSkill:           "Skill Market",
		TypePlugin:          "Plugin Market",
		TypeAgent:           "Agents Market",
		TypeSandboxImage:    "Sandbox Image Market",
		TypePet:             "Pet Market",
		TypeCLITool:         "CLI Tool Market",
		TypeWebsiteApp:      "WebApps Market",
		TypeSoftwarePackage: "Software Package Market",
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
