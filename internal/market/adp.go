package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const adpSchemaVersion = "0.1"

var adpTopLevelFields = map[string]struct{}{
	"schema":             {},
	"name":               {},
	"requires_privilege": {},
	"defaults":           {},
	"env":                {},
	"hooks":              {},
	"packages":           {},
}

var adpPackageFields = map[string]struct{}{
	"id":        {},
	"version":   {},
	"from":      {},
	"sha256":    {},
	"archive":   {},
	"expose":    {},
	"needs":     {},
	"platforms": {},
	"env":       {},
	"configs":   {},
	"hooks":     {},
}

var adpProviderFields = map[string]struct{}{
	"apt":    {},
	"dnf":    {},
	"pacman": {},
	"apk":    {},
	"zypper": {},
	"brew":   {},
	"winget": {},
	"scoop":  {},
	"choco":  {},
}

var adpPlatformKeys = map[string]struct{}{
	"linux-x64":     {},
	"linux-arm64":   {},
	"macos-x64":     {},
	"macos-arm64":   {},
	"windows-x64":   {},
	"windows-arm64": {},
}

var adpHookOS = map[string]struct{}{
	"linux":   {},
	"macos":   {},
	"windows": {},
}

var adpHookArch = map[string]struct{}{
	"x64":   {},
	"arm64": {},
}

type adpDocument map[string]any

func normalizePublishADP(ctx context.Context, store *Store, publicBaseURL string, req *PublishRequest, artifact *storedArtifact) error {
	if req.Type != TypeCLITool && req.Type != TypeSkill {
		req.ADPYAML = ""
		return nil
	}
	if req.Type == TypeSkill && req.Skill != nil && req.Skill.Kind == SkillKindPackage {
		req.ADPYAML = ""
		return nil
	}
	if strings.TrimSpace(req.ADPYAML) == "" {
		return nil
	}
	current, err := parseADPDocument(req.ADPYAML)
	if err != nil {
		return err
	}
	if artifact != nil {
		if err := bindADPArtifact(current, publicBaseURL, *req, *artifact); err != nil {
			return err
		}
	} else if adpHasArtifactBinding(current) {
		return errors.New("adp.yaml declares x-zenmind-artifact but no artifact was uploaded")
	}
	if err := validateADPDocument(current); err != nil {
		return err
	}
	merged := current
	if previousYAML, err := store.GetPublishADPYAML(ctx, req.Type, req.ID, req.Version); err == nil && strings.TrimSpace(previousYAML) != "" {
		previous, err := parseADPDocument(previousYAML)
		if err != nil {
			return fmt.Errorf("stored adp.yaml is invalid: %w", err)
		}
		merged, err = mergeADPDocuments(previous, current)
		if err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := validateADPDocument(merged); err != nil {
		return err
	}
	raw, err := yaml.Marshal(merged)
	if err != nil {
		return err
	}
	req.ADPYAML = strings.TrimSpace(string(raw)) + "\n"
	return nil
}

func parseADPDocument(raw string) (adpDocument, error) {
	var decoded any
	if err := yaml.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("invalid adp.yaml: %w", err)
	}
	normalized, ok := normalizeYAMLValue(decoded).(map[string]any)
	if !ok {
		return nil, errors.New("adp.yaml must be a mapping")
	}
	doc := adpDocument(normalized)
	if len(doc) == 0 {
		return nil, errors.New("adp.yaml is empty")
	}
	return doc, nil
}

func normalizeYAMLValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = normalizeYAMLValue(item)
		}
		return result
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = normalizeYAMLValue(item)
		}
		return result
	default:
		return value
	}
}

func validateADPDocument(doc adpDocument) error {
	for key := range doc {
		if _, ok := adpTopLevelFields[key]; ok {
			continue
		}
		if strings.HasPrefix(key, "x-") {
			continue
		}
		return fmt.Errorf("adp.yaml has unknown top-level field %q", key)
	}
	if stringField(doc, "schema") != adpSchemaVersion {
		return fmt.Errorf("adp.yaml schema must be %q", adpSchemaVersion)
	}
	if strings.TrimSpace(stringField(doc, "name")) == "" {
		return errors.New("adp.yaml name is required")
	}
	if err := validateADPHooks(doc, "hooks"); err != nil {
		return err
	}
	packages, err := adpPackages(doc)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		return errors.New("adp.yaml packages is required")
	}
	ids := map[string]struct{}{}
	for index, pkg := range packages {
		id, err := adpPackageID(pkg)
		if err != nil {
			return fmt.Errorf("adp package[%d]: %w", index, err)
		}
		if _, ok := ids[id]; ok {
			return fmt.Errorf("adp package %q is duplicated", id)
		}
		ids[id] = struct{}{}
		if err := validateADPPackage(pkg); err != nil {
			return fmt.Errorf("adp package %q: %w", id, err)
		}
	}
	for _, pkg := range packages {
		pkgMap, ok := pkg.(map[string]any)
		if !ok {
			continue
		}
		for _, need := range stringSliceField(pkgMap, "needs") {
			if _, ok := ids[need]; !ok {
				id, _ := adpPackageID(pkg)
				return fmt.Errorf("adp package %q needs missing package %q", id, need)
			}
		}
	}
	return nil
}

