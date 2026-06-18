#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

API_BASE="${MARKET_API_BASE:-http://localhost:8088/api/v1}"
ADMIN_TOKEN="${MARKET_ADMIN_TOKEN:-dev-secret}"
DB_PATH="${MARKET_DB_PATH:-data/market.db}"
if [[ "${DB_PATH}" != /* ]]; then
  DB_PATH="${REPO_ROOT}/${DB_PATH}"
fi
ARTIFACT_ROOT="${MARKET_ARTIFACT_ROOT:-data/artifacts}"
if [[ "${ARTIFACT_ROOT}" != /* ]]; then
  ARTIFACT_ROOT="${REPO_ROOT}/${ARTIFACT_ROOT}"
fi
PUBLIC_BASE="${MARKET_PUBLIC_BASE_URL:-${API_BASE%/api/v1}}"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

require_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 1
  fi
}

require_tool curl
require_tool zip
require_tool sqlite3

write_file() {
  local path="$1"
  mkdir -p "$(dirname "${path}")"
  cat >"${path}"
}

zip_dir() {
  local source_dir="$1"
  local output="$2"
  (cd "${source_dir}" && zip -qr "${output}" .)
}

publish_multipart() {
  local route="$1"
  local metadata="$2"
  local artifact="$3"
  local adp="${4:-}"
  local response="${TMP_DIR}/response-$(basename "${metadata}" .json).json"
  local status
  local args=(
    -sS
    -X POST
    "${API_BASE}/admin/${route}/publish"
    -H "Authorization: Bearer ${ADMIN_TOKEN}"
    -F "metadata=<${metadata};type=application/json"
    -F "artifact=@${artifact};filename=artifact.zip;type=application/zip"
  )
  if [[ -n "${adp}" ]]; then
    args+=(-F "adp=@${adp};filename=adp.yaml;type=application/x-yaml")
  fi
  status="$(curl "${args[@]}" -o "${response}" -w "%{http_code}")"
  if [[ "${status}" != 2* ]]; then
    echo "publish failed for ${route}/$(basename "${metadata}" .json): HTTP ${status}" >&2
    cat "${response}" >&2
    echo >&2
    exit 1
  fi
  echo "published ${route}: $(basename "${metadata}" .json)"
}

publish_json() {
  local route="$1"
  local metadata="$2"
  local response="${TMP_DIR}/response-$(basename "${metadata}" .json).json"
  local status
  status="$(curl -sS \
    -X POST \
    "${API_BASE}/admin/${route}/publish" \
    -H "Authorization: Bearer ${ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    --data-binary @"${metadata}" \
    -o "${response}" \
    -w "%{http_code}")"
  if [[ "${status}" != 2* ]]; then
    echo "publish failed for ${route}/$(basename "${metadata}" .json): HTTP ${status}" >&2
    cat "${response}" >&2
    echo >&2
    exit 1
  fi
  echo "published ${route}: $(basename "${metadata}" .json)"
}

blank_adp_for_legacy_item() {
  local item_type="$1"
  local item_id="$2"
  local version="$3"
  sqlite3 "${DB_PATH}" \
    "UPDATE items SET adp_yaml = '' WHERE type = '${item_type}' AND id = '${item_id}';
     UPDATE versions SET adp_yaml = '' WHERE item_type = '${item_type}' AND item_id = '${item_id}' AND version = '${version}';"
  echo "marked legacy no-ADP item: ${item_type}/${item_id}@${version}"
}

reset_demo_data() {
  if [[ "${RESET_DEMO_DATA:-1}" != "1" ]]; then
    return
  fi
  sqlite3 "${DB_PATH}" <<'SQL'
DELETE FROM download_events
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM favorite_items
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM tags
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM version_platforms
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM artifacts
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM versions
WHERE (item_type = 'cli-tool' AND item_id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (item_type = 'skill' AND item_id IN ('research-skill', 'legacy-skill'))
   OR (item_type = 'plugin' AND item_id = 'calendar-plugin')
   OR (item_type = 'agent' AND item_id = 'brief-agent')
   OR (item_type = 'sandbox-image' AND item_id = 'node-sandbox')
   OR (item_type = 'pet' AND item_id = 'mira-pet')
   OR (item_type = 'website-app' AND item_id IN ('workbench-web', 'docs-portal'));
DELETE FROM items
WHERE (type = 'cli-tool' AND id IN ('zmctl', 'provider-plan-demo', 'system-deps-demo', 'legacy-codegen'))
   OR (type = 'skill' AND id IN ('research-skill', 'legacy-skill'))
   OR (type = 'plugin' AND id = 'calendar-plugin')
   OR (type = 'agent' AND id = 'brief-agent')
   OR (type = 'sandbox-image' AND id = 'node-sandbox')
   OR (type = 'pet' AND id = 'mira-pet')
   OR (type = 'website-app' AND id IN ('workbench-web', 'docs-portal'));
SQL
  rm -rf \
    "${ARTIFACT_ROOT}/cli-tool/zmctl" \
    "${ARTIFACT_ROOT}/cli-tool/provider-plan-demo" \
    "${ARTIFACT_ROOT}/cli-tool/system-deps-demo" \
    "${ARTIFACT_ROOT}/cli-tool/legacy-codegen" \
    "${ARTIFACT_ROOT}/skill/research-skill" \
    "${ARTIFACT_ROOT}/skill/legacy-skill" \
    "${ARTIFACT_ROOT}/plugin/calendar-plugin" \
    "${ARTIFACT_ROOT}/agent/brief-agent" \
    "${ARTIFACT_ROOT}/sandbox-image/node-sandbox" \
    "${ARTIFACT_ROOT}/pet/mira-pet" \
    "${ARTIFACT_ROOT}/website-app/workbench-web" \
    "${ARTIFACT_ROOT}/website-app/docs-portal"
  echo "reset demo data"
}

record_download() {
  local route="$1"
  local item_id="$2"
  local query="${3:-}"
  curl -fsS -L -o /dev/null "${API_BASE}/${route}/${item_id}/download${query}"
}

mkdir -p "${TMP_DIR}/metadata" "${TMP_DIR}/adp" "${TMP_DIR}/artifacts"
reset_demo_data

write_file "${TMP_DIR}/metadata/zmctl-darwin-arm64.json" <<'JSON'
{
  "id": "zmctl",
  "name": "ZenMind CLI",
  "version": "1.0.0",
  "description": "ADP-enabled CLI with dependency, env, config, hook, and expose examples.",
  "readme": "This item verifies version-level ADP aggregation across macOS arm64 and Linux x64 artifacts.",
  "tags": ["adp", "cli", "multi-platform"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "darwin-arm64",
    "os": "darwin",
    "arch": "arm64",
    "description": "macOS Apple Silicon build"
  }
}
JSON

write_file "${TMP_DIR}/metadata/zmctl-linux-amd64.json" <<'JSON'
{
  "id": "zmctl",
  "name": "ZenMind CLI",
  "version": "1.0.0",
  "description": "ADP-enabled CLI with dependency, env, config, hook, and expose examples.",
  "readme": "This item verifies version-level ADP aggregation across macOS arm64 and Linux x64 artifacts.",
  "tags": ["adp", "cli", "multi-platform"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "linux-amd64",
    "os": "linux",
    "arch": "amd64",
    "description": "Linux x64 build"
  }
}
JSON

write_file "${TMP_DIR}/zmctl-darwin/bin/zmctl" <<'SH'
#!/bin/sh
echo "zmctl demo darwin-arm64 1.0.0"
SH
chmod +x "${TMP_DIR}/zmctl-darwin/bin/zmctl"
zip_dir "${TMP_DIR}/zmctl-darwin" "${TMP_DIR}/artifacts/zmctl-darwin-arm64.zip"

write_file "${TMP_DIR}/zmctl-linux/bin/zmctl" <<'SH'
#!/bin/sh
echo "zmctl demo linux-x64 1.0.0"
SH
chmod +x "${TMP_DIR}/zmctl-linux/bin/zmctl"
zip_dir "${TMP_DIR}/zmctl-linux" "${TMP_DIR}/artifacts/zmctl-linux-amd64.zip"

zmctl_darwin_sha="$(shasum -a 256 "${TMP_DIR}/artifacts/zmctl-darwin-arm64.zip" | awk '{print $1}')"
zmctl_linux_sha="$(shasum -a 256 "${TMP_DIR}/artifacts/zmctl-linux-amd64.zip" | awk '{print $1}')"
write_file "${TMP_DIR}/adp/zmctl.yaml" <<YAML
schema: "0.1"
name: zmctl
defaults:
  providers: [apt, brew, winget]
env:
  ZENMIND_MARKET:
    value: local
    mode: set_if_unset
  PATH:
    value: "\${ADP_BIN}"
    mode: prepend_path
packages:
  - id: zmctl-runtime
    version: "1.0.0"
    platforms: [macos-arm64, linux-x64]
    from:
      macos-arm64: "${PUBLIC_BASE}/api/v1/cli-tools/zmctl/download?version=1.0.0&platform=darwin-arm64"
      linux-x64: "${PUBLIC_BASE}/api/v1/cli-tools/zmctl/download?version=1.0.0&platform=linux-amd64"
    sha256:
      macos-arm64: "${zmctl_darwin_sha}"
      linux-x64: "${zmctl_linux_sha}"
    archive:
      macos-arm64: { format: zip, strip: 0 }
      linux-x64: { format: zip, strip: 0 }
    x-adp-managed-paths:
      - "\${ADP_HOME}/zenmind/runtime/zmctl/1.0.0"
    hooks:
      post:
        - command: "mkdir -p \${ADP_HOME}/zenmind/runtime/zmctl/1.0.0 && printf 'runtime ready\\n' > \${ADP_HOME}/zenmind/runtime/zmctl/1.0.0/status.txt"
          timeout: 30
      verify:
        - command: "test -f \${ADP_PKG_DIR}/.adp_sha256"
          timeout: 30
  - id: zmctl
    version: "1.0.0"
    x-zenmind-artifact: primary
    x-adp-managed-paths:
      - "\${ADP_HOME}/zenmind/cli/zmctl/1.0.0"
    needs: [zmctl-runtime]
    expose:
      zmctl: bin/zmctl
    env:
      ZENMIND_HOME:
        value: "\${ADP_HOME}/zenmind"
        mode: set_if_unset
    configs:
      "\${ADP_HOME}/zenmind/config/zmctl.yaml":
        content: |
          market: local
          binary: \${ADP_BIN}/zmctl
          package: \${ADP_PKG_DIR}
          telemetry: false
        create_parent: true
        on_conflict: backup_and_replace
        mode: "0644"
    hooks:
      pre:
        - command: "echo preparing zmctl install"
          timeout: 30
      post:
        - command: "mkdir -p \${ADP_HOME}/zenmind/cli/zmctl/1.0.0 && printf 'installed from %s\\n' \"\${ADP_PKG_DIR}\" > \${ADP_HOME}/zenmind/cli/zmctl/1.0.0/install.log"
          timeout: 30
      verify:
        - command: "test -x \${ADP_BIN}/zmctl"
          timeout: 30
YAML

publish_multipart "cli-tools" "${TMP_DIR}/metadata/zmctl-darwin-arm64.json" "${TMP_DIR}/artifacts/zmctl-darwin-arm64.zip" "${TMP_DIR}/adp/zmctl.yaml"
publish_multipart "cli-tools" "${TMP_DIR}/metadata/zmctl-linux-amd64.json" "${TMP_DIR}/artifacts/zmctl-linux-amd64.zip" "${TMP_DIR}/adp/zmctl.yaml"

write_file "${TMP_DIR}/adp/provider-plan-demo.yaml" <<'YAML'
schema: "0.1"
name: provider-plan-demo
defaults:
  providers: [brew, apt, winget]
packages:
  - id: provider-plan-demo
    version: "1.0.0"
    x-zenmind-artifact: primary
    brew: git
    apt: git
    winget: Git.Git
    expose:
      provider-plan-demo: bin/provider-plan-demo
    hooks:
      verify:
        - command: "provider-plan-demo --version"
          timeout: 30
YAML

write_file "${TMP_DIR}/metadata/provider-plan-demo.json" <<'JSON'
{
  "id": "provider-plan-demo",
  "name": "Provider Plan Demo",
  "version": "1.0.0",
  "description": "Plan-focused ADP example showing provider mappings plus binary fallback.",
  "readme": "Use `adp plan` on this item to verify brew/apt/winget provider selection and binary fallback. It intentionally models a system provider path, so use zmctl for safe end-to-end install.",
  "tags": ["adp", "provider", "plan-only"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal",
    "description": "Universal provider planning demo"
  }
}
JSON

write_file "${TMP_DIR}/provider-plan-demo/bin/provider-plan-demo" <<'SH'
#!/bin/sh
echo "provider-plan-demo 1.0.0"
SH
chmod +x "${TMP_DIR}/provider-plan-demo/bin/provider-plan-demo"
zip_dir "${TMP_DIR}/provider-plan-demo" "${TMP_DIR}/artifacts/provider-plan-demo.zip"
publish_multipart "cli-tools" "${TMP_DIR}/metadata/provider-plan-demo.json" "${TMP_DIR}/artifacts/provider-plan-demo.zip" "${TMP_DIR}/adp/provider-plan-demo.yaml"

write_file "${TMP_DIR}/adp/system-deps-demo.yaml" <<'YAML'
schema: "0.1"
name: system-deps-demo
requires_privilege: true
defaults:
  providers: [brew, apt, winget]
packages:
  - id: git
    brew: git
    apt: git
    winget: Git.Git
  - id: jq
    brew: jq
    apt: jq
    winget: jqlang.jq
  - id: system-deps-demo
    version: "1.0.0"
    x-zenmind-artifact: primary
    needs: [git, jq]
    expose:
      system-deps-demo: bin/system-deps-demo
    hooks:
      pre:
        - command: "git --version"
          timeout: 60
      post:
        - command: "jq --version"
          timeout: 60
      verify:
        - command: "system-deps-demo --version"
          timeout: 30
YAML

write_file "${TMP_DIR}/metadata/system-deps-demo.json" <<'JSON'
{
  "id": "system-deps-demo",
  "name": "System Dependencies Demo",
  "version": "1.0.0",
  "description": "ADP example that actually installs/checks git and jq through system providers before installing a CLI artifact.",
  "readme": "Use this item to verify provider execution, dependency ordering, artifact binding, expose, and hooks. It may run Homebrew/apt/winget install commands for git and jq.",
  "tags": ["adp", "provider", "system-deps"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal",
    "description": "Universal system dependency demo"
  }
}
JSON

write_file "${TMP_DIR}/system-deps-demo/bin/system-deps-demo" <<'SH'
#!/bin/sh
if [ "${1:-}" = "--version" ]; then
  echo "system-deps-demo 1.0.0"
else
  echo "system-deps-demo ready"
fi
SH
chmod +x "${TMP_DIR}/system-deps-demo/bin/system-deps-demo"
zip_dir "${TMP_DIR}/system-deps-demo" "${TMP_DIR}/artifacts/system-deps-demo.zip"
publish_multipart "cli-tools" "${TMP_DIR}/metadata/system-deps-demo.json" "${TMP_DIR}/artifacts/system-deps-demo.zip" "${TMP_DIR}/adp/system-deps-demo.yaml"

write_file "${TMP_DIR}/adp/research-skill.yaml" <<'YAML'
schema: "0.1"
name: research-skill
packages:
  - id: research-skill
    version: "1.2.0"
    x-zenmind-artifact: primary
    x-adp-managed-paths:
      - "${ADP_HOME}/zenmind/skills/research-skill/1.2.0"
    hooks:
      post:
        - command: "mkdir -p ${ADP_HOME}/zenmind/skills/research-skill/1.2.0 && cp -R ${ADP_PKG_DIR}/. ${ADP_HOME}/zenmind/skills/research-skill/1.2.0"
          timeout: 60
      verify:
        - command: "test -f ${ADP_HOME}/zenmind/skills/research-skill/1.2.0/SKILL.md"
          timeout: 30
YAML

write_file "${TMP_DIR}/metadata/research-skill.json" <<'JSON'
{
  "id": "research-skill",
  "name": "Research Skill Pack",
  "version": "1.2.0",
  "description": "Hook-only skill package that registers itself under ADP_HOME.",
  "readme": "This item verifies hook-only ADP installs, managed paths, and skill artifact validation.",
  "tags": ["skill", "hook-only", "adp"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal",
    "description": "Universal skill archive"
  }
}
JSON

write_file "${TMP_DIR}/research-skill/SKILL.md" <<'MD'
# Research Skill Pack

Demo ZenMind skill for market ADP install verification.
MD
write_file "${TMP_DIR}/research-skill/prompts/research.md" <<'MD'
Summarize market signals and produce an action list.
MD
zip_dir "${TMP_DIR}/research-skill" "${TMP_DIR}/artifacts/research-skill.zip"
publish_multipart "skills" "${TMP_DIR}/metadata/research-skill.json" "${TMP_DIR}/artifacts/research-skill.zip" "${TMP_DIR}/adp/research-skill.yaml"

write_file "${TMP_DIR}/adp/legacy-codegen.yaml" <<'YAML'
schema: "0.1"
name: legacy-codegen
packages:
  - id: legacy-codegen
    version: "0.8.0"
    x-zenmind-artifact: primary
    expose:
      legacy-codegen: bin/legacy-codegen
YAML

write_file "${TMP_DIR}/metadata/legacy-codegen.json" <<'JSON'
{
  "id": "legacy-codegen",
  "name": "Legacy Codegen CLI",
  "version": "0.8.0",
  "description": "Legacy CLI item kept as download-only after ADP data is removed.",
  "readme": "This simulates historical CLI entries that should keep artifact download but hide one-click ADP install.",
  "tags": ["legacy", "cli"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON

write_file "${TMP_DIR}/legacy-codegen/bin/legacy-codegen" <<'SH'
#!/bin/sh
echo "legacy codegen demo 0.8.0"
SH
chmod +x "${TMP_DIR}/legacy-codegen/bin/legacy-codegen"
zip_dir "${TMP_DIR}/legacy-codegen" "${TMP_DIR}/artifacts/legacy-codegen.zip"
publish_multipart "cli-tools" "${TMP_DIR}/metadata/legacy-codegen.json" "${TMP_DIR}/artifacts/legacy-codegen.zip" "${TMP_DIR}/adp/legacy-codegen.yaml"
blank_adp_for_legacy_item "cli-tool" "legacy-codegen" "0.8.0"

write_file "${TMP_DIR}/adp/legacy-skill.yaml" <<'YAML'
schema: "0.1"
name: legacy-skill
packages:
  - id: legacy-skill
    version: "0.5.0"
    x-zenmind-artifact: primary
    x-adp-managed-paths:
      - "${ADP_HOME}/zenmind/skills/legacy-skill/0.5.0"
    hooks:
      post:
        - "echo legacy skill installed"
YAML

write_file "${TMP_DIR}/metadata/legacy-skill.json" <<'JSON'
{
  "id": "legacy-skill",
  "name": "Legacy Skill",
  "version": "0.5.0",
  "description": "Legacy skill item kept as download-only after ADP data is removed.",
  "readme": "This simulates historical skill entries without ADP one-click install.",
  "tags": ["legacy", "skill"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON

write_file "${TMP_DIR}/legacy-skill/SKILL.md" <<'MD'
# Legacy Skill

Download-only demo skill.
MD
zip_dir "${TMP_DIR}/legacy-skill" "${TMP_DIR}/artifacts/legacy-skill.zip"
publish_multipart "skills" "${TMP_DIR}/metadata/legacy-skill.json" "${TMP_DIR}/artifacts/legacy-skill.zip" "${TMP_DIR}/adp/legacy-skill.yaml"
blank_adp_for_legacy_item "skill" "legacy-skill" "0.5.0"

write_file "${TMP_DIR}/metadata/calendar-plugin.json" <<'JSON'
{
  "id": "calendar-plugin",
  "name": "Calendar Plugin",
  "version": "1.0.0",
  "description": "Plugin demo for non-ADP market artifact flows.",
  "readme": "This verifies plugin artifact validation and download UI.",
  "tags": ["plugin", "calendar"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON
write_file "${TMP_DIR}/calendar-plugin/manifest.json" <<'JSON'
{"kind":"plugin","id":"calendar-plugin","version":"1.0.0"}
JSON
write_file "${TMP_DIR}/calendar-plugin/index.js" <<'JS'
export function activate() {}
JS
zip_dir "${TMP_DIR}/calendar-plugin" "${TMP_DIR}/artifacts/calendar-plugin.zip"
publish_multipart "plugins" "${TMP_DIR}/metadata/calendar-plugin.json" "${TMP_DIR}/artifacts/calendar-plugin.zip"

write_file "${TMP_DIR}/metadata/brief-agent.json" <<'JSON'
{
  "id": "brief-agent",
  "name": "Brief Agent",
  "version": "0.3.0",
  "description": "Agent demo with a skill dependency.",
  "readme": "This verifies agent catalog and artifact handling.",
  "tags": ["agent", "brief"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "dependencies": [
    {
      "kind": "skill",
      "phase": "runtime",
      "required": true,
      "id": "research-skill",
      "displayName": "Research Skill Pack"
    }
  ],
  "platform": {
    "key": "universal"
  }
}
JSON
write_file "${TMP_DIR}/brief-agent/agent.yml" <<'YAML'
name: Brief Agent
version: 0.3.0
YAML
zip_dir "${TMP_DIR}/brief-agent" "${TMP_DIR}/artifacts/brief-agent.zip"
publish_multipart "agents" "${TMP_DIR}/metadata/brief-agent.json" "${TMP_DIR}/artifacts/brief-agent.zip"

write_file "${TMP_DIR}/metadata/node-sandbox.json" <<'JSON'
{
  "id": "node-sandbox",
  "name": "Node Sandbox",
  "version": "20.0.0",
  "description": "Sandbox environment template demo.",
  "readme": "This verifies sandbox-image publishing with environment.json.",
  "tags": ["sandbox", "node"],
  "archiveType": "zip",
  "sandboxKind": "environment-template",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON
write_file "${TMP_DIR}/node-sandbox/environment.json" <<'JSON'
{"name":"node-sandbox","image_repository":"node","image_tag":"20-bookworm"}
JSON
zip_dir "${TMP_DIR}/node-sandbox" "${TMP_DIR}/artifacts/node-sandbox.zip"
publish_multipart "sandbox-images" "${TMP_DIR}/metadata/node-sandbox.json" "${TMP_DIR}/artifacts/node-sandbox.zip"

write_file "${TMP_DIR}/metadata/mira-pet.json" <<'JSON'
{
  "id": "mira-pet",
  "name": "Mira Pet",
  "version": "1.0.0",
  "description": "Desktop pet demo package.",
  "readme": "This verifies pet package validation.",
  "tags": ["pet", "desktop"],
  "archiveType": "zip",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON
write_file "${TMP_DIR}/mira-pet/pet.json" <<'JSON'
{"id":"mira-pet","version":"1.0.0"}
JSON
write_file "${TMP_DIR}/mira-pet/pet-idle.png" <<'TXT'
demo-png-placeholder
TXT
zip_dir "${TMP_DIR}/mira-pet" "${TMP_DIR}/artifacts/mira-pet.zip"
publish_multipart "pets" "${TMP_DIR}/metadata/mira-pet.json" "${TMP_DIR}/artifacts/mira-pet.zip"

write_file "${TMP_DIR}/metadata/workbench-web.json" <<'JSON'
{
  "id": "workbench-web",
  "name": "Workbench Web",
  "version": "1.0.0",
  "description": "Local website app demo package.",
  "readme": "This verifies local website-app artifact validation and download UI.",
  "tags": ["webapp", "local"],
  "archiveType": "zip",
  "websiteKind": "local-app",
  "metadata": {
    "author": "ZenMind Labs"
  },
  "platform": {
    "key": "universal"
  }
}
JSON
write_file "${TMP_DIR}/workbench-web/website.json" <<'JSON'
{"id":"workbench-web","version":"1.0.0"}
JSON
write_file "${TMP_DIR}/workbench-web/index.html" <<'HTML'
<!doctype html><html><body><h1>Workbench Web</h1></body></html>
HTML
zip_dir "${TMP_DIR}/workbench-web" "${TMP_DIR}/artifacts/workbench-web.zip"
publish_multipart "webapps" "${TMP_DIR}/metadata/workbench-web.json" "${TMP_DIR}/artifacts/workbench-web.zip"

write_file "${TMP_DIR}/metadata/docs-portal.json" <<'JSON'
{
  "id": "docs-portal",
  "name": "Docs Portal",
  "version": "1.0.0",
  "description": "External website app demo without an artifact.",
  "readme": "This verifies external webapp catalog entries.",
  "tags": ["webapp", "external"],
  "archiveType": "zip",
  "websiteKind": "external",
  "metadata": {
    "author": "ZenMind Labs",
    "url": "https://docs.zenmind.local/demo"
  },
  "platform": {
    "key": "universal"
  }
}
JSON
publish_json "webapps" "${TMP_DIR}/metadata/docs-portal.json"

record_download "cli-tools" "zmctl" "?platform=darwin-arm64"
record_download "skills" "research-skill"
record_download "plugins" "calendar-plugin"

echo
echo "Seed complete."
echo "Catalog: ${API_BASE}/catalog"
echo "ADP manifest: ${API_BASE}/adp/cli-tool/zmctl"
