package market

import (
	"context"
	"database/sql"
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
	db.SetMaxOpenConns(4)
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
	return nil
}

func (s *Store) Publish(ctx context.Context, req PublishRequest, artifact *storedArtifact) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
		type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, published, published_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
	ON CONFLICT(type, id) DO UPDATE SET
		name = excluded.name,
		description = excluded.description,
		readme = excluded.readme,
		latest_version = excluded.latest_version,
		min_desktop_version = excluded.min_desktop_version,
		sandbox_kind = excluded.sandbox_kind,
		published = 1,
		updated_at = excluded.updated_at`,
		req.Type, req.ID, req.Name, req.Description, req.Readme, req.Version, req.MinDesktopVersion, req.SandboxKind, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO versions (
		item_type, item_id, version, description, readme, published, published_at
	) VALUES (?, ?, ?, ?, ?, 1, ?)
	ON CONFLICT(item_type, item_id, version) DO UPDATE SET
		description = excluded.description,
		readme = excluded.readme,
		published = 1`,
		req.Type, req.ID, req.Version, req.Description, req.Readme, now); err != nil {
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
			item_type, item_id, version, platform_key, archive_type, path, url, sha256, integrity, size_bytes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id, version, platform_key) DO UPDATE SET
			archive_type = excluded.archive_type,
			path = excluded.path,
			url = excluded.url,
			sha256 = excluded.sha256,
			integrity = excluded.integrity,
			size_bytes = excluded.size_bytes,
			created_at = excluded.created_at`,
			req.Type, req.ID, req.Version, artifact.PlatformKey, artifact.ArchiveType, artifact.Path, artifact.URL, artifact.SHA256, artifact.Integrity, artifact.SizeBytes, now); err != nil {
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
	query := `SELECT type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, published, published_at, updated_at
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
		if err := rows.Scan(&item.Type, &item.ID, &item.Name, &item.Description, &item.Readme, &item.LatestVersion, &item.MinDesktopVersion, &item.SandboxKind, &published, &publishedAt, &updatedAt); err != nil {
			return nil, err
		}
		item.Published = published == 1
		item.PublishedAt = parseTime(publishedAt)
		item.UpdatedAt = parseTime(updatedAt)
		item.Tags, err = s.tags(ctx, item.Type, item.ID)
		if err != nil {
			return nil, err
		}
		item.Assets, err = s.assets(ctx, item.Type, item.ID, item.LatestVersion)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
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
			return artifact, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return storedArtifact{}, err
		}
	}
	return storedArtifact{}, sql.ErrNoRows
}

func (s *Store) getArtifact(ctx context.Context, itemType ItemType, id, version, platform string) (storedArtifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT platform_key, archive_type, path, url, sha256, integrity, size_bytes
		FROM artifacts WHERE item_type = ? AND item_id = ? AND version = ? AND platform_key = ?`,
		itemType, id, version, platform)
	var artifact storedArtifact
	err := row.Scan(&artifact.PlatformKey, &artifact.ArchiveType, &artifact.Path, &artifact.URL, &artifact.SHA256, &artifact.Integrity, &artifact.SizeBytes)
	return artifact, err
}

func (s *Store) RecordDownload(ctx context.Context, itemType ItemType, id, version, platform, userAgent, ip string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO download_events (
		item_type, item_id, version, artifact_platform, user_agent, ip, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, itemType, id, version, platform, userAgent, ip, time.Now().UTC().Format(time.RFC3339Nano))
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
	rows, err := s.db.QueryContext(ctx, `SELECT platform_key, archive_type, url, sha256, integrity, size_bytes
		FROM artifacts WHERE item_type = ? AND item_id = ? AND version = ? ORDER BY platform_key`, itemType, id, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := map[string]PublicAsset{}
	for rows.Next() {
		var key string
		var asset PublicAsset
		if err := rows.Scan(&key, &asset.ArchiveType, &asset.URL, &asset.SHA256, &asset.Integrity, &asset.SizeBytes); err != nil {
			return nil, err
		}
		asset.Platform = key
		assets[key] = asset
	}
	return assets, rows.Err()
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
	case "sandbox", "sandboxes", "sandbox-image", "sandbox-images":
		return TypeSandbox, nil
	default:
		return "", fmt.Errorf("unsupported item type %q", value)
	}
}

func publicItem(item storedItem) PublicItem {
	itemType := string(item.Type)
	if item.Type == TypeSandbox {
		itemType = "sandbox-image"
	}
	return PublicItem{
		ID:                item.ID,
		Type:              itemType,
		Name:              item.Name,
		Version:           item.LatestVersion,
		Description:       item.Description,
		Readme:            item.Readme,
		Tags:              item.Tags,
		MinDesktopVersion: item.MinDesktopVersion,
		SandboxKind:       item.SandboxKind,
		NpmPackage:        npmPackageName(item.Type, item.ID),
		Assets:            item.Assets,
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