func validateADPPackage(pkg any) error {
	switch value := pkg.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return errors.New("string package must not be empty")
		}
		return nil
	case map[string]any:
		if isProviderMappingPackage(value) {
			return validateProviderMappingPackage(value)
		}
		for key := range value {
			if _, ok := adpPackageFields[key]; ok {
				continue
			}
			if _, ok := adpProviderFields[strings.ToLower(key)]; ok {
				continue
			}
			if strings.HasPrefix(key, "x-") {
				continue
			}
			return fmt.Errorf("unknown field %q", key)
		}
		if strings.TrimSpace(stringField(value, "id")) == "" {
			return errors.New("object package requires id")
		}
		if err := validateManagedPaths(value); err != nil {
			return err
		}
		if err := validateADPHooks(value, "hooks"); err != nil {
			return err
		}
		if err := validatePlatformMap(value, "from"); err != nil {
			return err
		}
		if err := validatePlatformMap(value, "sha256"); err != nil {
			return err
		}
		if err := validateArchiveMap(value); err != nil {
			return err
		}
		if hasMapField(value, "from") {
			if strings.TrimSpace(stringField(value, "version")) == "" {
				return errors.New("binary package requires version")
			}
			if !hasMapField(value, "sha256") {
				return errors.New("binary package requires sha256")
			}
			if !hasMapField(value, "expose") && !hasPostHook(value) {
				return errors.New("binary package requires expose or post hook")
			}
			from := mapField(value, "from")
			sha := mapField(value, "sha256")
			for key := range from {
				if _, ok := sha[key]; !ok {
					return fmt.Errorf("missing sha256 for platform %s", key)
				}
			}
		}
		return nil
	default:
		return errors.New("package must be a string or mapping")
	}
}

func validateProviderMappingPackage(pkg map[string]any) error {
	for id, value := range pkg {
		if strings.TrimSpace(id) == "" {
			return errors.New("provider mapping package id is required")
		}
		providers, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("provider mapping for %q must be a map", id)
		}
		for provider, packageName := range providers {
			if _, ok := adpProviderFields[strings.ToLower(provider)]; !ok {
				return fmt.Errorf("unknown provider %q", provider)
			}
			if packageName != nil {
				if _, ok := packageName.(string); !ok {
					return fmt.Errorf("provider %q package name must be a string or null", provider)
				}
			}
		}
	}
	return nil
}

func bindADPArtifact(doc adpDocument, publicBaseURL string, req PublishRequest, artifact storedArtifact) error {
	packages, err := adpPackages(doc)
	if err != nil {
		return err
	}
	var bound map[string]any
	for _, rawPkg := range packages {
		pkg, ok := rawPkg.(map[string]any)
		if !ok {
			continue
		}
		if artifactBindingRole(pkg) == artifact.AssetRole {
			if bound != nil {
				return fmt.Errorf("adp.yaml has multiple packages bound to artifact role %q", artifact.AssetRole)
			}
			bound = pkg
		}
	}
	if bound == nil {
		return fmt.Errorf("adp.yaml must declare x-zenmind-artifact: %s on the artifact package", artifact.AssetRole)
	}
	downloadURL := adpDownloadURL(publicBaseURL, req.Type, req.ID, req.Version, artifact.PlatformKey)
	platformKeys := marketPlatformToADPKeys(artifact.PlatformKey)
	if len(platformKeys) == 0 {
		return fmt.Errorf("unsupported artifact platform %q for ADP", artifact.PlatformKey)
	}
	ensureMap(bound, "from")
	ensureMap(bound, "sha256")
	ensureMap(bound, "archive")
	for _, key := range platformKeys {
		mapField(bound, "from")[key] = downloadURL
		mapField(bound, "sha256")[key] = artifact.SHA256
		mapField(bound, "archive")[key] = map[string]any{"format": artifact.ArchiveType, "strip": 0}
	}
	bound["platforms"] = mergeStringList(stringSliceField(bound, "platforms"), platformKeys)
	return nil
}

