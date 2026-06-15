package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func OpenStore(ctx context.Context, databasePath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS items (
			type TEXT NOT NULL,
			id TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			readme TEXT NOT NULL DEFAULT '',
			latest_version TEXT NOT NULL,
			min_desktop_version TEXT NOT NULL DEFAULT '',
			sandbox_kind TEXT NOT NULL DEFAULT '',
			website_kind TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			dependencies_json TEXT NOT NULL DEFAULT '[]',
			protocol_json TEXT NOT NULL DEFAULT '{}',
			published INTEGER NOT NULL DEFAULT 1,
			published_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (type, id)
		)`,
		`CREATE TABLE IF NOT EXISTS versions (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			version TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			readme TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			dependencies_json TEXT NOT NULL DEFAULT '[]',
			protocol_json TEXT NOT NULL DEFAULT '{}',
			published INTEGER NOT NULL DEFAULT 1,
			published_at TEXT NOT NULL,
			PRIMARY KEY (item_type, item_id, version)
		)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			version TEXT NOT NULL,
			platform_key TEXT NOT NULL,
			archive_type TEXT NOT NULL,
			asset_role TEXT NOT NULL DEFAULT 'primary',
			path TEXT NOT NULL,
			url TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			integrity TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (item_type, item_id, version, platform_key)
		)`,
		`CREATE TABLE IF NOT EXISTS tags (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			tag TEXT NOT NULL,
			PRIMARY KEY (item_type, item_id, tag)
		)`,
		`CREATE TABLE IF NOT EXISTS download_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			version TEXT NOT NULL,
			artifact_platform TEXT NOT NULL,
			user_agent TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "items", "website_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "metadata_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "dependencies_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "protocol_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "metadata_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "dependencies_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "protocol_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "artifacts", "asset_role", "TEXT NOT NULL DEFAULT 'primary'"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET type = ? WHERE type = ?`, TypeSandboxImage, "sandbox"); err != nil {
		return err
	}
	for _, table := range []string{"versions", "artifacts", "tags", "download_events"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET item_type = ? WHERE item_type = ?`, table), TypeSandboxImage, "sandbox"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	return err
}

func (s *Store) Publish(ctx context.Context, req PublishRequest, artifact *storedArtifact) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metadataJSON, err := encodeJSONText(req.Metadata, "{}")
	if err != nil {
		return err
	}
	dependenciesJSON, err := encodeJSONText(req.Dependencies, "[]")
	if err != nil {
		return err
	}
	protocolJSON, err := encodeJSONText(protocolRecord{
		Install:   req.Install,
		Uninstall: req.Uninstall,
		Detect:    req.Detect,
	}, "{}")
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `INSERT INTO items (
		type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, website_kind, metadata_json, dependencies_json, protocol_json, published, published_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
	ON CONFLICT(type, id) DO UPDATE SET
		name = excluded.name,
		description = excluded.description,
		readme = excluded.readme,
		latest_version = excluded.latest_version,
		min_desktop_version = excluded.min_desktop_version,
		sandbox_kind = excluded.sandbox_kind,
		website_kind = excluded.website_kind,
		metadata_json = excluded.metadata_json,
		dependencies_json = excluded.dependencies_json,
		protocol_json = excluded.protocol_json,
		published = 1,
		updated_at = excluded.updated_at`,
		req.Type, req.ID, req.Name, req.Description, req.Readme, req.Version, req.MinDesktopVersion, req.SandboxKind, req.WebsiteKind, metadataJSON, dependenciesJSON, protocolJSON, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO versions (
		item_type, item_id, version, description, readme, metadata_json, dependencies_json, protocol_json, published, published_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
	ON CONFLICT(item_type, item_id, version) DO UPDATE SET
		description = excluded.description,
		readme = excluded.readme,
		metadata_json = excluded.metadata_json,
		dependencies_json = excluded.dependencies_json,
		protocol_json = excluded.protocol_json,
		published = 1`,
		req.Type, req.ID, req.Version, req.Description, req.Readme, metadataJSON, dependenciesJSON, protocolJSON, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tags WHERE item_type = ? AND item_id = ?`, req.Type, req.ID); err != nil {
		return err
	}
	for _, tag := range req.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO tags (item_type, item_id, tag) VALUES (?, ?, ?)`, req.Type, req.ID, tag); err != nil {
			return err
		}
	}
	if artifact != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO artifacts (
			item_type, item_id, version, platform_key, archive_type, asset_role, path, url, sha256, integrity, size_bytes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id, version, platform_key) DO UPDATE SET
			archive_type = excluded.archive_type,
			asset_role = excluded.asset_role,
			path = excluded.path,
			url = excluded.url,
			sha256 = excluded.sha256,
			integrity = excluded.integrity,
			size_bytes = excluded.size_bytes,
			created_at = excluded.created_at`,
			req.Type, req.ID, req.Version, artifact.PlatformKey, artifact.ArchiveType, artifact.AssetRole, artifact.Path, artifact.URL, artifact.SHA256, artifact.Integrity, artifact.SizeBytes, now); err != nil {
			return err
		}
	}
	err = tx.Commit()
	return err
}

func (s *Store) Unpublish(ctx context.Context, itemType ItemType, id, version string) error {
	if version == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE items SET published = 0, updated_at = ? WHERE type = ? AND id = ?`, time.Now().UTC().Format(time.RFC3339Nano), itemType, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET published = 0 WHERE item_type = ? AND item_id = ? AND version = ?`, itemType, id, version)
	return err
}

func (s *Store) ListPublic(ctx context.Context, onlyType ItemType) ([]storedItem, error) {
	query := `SELECT type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, website_kind, metadata_json, dependencies_json, protocol_json, published, published_at, updated_at
		FROM items WHERE published = 1`
	args := []any{}
	if onlyType != "" {
		query += ` AND type = ?`
		args = append(args, onlyType)
	}
	query += ` ORDER BY type, name COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []storedItem
	for rows.Next() {
		var item storedItem
		var published int
		var publishedAt, updatedAt string
		var metadataJSON, dependenciesJSON, protocolJSON string
		if err := rows.Scan(&item.Type, &item.ID, &item.Name, &item.Description, &item.Readme, &item.LatestVersion, &item.MinDesktopVersion, &item.SandboxKind, &item.WebsiteKind, &metadataJSON, &dependenciesJSON, &protocolJSON, &published, &publishedAt, &updatedAt); err != nil {
			return nil, err
		}
		item.Published = published == 1
		item.PublishedAt = parseTime(publishedAt)
		item.UpdatedAt = parseTime(updatedAt)
		if err := decodeItemJSON(&item, metadataJSON, dependenciesJSON, protocolJSON); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.loadTagsForItems(ctx, items, onlyType); err != nil {
		return nil, err
	}
	if err := s.loadAssetsForItems(ctx, items, onlyType); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) GetPublic(ctx context.Context, itemType ItemType, id string) (storedItem, error) {
	items, err := s.ListPublic(ctx, itemType)
	if err != nil {
		return storedItem{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return storedItem{}, sql.ErrNoRows
}

func (s *Store) GetArtifact(ctx context.Context, itemType ItemType, id, version, platform string) (storedArtifact, error) {
	if version == "" {
		row := s.db.QueryRowContext(ctx, `SELECT latest_version FROM items WHERE type = ? AND id = ? AND published = 1`, itemType, id)
		if err := row.Scan(&version); err != nil {
			return storedArtifact{}, err
		}
	}
	platforms := []string{platform, "universal"}
	if platform == "" || platform == "universal" {
		platforms = []string{"universal"}
	}
	for _, key := range platforms {
		artifact, err := s.getArtifact(ctx, itemType, id, version, key)
		if err == nil {
			artifact.Version = version
			return artifact, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return storedArtifact{}, err
		}
	}
	return storedArtifact{}, sql.ErrNoRows
}

func (s *Store) getArtifact(ctx context.Context, itemType ItemType, id, version, platform string) (storedArtifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT platform_key, archive_type, asset_role, path, url, sha256, integrity, size_bytes
		FROM artifacts WHERE item_type = ? AND item_id = ? AND version = ? AND platform_key = ?`,
		itemType, id, version, platform)
	var artifact storedArtifact
	artifact.Version = version
	err := row.Scan(&artifact.PlatformKey, &artifact.ArchiveType, &artifact.AssetRole, &artifact.Path, &artifact.URL, &artifact.SHA256, &artifact.Integrity, &artifact.SizeBytes)
	return artifact, err
}

func (s *Store) RecordDownload(ctx context.Context, itemType ItemType, id, version, platform, userAgent, ip string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO download_events (
		item_type, item_id, version, artifact_platform, user_agent, ip, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, itemType, id, version, platform, userAgent, ip, time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *Store) ListVersions(ctx context.Context, itemType ItemType, id string) ([]PublicVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version, description, readme, metadata_json, dependencies_json, published_at
		FROM versions WHERE item_type = ? AND item_id = ? AND published = 1 ORDER BY published_at DESC, version DESC`, itemType, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []PublicVersion
	for rows.Next() {
		var version PublicVersion
		var metadataJSON, dependenciesJSON, publishedAt string
		if err := rows.Scan(&version.Version, &version.Description, &version.Readme, &metadataJSON, &dependenciesJSON, &publishedAt); err != nil {
			return nil, err
		}
		version.PublishedAt = parseTime(publishedAt)
		if err := decodeJSONText(metadataJSON, &version.Metadata); err != nil {
			return nil, err
		}
		if err := decodeJSONText(dependenciesJSON, &version.Dependencies); err != nil {
			return nil, err
		}
		if version.Dependencies == nil {
			version.Dependencies = []MarketDependency{}
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.loadAssetsForVersions(ctx, itemType, id, versions); err != nil {
		return nil, err
	}
	return versions, nil
}

func (s *Store) tags(ctx context.Context, itemType ItemType, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM tags WHERE item_type = ? AND item_id = ? ORDER BY tag COLLATE NOCASE`, itemType, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (s *Store) assets(ctx context.Context, itemType ItemType, id, version string) (map[string]PublicAsset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT platform_key, archive_type, asset_role, url, sha256, integrity, size_bytes
		FROM artifacts WHERE item_type = ? AND item_id = ? AND version = ? ORDER BY asset_role, platform_key`, itemType, id, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := map[string]PublicAsset{}
	for rows.Next() {
		var key string
		var asset PublicAsset
		if err := rows.Scan(&key, &asset.ArchiveType, &asset.Role, &asset.URL, &asset.SHA256, &asset.Integrity, &asset.SizeBytes); err != nil {
			return nil, err
		}
		asset.Platform = key
		if asset.Role != "" && asset.Role != AssetRolePrimary {
			key = asset.Role
		}
		assets[key] = asset
	}
	return assets, rows.Err()
}

type itemLookupKey struct {
	itemType ItemType
	id       string
}

type versionLookupKey struct {
	itemType ItemType
	id       string
	version  string
}

func (s *Store) loadTagsForItems(ctx context.Context, items []storedItem, onlyType ItemType) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[itemLookupKey]int, len(items))
	for i := range items {
		index[itemLookupKey{itemType: items[i].Type, id: items[i].ID}] = i
	}
	query := `SELECT t.item_type, t.item_id, t.tag
		FROM tags t
		JOIN items i ON i.type = t.item_type AND i.id = t.item_id
		WHERE i.published = 1`
	args := []any{}
	if onlyType != "" {
		query += ` AND i.type = ?`
		args = append(args, onlyType)
	}
	query += ` ORDER BY t.item_type, t.item_id, t.tag COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, id, tag string
		if err := rows.Scan(&rawType, &id, &tag); err != nil {
			return err
		}
		if itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: id}]; ok {
			items[itemIndex].Tags = append(items[itemIndex].Tags, tag)
		}
	}
	return rows.Err()
}

func (s *Store) loadAssetsForItems(ctx context.Context, items []storedItem, onlyType ItemType) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[versionLookupKey]int, len(items))
	for i := range items {
		items[i].Assets = map[string]PublicAsset{}
		index[versionLookupKey{itemType: items[i].Type, id: items[i].ID, version: items[i].LatestVersion}] = i
	}
	query := `SELECT a.item_type, a.item_id, a.version, a.platform_key, a.archive_type, a.asset_role, a.url, a.sha256, a.integrity, a.size_bytes
		FROM artifacts a
		JOIN items i ON i.type = a.item_type AND i.id = a.item_id AND i.latest_version = a.version
		WHERE i.published = 1`
	args := []any{}
	if onlyType != "" {
		query += ` AND i.type = ?`
		args = append(args, onlyType)
	}
	query += ` ORDER BY a.item_type, a.item_id, a.version, a.asset_role, a.platform_key`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, id, version, platformKey string
		var asset PublicAsset
		if err := rows.Scan(&rawType, &id, &version, &platformKey, &asset.ArchiveType, &asset.Role, &asset.URL, &asset.SHA256, &asset.Integrity, &asset.SizeBytes); err != nil {
			return err
		}
		itemIndex, ok := index[versionLookupKey{itemType: ItemType(rawType), id: id, version: version}]
		if !ok {
			continue
		}
		assignAsset(items[itemIndex].Assets, platformKey, asset)
	}
	return rows.Err()
}

func (s *Store) loadAssetsForVersions(ctx context.Context, itemType ItemType, id string, versions []PublicVersion) error {
	if len(versions) == 0 {
		return nil
	}
	index := make(map[string]int, len(versions))
	for i := range versions {
		versions[i].Assets = map[string]PublicAsset{}
		index[versions[i].Version] = i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version, platform_key, archive_type, asset_role, url, sha256, integrity, size_bytes
		FROM artifacts WHERE item_type = ? AND item_id = ? ORDER BY version, asset_role, platform_key`, itemType, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version, platformKey string
		var asset PublicAsset
		if err := rows.Scan(&version, &platformKey, &asset.ArchiveType, &asset.Role, &asset.URL, &asset.SHA256, &asset.Integrity, &asset.SizeBytes); err != nil {
			return err
		}
		versionIndex, ok := index[version]
		if !ok {
			continue
		}
		assignAsset(versions[versionIndex].Assets, platformKey, asset)
	}
	return rows.Err()
}

func assignAsset(assets map[string]PublicAsset, platformKey string, asset PublicAsset) {
	asset.Platform = platformKey
	key := platformKey
	if asset.Role != "" && asset.Role != AssetRolePrimary {
		key = asset.Role
	}
	assets[key] = asset
}

type protocolRecord struct {
	Install   *MarketScriptSpec `json:"install,omitempty"`
	Uninstall *MarketScriptSpec `json:"uninstall,omitempty"`
	Detect    *MarketDetectSpec `json:"detect,omitempty"`
}

func encodeJSONText(value any, fallback string) (string, error) {
	if value == nil {
		return fallback, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if string(data) == "null" {
		return fallback, nil
	}
	return string(data), nil
}

func decodeItemJSON(item *storedItem, metadataJSON, dependenciesJSON, protocolJSON string) error {
	if err := decodeJSONText(metadataJSON, &item.Metadata); err != nil {
		return err
	}
	if err := decodeJSONText(dependenciesJSON, &item.Dependencies); err != nil {
		return err
	}
	if item.Dependencies == nil {
		item.Dependencies = []MarketDependency{}
	}
	var protocol protocolRecord
	if err := decodeJSONText(protocolJSON, &protocol); err != nil {
		return err
	}
	item.Install = protocol.Install
	item.Uninstall = protocol.Uninstall
	item.Detect = protocol.Detect
	return nil
}

func decodeJSONText(value string, target any) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return json.Unmarshal([]byte(value), target)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func normalizeItemType(value string) (ItemType, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "skill", "skills":
		return TypeSkill, nil
	case "plugin", "plugins":
		return TypePlugin, nil
	case "agent", "agents", "智能体":
		return TypeAgent, nil
	case "sandbox-image", "sandbox-images":
		return TypeSandboxImage, nil
	case "pet", "pets":
		return TypePet, nil
	case "cli-tool", "cli-tools":
		return TypeCLITool, nil
	case "website-app", "website-apps", "website", "websites", "webapp", "webapps", "网站应用":
		return TypeWebsiteApp, nil
	default:
		return "", fmt.Errorf("unsupported item type %q", value)
	}
}

func publicItem(item storedItem) PublicItem {
	return PublicItem{
		ID:                item.ID,
		Type:              string(item.Type),
		Name:              item.Name,
		Version:           item.LatestVersion,
		Description:       item.Description,
		Readme:            item.Readme,
		Tags:              item.Tags,
		MinDesktopVersion: item.MinDesktopVersion,
		SandboxKind:       item.SandboxKind,
		WebsiteKind:       item.WebsiteKind,
		NpmPackage:        npmPackageName(item.Type, item.ID),
		Assets:            item.Assets,
		Dependencies:      item.Dependencies,
		Metadata:          item.Metadata,
		Install:           item.Install,
		Uninstall:         item.Uninstall,
		Detect:            item.Detect,
		PublishedAt:       item.PublishedAt,
		UpdatedAt:         item.UpdatedAt,
	}
}

func publicItems(items []storedItem) []PublicItem {
	result := make([]PublicItem, 0, len(items))
	for _, item := range items {
		result = append(result, publicItem(item))
	}
	return result
}

func sortPublicItems(items []PublicItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type < items[j].Type
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}
