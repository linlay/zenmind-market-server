package market

import "time"

type Config struct {
	Addr                string
	DatabasePath        string
	ArtifactRoot        string
	PublicBaseURL       string
	AdminToken          string
	ProxyToken          string
	SSOJWTIssuer        string
	SSOJWTPublicKeyFile string
	SSOJWTPublicKeyPEM  string
	SSOJWTAudience      string
	MaxUploadBytes      int64
}

type ItemType string

const (
	TypeSkill        ItemType = "skill"
	TypePlugin       ItemType = "plugin"
	TypeAgent        ItemType = "agent"
	TypeSandboxImage ItemType = "sandbox-image"
	TypePet          ItemType = "pet"
	TypeCLITool      ItemType = "cli-tool"
	TypeWebsiteApp   ItemType = "website-app"
)

const (
	DependencyBuiltinService    = "builtin-service"
	DependencyPlugin            = "plugin"
	DependencySkill             = "skill"
	DependencySandboxImage      = "sandbox-image"
	DependencyCLITool           = "cli-tool"
	DependencySystemCommand     = "system-command"
	DependencySystemRuntime     = "system-runtime"
	DependencyDesktopCapability = "desktop-capability"
	DependencyPhaseInstall      = "install"
	DependencyPhasePostInstall  = "postInstall"
	DependencyPhaseRuntime      = "runtime"
	DependencyPhaseOptional     = "optional"
	AssetRolePrimary            = "primary"
	SandboxKindEnvironment      = "environment-template"
	SandboxKindContainerImage   = "container-image"
	WebsiteKindExternal         = "external"
	WebsiteKindLocalApp         = "local-app"
)

type PublishRequest struct {
	Type              ItemType            `json:"type"`
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Version           string              `json:"version"`
	Description       string              `json:"description"`
	Readme            string              `json:"readme"`
	Tags              []string            `json:"tags"`
	MinDesktopVersion string              `json:"minDesktopVersion"`
	SandboxKind       string              `json:"sandboxKind"`
	WebsiteKind       string              `json:"websiteKind"`
	PlatformKey       string              `json:"platformKey"`
	AssetRole         string              `json:"assetRole"`
	ArchiveType       string              `json:"archiveType"`
	Metadata          map[string]string   `json:"metadata"`
	Dependencies      []MarketDependency  `json:"dependencies"`
	Install           *MarketScriptSpec   `json:"install"`
	Uninstall         *MarketScriptSpec   `json:"uninstall"`
	Detect            *MarketDetectSpec   `json:"detect"`
	Platform          *MarketPlatformSpec `json:"platform"`
	ADPYAML           string              `json:"adpYaml"`
}

type PublicAsset struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256,omitempty"`
	Integrity   string `json:"integrity,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
	ArchiveType string `json:"archiveType"`
	Platform    string `json:"platform,omitempty"`
	Role        string `json:"role,omitempty"`
}

type MarketPlatformSpec struct {
	Key               string             `json:"key,omitempty"`
	Platform          string             `json:"platform,omitempty"`
	OS                string             `json:"os,omitempty"`
	Arch              string             `json:"arch,omitempty"`
	Description       string             `json:"description,omitempty"`
	Readme            string             `json:"readme,omitempty"`
	MinDesktopVersion string             `json:"minDesktopVersion,omitempty"`
	Metadata          map[string]string  `json:"metadata,omitempty"`
	Dependencies      []MarketDependency `json:"dependencies,omitempty"`
	Install           *MarketScriptSpec  `json:"install,omitempty"`
	Uninstall         *MarketScriptSpec  `json:"uninstall,omitempty"`
	Detect            *MarketDetectSpec  `json:"detect,omitempty"`
}

type PublicPlatform struct {
	Platform          string             `json:"platform"`
	OS                string             `json:"os,omitempty"`
	Arch              string             `json:"arch,omitempty"`
	Description       string             `json:"description,omitempty"`
	Readme            string             `json:"readme,omitempty"`
	MinDesktopVersion string             `json:"minDesktopVersion,omitempty"`
	Metadata          map[string]string  `json:"metadata"`
	Dependencies      []MarketDependency `json:"dependencies"`
	Install           *MarketScriptSpec  `json:"install,omitempty"`
	Uninstall         *MarketScriptSpec  `json:"uninstall,omitempty"`
	Detect            *MarketDetectSpec  `json:"detect,omitempty"`
}

type MarketDependency struct {
	Kind        string `json:"kind"`
	Phase       string `json:"phase"`
	Required    bool   `json:"required"`
	ID          string `json:"id,omitempty"`
	ServiceID   string `json:"serviceId,omitempty"`
	Command     string `json:"command,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Capability  string `json:"capability,omitempty"`
	Version     string `json:"version,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	InstallHint string `json:"installHint,omitempty"`
}

type MarketScriptSpec struct {
	Command   string `json:"command,omitempty"`
	ScriptURL string `json:"scriptUrl,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Integrity string `json:"integrity,omitempty"`
}

type MarketDetectSpec struct {
	Commands       []string `json:"commands,omitempty"`
	VersionCommand string   `json:"versionCommand,omitempty"`
}