func adpHasArtifactBinding(doc adpDocument) bool {
	packages, err := adpPackages(doc)
	if err != nil {
		return false
	}
	for _, rawPkg := range packages {
		pkg, ok := rawPkg.(map[string]any)
		if ok && artifactBindingRole(pkg) != "" {
			return true
		}
	}
	return false
}

func artifactBindingRole(pkg map[string]any) string {
	value, ok := pkg["x-zenmind-artifact"]
	if !ok || value == nil {
		return ""
	}
	role, ok := value.(string)
	if !ok {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return strings.TrimSpace(role)
}

func mergeADPDocuments(previous, current adpDocument) (adpDocument, error) {
	previousPackages, err := adpPackages(previous)
	if err != nil {
		return nil, err
	}
	currentPackages, err := adpPackages(current)
	if err != nil {
		return nil, err
	}
	for key, currentValue := range current {
		if key == "packages" {
			continue
		}
		if previousValue, ok := previous[key]; ok && !reflect.DeepEqual(previousValue, currentValue) {
			return nil, fmt.Errorf("adp.yaml field %q conflicts with previously published platform", key)
		}
		if _, ok := previous[key]; !ok {
			previous[key] = currentValue
		}
	}
	index := map[string]int{}
	for i, rawPkg := range previousPackages {
		id, err := adpPackageID(rawPkg)
		if err != nil {
			return nil, err
		}
		index[id] = i
	}
	for _, rawCurrent := range currentPackages {
		id, err := adpPackageID(rawCurrent)
		if err != nil {
			return nil, err
		}
		previousIndex, ok := index[id]
		if !ok {
			previousPackages = append(previousPackages, rawCurrent)
			continue
		}
		mergedPkg, err := mergeADPPackage(previousPackages[previousIndex], rawCurrent)
		if err != nil {
			return nil, fmt.Errorf("adp package %q conflicts with previously published platform: %w", id, err)
		}
		previousPackages[previousIndex] = mergedPkg
	}
	previous["packages"] = previousPackages
	return previous, nil
}

func mergeADPPackage(previous, current any) (any, error) {
	prevMap, prevOK := previous.(map[string]any)
	curMap, curOK := current.(map[string]any)
	if !prevOK || !curOK {
		if reflect.DeepEqual(previous, current) {
			return previous, nil
		}
		return nil, errors.New("non-object package differs")
	}
	platformFields := map[string]struct{}{"from": {}, "sha256": {}, "archive": {}, "platforms": {}}
	prevComparable := cloneMapWithout(prevMap, platformFields)
	curComparable := cloneMapWithout(curMap, platformFields)
	if !reflect.DeepEqual(prevComparable, curComparable) {
		return nil, errors.New("non-platform fields differ")
	}
	for _, key := range []string{"from", "sha256", "archive"} {
		if hasMapField(curMap, key) {
			ensureMap(prevMap, key)
			for platformKey, value := range mapField(curMap, key) {
				if existing, ok := mapField(prevMap, key)[platformKey]; ok && !reflect.DeepEqual(existing, value) {
					return nil, fmt.Errorf("%s.%s differs", key, platformKey)
				}
				mapField(prevMap, key)[platformKey] = value
			}
		}
	}
	prevMap["platforms"] = mergeStringList(stringSliceField(prevMap, "platforms"), stringSliceField(curMap, "platforms"))
	return prevMap, nil
}

func adpPackages(doc adpDocument) ([]any, error) {
	raw, ok := doc["packages"]
	if !ok {
		return nil, errors.New("adp.yaml packages is required")
	}
	packages, ok := raw.([]any)
	if !ok {
		return nil, errors.New("adp.yaml packages must be an array")
	}
	return packages, nil
}

func adpPackageID(pkg any) (string, error) {
	switch value := pkg.(type) {
	case string:
		id, _, _ := strings.Cut(strings.TrimSpace(value), "@")
		if id == "" {
			return "", errors.New("package id is required")
		}
		return id, nil
	case map[string]any:
		if id := strings.TrimSpace(stringField(value, "id")); id != "" {
			return id, nil
		}
		if isProviderMappingPackage(value) {
			for id := range value {
				return id, nil
			}
		}
		return "", errors.New("package id is required")
	default:
		return "", errors.New("package must be string or map")
	}
}

func isProviderMappingPackage(pkg map[string]any) bool {
	if len(pkg) != 1 {
		return false
	}
	for key := range pkg {
		if _, ok := adpPackageFields[key]; ok {
			return false
		}
		if _, ok := adpProviderFields[strings.ToLower(key)]; ok {
			return false
		}
		if strings.HasPrefix(key, "x-") {
			return false
		}
		return true
	}
	return false
}

func validatePlatformMap(pkg map[string]any, field string) error {
	if !hasMapField(pkg, field) {
		return nil
	}
	for key, value := range mapField(pkg, field) {
		if _, ok := adpPlatformKeys[strings.ToLower(key)]; !ok {
			return fmt.Errorf("%s has invalid ADP platform %q", field, key)
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s.%s must be a string", field, key)
		}
	}
	return nil
}

func validateArchiveMap(pkg map[string]any) error {
	if !hasMapField(pkg, "archive") {
		return nil
	}
	for key, value := range mapField(pkg, "archive") {
		if _, ok := adpPlatformKeys[strings.ToLower(key)]; !ok {
			return fmt.Errorf("archive has invalid ADP platform %q", key)
		}
		spec, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("archive.%s must be a map", key)
		}
		format := strings.TrimSpace(stringField(spec, "format"))
		if format != "zip" && format != "tar.gz" && format != "tgz" {
			return fmt.Errorf("archive.%s has unsupported format %q", key, format)
		}
	}
	return nil
}

