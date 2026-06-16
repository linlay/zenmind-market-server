package market

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type storedArtifact struct {
	Version     string
	PlatformKey string
	AssetRole   string
	ArchiveType string
	Path        string
	URL         string
	SHA256      string
	Integrity   string
	SizeBytes   int64
}

var safeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func validatePublishRequest(req *PublishRequest) error {
	itemType, err := normalizeItemType(string(req.Type))
	if err != nil {
		return err
	}
	req.Type = itemType
	req.ID = sanitizeSlug(req.ID)
	if req.ID == "" || !safeIDPattern.MatchString(req.ID) {
		return errors.New("id must contain only lowercase letters, numbers, dot, underscore, or dash")
	}
	req.Version = canonicalVersion(req.Version)
	if req.Version == "" {
		return errors.New("version is required")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = req.ID
	}
	req.PlatformKey = sanitizePlatform(req.PlatformKey)
	if req.PlatformKey == "" {
		req.PlatformKey = "universal"
	}
	req.AssetRole = sanitizePlatform(req.AssetRole)
	if req.AssetRole == "" {
		req.AssetRole = AssetRolePrimary
	}
	req.SandboxKind = strings.TrimSpace(req.SandboxKind)
	req.WebsiteKind = strings.TrimSpace(req.WebsiteKind)
	if req.SandboxKind == "" && req.Type == TypeSandboxImage {
		req.SandboxKind = SandboxKindEnvironment
	}
	if req.WebsiteKind == "" && req.Type == TypeWebsiteApp {
		req.WebsiteKind = WebsiteKindExternal
	}
	req.ArchiveType = normalizeArchiveType(req.ArchiveType, req.Type, req.SandboxKind, req.WebsiteKind)
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	if req.Dependencies == nil {
		req.Dependencies = []MarketDependency{}
	}
	if err := normalizeDependencies(req.Type, req.Dependencies); err != nil {
		return err
	}
	if err := normalizePublishPlatform(req); err != nil {
		return err
	}
	if err := validatePublishProtocol(req); err != nil {
		return err
	}
	return nil
}

func normalizePublishPlatform(req *PublishRequest) error {
	spec := req.Platform
	if spec == nil {
		spec = &MarketPlatformSpec{
			Key:               req.PlatformKey,
			Description:       req.Description,
			Readme:            req.Readme,
			MinDesktopVersion: req.MinDesktopVersion,
			Metadata:          req.Metadata,
			Dependencies:      req.Dependencies,
			Install:           req.Install,
			Uninstall:         req.Uninstall,
			Detect:            req.Detect,
		}
	} else {
		if strings.TrimSpace(spec.Key) == "" {
			spec.Key = spec.Platform
		}
		if strings.TrimSpace(spec.Key) == "" {
			spec.Key = req.PlatformKey
		}
	}
	spec.Key = sanitizePlatform(spec.Key)
	if spec.Key == "" {
		spec.Key = "universal"
	}
	spec.Platform = sanitizePlatform(spec.Platform)
	if spec.Platform == "" {
		spec.Platform = spec.Key
	}
	spec.OS = sanitizePlatform(spec.OS)
	spec.Arch = sanitizePlatform(spec.Arch)
	spec.Description = strings.TrimSpace(spec.Description)
	spec.Readme = strings.TrimSpace(spec.Readme)
	spec.MinDesktopVersion = strings.TrimSpace(spec.MinDesktopVersion)
	if spec.Metadata == nil {
		spec.Metadata = map[string]string{}
	}
	if spec.Dependencies == nil {
		spec.Dependencies = []MarketDependency{}
	}
	if err := normalizeDependencies(req.Type, spec.Dependencies); err != nil {
		return err
	}
	req.PlatformKey = spec.Key
	req.Platform = spec
	return nil
}

func saveAndValidateArtifact(root, publicBaseURL string, req PublishRequest, file multipart.File, header *multipart.FileHeader) (*storedArtifact, error) {
	if file == nil || header == nil {
		return nil, nil
	}
	temp, err := os.CreateTemp("", "zenmind-market-upload-*")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	hash256 := sha256.New()
	hash512 := sha512.New()
	size, err := io.Copy(io.MultiWriter(temp, hash256, hash512), file)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}

	shaHex := hex.EncodeToString(hash256.Sum(nil))
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(hash512.Sum(nil))
	if err := validateArtifactByType(tempPath, req); err != nil {
		return nil, err
	}

	extension := artifactExtension(req.ArchiveType, header.Filename)
	relative := filepath.Join(string(req.Type), req.ID, req.Version, fmt.Sprintf("%s-%s%s", req.PlatformKey, shaHex[:12], extension))
	target := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(tempPath, target); err != nil {
		return nil, err
	}
	return &storedArtifact{
		Version:     req.Version,
		PlatformKey: req.PlatformKey,
		AssetRole:   req.AssetRole,
		ArchiveType: req.ArchiveType,
		Path:        target,
		URL:         strings.TrimRight(publicBaseURL, "/") + "/artifacts/" + filepath.ToSlash(relative),
		SHA256:      shaHex,
		Integrity:   integrity,
		SizeBytes:   size,
	}, nil
}

