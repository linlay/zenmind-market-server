package market

import "time"

type Config struct {
	Addr           string
	DatabasePath   string
	ArtifactRoot   string
	PublicBaseURL  string
	AdminToken     string
	ProxyToken     string
	MaxUploadBytes int64
}

type ItemType string

const (
	TypeSkill   ItemType = "skill"
	TypePlugin  ItemType = "plugin"
	TypeSandbox ItemType = "sandbox"
)

type PublishRequest struct {
	Type              ItemType          `json:"type"`
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Version           string            `json:"version"`
	Description       string            `json:"description"`
	Readme            string            `json:"readme"`
	Tags              []string          `json:"tags"`
	MinDesktopVersion string            `json:"minDesktopVersion"`
	SandboxKind       string            `json:"sandboxKind"`
	PlatformKey       string            `json:"platformKey"`
	ArchiveType       string            `json:"archiveType"`
	Metadata          map[string]string `json:"metadata"`
}

type PublicAsset struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256,omitempty"`
	Integrity   string `json:"integrity,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
	ArchiveType string `json:"archiveType"`
	Platform    string `json:"platform,omitempty"`
}

type PublicItem struct {
	ID                string                 `json:"id"`
	Type              string                 `json:"type"`
	Name              string                 `json:"name"`
	Version           string                 `json:"version"`
	Description       string                 `json:"description"`
	Readme            string                 `json:"readme,omitempty"`
	Tags              []string               `json:"tags"`
	MinDesktopVersion string                 `json:"minDesktopVersion,omitempty"`
	SandboxKind       string                 `json:"sandboxKind,omitempty"`
	NpmPackage        string                 `json:"npmPackage"`
	Assets            map[string]PublicAsset `json:"assets"`
	PublishedAt       time.Time              `json:"publishedAt"`
	UpdatedAt         time.Time              `json:"updatedAt"`
}

type CatalogResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Items         []PublicItem `json:"items"`
}

type SkillAPIItem struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	Description   string   `json:"description"`
	LatestVersion string   `json:"latest_version"`
	Tags          []string `json:"tags"`
}

type SkillAPIResponse struct {
	Success    bool           `json:"success"`
	Data       []SkillAPIItem `json:"data"`
	Pagination Pagination     `json:"pagination"`
}

type Pagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

type DesktopCatalogItem struct {
	ID                string                 `json:"id"`
	Type              string                 `json:"type"`
	Name              string                 `json:"name"`
	Version           string                 `json:"version"`
	Description       string                 `json:"description"`
	Tags              []string               `json:"tags"`
	MinDesktopVersion string                 `json:"minDesktopVersion,omitempty"`
	SandboxKind       string                 `json:"sandboxKind,omitempty"`
	Assets            map[string]PublicAsset `json:"assets"`
}

type DesktopCatalogResponse struct {
	SchemaVersion int                  `json:"schemaVersion"`
	GeneratedAt   time.Time            `json:"generatedAt"`
	Items         []DesktopCatalogItem `json:"items"`
}

type storedItem struct {
	Type              ItemType
	ID                string
	Name              string
	Description       string
	Readme            string
	LatestVersion     string
	MinDesktopVersion string
	SandboxKind       string
	Published         bool
	PublishedAt       time.Time
	UpdatedAt         time.Time
	Tags              []string
	Assets            map[string]PublicAsset
}
