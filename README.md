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

Public catalog and component list APIs are readable without a token, including `/api/v1/catalog`, `/api/v1/desktop/catalog`, and typed market list/detail/resolve/download routes. Desktop user actions such as favorite/unfavorite accept an official-site SSO JWT with `aud=zenmind-market-server` and `scope` containing `market`.

The market website auth flow is unchanged: it can continue to sit behind the official-site session flow or authentik gateway. Admin APIs keep accepting `Authorization: Bearer $MARKET_ADMIN_TOKEN` and trusted official proxy headers with `X-ZenMind-Market-Proxy-Token: $MARKET_PROXY_TOKEN`; they also accept official-site SSO JWTs with `role=admin` and `scope` containing `market`.

## OIDC login

Market can also act as an OIDC confidential client. Configure `MARKET_OIDC_ISSUER`, client credentials, and a high-entropy `MARKET_OIDC_SESSION_SECRET`; the server discovers provider endpoints and signing keys through OIDC Discovery. Register this exact callback URL with the identity provider:

```text
https://market.example.com/api/v1/auth/oidc/callback
```

Browser login starts at `GET /api/v1/auth/oidc/login`; after a successful callback Market stores only a signed, short-lived session cookie and redirects to `MARKET_OIDC_SUCCESS_REDIRECT`. It uses authorization-code flow with PKCE and validates the ID Token issuer, audience, signature and nonce. `MARKET_OIDC_ADMIN_USER_IDS` is a comma-separated allowlist of `staffno` or `sub` values that receive Market admin access. The default role for another authenticated OIDC user is `creator`.

`MARKET_ENABLE_LOCAL_AUTH` is disabled by default. Enable it only for local development; it exposes an unsigned test-login endpoint and must never be enabled in production.

## Storage and deployment

The server is the source of truth for market data. The website should read `/api/v1/catalog`; uploaded artifacts should never be baked into the frontend image or copied into the nginx static directory.

CLI tools and skills may include an ADP `schema: "0.1"` manifest when they need extra dependency installation. When supplied, the server validates the latest hook protocol (`exec`, `sh`, `pwsh`, `cmd` runners), rejects legacy hook syntax, and binds uploaded artifact URLs plus SHA-256 values into the stored `adp.yaml`.

In local-storage container deployments, keep SQLite and artifacts in the same persistent backend volume:

```yaml
environment:
  MARKET_DB_PATH: /data/market.db
  MARKET_ARTIFACT_ROOT: /data/artifacts
  MARKET_PUBLIC_BASE_URL: https://market.example.com
volumes:
  - /srv/zenmind-market/data:/data
```

Published artifact files are stored under `/data/artifacts/{type}/{id}/{version}/...`, while SQLite stores metadata, checksums, and generated artifact URLs. Configure `MARKET_PUBLIC_BASE_URL` to the final public market domain before publishing, because artifact URLs are written into SQLite when an artifact is uploaded.

For production object storage, set `MARKET_ARTIFACT_STORAGE=s3`. The server uploads validated artifacts to S3 (or an S3-compatible COS/MinIO endpoint), keeps only the object key and checksums in SQLite, and redirects `/artifacts/...` requests to a short-lived signed S3 URL. Keep `MARKET_PUBLIC_BASE_URL` pointed at the market API: the stable API URL is embedded in manifests while the signed S3 URL is generated at download time.

```yaml
environment:
  MARKET_ARTIFACT_STORAGE: s3
  MARKET_S3_BUCKET: zenmind-market-artifacts
  MARKET_S3_REGION: ap-guangzhou
  # COS example: bucket-specific endpoint; MinIO can use its S3 endpoint.
  MARKET_S3_ENDPOINT: https://zenmind-market-artifacts.cos.ap-guangzhou.myqcloud.com
  MARKET_S3_PREFIX: market
  MARKET_S3_ACCESS_KEY_ID: ${MARKET_S3_ACCESS_KEY_ID}
  MARKET_S3_SECRET_ACCESS_KEY: ${MARKET_S3_SECRET_ACCESS_KEY}
  MARKET_S3_PRESIGN_TTL: 5m
```

S3 credentials are required only in S3 mode. Do not commit them; provide them through deployment secrets. Local mode remains the default, so `go test ./...` and `go run ./cmd/market-server` work without any S3 configuration.

## Environment

The server loads `.env` from the current working directory before building its runtime config. Existing shell variables win over values in `.env`.

| Variable | Default | Description |
| --- | --- | --- |
| `MARKET_ADDR` | `:8088` | HTTP listen address. |
| `MARKET_DB_PATH` | `data/market.db` | SQLite database path. |
| `MARKET_ARTIFACT_ROOT` | `data/artifacts` | Local artifact storage directory. |
| `MARKET_ARTIFACT_STORAGE` | `local` | Artifact backend: `local` or `s3`. |
| `MARKET_S3_BUCKET` | empty | Required S3 bucket in `s3` mode. |
| `MARKET_S3_REGION` | `us-east-1` | S3 signing region. |
| `MARKET_S3_ENDPOINT` | empty | Optional bucket-specific endpoint for COS, MinIO, or another S3-compatible service. |
| `MARKET_S3_PREFIX` | empty | Optional prefix prepended to every stored object key. |
| `MARKET_S3_ACCESS_KEY_ID` | empty | Required access key in `s3` mode. |
| `MARKET_S3_SECRET_ACCESS_KEY` | empty | Required secret key in `s3` mode. |
| `MARKET_S3_SESSION_TOKEN` | empty | Optional temporary-credential session token. |
| `MARKET_S3_PRESIGN_TTL` | `5m` | Lifetime of signed S3 download URLs; maximum 7 days. |
| `MARKET_PUBLIC_BASE_URL` | `http://localhost:8088` | Public base URL used when generating artifact URLs. Use `https://market.zenmind.cc` in production. |
| `MARKET_ADMIN_TOKEN` | empty | Bearer token required for admin APIs. |
| `MARKET_PROXY_TOKEN` | empty | Trusted proxy header token. |
| `SSO_JWT_ISSUER` | empty | Expected issuer for official-site SSO JWTs. Leave empty to disable JWT auth. |
| `SSO_JWT_PUBLIC_KEY_FILE` | `configs/jwt-public.pem` | PEM public key file used to verify Desktop SSO JWTs. |
| `SSO_JWT_PUBLIC_KEY_PEM` | empty | PEM public key value fallback; supports escaped `\n`. |
| `SSO_JWT_AUDIENCE` | `zenmind-market-server` | Required JWT audience. |
| `MARKET_MAX_UPLOAD_BYTES` | `536870912` | Maximum upload size in bytes. |

Do not commit key material. Keep the official-site JWT public key in the ignored project-local file `configs/jwt-public.pem`, then mount `./configs` read-only in production.

Export the public key from the official-site JWT private key:

```bash
mkdir -p configs
openssl pkey -in /path/to/official-sso-private.pem -pubout -out configs/jwt-public.pem
```
# zenmind-market-server
# zenmind-market-server