func validateManagedPaths(pkg map[string]any) error {
	raw, ok := pkg["x-adp-managed-paths"]
	if !ok || raw == nil {
		return nil
	}
	paths, ok := raw.([]any)
	if !ok {
		return errors.New("x-adp-managed-paths must be an array")
	}
	for _, rawPath := range paths {
		managedPath, ok := rawPath.(string)
		if !ok {
			return errors.New("x-adp-managed-paths must contain only strings")
		}
		if !strings.HasPrefix(managedPath, "${ADP_HOME}/") {
			return fmt.Errorf("managed path must start with ${ADP_HOME}/: %s", managedPath)
		}
		clean := path.Clean(strings.TrimPrefix(managedPath, "${ADP_HOME}/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			return fmt.Errorf("managed path escapes ADP_HOME: %s", managedPath)
		}
	}
	return nil
}

func hasPostHook(pkg map[string]any) bool {
	hooks := mapField(pkg, "hooks")
	if len(hooks) == 0 {
		return false
	}
	post, ok := hooks["post"].([]any)
	return ok && len(post) > 0
}

func validateADPHooks(values map[string]any, field string) error {
	raw, ok := values[field]
	if !ok || raw == nil {
		return nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a map", field)
	}
	for phase, rawList := range hooks {
		switch phase {
		case "pre", "post", "verify":
		default:
			return fmt.Errorf("%s has unknown phase %q", field, phase)
		}
		list, ok := rawList.([]any)
		if !ok {
			return fmt.Errorf("%s.%s must be an array", field, phase)
		}
		for index, rawHook := range list {
			if err := validateADPHook(rawHook); err != nil {
				return fmt.Errorf("%s.%s[%d]: %w", field, phase, index, err)
			}
		}
	}
	return nil
}

func validateADPHook(raw any) error {
	hook, ok := raw.(map[string]any)
	if !ok {
		return errors.New("hook must be an object")
	}
	runners := 0
	for key, value := range hook {
		switch key {
		case "exec":
			runners++
			args, ok := value.([]any)
			if !ok {
				return errors.New("exec must be an array")
			}
			if len(args) == 0 {
				return errors.New("exec must not be empty")
			}
			for _, arg := range args {
				if text, ok := arg.(string); !ok || strings.TrimSpace(text) == "" {
					return errors.New("exec must contain only non-empty strings")
				}
			}
		case "sh", "pwsh", "cmd":
			runners++
			if text, ok := value.(string); !ok || strings.TrimSpace(text) == "" {
				return fmt.Errorf("%s must be a non-empty string", key)
			}
		case "allow_failure":
			if _, ok := value.(bool); !ok {
				return errors.New("allow_failure must be a boolean")
			}
		case "timeout":
			if !positiveIntValue(value) {
				return errors.New("timeout must be a positive integer")
			}
		case "env":
			env, ok := value.(map[string]any)
			if !ok {
				return errors.New("env must be a map")
			}
			for envKey, envValue := range env {
				if strings.TrimSpace(envKey) == "" {
					return errors.New("env keys must not be empty")
				}
				if _, ok := envValue.(string); !ok {
					return errors.New("env values must be strings")
				}
			}
		case "cwd":
			if _, ok := value.(string); !ok {
				return errors.New("cwd must be a string")
			}
		case "platforms":
			if err := validateStringEnumList(value, adpPlatformKeys, "platforms"); err != nil {
				return err
			}
		case "os":
			if err := validateStringEnumList(value, adpHookOS, "os"); err != nil {
				return err
			}
		case "arch":
			if err := validateStringEnumList(value, adpHookArch, "arch"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown hook field %q", key)
		}
	}
	if runners != 1 {
		return errors.New("hook must declare exactly one runner: exec, sh, pwsh, or cmd")
	}
	return nil
}

func positiveIntValue(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed > 0
	case int64:
		return typed > 0
	case uint64:
		return typed > 0
	case float64:
		return typed > 0 && typed == float64(int64(typed))
	default:
		return false
	}
}

func validateStringEnumList(raw any, allowed map[string]struct{}, field string) error {
	list, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", field)
	}
	for _, item := range list {
		value, ok := item.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must contain only non-empty strings", field)
		}
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(value))]; !ok {
			return fmt.Errorf("%s has unsupported value %q", field, value)
		}
	}
	return nil
}

