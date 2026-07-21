package market

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const storedItemSelectColumns = `type, id, name, description, readme, latest_version,
	min_desktop_version, sandbox_kind, website_kind, creator_id, metadata_json,
	dependencies_json, protocol_json, adp_yaml, review_status, review_note,
	reviewed_at, reviewed_by, detail_view_count, published, submitted_at, published_at, updated_at`

var (
	errCommentForbidden = errors.New("comment belongs to another user")
	errCommentHidden    = errors.New("hidden comment cannot be edited")
)

type oidcUserProfile struct {
	Issuer           string
	Subject          string
	Username         string
	DisplayName      string
	Email            string
	EmailVerified    bool
	HasEmailVerified bool
	ProviderAccount  string
	ExternalUserID   string
	StaffNumber      string
	IsAdmin          bool
}

type marketUser struct {
	ID            string
	Username      string
	DisplayName   string
	Email         string
	EmailVerified bool
	Status        string
	Role          string
}

func OpenStore(ctx context.Context, databasePath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(databasePath))
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

func sqliteDSN(databasePath string) string {
	separator := "?"
	if strings.Contains(databasePath, "?") {
		separator = "&"
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(2000)")
	return databasePath + separator + query.Encode()
}

func (s *Store) Close() error {
	return s.db.Close()
}

const detailViewIncrementTimeout = 8 * time.Second

var detailViewRetryDelays = [...]time.Duration{25 * time.Millisecond, 75 * time.Millisecond}

func (s *Store) IncrementDetailViewCount(ctx context.Context, itemType ItemType, id string) error {
	ctx, cancel := context.WithTimeout(ctx, detailViewIncrementTimeout)
	defer cancel()

	for attempt := 0; ; attempt++ {
		result, err := s.db.ExecContext(ctx, `UPDATE items
			SET detail_view_count = detail_view_count + 1
			WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`,
			itemType, id, ReviewStatusApproved)
		if err == nil {
			affected, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return rowsErr
			}
			if affected == 0 {
				return sql.ErrNoRows
			}
			return nil
		}
		if !isSQLiteBusy(err) || attempt >= len(detailViewRetryDelays) {
			return err
		}
		timer := time.NewTimer(detailViewRetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

func (s *Store) UpsertOIDCUser(ctx context.Context, profile oidcUserProfile) (marketUser, error) {
	profile.Issuer = strings.TrimSpace(profile.Issuer)
	profile.Subject = strings.TrimSpace(profile.Subject)
	if profile.Issuer == "" || profile.Subject == "" {
		return marketUser{}, errors.New("OIDC issuer and subject are required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return marketUser{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var userID string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?`, profile.Issuer, profile.Subject).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		userID, err = marketUserIDForProfile(profile)
		if err != nil {
			return marketUser{}, err
		}
		username, err := nextAvailableUsername(ctx, tx, profile.Username, profile.Subject)
		if err != nil {
			return marketUser{}, err
		}
		displayName := strings.TrimSpace(profile.DisplayName)
		if displayName == "" {
			displayName = username
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO users (
			id, username, display_name, email, email_verified, status, created_at, updated_at, last_login_at
		) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
			userID, username, displayName, strings.TrimSpace(profile.Email), boolToInt(profile.EmailVerified), now, now, now); err != nil {
			return marketUser{}, err
		}
		identityID, err := newMarketUserID()
		if err != nil {
			return marketUser{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_identities (
			id, user_id, issuer, subject, provider_account, external_user_id, staff_number, linked_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			identityID, userID, profile.Issuer, profile.Subject, strings.TrimSpace(profile.ProviderAccount), strings.TrimSpace(profile.ExternalUserID), strings.TrimSpace(profile.StaffNumber), now, now); err != nil {
			return marketUser{}, err
		}
	} else if err != nil {
		return marketUser{}, err
	} else {
		if staffNumber := strings.TrimSpace(profile.StaffNumber); staffNumber != "" && userID != staffNumber {
			if err = rekeyMarketUser(ctx, tx, userID, staffNumber); err != nil {
				return marketUser{}, err
			}
			userID = staffNumber
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET
			display_name = CASE WHEN ? <> '' THEN ? ELSE display_name END,
			email = CASE WHEN ? <> '' THEN ? ELSE email END,
			email_verified = CASE WHEN ? <> 0 THEN ? ELSE email_verified END, updated_at = ?, last_login_at = ?
			WHERE id = ?`,
			strings.TrimSpace(profile.DisplayName), strings.TrimSpace(profile.DisplayName), strings.TrimSpace(profile.Email), strings.TrimSpace(profile.Email), boolToInt(profile.HasEmailVerified), boolToInt(profile.EmailVerified), now, now, userID); err != nil {
			return marketUser{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user_identities SET provider_account = ?, external_user_id = ?, staff_number = ?, last_seen_at = ? WHERE issuer = ? AND subject = ?`,
			strings.TrimSpace(profile.ProviderAccount), strings.TrimSpace(profile.ExternalUserID), strings.TrimSpace(profile.StaffNumber), now, profile.Issuer, profile.Subject); err != nil {
			return marketUser{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND source = 'oidc'`, userID); err != nil {
		return marketUser{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source, granted_at) VALUES (?, 'creator', 'oidc', ?)`, userID, now); err != nil {
		return marketUser{}, err
	}
	if profile.IsAdmin {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source, granted_at) VALUES (?, 'admin', 'oidc', ?)`, userID, now); err != nil {
			return marketUser{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return marketUser{}, err
	}
	return s.GetMarketUser(ctx, userID)
}

func (s *Store) GetMarketUser(ctx context.Context, userID string) (marketUser, error) {
	var user marketUser
	var verified int
	err := s.db.QueryRowContext(ctx, `SELECT id, username, display_name, email, email_verified, status FROM users WHERE id = ?`, strings.TrimSpace(userID)).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &verified, &user.Status,
	)
	if err != nil {
		return marketUser{}, err
	}
	user.EmailVerified = verified == 1
	rows, err := s.db.QueryContext(ctx, `SELECT role FROM user_roles WHERE user_id = ? ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END`, user.ID)
	if err != nil {
		return marketUser{}, err
	}
	defer rows.Close()
	user.Role = "creator"
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return marketUser{}, err
		}
		if role == "admin" {
			user.Role = "admin"
			break
		}
	}
	return user, rows.Err()
}

func newMarketUserID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "usr_" + hex.EncodeToString(raw), nil
}

func marketUserIDForProfile(profile oidcUserProfile) (string, error) {
	if staffNumber := strings.TrimSpace(profile.StaffNumber); staffNumber != "" {
		if strings.ContainsAny(staffNumber, "\r\n\t ") {
			return "", errors.New("OIDC staff number contains whitespace")
		}
		return staffNumber, nil
	}
	return newMarketUserID()
}

func rekeyMarketUser(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = ?)`, newID).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return errors.New("OIDC staff number is already assigned to another market user")
	}
	for _, query := range []string{
		`UPDATE items SET creator_id = ? WHERE creator_id = ?`,
		`UPDATE versions SET creator_id = ? WHERE creator_id = ?`,
		`UPDATE favorite_items SET user_id = ? WHERE user_id = ?`,
		`UPDATE items SET reviewed_by = ? WHERE reviewed_by = ?`,
		`UPDATE versions SET reviewed_by = ? WHERE reviewed_by = ?`,
		`UPDATE user_roles SET user_id = ? WHERE user_id = ?`,
		`UPDATE user_identities SET user_id = ? WHERE user_id = ?`,
		`UPDATE users SET id = ? WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, query, newID, oldID); err != nil {
			return err
		}
	}
	return nil
}

func nextAvailableUsername(ctx context.Context, tx *sql.Tx, preferred, fallback string) (string, error) {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = strings.TrimSpace(fallback)
	}
	if base == "" {
		base = "market-user"
	}
	for suffix := 0; ; suffix++ {
		candidate := base
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%d", base, suffix+1)
		}
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username = ? COLLATE NOCASE)`, candidate).Scan(&exists)
		if err != nil {
			return "", err
		}
		if exists == 0 {
			return candidate, nil
		}
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			display_name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			email_verified INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_login_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_identities (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			issuer TEXT NOT NULL,
			subject TEXT NOT NULL,
			provider_account TEXT NOT NULL DEFAULT '',
			external_user_id TEXT NOT NULL DEFAULT '',
			staff_number TEXT NOT NULL DEFAULT '',
			linked_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			UNIQUE (issuer, subject)
		)`,
		`CREATE TABLE IF NOT EXISTS user_roles (
			user_id TEXT NOT NULL,
			role TEXT NOT NULL,
			source TEXT NOT NULL,
			granted_at TEXT NOT NULL,
			PRIMARY KEY (user_id, role, source)
		)`,
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
				creator_id TEXT NOT NULL DEFAULT '',
				metadata_json TEXT NOT NULL DEFAULT '{}',
			dependencies_json TEXT NOT NULL DEFAULT '[]',
			protocol_json TEXT NOT NULL DEFAULT '{}',
			adp_yaml TEXT NOT NULL DEFAULT '',
			review_status TEXT NOT NULL DEFAULT 'approved',
			review_note TEXT NOT NULL DEFAULT '',
			reviewed_at TEXT NOT NULL DEFAULT '',
			reviewed_by TEXT NOT NULL DEFAULT '',
			detail_view_count INTEGER NOT NULL DEFAULT 0,
			published INTEGER NOT NULL DEFAULT 1,
			submitted_at TEXT NOT NULL DEFAULT '',
			published_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (type, id)
		)`,
		`CREATE TABLE IF NOT EXISTS versions (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
				version TEXT NOT NULL,
				creator_id TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '',
			readme TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			dependencies_json TEXT NOT NULL DEFAULT '[]',
			protocol_json TEXT NOT NULL DEFAULT '{}',
			adp_yaml TEXT NOT NULL DEFAULT '',
			review_status TEXT NOT NULL DEFAULT 'approved',
			review_note TEXT NOT NULL DEFAULT '',
			reviewed_at TEXT NOT NULL DEFAULT '',
			reviewed_by TEXT NOT NULL DEFAULT '',
			validation_json TEXT NOT NULL DEFAULT '[]',
			published INTEGER NOT NULL DEFAULT 1,
			submitted_at TEXT NOT NULL DEFAULT '',
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
			original_filename TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL,
			url TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			integrity TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			contents_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			PRIMARY KEY (item_type, item_id, version, platform_key)
		)`,
		`CREATE TABLE IF NOT EXISTS version_platforms (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			version TEXT NOT NULL,
			platform_key TEXT NOT NULL,
			os TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			readme TEXT NOT NULL DEFAULT '',
			min_desktop_version TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			dependencies_json TEXT NOT NULL DEFAULT '[]',
			protocol_json TEXT NOT NULL DEFAULT '{}',
			published INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
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
		`CREATE TABLE IF NOT EXISTS favorite_items (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (item_type, item_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS item_comments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			sentiment TEXT NOT NULL CHECK (sentiment IN ('positive', 'negative')),
			content TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'visible' CHECK (status IN ('visible', 'hidden', 'deleted')),
			moderated_by TEXT NOT NULL DEFAULT '',
			moderation_reason TEXT NOT NULL DEFAULT '',
			moderated_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS skill_profiles (
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			category TEXT NOT NULL,
			scenario TEXT NOT NULL DEFAULT '',
			level TEXT NOT NULL DEFAULT '',
			package_mode TEXT NOT NULL DEFAULT '',
			featured INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (item_type, item_id)
		)`,
		`CREATE TABLE IF NOT EXISTS skill_package_items (
			package_type TEXT NOT NULL,
			package_id TEXT NOT NULL,
			skill_id TEXT NOT NULL,
			optional INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (package_type, package_id, skill_id)
		)`,
		`CREATE TABLE IF NOT EXISTS review_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_type TEXT NOT NULL,
			item_id TEXT NOT NULL,
			version TEXT NOT NULL,
			action TEXT NOT NULL,
			from_status TEXT NOT NULL DEFAULT '',
			to_status TEXT NOT NULL,
			actor_id TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_download_events_item ON download_events (item_type, item_id)`,
		`CREATE INDEX IF NOT EXISTS idx_favorite_items_item ON favorite_items (item_type, item_id)`,
		`CREATE INDEX IF NOT EXISTS idx_item_comments_item ON item_comments (item_type, item_id, status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_item_comments_user ON item_comments (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_user_identities_user ON user_identities (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_roles_user ON user_roles (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_skill_profiles_category ON skill_profiles (kind, category)`,
		`CREATE INDEX IF NOT EXISTS idx_review_events_item ON review_events (item_type, item_id, created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "items", "website_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "user_identities", "staff_number", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "creator_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
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
	if err := s.ensureColumn(ctx, "items", "adp_yaml", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "review_status", "TEXT NOT NULL DEFAULT 'approved'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "review_note", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "reviewed_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "reviewed_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "detail_view_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "items", "submitted_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "metadata_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "creator_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "dependencies_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "protocol_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "adp_yaml", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "review_status", "TEXT NOT NULL DEFAULT 'approved'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "review_note", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "reviewed_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "reviewed_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "validation_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "versions", "submitted_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_items_creator ON items (creator_id)`); err != nil {
		return err
	}
	if err := s.backfillCreatorIDs(ctx); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "artifacts", "asset_role", "TEXT NOT NULL DEFAULT 'primary'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "artifacts", "original_filename", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "artifacts", "contents_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET submitted_at = published_at WHERE submitted_at = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE versions SET submitted_at = published_at WHERE submitted_at = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET type = ? WHERE type = ?`, TypeSandboxImage, "sandbox"); err != nil {
		return err
	}
	for _, table := range []string{"versions", "artifacts", "version_platforms", "tags", "download_events", "favorite_items"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET item_type = ? WHERE item_type = ?`, table), TypeSandboxImage, "sandbox"); err != nil {
			return err
		}
	}
	if err := s.canonicalizeStoredVersions(ctx); err != nil {
		return err
	}
	if err := s.backfillVersionPlatforms(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) backfillCreatorIDs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT type, id, metadata_json FROM items WHERE creator_id = ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type creatorBackfill struct {
		itemType  ItemType
		id        string
		creatorID string
	}
	var updates []creatorBackfill
	for rows.Next() {
		var rawType, id, metadataJSON string
		if err := rows.Scan(&rawType, &id, &metadataJSON); err != nil {
			return err
		}
		var metadata map[string]string
		if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
			continue
		}
		creatorID := strings.TrimSpace(metadata["creatorId"])
		if creatorID == "" {
			continue
		}
		updates = append(updates, creatorBackfill{itemType: ItemType(rawType), id: id, creatorID: creatorID})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := s.db.ExecContext(ctx, `UPDATE items SET creator_id = ? WHERE type = ? AND id = ? AND creator_id = ''`, update.creatorID, update.itemType, update.id); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE versions SET creator_id = ? WHERE item_type = ? AND item_id = ? AND creator_id = ''`, update.creatorID, update.itemType, update.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) canonicalizeStoredVersions(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statements := []string{
		`DELETE FROM versions
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'
				AND (
					EXISTS (
						SELECT 1 FROM versions canonical
						WHERE canonical.item_type = versions.item_type
							AND canonical.item_id = versions.item_id
							AND canonical.version = substr(versions.version, 2)
					)
					OR rowid NOT IN (
						SELECT min(rowid) FROM versions
						WHERE substr(version, 1, 1) IN ('v', 'V')
							AND substr(version, 2, 1) BETWEEN '0' AND '9'
						GROUP BY item_type, item_id, substr(version, 2)
					)
				)`,
		`UPDATE versions
			SET version = substr(version, 2)
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'`,
		`DELETE FROM artifacts
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'
				AND (
					EXISTS (
						SELECT 1 FROM artifacts canonical
						WHERE canonical.item_type = artifacts.item_type
							AND canonical.item_id = artifacts.item_id
							AND canonical.version = substr(artifacts.version, 2)
							AND canonical.platform_key = artifacts.platform_key
					)
					OR rowid NOT IN (
						SELECT min(rowid) FROM artifacts
						WHERE substr(version, 1, 1) IN ('v', 'V')
							AND substr(version, 2, 1) BETWEEN '0' AND '9'
						GROUP BY item_type, item_id, substr(version, 2), platform_key
					)
				)`,
		`UPDATE artifacts
			SET version = substr(version, 2)
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'`,
		`DELETE FROM version_platforms
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'
				AND (
					EXISTS (
						SELECT 1 FROM version_platforms canonical
						WHERE canonical.item_type = version_platforms.item_type
							AND canonical.item_id = version_platforms.item_id
							AND canonical.version = substr(version_platforms.version, 2)
							AND canonical.platform_key = version_platforms.platform_key
					)
					OR rowid NOT IN (
						SELECT min(rowid) FROM version_platforms
						WHERE substr(version, 1, 1) IN ('v', 'V')
							AND substr(version, 2, 1) BETWEEN '0' AND '9'
						GROUP BY item_type, item_id, substr(version, 2), platform_key
					)
				)`,
		`UPDATE version_platforms
			SET version = substr(version, 2)
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'`,
		`UPDATE download_events
			SET version = substr(version, 2)
			WHERE substr(version, 1, 1) IN ('v', 'V')
				AND substr(version, 2, 1) BETWEEN '0' AND '9'`,
		`UPDATE items
			SET latest_version = substr(latest_version, 2)
			WHERE substr(latest_version, 1, 1) IN ('v', 'V')
				AND substr(latest_version, 2, 1) BETWEEN '0' AND '9'`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	err = tx.Commit()
	return err
}

func (s *Store) backfillVersionPlatforms(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO version_platforms (
		item_type, item_id, version, platform_key, os, arch, description, readme, min_desktop_version, metadata_json, dependencies_json, protocol_json, published, created_at, updated_at
	)
	SELECT v.item_type, v.item_id, v.version, 'universal', '', '', v.description, v.readme, i.min_desktop_version, v.metadata_json, v.dependencies_json, v.protocol_json, v.published, v.published_at, COALESCE(NULLIF(i.updated_at, ''), v.published_at)
	FROM versions v
	JOIN items i ON i.type = v.item_type AND i.id = v.item_id`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO version_platforms (
		item_type, item_id, version, platform_key, os, arch, description, readme, min_desktop_version, metadata_json, dependencies_json, protocol_json, published, created_at, updated_at
	)
	SELECT a.item_type, a.item_id, a.version, a.platform_key, '', '', v.description, v.readme, i.min_desktop_version, v.metadata_json, v.dependencies_json, v.protocol_json, v.published, COALESCE(NULLIF(a.created_at, ''), v.published_at), COALESCE(NULLIF(a.created_at, ''), v.published_at)
	FROM artifacts a
	JOIN versions v ON v.item_type = a.item_type AND v.item_id = a.item_id AND v.version = a.version
	JOIN items i ON i.type = v.item_type AND i.id = v.item_id
	WHERE a.platform_key <> ''`)
	return err
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
	req.Version = canonicalVersion(req.Version)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	reviewStatus := normalizeReviewStatus(req.ReviewStatus, ReviewStatusApproved)
	published := 0
	reviewedAt := ""
	reviewedBy := ""
	if reviewStatus == ReviewStatusApproved {
		published = 1
		reviewedAt = now
		reviewedBy = "admin"
	}
	metadataJSON, err := encodeJSONText(storageMetadata(req.Metadata), "{}")
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
	platform := req.Platform
	if platform == nil {
		platform = &MarketPlatformSpec{Key: req.PlatformKey}
	}
	platformMetadataJSON, err := encodeJSONText(platform.Metadata, "{}")
	if err != nil {
		return err
	}
	platformDependenciesJSON, err := encodeJSONText(platform.Dependencies, "[]")
	if err != nil {
		return err
	}
	platformProtocolJSON, err := encodeJSONText(protocolRecord{
		Install:   platform.Install,
		Uninstall: platform.Uninstall,
		Detect:    platform.Detect,
	}, "{}")
	if err != nil {
		return err
	}
	validationJSON, err := encodeJSONText(req.ValidationChecks, "[]")
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

	creatorID := strings.TrimSpace(req.CreatorID)
	priorStatus := ""
	_ = tx.QueryRowContext(ctx, `SELECT review_status FROM items WHERE type = ? AND id = ?`, req.Type, req.ID).Scan(&priorStatus)
	if _, err = tx.ExecContext(ctx, `INSERT INTO items (
			type, id, name, description, readme, latest_version, min_desktop_version, sandbox_kind, website_kind, creator_id, metadata_json, dependencies_json, protocol_json, adp_yaml, review_status, review_note, reviewed_at, reviewed_by, published, submitted_at, published_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(type, id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			readme = excluded.readme,
			latest_version = excluded.latest_version,
			min_desktop_version = excluded.min_desktop_version,
			sandbox_kind = excluded.sandbox_kind,
			website_kind = excluded.website_kind,
			creator_id = excluded.creator_id,
			metadata_json = excluded.metadata_json,
			dependencies_json = excluded.dependencies_json,
			protocol_json = excluded.protocol_json,
		adp_yaml = excluded.adp_yaml,
		review_status = excluded.review_status,
		review_note = excluded.review_note,
		reviewed_at = excluded.reviewed_at,
		reviewed_by = excluded.reviewed_by,
		published = excluded.published,
		submitted_at = excluded.submitted_at,
		updated_at = excluded.updated_at`,
		req.Type, req.ID, req.Name, req.Description, req.Readme, req.Version, req.MinDesktopVersion, req.SandboxKind, req.WebsiteKind, creatorID, metadataJSON, dependenciesJSON, protocolJSON, strings.TrimSpace(req.ADPYAML), reviewStatus, reviewedAt, reviewedBy, published, now, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO versions (
			item_type, item_id, version, creator_id, description, readme, metadata_json, dependencies_json, protocol_json, adp_yaml, review_status, review_note, reviewed_at, reviewed_by, validation_json, published, submitted_at, published_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id, version) DO UPDATE SET
			creator_id = excluded.creator_id,
			description = excluded.description,
			readme = excluded.readme,
		metadata_json = excluded.metadata_json,
		dependencies_json = excluded.dependencies_json,
		protocol_json = excluded.protocol_json,
		adp_yaml = excluded.adp_yaml,
		review_status = excluded.review_status,
		review_note = excluded.review_note,
		reviewed_at = excluded.reviewed_at,
		reviewed_by = excluded.reviewed_by,
		validation_json = excluded.validation_json,
		published = excluded.published,
		submitted_at = excluded.submitted_at`,
		req.Type, req.ID, req.Version, creatorID, req.Description, req.Readme, metadataJSON, dependenciesJSON, protocolJSON, strings.TrimSpace(req.ADPYAML), reviewStatus, reviewedAt, reviewedBy, validationJSON, published, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO version_platforms (
		item_type, item_id, version, platform_key, os, arch, description, readme, min_desktop_version, metadata_json, dependencies_json, protocol_json, published, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
	ON CONFLICT(item_type, item_id, version, platform_key) DO UPDATE SET
		os = excluded.os,
		arch = excluded.arch,
		description = excluded.description,
		readme = excluded.readme,
		min_desktop_version = excluded.min_desktop_version,
		metadata_json = excluded.metadata_json,
		dependencies_json = excluded.dependencies_json,
		protocol_json = excluded.protocol_json,
		published = 1,
		updated_at = excluded.updated_at`,
		req.Type, req.ID, req.Version, platform.Key, platform.OS, platform.Arch, platform.Description, platform.Readme, platform.MinDesktopVersion, platformMetadataJSON, platformDependenciesJSON, platformProtocolJSON, now, now); err != nil {
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
	if req.Type == TypeSkill {
		profile := req.Skill
		if profile == nil {
			profile = defaultSkillProfile()
		}
		featured := 0
		if profile.Featured {
			featured = 1
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO skill_profiles (
			item_type, item_id, kind, category, scenario, level, package_mode, featured
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id) DO UPDATE SET
			kind = excluded.kind,
			category = excluded.category,
			scenario = excluded.scenario,
			level = excluded.level,
			package_mode = excluded.package_mode,
			featured = excluded.featured`,
			req.Type, req.ID, profile.Kind, profile.Category, profile.Scenario, profile.Level, profile.PackageMode, featured); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM skill_package_items WHERE package_type = ? AND package_id = ?`, req.Type, req.ID); err != nil {
			return err
		}
		if profile.Kind == SkillKindPackage {
			for index, included := range profile.IncludedSkills {
				sortOrder := included.SortOrder
				if sortOrder == 0 {
					sortOrder = index + 1
				}
				optional := 0
				if included.Optional {
					optional = 1
				}
				if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO skill_package_items (
					package_type, package_id, skill_id, optional, sort_order
				) VALUES (?, ?, ?, ?, ?)`, req.Type, req.ID, included.ID, optional, sortOrder); err != nil {
					return err
				}
			}
		}
	}
	if artifact != nil {
		contentsJSON, encodeErr := encodeJSONText(artifact.Entries, "[]")
		if encodeErr != nil {
			return encodeErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO artifacts (
			item_type, item_id, version, platform_key, archive_type, asset_role, original_filename, path, url, sha256, integrity, size_bytes, contents_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id, version, platform_key) DO UPDATE SET
			archive_type = excluded.archive_type,
			asset_role = excluded.asset_role,
			original_filename = excluded.original_filename,
			path = excluded.path,
			url = excluded.url,
			sha256 = excluded.sha256,
			integrity = excluded.integrity,
			size_bytes = excluded.size_bytes,
			contents_json = excluded.contents_json,
			created_at = excluded.created_at`,
			req.Type, req.ID, req.Version, artifact.PlatformKey, artifact.ArchiveType, artifact.AssetRole, artifact.FileName, artifact.Path, artifact.URL, artifact.SHA256, artifact.Integrity, artifact.SizeBytes, contentsJSON, now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_events (
		item_type, item_id, version, action, from_status, to_status, actor_id, note, created_at
	) VALUES (?, ?, ?, 'submitted', ?, ?, ?, '', ?)`, req.Type, req.ID, req.Version, priorStatus, reviewStatus, creatorID, now); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

func (s *Store) Unpublish(ctx context.Context, itemType ItemType, id, version string) error {
	version = canonicalVersion(version)
	if version == "" {
		return errors.New("version is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var latestVersion string
	if err = tx.QueryRowContext(ctx, `SELECT latest_version FROM items WHERE type = ? AND id = ?`, itemType, id).Scan(&latestVersion); err != nil {
		return err
	}
	if version != canonicalVersion(latestVersion) {
		return errUnpublishNotLatest
	}
	result, err := tx.ExecContext(ctx, `UPDATE versions SET published = 0 WHERE item_type = ? AND item_id = ? AND version = ?`, itemType, id, version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE version_platforms SET published = 0, updated_at = ? WHERE item_type = ? AND item_id = ? AND version = ?`, now, itemType, id, version); err != nil {
		return err
	}

	replacement, err := findLatestPublishedVersion(ctx, tx, itemType, id)
	if err != nil {
		return err
	}
	if replacement == "" {
		_, err = tx.ExecContext(ctx, `UPDATE items SET published = 0, updated_at = ? WHERE type = ? AND id = ?`, now, itemType, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE items SET
			latest_version = ?, creator_id = (SELECT creator_id FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			description = (SELECT description FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			readme = (SELECT readme FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			metadata_json = (SELECT metadata_json FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			dependencies_json = (SELECT dependencies_json FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			protocol_json = (SELECT protocol_json FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			adp_yaml = (SELECT adp_yaml FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			review_status = (SELECT review_status FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			review_note = (SELECT review_note FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			reviewed_at = (SELECT reviewed_at FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			reviewed_by = (SELECT reviewed_by FROM versions WHERE item_type = ? AND item_id = ? AND version = ?),
			published = 1, updated_at = ?
			WHERE type = ? AND id = ?`,
			replacement,
			itemType, id, replacement, itemType, id, replacement, itemType, id, replacement,
			itemType, id, replacement, itemType, id, replacement, itemType, id, replacement,
			itemType, id, replacement, itemType, id, replacement, itemType, id, replacement,
			itemType, id, replacement, itemType, id, replacement,
			now, itemType, id)
	}
	if err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

var errUnpublishNotLatest = errors.New("only the current latest version can be unpublished")

func findLatestPublishedVersion(ctx context.Context, tx *sql.Tx, itemType ItemType, id string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM versions WHERE item_type = ? AND item_id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	latest := ""
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return "", err
		}
		if latest == "" {
			if _, ok := compareSemanticVersions(candidate, candidate); ok {
				latest = candidate
			}
			continue
		}
		if comparison, ok := compareSemanticVersions(candidate, latest); ok && comparison > 0 {
			latest = candidate
		}
	}
	return latest, rows.Err()
}

func (s *Store) UpdateReview(ctx context.Context, itemType ItemType, id, status, note, reviewer string) error {
	status = normalizeReviewStatus(status, "")
	if status == "" {
		return errors.New("invalid review status")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	reviewer = strings.TrimSpace(reviewer)
	if reviewer == "" {
		reviewer = "admin"
	}
	published := 0
	if status == ReviewStatusApproved {
		published = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var fromStatus, version string
	if err = tx.QueryRowContext(ctx, `SELECT review_status, latest_version FROM items WHERE type = ? AND id = ?`, itemType, id).Scan(&fromStatus, &version); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE items SET review_status = ?, review_note = ?, reviewed_at = ?, reviewed_by = ?, published = ?, updated_at = ? WHERE type = ? AND id = ?`,
		status, strings.TrimSpace(note), now, reviewer, published, now, itemType, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE versions SET review_status = ?, review_note = ?, reviewed_at = ?, reviewed_by = ?, published = ? WHERE item_type = ? AND item_id = ? AND version = ?`,
		status, strings.TrimSpace(note), now, reviewer, published, itemType, id, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_events (
		item_type, item_id, version, action, from_status, to_status, actor_id, note, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, itemType, id, version, status, fromStatus, status, reviewer, strings.TrimSpace(note), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListPublic(ctx context.Context, onlyType ItemType, viewerUserID string) ([]storedItem, error) {
	return s.listItems(ctx, onlyType, "", viewerUserID, true, "", "")
}

func (s *Store) ListAdmin(ctx context.Context, onlyType ItemType, reviewStatus, viewerUserID string) ([]storedItem, error) {
	return s.listItems(ctx, onlyType, reviewStatus, viewerUserID, false, "", "")
}

func (s *Store) ListCreator(ctx context.Context, creatorID, viewerUserID string) ([]storedItem, error) {
	return s.listItems(ctx, "", "", viewerUserID, false, strings.TrimSpace(creatorID), "")
}

func (s *Store) ListFavorites(ctx context.Context, userID string) ([]storedItem, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, errors.New("user ID is required")
	}
	return s.listItems(ctx, "", "", userID, true, "", userID)
}

func (s *Store) listItems(ctx context.Context, onlyType ItemType, reviewStatus, viewerUserID string, publicOnly bool, creatorID, favoriteUserID string) ([]storedItem, error) {
	query := `SELECT ` + storedItemSelectColumns + ` FROM items`
	args := []any{}
	conditions := []string{}
	if publicOnly {
		conditions = append(conditions, `published = 1`, `review_status = ?`)
		args = append(args, ReviewStatusApproved)
	}
	if onlyType != "" {
		conditions = append(conditions, `type = ?`)
		args = append(args, onlyType)
	}
	if reviewStatus != "" {
		conditions = append(conditions, `review_status = ?`)
		args = append(args, reviewStatus)
	}
	if creatorID != "" {
		conditions = append(conditions, `creator_id = ?`)
		args = append(args, creatorID)
	}
	if favoriteUserID != "" {
		conditions = append(conditions, `EXISTS (SELECT 1 FROM favorite_items f WHERE f.item_type = items.type AND f.item_id = items.id AND f.user_id = ?)`)
		args = append(args, favoriteUserID)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY type, name COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []storedItem
	for rows.Next() {
		item, err := scanStoredItem(rows)
		if err != nil {
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
	if err := s.hydrateItems(ctx, items, onlyType, viewerUserID, publicOnly); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) GetPublic(ctx context.Context, itemType ItemType, id, viewerUserID string) (storedItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+storedItemSelectColumns+`
		FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
	item, err := scanStoredItem(row)
	if err != nil {
		return storedItem{}, err
	}
	items := []storedItem{item}
	if err := s.hydrateItems(ctx, items, itemType, viewerUserID, true); err != nil {
		return storedItem{}, err
	}
	return items[0], nil
}

func (s *Store) GetAdmin(ctx context.Context, itemType ItemType, id, viewerUserID string) (storedItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+storedItemSelectColumns+` FROM items WHERE type = ? AND id = ?`, itemType, id)
	item, err := scanStoredItem(row)
	if err != nil {
		return storedItem{}, err
	}
	items := []storedItem{item}
	if err := s.hydrateItems(ctx, items, itemType, viewerUserID, false); err != nil {
		return storedItem{}, err
	}
	return items[0], nil
}

func (s *Store) GetAdminReviewDetail(ctx context.Context, itemType ItemType, id, viewerUserID string) (AdminReviewDetail, error) {
	item, err := s.GetAdmin(ctx, itemType, id, viewerUserID)
	if err != nil {
		return AdminReviewDetail{}, err
	}
	detail := AdminReviewDetail{
		Item:        publicItem(item),
		Creator:     ReviewCreator{ID: item.CreatorID, Username: item.CreatorUsername, Name: item.CreatorName},
		SubmittedAt: item.SubmittedAt,
		ADPYAML:     item.ADPYAML,
	}
	if detail.SubmittedAt.IsZero() {
		detail.SubmittedAt = item.PublishedAt
	}
	var validationJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT validation_json FROM versions WHERE item_type = ? AND item_id = ? AND version = ?`, itemType, id, item.LatestVersion).Scan(&validationJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AdminReviewDetail{}, err
	}
	if validationJSON != "" {
		if err := decodeJSONText(validationJSON, &detail.ValidationChecks); err != nil {
			return AdminReviewDetail{}, err
		}
	}
	if detail.ValidationChecks == nil {
		detail.ValidationChecks = []ReviewCheck{}
	}
	artifacts, err := s.listAdminReviewArtifacts(ctx, itemType, id, item.LatestVersion)
	if err != nil {
		return AdminReviewDetail{}, err
	}
	detail.Artifacts = artifacts
	history, err := s.listReviewEvents(ctx, itemType, id)
	if err != nil {
		return AdminReviewDetail{}, err
	}
	detail.History = history
	versions, err := s.ListVersions(ctx, itemType, id)
	if err != nil {
		return AdminReviewDetail{}, err
	}
	for index := range versions {
		if versions[index].Version == item.LatestVersion {
			continue
		}
		detail.PreviousVersion = &versions[index]
		detail.IsUpdate = true
		break
	}
	if detail.PreviousVersion != nil {
		detail.Changes = reviewFieldChanges(item, *detail.PreviousVersion)
	} else {
		detail.Changes = []ReviewFieldChange{}
	}
	return detail, nil
}

func (s *Store) listAdminReviewArtifacts(ctx context.Context, itemType ItemType, id, version string) ([]AdminReviewArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT platform_key, original_filename, archive_type, asset_role, url, sha256, integrity, size_bytes, contents_json
		FROM artifacts WHERE item_type = ? AND item_id = ? AND version = ? ORDER BY asset_role, platform_key`, itemType, id, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	artifacts := []AdminReviewArtifact{}
	for rows.Next() {
		var artifact AdminReviewArtifact
		var contentsJSON string
		if err := rows.Scan(&artifact.PlatformKey, &artifact.FileName, &artifact.ArchiveType, &artifact.AssetRole, &artifact.URL, &artifact.SHA256, &artifact.Integrity, &artifact.SizeBytes, &contentsJSON); err != nil {
			return nil, err
		}
		if err := decodeJSONText(contentsJSON, &artifact.Files); err != nil {
			return nil, err
		}
		if artifact.Files == nil {
			artifact.Files = []ArtifactFileSummary{}
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (s *Store) listReviewEvents(ctx context.Context, itemType ItemType, id string) ([]ReviewEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, item_type, item_id, version, action, from_status, to_status, actor_id, note, created_at
		FROM review_events WHERE item_type = ? AND item_id = ? ORDER BY created_at DESC, id DESC`, itemType, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []ReviewEvent{}
	for rows.Next() {
		var event ReviewEvent
		var createdAt string
		if err := rows.Scan(&event.ID, &event.ItemType, &event.ItemID, &event.Version, &event.Action, &event.FromStatus, &event.ToStatus, &event.ActorID, &event.Note, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTime(createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}

func reviewFieldChanges(item storedItem, previous PublicVersion) []ReviewFieldChange {
	candidates := []ReviewFieldChange{
		{Field: "version", Previous: previous.Version, Current: item.LatestVersion},
		{Field: "description", Previous: previous.Description, Current: item.Description},
		{Field: "readme", Previous: previous.Readme, Current: item.Readme},
		{Field: "metadata", Previous: reviewValue(previous.Metadata), Current: reviewValue(item.Metadata)},
		{Field: "dependencies", Previous: reviewValue(previous.Dependencies), Current: reviewValue(item.Dependencies)},
		{Field: "platforms", Previous: reviewValue(previous.Platforms), Current: reviewValue(item.Platforms)},
	}
	changes := make([]ReviewFieldChange, 0, len(candidates))
	for _, change := range candidates {
		if change.Previous != change.Current {
			changes = append(changes, change)
		}
	}
	return changes
}

func reviewValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

type itemScanner interface {
	Scan(dest ...any) error
}

func scanStoredItem(scanner itemScanner) (storedItem, error) {
	var item storedItem
	var published int
	var submittedAt, publishedAt, updatedAt, reviewedAt string
	var metadataJSON, dependenciesJSON, protocolJSON string
	if err := scanner.Scan(&item.Type, &item.ID, &item.Name, &item.Description, &item.Readme, &item.LatestVersion, &item.MinDesktopVersion, &item.SandboxKind, &item.WebsiteKind, &item.CreatorID, &metadataJSON, &dependenciesJSON, &protocolJSON, &item.ADPYAML, &item.ReviewStatus, &item.ReviewNote, &reviewedAt, &item.ReviewedBy, &item.DetailViewCount, &published, &submittedAt, &publishedAt, &updatedAt); err != nil {
		return storedItem{}, err
	}
	item.ReviewStatus = normalizeReviewStatus(item.ReviewStatus, ReviewStatusApproved)
	item.Published = published == 1
	item.PublishedAt = parseTime(publishedAt)
	item.SubmittedAt = parseTime(submittedAt)
	if item.SubmittedAt.IsZero() {
		item.SubmittedAt = item.PublishedAt
	}
	item.UpdatedAt = parseTime(updatedAt)
	item.ReviewedAt = parseTime(reviewedAt)
	if err := decodeItemJSON(&item, metadataJSON, dependenciesJSON, protocolJSON); err != nil {
		return storedItem{}, err
	}
	return item, nil
}

func (s *Store) hydrateItems(ctx context.Context, items []storedItem, onlyType ItemType, viewerUserID string, publicOnly bool) error {
	if err := s.loadCreatorProfilesForItems(ctx, items); err != nil {
		return err
	}
	if err := s.loadTagsForItems(ctx, items, onlyType, publicOnly); err != nil {
		return err
	}
	if err := s.loadAssetsForItems(ctx, items, onlyType, publicOnly); err != nil {
		return err
	}
	if err := s.loadPlatformsForItems(ctx, items, onlyType, publicOnly); err != nil {
		return err
	}
	if err := s.loadSkillProfilesForItems(ctx, items, onlyType, publicOnly); err != nil {
		return err
	}
	return s.loadStatsForItems(ctx, items, onlyType, viewerUserID, publicOnly)
}

func (s *Store) loadCreatorProfilesForItems(ctx context.Context, items []storedItem) error {
	creatorIDs := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		if item.CreatorID == "" {
			continue
		}
		if _, ok := seen[item.CreatorID]; ok {
			continue
		}
		seen[item.CreatorID] = struct{}{}
		creatorIDs = append(creatorIDs, item.CreatorID)
	}
	if len(creatorIDs) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(creatorIDs)), ",")
	args := make([]any, len(creatorIDs))
	for index, creatorID := range creatorIDs {
		args[index] = creatorID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, username FROM users WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type creatorProfile struct{ name, username string }
	profiles := map[string]creatorProfile{}
	for rows.Next() {
		var id, name, username string
		if err := rows.Scan(&id, &name, &username); err != nil {
			return err
		}
		profiles[id] = creatorProfile{name: name, username: username}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range items {
		if profile, ok := profiles[items[index].CreatorID]; ok {
			items[index].CreatorName = profile.name
			items[index].CreatorUsername = profile.username
		}
	}
	return nil
}

func (s *Store) GetADPYAML(ctx context.Context, itemType ItemType, id, version string) (string, error) {
	version = canonicalVersion(version)
	if version == "" {
		row := s.db.QueryRowContext(ctx, `SELECT adp_yaml FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
		var value string
		if err := row.Scan(&value); err != nil {
			return "", err
		}
		if strings.TrimSpace(value) == "" {
			return "", sql.ErrNoRows
		}
		return value, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT v.adp_yaml FROM versions v
		JOIN items i ON i.type = v.item_type AND i.id = v.item_id
		WHERE v.item_type = ? AND v.item_id = ? AND v.version = ?
			AND v.published = 1 AND v.review_status = ? AND i.published = 1 AND i.review_status = ?`, itemType, id, version, ReviewStatusApproved, ReviewStatusApproved)
	var value string
	if err := row.Scan(&value); err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", sql.ErrNoRows
	}
	return value, nil
}

func (s *Store) GetArtifact(ctx context.Context, itemType ItemType, id, version, platform string) (storedArtifact, error) {
	version = canonicalVersion(version)
	if version == "" {
		row := s.db.QueryRowContext(ctx, `SELECT latest_version FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
		if err := row.Scan(&version); err != nil {
			return storedArtifact{}, err
		}
	}
	for _, key := range platformFallbackCandidates(platform) {
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

func (s *Store) EnsurePublishedVersion(ctx context.Context, itemType ItemType, id, version string) error {
	version = canonicalVersion(version)
	if version == "" {
		return sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `SELECT 1
		FROM versions v
		JOIN items i ON i.type = v.item_type AND i.id = v.item_id
		WHERE v.item_type = ? AND v.item_id = ? AND v.version = ?
			AND v.published = 1 AND v.review_status = ?
			AND i.published = 1 AND i.review_status = ?`,
		itemType, id, version, ReviewStatusApproved, ReviewStatusApproved)
	var exists int
	return row.Scan(&exists)
}

func (s *Store) EnsurePublishedArtifactPath(ctx context.Context, artifactPath string) error {
	row := s.db.QueryRowContext(ctx, `SELECT 1
		FROM artifacts a
		JOIN versions v ON v.item_type = a.item_type AND v.item_id = a.item_id AND v.version = a.version
		JOIN items i ON i.type = a.item_type AND i.id = a.item_id
		WHERE a.path = ?
			AND v.published = 1 AND v.review_status = ?
			AND i.published = 1 AND i.review_status = ?`,
		artifactPath, ReviewStatusApproved, ReviewStatusApproved)
	var exists int
	return row.Scan(&exists)
}

func (s *Store) getArtifact(ctx context.Context, itemType ItemType, id, version, platform string) (storedArtifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT a.platform_key, a.archive_type, a.asset_role, a.path, a.url, a.sha256, a.integrity, a.size_bytes
		FROM artifacts a
		JOIN versions v ON v.item_type = a.item_type AND v.item_id = a.item_id AND v.version = a.version
		JOIN items i ON i.type = a.item_type AND i.id = a.item_id
		WHERE a.item_type = ? AND a.item_id = ? AND a.version = ? AND a.platform_key = ?
			AND v.published = 1 AND v.review_status = ? AND i.published = 1 AND i.review_status = ?`,
		itemType, id, version, platform, ReviewStatusApproved, ReviewStatusApproved)
	var artifact storedArtifact
	artifact.Version = version
	err := row.Scan(&artifact.PlatformKey, &artifact.ArchiveType, &artifact.AssetRole, &artifact.Path, &artifact.URL, &artifact.SHA256, &artifact.Integrity, &artifact.SizeBytes)
	return artifact, err
}

func (s *Store) GetPlatform(ctx context.Context, itemType ItemType, id, version, platform string) (PublicPlatform, error) {
	version = canonicalVersion(version)
	if version == "" {
		row := s.db.QueryRowContext(ctx, `SELECT latest_version FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
		if err := row.Scan(&version); err != nil {
			return PublicPlatform{}, err
		}
	}
	for _, key := range platformFallbackCandidates(platform) {
		spec, err := s.getPlatform(ctx, itemType, id, version, key)
		if err == nil {
			return spec, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return PublicPlatform{}, err
		}
	}
	return PublicPlatform{}, sql.ErrNoRows
}

func (s *Store) getPlatform(ctx context.Context, itemType ItemType, id, version, platform string) (PublicPlatform, error) {
	row := s.db.QueryRowContext(ctx, `SELECT p.platform_key, p.os, p.arch, p.description, p.readme, p.min_desktop_version, p.metadata_json, p.dependencies_json, p.protocol_json
		FROM version_platforms p
		JOIN versions v ON v.item_type = p.item_type AND v.item_id = p.item_id AND v.version = p.version
		JOIN items i ON i.type = p.item_type AND i.id = p.item_id
		WHERE p.item_type = ? AND p.item_id = ? AND p.version = ? AND p.platform_key = ? AND p.published = 1
			AND v.published = 1 AND v.review_status = ? AND i.published = 1 AND i.review_status = ?`,
		itemType, id, version, platform, ReviewStatusApproved, ReviewStatusApproved)
	var spec PublicPlatform
	var metadataJSON, dependenciesJSON, protocolJSON string
	if err := row.Scan(&spec.Platform, &spec.OS, &spec.Arch, &spec.Description, &spec.Readme, &spec.MinDesktopVersion, &metadataJSON, &dependenciesJSON, &protocolJSON); err != nil {
		return PublicPlatform{}, err
	}
	if err := decodePlatformJSON(&spec, metadataJSON, dependenciesJSON, protocolJSON); err != nil {
		return PublicPlatform{}, err
	}
	return spec, nil
}

func (s *Store) RecordDownload(ctx context.Context, itemType ItemType, id, version, platform, userAgent, ip string) {
	version = canonicalVersion(version)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO download_events (
		item_type, item_id, version, artifact_platform, user_agent, ip, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, itemType, id, version, platform, userAgent, ip, time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *Store) FavoriteItem(ctx context.Context, itemType ItemType, id, userID string) error {
	if err := s.ensurePublishedItem(ctx, itemType, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO favorite_items (
		item_type, item_id, user_id, created_at
	) VALUES (?, ?, ?, ?)`, itemType, id, userID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) UnfavoriteItem(ctx context.Context, itemType ItemType, id, userID string) error {
	if err := s.ensurePublishedItem(ctx, itemType, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM favorite_items WHERE item_type = ? AND item_id = ? AND user_id = ?`, itemType, id, userID)
	return err
}

func (s *Store) ensurePublishedItem(ctx context.Context, itemType ItemType, id string) error {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, id, ReviewStatusApproved)
	var exists int
	return row.Scan(&exists)
}

func (s *Store) ListVersions(ctx context.Context, itemType ItemType, id string) ([]PublicVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version, description, readme, metadata_json, dependencies_json, published_at
		FROM versions WHERE item_type = ? AND item_id = ? AND published = 1 AND review_status = ? ORDER BY published_at DESC, version DESC`, itemType, id, ReviewStatusApproved)
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
	if err := s.loadPlatformsForVersions(ctx, itemType, id, versions); err != nil {
		return nil, err
	}
	return versions, nil
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

func (s *Store) loadTagsForItems(ctx context.Context, items []storedItem, onlyType ItemType, publicOnly bool) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[itemLookupKey]int, len(items))
	for i := range items {
		index[itemLookupKey{itemType: items[i].Type, id: items[i].ID}] = i
	}
	query := `SELECT t.item_type, t.item_id, t.tag
		FROM tags t
		JOIN items i ON i.type = t.item_type AND i.id = t.item_id`
	args := []any{}
	if publicOnly {
		query += ` WHERE i.published = 1 AND i.review_status = ?`
		args = append(args, ReviewStatusApproved)
	}
	if onlyType != "" {
		if publicOnly {
			query += ` AND i.type = ?`
		} else {
			query += ` WHERE i.type = ?`
		}
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

func (s *Store) loadAssetsForItems(ctx context.Context, items []storedItem, onlyType ItemType, publicOnly bool) error {
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
		JOIN versions v ON v.item_type = a.item_type AND v.item_id = a.item_id AND v.version = a.version`
	args := []any{}
	if publicOnly {
		query += ` WHERE i.published = 1 AND i.review_status = ? AND v.published = 1 AND v.review_status = ?`
		args = append(args, ReviewStatusApproved, ReviewStatusApproved)
	}
	if onlyType != "" {
		if publicOnly {
			query += ` AND i.type = ?`
		} else {
			query += ` WHERE i.type = ?`
		}
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

func (s *Store) loadSkillProfilesForItems(ctx context.Context, items []storedItem, onlyType ItemType, publicOnly bool) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[itemLookupKey]int, len(items))
	for i := range items {
		if items[i].Type == TypeSkill {
			items[i].Skill = &PublicSkillProfile{Kind: SkillKindSingle, Category: "other"}
			index[itemLookupKey{itemType: items[i].Type, id: items[i].ID}] = i
		}
	}
	if len(index) == 0 || (onlyType != "" && onlyType != TypeSkill) {
		return nil
	}
	query := `SELECT sp.item_type, sp.item_id, sp.kind, sp.category, sp.scenario, sp.level, sp.package_mode, sp.featured
		FROM skill_profiles sp
		JOIN items i ON i.type = sp.item_type AND i.id = sp.item_id
		WHERE sp.item_type = ?`
	args := []any{TypeSkill}
	if publicOnly {
		query += ` AND i.published = 1 AND i.review_status = ?`
		args = append(args, ReviewStatusApproved)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, id string
		var profile PublicSkillProfile
		var featured int
		if err := rows.Scan(&rawType, &id, &profile.Kind, &profile.Category, &profile.Scenario, &profile.Level, &profile.PackageMode, &featured); err != nil {
			return err
		}
		profile.Featured = featured == 1
		key := itemLookupKey{itemType: ItemType(rawType), id: id}
		if itemIndex, ok := index[key]; ok {
			items[itemIndex].Skill = &profile
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return s.loadSkillPackageItemsForItems(ctx, items, index, publicOnly)
}

func (s *Store) loadSkillPackageItemsForItems(ctx context.Context, items []storedItem, index map[itemLookupKey]int, publicOnly bool) error {
	query := `SELECT spi.package_type, spi.package_id, spi.skill_id, COALESCE(i.name, ''), spi.optional, spi.sort_order
		FROM skill_package_items spi
		LEFT JOIN items i ON i.type = ? AND i.id = spi.skill_id`
	if publicOnly {
		query += ` AND i.published = 1 AND i.review_status = ?`
	}
	query += ` ORDER BY spi.package_type, spi.package_id, spi.sort_order, spi.skill_id`
	args := []any{TypeSkill}
	if publicOnly {
		args = append(args, ReviewStatusApproved)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, packageID, skillID, name string
		var optional, sortOrder int
		if err := rows.Scan(&rawType, &packageID, &skillID, &name, &optional, &sortOrder); err != nil {
			return err
		}
		itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: packageID}]
		if !ok || items[itemIndex].Skill == nil || items[itemIndex].Skill.Kind != SkillKindPackage {
			continue
		}
		items[itemIndex].Skill.IncludedSkills = append(items[itemIndex].Skill.IncludedSkills, PublicSkillPackageItem{
			ID:        skillID,
			Name:      name,
			Optional:  optional == 1,
			SortOrder: sortOrder,
		})
	}
	return rows.Err()
}

func (s *Store) loadPlatformsForItems(ctx context.Context, items []storedItem, onlyType ItemType, publicOnly bool) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[versionLookupKey]int, len(items))
	for i := range items {
		items[i].Platforms = map[string]PublicPlatform{}
		index[versionLookupKey{itemType: items[i].Type, id: items[i].ID, version: items[i].LatestVersion}] = i
	}
	query := `SELECT p.item_type, p.item_id, p.version, p.platform_key, p.os, p.arch, p.description, p.readme, p.min_desktop_version, p.metadata_json, p.dependencies_json, p.protocol_json
		FROM version_platforms p
		JOIN items i ON i.type = p.item_type AND i.id = p.item_id AND i.latest_version = p.version
		JOIN versions v ON v.item_type = p.item_type AND v.item_id = p.item_id AND v.version = p.version`
	args := []any{}
	if publicOnly {
		query += ` WHERE i.published = 1 AND i.review_status = ? AND p.published = 1 AND v.published = 1 AND v.review_status = ?`
		args = append(args, ReviewStatusApproved, ReviewStatusApproved)
	}
	if onlyType != "" {
		if publicOnly {
			query += ` AND i.type = ?`
		} else {
			query += ` WHERE i.type = ?`
		}
		args = append(args, onlyType)
	}
	query += ` ORDER BY p.item_type, p.item_id, p.version, p.platform_key`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, id, version string
		var spec PublicPlatform
		var metadataJSON, dependenciesJSON, protocolJSON string
		if err := rows.Scan(&rawType, &id, &version, &spec.Platform, &spec.OS, &spec.Arch, &spec.Description, &spec.Readme, &spec.MinDesktopVersion, &metadataJSON, &dependenciesJSON, &protocolJSON); err != nil {
			return err
		}
		itemIndex, ok := index[versionLookupKey{itemType: ItemType(rawType), id: id, version: version}]
		if !ok {
			continue
		}
		if err := decodePlatformJSON(&spec, metadataJSON, dependenciesJSON, protocolJSON); err != nil {
			return err
		}
		items[itemIndex].Platforms[spec.Platform] = spec
	}
	return rows.Err()
}

func (s *Store) loadStatsForItems(ctx context.Context, items []storedItem, onlyType ItemType, viewerUserID string, publicOnly bool) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[itemLookupKey]int, len(items))
	for i := range items {
		index[itemLookupKey{itemType: items[i].Type, id: items[i].ID}] = i
	}

	downloadQuery := `SELECT d.item_type, d.item_id, COUNT(*)
		FROM download_events d
		JOIN items i ON i.type = d.item_type AND i.id = d.item_id`
	args := []any{}
	if publicOnly {
		downloadQuery += ` WHERE i.published = 1 AND i.review_status = ?`
		args = append(args, ReviewStatusApproved)
	}
	if onlyType != "" {
		if publicOnly {
			downloadQuery += ` AND i.type = ?`
		} else {
			downloadQuery += ` WHERE i.type = ?`
		}
		args = append(args, onlyType)
	}
	downloadQuery += ` GROUP BY d.item_type, d.item_id`
	rows, err := s.db.QueryContext(ctx, downloadQuery, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rawType, id string
		var count int
		if err := rows.Scan(&rawType, &id, &count); err != nil {
			_ = rows.Close()
			return err
		}
		if itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: id}]; ok {
			items[itemIndex].DownloadCount = count
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	favoriteQuery := `SELECT f.item_type, f.item_id, COUNT(*)
		FROM favorite_items f
		JOIN items i ON i.type = f.item_type AND i.id = f.item_id`
	args = []any{}
	if publicOnly {
		favoriteQuery += ` WHERE i.published = 1 AND i.review_status = ?`
		args = append(args, ReviewStatusApproved)
	}
	if onlyType != "" {
		if publicOnly {
			favoriteQuery += ` AND i.type = ?`
		} else {
			favoriteQuery += ` WHERE i.type = ?`
		}
		args = append(args, onlyType)
	}
	favoriteQuery += ` GROUP BY f.item_type, f.item_id`
	rows, err = s.db.QueryContext(ctx, favoriteQuery, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rawType, id string
		var count int
		if err := rows.Scan(&rawType, &id, &count); err != nil {
			_ = rows.Close()
			return err
		}
		if itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: id}]; ok {
			items[itemIndex].FavoriteCount = count
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	commentQuery := `SELECT c.item_type, c.item_id, COUNT(*),
		SUM(CASE WHEN c.sentiment = ? THEN 1 ELSE 0 END),
		SUM(CASE WHEN c.sentiment = ? THEN 1 ELSE 0 END)
		FROM item_comments c
		JOIN items i ON i.type = c.item_type AND i.id = c.item_id
		WHERE c.status = ?`
	args = []any{CommentSentimentPositive, CommentSentimentNegative, CommentStatusVisible}
	if publicOnly {
		commentQuery += ` AND i.published = 1 AND i.review_status = ?`
		args = append(args, ReviewStatusApproved)
	}
	if onlyType != "" {
		commentQuery += ` AND i.type = ?`
		args = append(args, onlyType)
	}
	commentQuery += ` GROUP BY c.item_type, c.item_id`
	rows, err = s.db.QueryContext(ctx, commentQuery, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rawType, id string
		var total, positive, negative int
		if err := rows.Scan(&rawType, &id, &total, &positive, &negative); err != nil {
			_ = rows.Close()
			return err
		}
		if itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: id}]; ok {
			items[itemIndex].CommentCount = total
			items[itemIndex].PositiveCount = positive
			items[itemIndex].NegativeCount = negative
			if total > 0 {
				items[itemIndex].PositiveRate = float64(positive) * 100 / float64(total)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	viewerUserID = strings.TrimSpace(viewerUserID)
	if viewerUserID == "" {
		return nil
	}
	viewerQuery := `SELECT f.item_type, f.item_id
		FROM favorite_items f
		JOIN items i ON i.type = f.item_type AND i.id = f.item_id
		WHERE i.published = 1 AND f.user_id = ?`
	args = []any{viewerUserID}
	if onlyType != "" {
		viewerQuery += ` AND i.type = ?`
		args = append(args, onlyType)
	}
	rows, err = s.db.QueryContext(ctx, viewerQuery, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rawType, id string
		if err := rows.Scan(&rawType, &id); err != nil {
			return err
		}
		if itemIndex, ok := index[itemLookupKey{itemType: ItemType(rawType), id: id}]; ok {
			items[itemIndex].Favorited = true
		}
	}
	return rows.Err()
}

func (s *Store) ListPublicComments(ctx context.Context, itemType ItemType, itemID, viewerUserID string, page, limit int) ([]ItemComment, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_comments WHERE item_type = ? AND item_id = ? AND status = ?`, itemType, itemID, CommentStatusVisible).Scan(&total); err != nil {
		return nil, 0, err
	}
	start, _ := pageWindow(page, limit, total)
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.item_type, c.item_id, c.user_id,
		COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), c.user_id),
		c.sentiment, c.content, c.created_at, c.updated_at
		FROM item_comments c LEFT JOIN users u ON u.id = c.user_id
		WHERE c.item_type = ? AND c.item_id = ? AND c.status = ?
		ORDER BY c.created_at DESC, c.id DESC LIMIT ? OFFSET ?`, itemType, itemID, CommentStatusVisible, limit, start)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	comments := make([]ItemComment, 0)
	for rows.Next() {
		var comment ItemComment
		var ownerID string
		if err := rows.Scan(&comment.ID, &comment.ItemType, &comment.ItemID, &ownerID, &comment.Author, &comment.Sentiment, &comment.Content, &comment.CreatedAt, &comment.UpdatedAt); err != nil {
			return nil, 0, err
		}
		comment.Mine = viewerUserID != "" && ownerID == viewerUserID
		comments = append(comments, comment)
	}
	return comments, total, rows.Err()
}

func (s *Store) CreateComment(ctx context.Context, itemType ItemType, itemID, userID, sentiment, content string) (ItemComment, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM items WHERE type = ? AND id = ? AND published = 1 AND review_status = ?`, itemType, itemID, ReviewStatusApproved).Scan(&exists); err != nil {
		return ItemComment{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `INSERT INTO item_comments (item_type, item_id, user_id, sentiment, content, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, itemType, itemID, userID, sentiment, content, CommentStatusVisible, now, now)
	if err != nil {
		return ItemComment{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ItemComment{}, err
	}
	return s.getComment(ctx, id, userID, false)
}

func (s *Store) UpdateOwnComment(ctx context.Context, id int64, itemType ItemType, itemID, userID, sentiment, content string) (ItemComment, error) {
	ownerID, status, err := s.commentOwnerAndStatusForItem(ctx, id, itemType, itemID)
	if err != nil {
		return ItemComment{}, err
	}
	if ownerID != userID {
		return ItemComment{}, errCommentForbidden
	}
	if status == CommentStatusHidden {
		return ItemComment{}, errCommentHidden
	}
	if status == CommentStatusDeleted {
		return ItemComment{}, sql.ErrNoRows
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE item_comments SET sentiment = ?, content = ?, updated_at = ? WHERE id = ?`, sentiment, content, now, id); err != nil {
		return ItemComment{}, err
	}
	return s.getComment(ctx, id, userID, false)
}

func (s *Store) DeleteOwnComment(ctx context.Context, id int64, itemType ItemType, itemID, userID string) error {
	ownerID, status, err := s.commentOwnerAndStatusForItem(ctx, id, itemType, itemID)
	if err != nil {
		return err
	}
	if ownerID != userID {
		return errCommentForbidden
	}
	if status == CommentStatusDeleted {
		return sql.ErrNoRows
	}
	_, err = s.db.ExecContext(ctx, `UPDATE item_comments SET status = ?, updated_at = ? WHERE id = ?`, CommentStatusDeleted, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) ListAdminComments(ctx context.Context, status string, page, limit int) ([]ItemComment, int, error) {
	where := ` WHERE c.status <> ?`
	args := []any{CommentStatusDeleted}
	if status == CommentStatusVisible || status == CommentStatusHidden {
		where += ` AND c.status = ?`
		args = append(args, status)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_comments c`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	start, _ := pageWindow(page, limit, total)
	query := `SELECT c.id, c.item_type, c.item_id, c.user_id,
		COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), c.user_id),
		c.sentiment, c.content, c.status, c.moderated_by, c.moderation_reason, c.moderated_at, c.created_at, c.updated_at
		FROM item_comments c LEFT JOIN users u ON u.id = c.user_id` + where + ` ORDER BY c.created_at DESC, c.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, start)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	comments := make([]ItemComment, 0)
	for rows.Next() {
		var comment ItemComment
		if err := rows.Scan(&comment.ID, &comment.ItemType, &comment.ItemID, &comment.UserID, &comment.Author, &comment.Sentiment, &comment.Content, &comment.Status, &comment.ModeratedBy, &comment.ModerationReason, &comment.ModeratedAt, &comment.CreatedAt, &comment.UpdatedAt); err != nil {
			return nil, 0, err
		}
		comments = append(comments, comment)
	}
	return comments, total, rows.Err()
}

func (s *Store) ModerateComment(ctx context.Context, id int64, status, reason, moderator string) (ItemComment, error) {
	_, currentStatus, err := s.commentOwnerAndStatus(ctx, id)
	if err != nil {
		return ItemComment{}, err
	}
	if currentStatus == CommentStatusDeleted {
		return ItemComment{}, sql.ErrNoRows
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE item_comments SET status = ?, moderated_by = ?, moderation_reason = ?, moderated_at = ?, updated_at = ? WHERE id = ?`, status, moderator, reason, now, now, id); err != nil {
		return ItemComment{}, err
	}
	return s.getComment(ctx, id, "", true)
}

func (s *Store) commentOwnerAndStatus(ctx context.Context, id int64) (string, string, error) {
	var ownerID, status string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, status FROM item_comments WHERE id = ?`, id).Scan(&ownerID, &status)
	return ownerID, status, err
}

func (s *Store) commentOwnerAndStatusForItem(ctx context.Context, id int64, itemType ItemType, itemID string) (string, string, error) {
	var ownerID, status string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, status FROM item_comments WHERE id = ? AND item_type = ? AND item_id = ?`, id, itemType, itemID).Scan(&ownerID, &status)
	return ownerID, status, err
}

func (s *Store) getComment(ctx context.Context, id int64, viewerUserID string, admin bool) (ItemComment, error) {
	var comment ItemComment
	err := s.db.QueryRowContext(ctx, `SELECT c.id, c.item_type, c.item_id, c.user_id,
		COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), c.user_id),
		c.sentiment, c.content, c.status, c.moderated_by, c.moderation_reason, c.moderated_at, c.created_at, c.updated_at
		FROM item_comments c LEFT JOIN users u ON u.id = c.user_id WHERE c.id = ?`, id).Scan(
		&comment.ID, &comment.ItemType, &comment.ItemID, &comment.UserID, &comment.Author, &comment.Sentiment, &comment.Content, &comment.Status,
		&comment.ModeratedBy, &comment.ModerationReason, &comment.ModeratedAt, &comment.CreatedAt, &comment.UpdatedAt,
	)
	if err != nil {
		return ItemComment{}, err
	}
	comment.Mine = viewerUserID != "" && comment.UserID == viewerUserID
	if !admin {
		comment.UserID = ""
		comment.Status = ""
		comment.ModeratedBy = ""
		comment.ModerationReason = ""
		comment.ModeratedAt = ""
	}
	return comment, nil
}

func (s *Store) loadPlatformsForVersions(ctx context.Context, itemType ItemType, id string, versions []PublicVersion) error {
	if len(versions) == 0 {
		return nil
	}
	index := make(map[string]int, len(versions))
	for i := range versions {
		versions[i].Platforms = map[string]PublicPlatform{}
		index[versions[i].Version] = i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version, platform_key, os, arch, description, readme, min_desktop_version, metadata_json, dependencies_json, protocol_json
		FROM version_platforms WHERE item_type = ? AND item_id = ? AND published = 1 ORDER BY version, platform_key`, itemType, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		var spec PublicPlatform
		var metadataJSON, dependenciesJSON, protocolJSON string
		if err := rows.Scan(&version, &spec.Platform, &spec.OS, &spec.Arch, &spec.Description, &spec.Readme, &spec.MinDesktopVersion, &metadataJSON, &dependenciesJSON, &protocolJSON); err != nil {
			return err
		}
		versionIndex, ok := index[version]
		if !ok {
			continue
		}
		if err := decodePlatformJSON(&spec, metadataJSON, dependenciesJSON, protocolJSON); err != nil {
			return err
		}
		versions[versionIndex].Platforms[spec.Platform] = spec
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

func storageMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, len(metadata))
	for key, value := range metadata {
		if strings.EqualFold(strings.TrimSpace(key), "creatorId") {
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func decodePlatformJSON(spec *PublicPlatform, metadataJSON, dependenciesJSON, protocolJSON string) error {
	if err := decodeJSONText(metadataJSON, &spec.Metadata); err != nil {
		return err
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]string{}
	}
	if err := decodeJSONText(dependenciesJSON, &spec.Dependencies); err != nil {
		return err
	}
	if spec.Dependencies == nil {
		spec.Dependencies = []MarketDependency{}
	}
	var protocol protocolRecord
	if err := decodeJSONText(protocolJSON, &protocol); err != nil {
		return err
	}
	spec.Install = protocol.Install
	spec.Uninstall = protocol.Uninstall
	spec.Detect = protocol.Detect
	return nil
}

func platformFallbackCandidates(platform string) []string {
	platform = sanitizePlatform(platform)
	if platform == "" || platform == "universal" {
		return []string{"universal"}
	}
	var candidates []string
	addCandidate := func(candidate string) {
		candidate = sanitizePlatform(candidate)
		if candidate == "" {
			return
		}
		for _, existing := range candidates {
			if existing == candidate {
				return
			}
		}
		candidates = append(candidates, candidate)
	}
	addCandidate(platform)
	parts := strings.Split(platform, "-")
	if len(parts) >= 2 {
		addCandidate(parts[0] + "-" + parts[1])
	}
	if len(parts) >= 1 {
		addCandidate(parts[0])
	}
	addCandidate("universal")
	return candidates
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
	case "software-package", "software-packages", "software", "softwares", "dependency-package", "dependency-packages", "软件依赖包":
		return TypeSoftwarePackage, nil
	default:
		return "", fmt.Errorf("unsupported item type %q", value)
	}
}

func itemAuthor(item storedItem) string {
	if name := strings.TrimSpace(item.CreatorName); name != "" {
		return name
	}
	if username := strings.TrimSpace(item.CreatorUsername); username != "" {
		return username
	}
	if item.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(item.Metadata["author"])
}

func normalizeReviewStatus(status, fallback string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case ReviewStatusPending:
		return ReviewStatusPending
	case ReviewStatusApproved:
		return ReviewStatusApproved
	case ReviewStatusRejected:
		return ReviewStatusRejected
	default:
		return fallback
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
		Author:            itemAuthor(item),
		MinDesktopVersion: item.MinDesktopVersion,
		SandboxKind:       item.SandboxKind,
		WebsiteKind:       item.WebsiteKind,
		NpmPackage:        npmPackageName(item.Type, item.ID),
		Assets:            item.Assets,
		Platforms:         item.Platforms,
		Dependencies:      item.Dependencies,
		Metadata:          publicMetadata(item.Metadata),
		Install:           item.Install,
		Uninstall:         item.Uninstall,
		Detect:            item.Detect,
		Skill:             item.Skill,
		ADPInstallURL:     adpInstallURL(item.Type, item.ID, item.ADPYAML),
		ReviewStatus:      item.ReviewStatus,
		ReviewNote:        item.ReviewNote,
		ReviewedAt:        timePtr(item.ReviewedAt),
		ReviewedBy:        item.ReviewedBy,
		CreatedAt:         item.PublishedAt,
		PublishedAt:       item.PublishedAt,
		UpdatedAt:         item.UpdatedAt,
		DownloadCount:     item.DownloadCount,
		FavoriteCount:     item.FavoriteCount,
		Favorited:         item.Favorited,
		CommentCount:      item.CommentCount,
		PositiveCount:     item.PositiveCount,
		NegativeCount:     item.NegativeCount,
		PositiveRate:      item.PositiveRate,
	}
}

func marketItem(item storedItem) PublicItem {
	result := publicItem(item)
	result.ReviewStatus = ""
	result.ReviewNote = ""
	result.ReviewedAt = nil
	result.ReviewedBy = ""
	return result
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func publicMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, len(metadata))
	for key, value := range metadata {
		if strings.EqualFold(strings.TrimSpace(key), "creatorId") {
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func adpInstallURL(itemType ItemType, id string, adpYAML string) string {
	if strings.TrimSpace(adpYAML) == "" {
		return ""
	}
	switch itemType {
	case TypeCLITool, TypeSkill:
	default:
		return ""
	}
	return "/api/v1/adp/" + string(itemType) + "/" + id
}

func publicItems(items []storedItem) []PublicItem {
	result := make([]PublicItem, 0, len(items))
	for _, item := range items {
		result = append(result, publicItem(item))
	}
	return result
}

func creatorItems(items []storedItem) []CreatorItem {
	result := make([]CreatorItem, 0, len(items))
	for _, item := range items {
		result = append(result, CreatorItem{
			PublicItem:      publicItem(item),
			DetailViewCount: item.DetailViewCount,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func marketItems(items []storedItem) []PublicItem {
	result := make([]PublicItem, 0, len(items))
	for _, item := range items {
		result = append(result, marketItem(item))
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