type PublicItem struct {
	ID                string                    `json:"id"`
	Type              string                    `json:"type"`
	Name              string                    `json:"name"`
	Version           string                    `json:"version"`
	Description       string                    `json:"description"`
	Readme            string                    `json:"readme,omitempty"`
	Tags              []string                  `json:"tags"`
	Author            string                    `json:"author"`
	MinDesktopVersion string                    `json:"minDesktopVersion,omitempty"`
	SandboxKind       string                    `json:"sandboxKind,omitempty"`
	WebsiteKind       string                    `json:"websiteKind,omitempty"`
	NpmPackage        string                    `json:"npmPackage"`
	Assets            map[string]PublicAsset    `json:"assets"`
	Platforms         map[string]PublicPlatform `json:"platforms"`
	Dependencies      []MarketDependency        `json:"dependencies"`
	Metadata          map[string]string         `json:"metadata,omitempty"`
	Install           *MarketScriptSpec         `json:"install,omitempty"`
	Uninstall         *MarketScriptSpec         `json:"uninstall,omitempty"`
	Detect            *MarketDetectSpec         `json:"detect,omitempty"`
	ADPInstallURL     string                    `json:"adpInstallUrl,omitempty"`
	CreatedAt         time.Time                 `json:"createdAt"`
	PublishedAt       time.Time                 `json:"publishedAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
	DownloadCount     int                       `json:"downloadCount"`
	FavoriteCount     int                       `json:"favoriteCount"`
	Favorited         bool                      `json:"favorited"`
}

type CatalogResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Items         []PublicItem `json:"items"`
}

type MarketInfo struct {
	Type                  string   `json:"type"`
	Route                 string   `json:"route"`
	Name                  string   `json:"name"`
	DesktopManagedInstall bool     `json:"desktopManagedInstall"`
	AllowsMarketScripts   bool     `json:"allowsMarketScripts"`
	ArchiveTypes          []string `json:"archiveTypes"`
	DependencyKinds       []string `json:"dependencyKinds"`
}

type MarketsResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Markets       []MarketInfo `json:"markets"`
}

type MarketItemsResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Market        string       `json:"market"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Items         []PublicItem `json:"items"`
	Pagination    Pagination   `json:"pagination"`
}

type Pagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

type DesktopCatalogItem struct {
	ID                string                    `json:"id"`
	Type              string                    `json:"type"`
	Name              string                    `json:"name"`
	Version           string                    `json:"version"`
	Description       string                    `json:"description"`
	Readme            string                    `json:"readme,omitempty"`
	Tags              []string                  `json:"tags"`
	Author            string                    `json:"author"`
	MinDesktopVersion string                    `json:"minDesktopVersion,omitempty"`
	SandboxKind       string                    `json:"sandboxKind,omitempty"`
	WebsiteKind       string                    `json:"websiteKind,omitempty"`
	Assets            map[string]PublicAsset    `json:"assets"`
	Platforms         map[string]PublicPlatform `json:"platforms"`
	Dependencies      []MarketDependency        `json:"dependencies"`
	Metadata          map[string]string         `json:"metadata,omitempty"`
	Install           *MarketScriptSpec         `json:"install,omitempty"`
	Uninstall         *MarketScriptSpec         `json:"uninstall,omitempty"`
	Detect            *MarketDetectSpec         `json:"detect,omitempty"`
	ADPInstallURL     string                    `json:"adpInstallUrl,omitempty"`
	CreatedAt         time.Time                 `json:"createdAt"`
	PublishedAt       time.Time                 `json:"publishedAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
	DownloadCount     int                       `json:"downloadCount"`
	FavoriteCount     int                       `json:"favoriteCount"`
	Favorited         bool                      `json:"favorited"`
}

type DesktopCatalogResponse struct {
	SchemaVersion int                  `json:"schemaVersion"`
	GeneratedAt   time.Time            `json:"generatedAt"`
	Items         []DesktopCatalogItem `json:"items"`
}

type PublicVersion struct {
	Version      string                    `json:"version"`
	Description  string                    `json:"description"`
	Readme       string                    `json:"readme,omitempty"`
	Dependencies []MarketDependency        `json:"dependencies"`
	Metadata     map[string]string         `json:"metadata,omitempty"`
	Assets       map[string]PublicAsset    `json:"assets"`
	Platforms    map[string]PublicPlatform `json:"platforms"`
	PublishedAt  time.Time                 `json:"publishedAt"`
}

type VersionsResponse struct {
	SchemaVersion int             `json:"schemaVersion"`
	Item          PublicItem      `json:"item"`
	Versions      []PublicVersion `json:"versions"`
}

type ResolveResponse struct {
	SchemaVersion int             `json:"schemaVersion"`
	Item          PublicItem      `json:"item"`
	Version       string          `json:"version"`
	Platform      string          `json:"platform,omitempty"`
	PlatformSpec  *PublicPlatform `json:"platformSpec,omitempty"`
	Asset         *PublicAsset    `json:"asset,omitempty"`
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
	WebsiteKind       string
	Published         bool
	PublishedAt       time.Time
	UpdatedAt         time.Time
	Tags              []string
	Assets            map[string]PublicAsset
	Platforms         map[string]PublicPlatform
	Dependencies      []MarketDependency
	Metadata          map[string]string
	Install           *MarketScriptSpec
	Uninstall         *MarketScriptSpec
	Detect            *MarketDetectSpec
	ADPYAML           string
	DownloadCount     int
	FavoriteCount     int
	Favorited         bool
}