func validateArtifactByType(filePath string, req PublishRequest) error {
	if err := validateArchiveTypeContract(req); err != nil {
		return err
	}
	validator, ok := artifactValidators[req.Type]
	if !ok {
		return fmt.Errorf("unsupported item type %q", req.Type)
	}
	return validator(filePath, req)
}

func validateArchiveTypeContract(req PublishRequest) error {
	if req.Type == TypeSandboxImage {
		switch req.SandboxKind {
		case SandboxKindContainerImage:
			if req.ArchiveType != "tar.gz" {
				return fmt.Errorf("sandbox-image container-image artifact must use tar.gz, got %q", req.ArchiveType)
			}
			return nil
		case SandboxKindEnvironment:
			if req.ArchiveType != "zip" {
				return fmt.Errorf("sandbox-image environment-template artifact must use zip, got %q", req.ArchiveType)
			}
			return nil
		default:
			return fmt.Errorf("unsupported sandboxKind %q", req.SandboxKind)
		}
	}
	if req.ArchiveType != "zip" {
		return fmt.Errorf("%s artifact must use zip, got %q", req.Type, req.ArchiveType)
	}
	return nil
}

var artifactValidators = map[ItemType]func(string, PublishRequest) error{
	TypeSkill:        validateSkillArtifact,
	TypePlugin:       validatePluginArtifact,
	TypeAgent:        validateAgentArtifact,
	TypeSandboxImage: validateSandboxImageArtifact,
	TypePet:          validatePetArtifact,
	TypeCLITool:      validateCLIToolArtifact,
	TypeWebsiteApp:   validateWebsiteAppArtifact,
}

func validateSkillArtifact(filePath string, req PublishRequest) error {
	if req.ArchiveType == "md" {
		return nil
	}
	if found, err := archiveContains(filePath, req.ArchiveType, "SKILL.md"); err != nil {
		return err
	} else if !found {
		return errors.New("skill artifact must contain SKILL.md")
	}
	return nil
}

