# ZenMind Market Server

Official-curated marketplace API for ZenMind skills, plugins, agents, sandbox images, pets, CLI tools, and website WebApps.

Canonical market types are `skill`, `plugin`, `agent`, `sandbox-image`, `pet`, `cli-tool`, and `website-app`. Website applications are exposed through the primary `/api/v1/webapps` route, while the legacy `/api/v1/website-apps` route remains available for compatibility.

## Development

```bash
cp .env.example .env
go test ./...
go run ./cmd/market-server
```

Default paths:

- database: `data/market.db`
- artifacts: `data/artifacts`
- listen address: `:8088`

Admin APIs accept either `Authorization: Bearer $MARKET_ADMIN_TOKEN` or trusted official proxy headers with `X-ZenMind-Market-Proxy-Token: $MARKET_PROXY_TOKEN`.

## Environment

The server loads `.env` from the current working directory before building its runtime config. Existing shell variables win over values in `.env`.

| Variable | Default | Description |
| --- | --- | --- |
| `MARKET_ADDR` | `:8088` | HTTP listen address. |
| `MARKET_DB_PATH` | `data/market.db` | SQLite database path. |
| `MARKET_ARTIFACT_ROOT` | `data/artifacts` | Local artifact storage directory. |
| `MARKET_PUBLIC_BASE_URL` | `http://localhost:8088` | Public base URL used when generating artifact URLs. Use `https://market.zenmind.cc` in production. |
| `MARKET_ADMIN_TOKEN` | empty | Bearer token required for admin APIs. |
| `MARKET_PROXY_TOKEN` | empty | Trusted proxy header token. |
| `MARKET_MAX_UPLOAD_BYTES` | `536870912` | Maximum upload size in bytes. |
# zenmind-market-server
# zenmind-market-server
