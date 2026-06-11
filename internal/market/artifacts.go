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
	PlatformKey string
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
	req.Version = strings.TrimSpace(req.Version)
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
	req.ArchiveType = normalizeArchiveType(req.ArchiveType, req.Type)
	if req.SandboxKind == "" && req.Type == TypeSandbox {
		req.SandboxKind = "environment-template"
	}
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
		PlatformKey: req.PlatformKey,
		ArchiveType: req.ArchiveType,
		Path:        target,
		URL:         strings.TrimRight(publicBaseURL, "/") + "/artifacts/" + filepath.ToSlash(relative),
		SHA256:      shaHex,
		Integrity:   integrity,
		SizeBytes:   size,
	}, nil
}

func validateArtifactByType(filePath string, req PublishRequest) error {
	switch req.Type {
	case TypeSkill:
		if req.ArchiveType == "md" {
			return nil
		}
		if found, err := archiveContains(filePath, req.ArchiveType, "SKILL.md"); err != nil {
			return err
		} else if !found {
			return errors.New("skill artifact must contain SKILL.md")
		}
	case TypePlugin:
		content, err := archiveFileContent(filePath, req.ArchiveType, "manifest.json")
		if err != nil {
			return err
		}
		if len(content) == 0 {
			return errors.New("plugin artifact must contain manifest.json")
		}
		var manifest struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(content, &manifest); err != nil {
			return fmt.Errorf("plugin manifest is invalid JSON: %w", err)
		}
		if manifest.Kind != "plugin" {
			return errors.New("plugin manifest must declare kind=plugin")
		}
		if manifest.ID != "" && sanitizeSlug(manifest.ID) != req.ID {
			return fmt.Errorf("plugin manifest id mismatch: expected %s, got %s", req.ID, manifest.ID)
		}
	case TypeSandbox:
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
	}
	return nil
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
	if archiveType == "tar.gz" || archiveType == "sandbox-template" || strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".tgz") {
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
	value = strings.TrimPrefix(value, "@zenmind-sandbox/")
	value = strings.ReplaceAll(value, " ", "-")
	value = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func sanitizePlatform(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, " ", "-")
	value = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func normalizeArchiveType(value string, itemType ItemType) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "tar.gz", "tgz", "zip", "skill", "md", "sandbox-template":
		if value == "tgz" {
			return "tar.gz"
		}
		return value
	}
	if itemType == TypeSandbox {
		return "sandbox-template"
	}
	if itemType == TypeSkill {
		return "tar.gz"
	}
	return "tar.gz"
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
	default:
		return ".tar.gz"
	}
}