func validatePluginArtifact(filePath string, req PublishRequest) error {
	content, err := archiveFileContent(filePath, req.ArchiveType, "manifest.json")
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return errors.New("plugin artifact must contain manifest.json")
	}
	var manifest struct {
		ID               string `json:"id"`
		Kind             string `json:"kind"`
		PluginAPIVersion int    `json:"pluginApiVersion"`
		Version          string `json:"version"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("plugin manifest is invalid JSON: %w", err)
	}
	if manifest.Kind != "" && manifest.Kind != "plugin" {
		return errors.New("plugin manifest kind must be plugin")
	}
	if manifest.Kind == "" && manifest.PluginAPIVersion == 0 {
		return errors.New("plugin manifest must declare kind=plugin or pluginApiVersion")
	}
	if manifest.ID == "" || sanitizeSlug(manifest.ID) != req.ID {
		return fmt.Errorf("plugin manifest id mismatch: expected %s, got %s", req.ID, manifest.ID)
	}
	if canonicalVersion(manifest.Version) != "" && canonicalVersion(manifest.Version) != req.Version {
		return fmt.Errorf("plugin manifest version mismatch: expected %s, got %s", req.Version, manifest.Version)
	}
	return nil
}

func validateAgentArtifact(filePath string, req PublishRequest) error {
	if found, err := archiveContains(filePath, req.ArchiveType, "agent.yml"); err != nil {
		return err
	} else if found {
		return nil
	}
	if found, err := archiveContains(filePath, req.ArchiveType, "agent.yaml"); err != nil {
		return err
	} else if found {
		return nil
	}
	return errors.New("agent artifact must contain agent.yml or agent.yaml")
}

func validateSandboxImageArtifact(filePath string, req PublishRequest) error {
	switch req.SandboxKind {
	case SandboxKindEnvironment:
		content, err := archiveFileContent(filePath, req.ArchiveType, "environment.json")
		if err != nil {
			return err
		}
		if len(content) == 0 {
			return errors.New("sandbox template artifact must contain environment.json")
		}
		var environment struct {
			Name            string `json:"name"`
			ImageRepository string `json:"image_repository"`
			ImageTag        string `json:"image_tag"`
		}
		if err := json.Unmarshal(content, &environment); err != nil {
			return fmt.Errorf("sandbox environment.json is invalid JSON: %w", err)
		}
		if sanitizeSlug(environment.Name) == "" {
			return errors.New("sandbox environment.json must declare name")
		}
		if strings.TrimSpace(environment.ImageRepository) == "" || strings.TrimSpace(environment.ImageTag) == "" {
			return errors.New("sandbox environment.json must declare image_repository and image_tag")
		}
		return nil
	case SandboxKindContainerImage:
		return validateSafeArchive(filePath, req.ArchiveType)
	default:
		return fmt.Errorf("unsupported sandboxKind %q", req.SandboxKind)
	}
}

func validatePetArtifact(filePath string, req PublishRequest) error {
	content, err := archiveFileContent(filePath, req.ArchiveType, "pet.json")
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return errors.New("pet artifact must contain pet.json")
	}
	if found, err := archiveContains(filePath, req.ArchiveType, "pet-idle.png"); err != nil {
		return err
	} else if !found {
		return errors.New("pet artifact must contain pet-idle.png")
	}
	var manifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("pet.json is invalid JSON: %w", err)
	}
	if manifest.ID != "" && sanitizeSlug(manifest.ID) != req.ID {
		return fmt.Errorf("pet manifest id mismatch: expected %s, got %s", req.ID, manifest.ID)
	}
	if canonicalVersion(manifest.Version) != "" && canonicalVersion(manifest.Version) != req.Version {
		return fmt.Errorf("pet manifest version mismatch: expected %s, got %s", req.Version, manifest.Version)
	}
	return nil
}

func validateCLIToolArtifact(filePath string, req PublishRequest) error {
	return validateSafeArchive(filePath, req.ArchiveType)
}

func validateWebsiteAppArtifact(filePath string, req PublishRequest) error {
	if req.WebsiteKind == WebsiteKindExternal {
		return validateSafeArchive(filePath, req.ArchiveType)
	}
	if req.WebsiteKind != WebsiteKindLocalApp {
		return fmt.Errorf("unsupported websiteKind %q", req.WebsiteKind)
	}
	content, err := archiveFileContent(filePath, req.ArchiveType, "website.json")
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return errors.New("local website app artifact must contain website.json")
	}
	var manifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("website.json is invalid JSON: %w", err)
	}
	if manifest.ID != "" && sanitizeSlug(manifest.ID) != req.ID {
		return fmt.Errorf("website manifest id mismatch: expected %s, got %s", req.ID, manifest.ID)
	}
	if canonicalVersion(manifest.Version) != "" && canonicalVersion(manifest.Version) != req.Version {
		return fmt.Errorf("website manifest version mismatch: expected %s, got %s", req.Version, manifest.Version)
	}
	return nil
}

func validateSafeArchive(filePath, archiveType string) error {
	_, err := archiveFileContent(filePath, archiveType, "__zenmind_missing_validation_probe__")
	return err
}

func validateArtifactRequirement(req PublishRequest, hasArtifact bool) error {
	required := map[ItemType]bool{
		TypeSkill:        true,
		TypePlugin:       true,
		TypeAgent:        true,
		TypeSandboxImage: true,
		TypePet:          true,
	}
	if req.Type == TypeWebsiteApp && req.WebsiteKind == WebsiteKindLocalApp {
		required[TypeWebsiteApp] = true
	}
	if required[req.Type] && !hasArtifact {
		return fmt.Errorf("%s publish requires an artifact package", req.Type)
	}
	if req.Type == TypeWebsiteApp && req.WebsiteKind == WebsiteKindExternal && strings.TrimSpace(req.Metadata["url"]) == "" && strings.TrimSpace(req.Metadata["entryUrl"]) == "" {
		return errors.New("external website-app publish requires metadata.url or metadata.entryUrl")
	}
	return nil
}

func normalizeDependencies(itemType ItemType, dependencies []MarketDependency) error {
	for index := range dependencies {
		dep := &dependencies[index]
		dep.Kind = strings.TrimSpace(dep.Kind)
		dep.Phase = strings.TrimSpace(dep.Phase)
		if dep.Phase == "" {
			dep.Phase = DependencyPhaseRuntime
		}
		if !validDependencyKind(dep.Kind) {
			return fmt.Errorf("dependency[%d] has unsupported kind %q", index, dep.Kind)
		}
		if !validDependencyPhase(dep.Phase) {
			return fmt.Errorf("dependency[%d] has unsupported phase %q", index, dep.Phase)
		}
		if dep.Required && dep.Phase == DependencyPhaseOptional {
			return fmt.Errorf("dependency[%d] cannot be required with optional phase", index)
		}
		if !dependencyAllowedForType(itemType, dep.Kind) {
			return fmt.Errorf("%s cannot depend on %s", itemType, dep.Kind)
		}
		dep.ID = sanitizeOptionalID(dep.ID)
		dep.ServiceID = sanitizeOptionalID(dep.ServiceID)
		dep.Command = strings.TrimSpace(dep.Command)
		dep.Runtime = strings.TrimSpace(dep.Runtime)
		dep.Capability = strings.TrimSpace(dep.Capability)
		dep.Version = strings.TrimSpace(dep.Version)
		dep.DisplayName = strings.TrimSpace(dep.DisplayName)
		dep.InstallHint = strings.TrimSpace(dep.InstallHint)
	}
	return nil
}

func validDependencyKind(kind string) bool {
	switch kind {
	case DependencyBuiltinService, DependencyPlugin, DependencySkill, DependencySandboxImage, DependencyCLITool, DependencySystemCommand, DependencySystemRuntime, DependencyDesktopCapability:
		return true
	default:
		return false
	}
}

func validDependencyPhase(phase string) bool {
	switch phase {
	case DependencyPhaseInstall, DependencyPhasePostInstall, DependencyPhaseRuntime, DependencyPhaseOptional:
		return true
	default:
		return false
	}
}

func dependencyAllowedForType(itemType ItemType, kind string) bool {
	allowed := allowedDependencyKinds[itemType]
	for _, candidate := range allowed {
		if candidate == kind {
			return true
		}
	}
	return false
}

var allowedDependencyKinds = map[ItemType][]string{
	TypeSkill: {
		DependencyBuiltinService,
		DependencySandboxImage,
		DependencyCLITool,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
	TypePlugin: {
		DependencyBuiltinService,
		DependencyPlugin,
		DependencyCLITool,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
	TypeAgent: {
		DependencyBuiltinService,
		DependencyPlugin,
		DependencySkill,
		DependencySandboxImage,
		DependencyCLITool,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
	TypeSandboxImage: {
		DependencyBuiltinService,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
	TypePet: {
		DependencyBuiltinService,
		DependencyDesktopCapability,
	},
	TypeCLITool: {
		DependencyBuiltinService,
		DependencyCLITool,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
	TypeWebsiteApp: {
		DependencyBuiltinService,
		DependencyPlugin,
		DependencyCLITool,
		DependencySystemCommand,
		DependencySystemRuntime,
		DependencyDesktopCapability,
	},
}

func validatePublishProtocol(req *PublishRequest) error {
	if err := validateProtocolForType(req.Type, "market-level", req.Install, req.Uninstall, req.Detect); err != nil {
		return err
	}
	if req.Platform != nil {
		if err := validateProtocolForType(req.Type, "platform-level", req.Platform.Install, req.Platform.Uninstall, req.Platform.Detect); err != nil {
			return err
		}
	}
	return nil
}

func validateProtocolForType(itemType ItemType, label string, install, uninstall *MarketScriptSpec, detect *MarketDetectSpec) error {
	if itemType != TypeCLITool && (scriptSpecHasContent(install) || scriptSpecHasContent(uninstall)) {
		return fmt.Errorf("%s does not allow %s install or uninstall scripts", itemType, label)
	}
	if itemType == TypeCLITool {
		if err := validateScriptSpec(label+".install", install); err != nil {
			return err
		}
		if err := validateScriptSpec(label+".uninstall", uninstall); err != nil {
			return err
		}
		return nil
	}
	if detect != nil && (len(detect.Commands) > 0 || strings.TrimSpace(detect.VersionCommand) != "") {
		return fmt.Errorf("%s does not support %s detect commands", itemType, label)
	}
	return nil
}

func validateScriptSpec(label string, spec *MarketScriptSpec) error {
	if spec == nil {
		return nil
	}
	spec.Command = strings.TrimSpace(spec.Command)
	spec.ScriptURL = strings.TrimSpace(spec.ScriptURL)
	spec.SHA256 = strings.TrimSpace(spec.SHA256)
	spec.Integrity = strings.TrimSpace(spec.Integrity)
	if spec.ScriptURL != "" && spec.SHA256 == "" && spec.Integrity == "" {
		return fmt.Errorf("%s.scriptUrl requires sha256 or integrity", label)
	}
	return nil
}

func scriptSpecHasContent(spec *MarketScriptSpec) bool {
	return spec != nil && (strings.TrimSpace(spec.Command) != "" || strings.TrimSpace(spec.ScriptURL) != "" || strings.TrimSpace(spec.SHA256) != "" || strings.TrimSpace(spec.Integrity) != "")
}

func archiveContains(filePath, archiveType, basename string) (bool, error) {
	content, err := archiveFileContent(filePath, archiveType, basename)
	return len(content) > 0, err
}

func archiveFileContent(filePath, archiveType, basename string) ([]byte, error) {
	lower := strings.ToLower(filePath)
	if archiveType == "zip" || archiveType == "skill" || strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".skill") {
		reader, err := zip.OpenReader(filePath)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		for _, file := range reader.File {
			if unsafeArchivePath(file.Name) {
				return nil, fmt.Errorf("archive contains unsafe path %q", file.Name)
			}
			if pathBase(file.Name) != basename {
				continue
			}
			opened, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer opened.Close()
			return io.ReadAll(io.LimitReader(opened, 2*1024*1024))
		}
		return nil, nil
	}
	raw, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	var stream io.Reader = raw
	if archiveType == "tar.gz" || archiveType == "agent" || archiveType == "sandbox-template" || archiveType == "pet" || archiveType == "website-app" || strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".tgz") {
		gzipReader, err := gzip.NewReader(raw)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		stream = gzipReader
	}
	tarReader := tar.NewReader(stream)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if unsafeArchivePath(header.Name) {
			return nil, fmt.Errorf("archive contains unsafe path %q", header.Name)
		}
		if header.Typeflag == tar.TypeReg && pathBase(header.Name) == basename {
			return io.ReadAll(io.LimitReader(tarReader, 2*1024*1024))
		}
	}
}

func unsafeArchivePath(value string) bool {
	normalized := strings.ReplaceAll(value, "\\", "/")
	return normalized == "" || strings.HasPrefix(normalized, "/") || strings.Contains(normalized, "../") || normalized == ".." || regexp.MustCompile(`^[A-Za-z]:/`).MatchString(normalized)
}

func pathBase(value string) string {
	normalized := strings.Trim(strings.ReplaceAll(value, "\\", "/"), "/")
	parts := strings.Split(normalized, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func copyFile(from, to string) error {
	input, err := os.Open(from)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(to)
	if err != nil {
		return err
	}
	defer output.Close()
	_, err = io.Copy(output, input)
	return err
}

func sanitizeSlug(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "@zenmind-skill/")
	value = strings.TrimPrefix(value, "@zenmind-plugin/")
	value = strings.TrimPrefix(value, "@zenmind-agent/")
	value = strings.TrimPrefix(value, "@zenmind-sandbox/")
	value = strings.TrimPrefix(value, "@zenmind-pet/")
	value = strings.TrimPrefix(value, "@zenmind-cli/")
	value = strings.TrimPrefix(value, "@zenmind-website-app/")
	value = strings.ReplaceAll(value, " ", "-")
	value = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func sanitizeOptionalID(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return sanitizeSlug(value)
}

func sanitizePlatform(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, " ", "-")
	value = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func normalizeArchiveType(value string, itemType ItemType, sandboxKind, websiteKind string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value != "" {
		switch value {
		case "tgz":
			return "tar.gz"
		case "container-image":
			if itemType == TypeSandboxImage && sandboxKind == SandboxKindContainerImage {
				return "tar.gz"
			}
			return value
		case "tar.gz", "zip", "skill", "md", "agent", "sandbox-template", "pet", "website-app":
			return value
		default:
			return value
		}
	}
	if itemType == TypeSandboxImage && sandboxKind == SandboxKindContainerImage {
		return "tar.gz"
	}
	return "zip"
}

func artifactExtension(archiveType, filename string) string {
	switch archiveType {
	case "zip":
		return ".zip"
	case "skill":
		return ".skill"
	case "md":
		return ".md"
	case "sandbox-template":
		lower := strings.ToLower(filename)
		if strings.HasSuffix(lower, ".zip") {
			return ".zip"
		}
		return ".tar.gz"
	case "container-image":
		return ".tar"
	case "pet":
		return ".tar.gz"
	case "agent":
		return ".tar.gz"
	case "website-app":
		return ".tar.gz"
	default:
		return ".tar.gz"
	}
}