func marketPlatformToADPKeys(platform string) []string {
	platform = sanitizePlatform(platform)
	switch platform {
	case "universal":
		return []string{"linux-x64", "linux-arm64", "macos-x64", "macos-arm64", "windows-x64", "windows-arm64"}
	case "darwin-amd64", "macos-amd64", "darwin-x64", "macos-x64":
		return []string{"macos-x64"}
	case "darwin-arm64", "macos-arm64":
		return []string{"macos-arm64"}
	case "linux-amd64", "linux-x64":
		return []string{"linux-x64"}
	case "linux-arm64":
		return []string{"linux-arm64"}
	case "windows-amd64", "windows-x64":
		return []string{"windows-x64"}
	case "windows-arm64":
		return []string{"windows-arm64"}
	default:
		if _, ok := adpPlatformKeys[platform]; ok {
			return []string{platform}
		}
		return nil
	}
}

func adpDownloadURL(publicBaseURL string, itemType ItemType, id, version, platform string) string {
	route := primaryRoutePath(itemType)
	url := strings.TrimRight(publicBaseURL, "/") + "/api/v1/" + route + "/" + id + "/download?version=" + version
	if platform != "" {
		url += "&platform=" + platform
	}
	return url
}

func primaryRoutePath(itemType ItemType) string {
	switch itemType {
	case TypeCLITool:
		return "cli-tools"
	case TypeSkill:
		return "skills"
	case TypePlugin:
		return "plugins"
	case TypeAgent:
		return "agents"
	case TypeSandboxImage:
		return "sandbox-images"
	case TypePet:
		return "pets"
	case TypeWebsiteApp:
		return "webapps"
	default:
		return string(itemType)
	}
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func hasMapField(values map[string]any, key string) bool {
	_, ok := values[key].(map[string]any)
	return ok
}

func mapField(values map[string]any, key string) map[string]any {
	result, _ := values[key].(map[string]any)
	return result
}

func ensureMap(values map[string]any, key string) {
	if _, ok := values[key].(map[string]any); !ok {
		values[key] = map[string]any{}
	}
}

func stringSliceField(values map[string]any, key string) []string {
	switch raw := values[key].(type) {
	case []string:
		result := make([]string, 0, len(raw))
		for _, item := range raw {
			if strings.TrimSpace(item) != "" {
				result = append(result, strings.TrimSpace(item))
			}
		}
		return result
	case []any:
		result := make([]string, 0, len(raw))
		for _, item := range raw {
			value, ok := item.(string)
			if ok && strings.TrimSpace(value) != "" {
				result = append(result, strings.TrimSpace(value))
			}
		}
		return result
	default:
		return nil
	}
}

func mergeStringList(left, right []string) []string {
	seen := map[string]struct{}{}
	for _, value := range left {
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range right {
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneMapWithout(values map[string]any, without map[string]struct{}) map[string]any {
	clone := make(map[string]any, len(values))
	for key, value := range values {
		if _, skip := without[key]; skip {
			continue
		}
		clone[key] = value
	}
	return clone
}
