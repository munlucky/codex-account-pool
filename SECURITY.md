# Security Policy

## Trust boundary

GPT Codex Router is a **local-only, single-user ChatGPT subscription gateway**. It handles Codex OAuth credentials, so its security model depends on keeping the HTTP listener on the same machine as the user.

```text
Codex Desktop -> host loopback -> GPT Codex Router -> chatgpt.com
                                      |
                                      `-> selected isolated Codex auth state
```

The project is not designed to be exposed to a LAN, the public internet, or multiple OS users.

## Credential ownership and storage

The host setup script invokes the official Codex login flow with a dedicated `CODEX_HOME` for each profile and `cli_auth_credentials_store="file"`. Profile state is kept under the platform state directory:

```text
Windows: %APPDATA%\GPTCodexRouter\profiles\codex\<profile>
macOS:   ~/Library/Application Support/GPTCodexRouter/profiles/codex/<profile>
```

The router reads the selected profile's Codex `auth.json` only to authenticate ChatGPT backend requests and maintain that same profile's OAuth refresh lifecycle.

The project must never:

- copy one profile's credentials into another profile;
- print bearer, refresh, or ID tokens;
- print ChatGPT account IDs, cookies, prompt bodies, or upstream error bodies;
- trust inbound `Authorization`, `ChatGPT-Account-ID`, `Proxy-Authorization`, or `Cookie` values as upstream authentication;
- switch profiles because of a generic 429, 401/403, transport error, or OAuth refresh failure;
- expose credential-bearing proxy traffic beyond the local-machine boundary.

## Docker boundary

The supplied Compose configuration deliberately has different container and host listen scopes:

```text
container: 0.0.0.0:8317
host:      127.0.0.1:8317
```

`GPT_CODEX_ROUTER_CONTAINER=1` allows an unspecified address only inside container mode. It is not permission to expose the service externally. Do not change the Compose mapping to `8317:8317` or `0.0.0.0:8317:8317`.

Additional container controls:

- the platform state directory is bind-mounted at `/data`; credential files are never copied into the image or build context;
- the runtime process runs as a non-root user;
- Linux capabilities are dropped;
- `no-new-privileges` is enabled;
- `/healthz` is unauthenticated but contains no credential data and never contacts ChatGPT.

Only one router process should actively use a profile directory at a time. The setup script is intended to perform interactive login before starting/restarting the container.

## Upstream request sanitization

Only `/backend-api` and `/backend-api/*` are accepted for proxying. Before an upstream request is sent, the router removes inbound authentication-bearing headers and attaches only the credentials selected from local profile state.

The local `GET /healthz` route is handled without reading profile credentials or contacting ChatGPT.

## OAuth refresh lifecycle

The broker refreshes an access token only when it is near expiration. Refresh behavior is fail-closed:

- non-success response bodies are discarded rather than echoed;
- a refresh response without an access token is rejected;
- a refreshed ID token that resolves to a different ChatGPT account is rejected;
- refresh never causes profile failover;
- successful updates are written atomically back to the same profile auth file.

If a refresh grant is no longer usable, explicitly sign in to that profile again by rerunning `setup.ps1` on Windows or `setup.sh` on macOS (or using the native CLI workflow during development).

## Profile selection and usage-limit failover

An explicit profile selection becomes the preferred account for new requests. Automatic failover is intentionally narrow: only a ChatGPT HTTP 429 whose structured error identifies a subscription usage limit (`usage_limit_reached` or `usage_limit_exceeded`) can suppress the current profile and select another registered profile.

A request is replayed only after a successful profile change. A per-request attempted-profile guard prevents cycling when every registered account is exhausted. Existing streams remain bound to the credentials selected when that request began.

This is profile routing, not quota bypassing or quota aggregation.

## Codex Desktop transport routing

Codex Desktop is started normally. GPT Codex Router does not patch, inject into, resume, or modify the Desktop application.

The integration uses these root Codex configuration values:

```toml
chatgpt_base_url = "http://127.0.0.1:8317/backend-api"
openai_base_url = "http://127.0.0.1:8317/backend-api/codex"
```

The router returns HTTP 426 only for the `/backend-api/codex/responses` WebSocket upgrade so Codex can fall back to HTTP/SSE, where a structured usage-limit response can be classified before a replay. Other backend WebSocket routes are proxied normally.

## Child-process environment

Native CLI login/status/run operations use an isolated `CODEX_HOME` and remove known API-key/token override variables before launching Codex. both platform setup scripts apply the same isolation when invoking the official login command.

## Testing and repository hygiene

Automated tests must use synthetic credentials and local test servers. Never commit or attach real:

- `auth.json` files;
- access/refresh/ID tokens;
- cookies;
- ChatGPT account IDs;
- environment files containing credentials;
- private proxy logs containing request data.

Runtime profile directories, common credential exports, environment files, logs, and build output are ignored by Git. Always inspect `git status` and the staged diff before publishing.

## Reporting a vulnerability

Open a GitHub security advisory for issues involving credential disclosure, cross-profile credential writes, listener exposure, inbound-auth trust, OAuth account identity changes, or unintended profile switching. Do not include live credentials in reports.
