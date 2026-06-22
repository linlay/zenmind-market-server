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

## Storage and deployment

The server is the source of truth for market data. The website should read `/api/v1/catalog`; uploaded artifacts should never be baked into the frontend image or copied into the nginx static directory.

CLI tools and skills require an ADP `schema: "0.1"` manifest at publish time. The server validates the latest hook protocol (`exec`, `sh`, `pwsh`, `cmd` runners), rejects legacy hook syntax, and binds uploaded artifact URLs plus SHA-256 values into the stored `adp.yaml`.

In container deployments, keep SQLite and artifacts in the same persistent backend volume:

```yaml
environment:
  MARKET_DB_PATH: /data/market.db
  MARKET_ARTIFACT_ROOT: /data/artifacts
  MARKET_PUBLIC_BASE_URL: https://market.example.com
volumes:
  - /srv/zenmind-market/data:/data
```

Published artifact files are stored under `/data/artifacts/{type}/{id}/{version}/...`, while SQLite stores metadata, checksums, and generated artifact URLs. Configure `MARKET_PUBLIC_BASE_URL` to the final public market domain before publishing, because artifact URLs are written into SQLite when an artifact is uploaded.

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
