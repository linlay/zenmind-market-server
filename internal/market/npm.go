package market

import (
	"fmt"
	"strings"
	"time"
)

func npmPackageName(itemType ItemType, id string) string {
	switch itemType {
	case TypeSkill:
		return "@zenmind-skill/" + id
	case TypePlugin:
		return "@zenmind-plugin/" + id
	case TypeSandbox:
		return "@zenmind-sandbox/" + id
	default:
		return "@zenmind/" + id
	}
}

func parseNpmPackageName(name string) (ItemType, string, error) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unsupported package name %q", name)
	}
	id := sanitizeSlug(parts[1])
	switch parts[0] {
	case "@zenmind-skill":
		return TypeSkill, id, nil
	case "@zenmind-plugin":
		return TypePlugin, id, nil
	case "@zenmind-sandbox":
		return TypeSandbox, id, nil
	default:
		return "", "", fmt.Errorf("unsupported package scope %q", parts[0])
	}
}

func npmPackument(item storedItem) map[string]any {
	name := npmPackageName(item.Type, item.ID)
	asset := firstAsset(item.Assets)
	version := item.LatestVersion
	now := item.PublishedAt.Format(time.RFC3339)
	if item.PublishedAt.IsZero() {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"_id":         name,
		"name":        name,
		"description": item.Description,
		"dist-tags": map[string]string{
			"latest": version,
		},
		"time": map[string]string{
			"created":  now,
			"modified": item.UpdatedAt.Format(time.RFC3339),
			version:    now,
		},
		"versions": map[string]any{
			version: map[string]any{
				"_id":         name + "@" + version,
				"name":        name,
				"version":     version,
				"description": item.Description,
				"keywords":    item.Tags,
				"readme":      item.Readme,
				"dist": map[string]any{
					"tarball":   asset.URL,
					"shasum":    asset.SHA256,
					"integrity": asset.Integrity,
				},
				"zenmind": map[string]any{
					"type":        item.Type,
					"id":          item.ID,
					"sandboxKind": item.SandboxKind,
				},
			},
		},
		"readme": item.Readme,
	}
}

func firstAsset(assets map[string]PublicAsset) PublicAsset {
	if asset, ok := assets["universal"]; ok {
		return asset
	}
	for _, asset := range assets {
		return asset
	}
	return PublicAsset{}
}
