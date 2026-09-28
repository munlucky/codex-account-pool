# GPT Codex Router

Local, Docker-first subscription/OAuth routing for Codex Desktop, Codex CLI, Google Antigravity, and OpenAI-compatible local LLM clients.

GPT Codex Router lets you sign in to multiple ChatGPT accounts through the **official Codex login flow**, keeps each account in an isolated `CODEX_HOME`, and preserves the existing Codex `/backend-api/*` data plane. The authenticated `/v1/responses`, `/v1/chat/completions`, and `/v1/models` surface can additionally route explicitly prefixed `google-antigravity/<model>` requests through an isolated Google OAuth profile and Cloud Code Assist adapter. Codex quota failover behavior remains separate from Antigravity routing.

> **Unofficial project.** This repository is not affiliated with or endorsed by OpenAI. OpenAI, ChatGPT, GPT, and Codex names are used only to describe interoperability. Use only accounts you are authorized to use and follow the applicable product terms and limits.

## Why this exists

Codex Desktop normally uses one ChatGPT login context. GPT Codex Router adds a small local routing layer without modifying the Desktop app, MSIX package, renderer, or Codex binaries.

```text
Codex Desktop / Codex CLI
   |
   | chatgpt_base_url + openai_base_url
   |
   +------------------------------+
                                  |
Qwen Code / OpenAI-compatible     |
local clients                     |
   |                              |
   | /v1 + local client-key       |
   v                              v
            127.0.0.1:8317
                   |
                   v
          GPT Codex Router
          |- local /v1 adapter
          |- OAuth/profile broker
          |- usage-limit failover
          |- account-1 -> isolated CODEX_HOME/auth.json
          |- account-2 -> isolated CODEX_HOME/auth.json
          `- account-N -> isolated CODEX_HOME/auth.json
                   |
                   v
          chatgpt.com/backend-api
```

## Quick start: Windows or macOS + Docker Desktop

### Requirements

- Windows 10/11 or macOS 12+
- Docker Desktop with Docker Compose v2
- Official `codex` CLI available in `PATH`
- One or more ChatGPT accounts that can sign in through Codex

No local Go installation is required for the normal Docker setup.

Install Codex on Windows if needed:

```powershell
irm https://chatgpt.com/codex/install.ps1 | iex
```

Install Codex on macOS if needed:

```bash
curl -fsSL https://chatgpt.com/codex/install.sh | sh
```

Homebrew is also supported upstream with `brew install --cask codex`. See the [upstream Codex repository](https://github.com/openai/codex) for current installation methods.

### 1. Clone and run setup

Windows:

```powershell
git clone https://github.com/munlucky/codex-account-pool.git gpt-codex-router
cd gpt-codex-router
.\setup.cmd
```

macOS:

```bash
git clone https://github.com/munlucky/codex-account-pool.git gpt-codex-router
cd gpt-codex-router
./setup.sh
```

Both setup scripts assign `account-1`, `account-2`, and so on automatically. You only repeat the official ChatGPT/Codex sign-in for each account you want to add:

```text
account-1 -> official `codex login` -> browser ChatGPT sign-in
          -> sign in to another account? y
account-2 -> official `codex login` -> browser ChatGPT sign-in
          -> sign in to another account? n
          -> Docker build + start + health check
```

When the final login is complete, setup automatically:

1. writes the local profile registry;
2. creates or reuses the router-local `/v1` `client-key`;
3. records the installed Codex client version for `/v1/models`;
4. writes the host-state path to the ignored local `.env` used by Compose;
5. builds and starts the Docker container;
6. checks `http://127.0.0.1:8317/healthz`;
7. backs up and configures `~/.codex/config.toml` (or `%USERPROFILE%\.codex\config.toml` on Windows);
8. creates separate administrator/host-worker credentials and starts the host login worker;
9. exposes the local account-management UI at `http://127.0.0.1:8317/admin`.

Then **fully restart Codex Desktop**. Future Codex account checks, additions, and re-logins can be started from the management UI without stopping the Docker service.

For the **initial bootstrap only**, you can pass custom profile names while completing each browser login interactively:

Windows:

```powershell
.\setup.ps1 -Profiles personal,work,backup
```

macOS:

```bash
./setup.sh --profiles personal,work,backup
```

The setup scripts intentionally retain the legacy bootstrap behavior of stopping the router before those interactive logins. After the first installation, do **not** rerun setup just to add or re-authenticate an account. Use `http://127.0.0.1:8317/admin` instead; the host worker performs that login without stopping the Docker service or changing the currently selected account.

If you downloaded a ZIP instead of cloning, extract it and run `setup.cmd` on Windows or `./setup.sh` on macOS from that folder. If the ZIP loses the executable bit on macOS, run `chmod +x setup.sh` once.

### 2. Verify

Windows:

```powershell
docker compose ps
Invoke-WebRequest http://127.0.0.1:8317/healthz
docker compose logs --tail=100 gpt-codex-router
```

macOS:

```bash
docker compose ps
curl -fsS http://127.0.0.1:8317/healthz
docker compose logs --tail=100 gpt-codex-router
```

Expected health body:

```text
ok
```

A normal request emits structured JSONL lifecycle events. Values below are abbreviated examples; real request/profile references are process-local opaque values:

```json
{"schema_version":1,"event_type":"request_start","request_id":"r_...","method":"POST","route_template":"/backend-api/codex/responses","transport":"http","peer_class":"container-network"}
{"schema_version":1,"event_type":"upstream_attempt","request_id":"r_...","profile_ref":"p_...","status_code":200,"status_origin":"upstream","attempt":1}
{"schema_version":1,"event_type":"request_end","request_id":"r_...","profile_ref":"p_...","status_code":200,"outcome":"body_eof","semantic_outcome":"completed"}
```

OAuth/access/refresh/ID token values, raw ChatGPT account IDs, prompt/response bodies, query strings, complete headers, and provider error bodies are not logged. Numeric token-usage counters supplied by ChatGPT (for example `input_tokens` or `cached_input_tokens`) may be recorded as observability metadata.

## What setup changes

### Router state

Host state lives outside the repository:

```text
Windows: %APPDATA%\GPTCodexRouter
macOS:   ~/Library/Application Support/GPTCodexRouter
```

Each Codex login is isolated below `profiles/codex/<profile>/auth.json`. Google Antigravity credentials are independently isolated below `profiles/google-antigravity/<profile>/auth.json`; those files contain the Google access/refresh lifecycle and the Cloud Code Assist project binding for that same profile. Docker bind-mounts the platform state directory as `/data`, and the managed `profiles/` state path is excluded from the repository build context. Keep credential files in this managed state directory; do not copy standalone `auth.json` files into the source checkout.

The state root also contains router-owned runtime files:

```text
registry.json                              provider profiles + active profile per provider
client-key                                 local bearer key for /v1/*
codex-client-version                       Codex version used for model-catalog compatibility
admin-key                                  browser administrator bootstrap key
worker-key                                 host login worker key
admin-state.json                           bounded profile check/login job state
host-worker.pid                            host-side login worker process id
observability/                             JSONL lifecycle events and daily summaries
profiles/codex/<profile>/                  isolated Codex OAuth state
  .gcr-login-backup.json                   transient 0600 login crash-recovery copy while a result is unsettled
profiles/google-antigravity/<profile>/     isolated Google Antigravity OAuth/project state
```

The setup script writes only the selected host-state path to the ignored repository-local `.env` file so ordinary `docker compose ...` commands use the same mount later. The `.env` file does not contain ChatGPT tokens or the local client key. Existing installations may deliberately point `.env` at a legacy/custom state root; do not delete, migrate, or rerun setup into a different root casually because the profile OAuth state and `client-key` live there.

### Codex Desktop config

`setup.ps1` and `setup.sh` manage these two top-level settings in the normal Codex config (`%USERPROFILE%\.codex\config.toml` on Windows, `~/.codex/config.toml` on macOS):

```toml
chatgpt_base_url = "http://127.0.0.1:8317/backend-api"
openai_base_url = "http://127.0.0.1:8317/backend-api/codex"
```

Before editing an existing config, setup creates a one-time backup next to it named `config.toml.gpt-codex-router.bak`.

Use `-SkipCodexConfig` on Windows or `--skip-codex-config` on macOS if you want to manage those settings yourself.

Both base URLs are required for quota-aware routing. `chatgpt_base_url` routes ChatGPT backend services through the gateway. `openai_base_url` also routes the built-in OpenAI provider's Responses endpoint through it, including existing threads that retain `model_provider = "openai"`.

For `/backend-api/codex/responses`, GPT Codex Router rejects only the Responses WebSocket upgrade with HTTP 426 so Codex falls back to HTTP/SSE. That makes a confirmed usage-limit response observable and replayable. Other backend WebSocket routes continue through the reverse proxy normally.

## OpenAI-compatible local API

The same `127.0.0.1:8317` listener exposes an authenticated local compatibility surface for tools that speak OpenAI APIs:

```text
POST /v1/responses
POST /v1/chat/completions
GET  /v1/models
```

Retrieve the router-local API key with:

```bash
docker compose exec -T gpt-codex-router gpt-codex-router api-key
```

Use:

```text
base URL: http://127.0.0.1:8317/v1
API key:  <output of gpt-codex-router api-key>
```

The key is stored as `<state-root>/client-key`, is separate from every upstream OAuth credential, and is removed before provider dispatch. `/backend-api/*` remains Codex-only. `/v1/*` chooses a provider from the requested model ID:

```text
gpt-* / o* / other bare model IDs  -> existing Codex backend
google-antigravity/<model>         -> Google Antigravity / Cloud Code Assist
other-provider/<model>              -> explicit unsupported-provider error
```

Bare `gemini-*` IDs are **not** inferred as Antigravity. The explicit prefix prevents provider-name collisions as additional providers are added later.

`/v1/responses` is the primary compatibility path. For Codex models, the existing compatibility rules remain unchanged: string input becomes response items, `store:false` and upstream `stream:true` are enforced, Qwen-style `max_output_tokens` is removed because the subscription backend rejects it, non-stream callers receive reconstructed completed JSON, and successful stream callers receive normalized SSE headers. For `google-antigravity/*`, the adapter translates Responses messages, instructions, function tools, tool calls/results, reasoning controls, streaming output, token usage, and provider thought signatures into and out of Cloud Code Assist while still exposing canonical Responses SSE locally. Explicit `store:true` is rejected locally.

`/v1/chat/completions` first translates the supported Chat Completions subset to the same canonical Responses request and then uses the same provider router, so Antigravity does not have a separate Chat-only transport.

`/v1/models` merges the Codex model catalog with a lazily discovered Antigravity catalog. Antigravity discovery has a bounded timeout and in-memory cache; startup never waits on it, and a bounded static catalog is used when live discovery is unavailable. Antigravity IDs are exposed only as `google-antigravity/<model>`.

### Google Antigravity login

The Google provider is optional. Add its first profile from the running container:

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth add google-antigravity google-1
```

The command prints the Google authorization URL. In a native run, the `127.0.0.1:51121` callback can complete automatically. In Docker mode the router intentionally disables the unreachable container-loopback callback and uses manual completion: after Google redirects to `127.0.0.1:51121`, copy the complete redirected URL from the browser address bar and paste it back into the CLI. The authorization code, access token, refresh token, account identity, and project ID are never printed by `auth list` or `auth status`.

Additional accounts and manual selection use the same provider-scoped commands:

```powershell
docker compose exec -it gpt-codex-router gpt-codex-router auth add google-antigravity google-2
docker compose exec -T  gpt-codex-router gpt-codex-router auth use google-antigravity google-2
docker compose exec -T  gpt-codex-router gpt-codex-router auth list
```

A 401 refreshes the **same** Google profile once before requiring login again. A generic 429 does not rotate accounts. The router only performs one alternate-profile replay when the failed profile's exact wire-model quota is positively observed as exhausted and another profile's same-model quota is positively observed as usable; token and Cloud Code Assist project bindings are switched as one credential snapshot.

Google Antigravity OAuth client credentials are intentionally not embedded in the repository. Set `GOOGLE_ANTIGRAVITY_CLIENT_ID` and, when required by that OAuth client, `GOOGLE_ANTIGRAVITY_CLIENT_SECRET` in the ignored local `.env` file before login. Compose passes them into the container without writing them into registry state.

Antigravity conversations keep their selected account across turns and verified quota failover. `auth use` sets the preference for new conversations. Tool clients can echo the router-issued opaque `call_id` for continuity without extra headers; the matching Antigravity profile/session/wire metadata and thought signature are persisted under the managed state root so normal container restarts do not discard active tool continuity. The default idle window is seven days with a 30-day absolute cap. Explicit conversation IDs also preserve affinity across plain-text turns. `X-AI-Account` selects an exact Antigravity profile and disables failover. See [account continuity and schema policy](docs/openai-compatible-api.md#account-and-thought-signature-continuity) and the [structural change record](docs/account-pool-architecture.md).

For credentialed end-to-end verification, including manual Docker callback completion, Responses streaming, two-turn function-call/signature replay, and Qwen Code, see [`docs/google-antigravity-live-test.md`](docs/google-antigravity-live-test.md).

Qwen Code's `openai-responses` provider is a tested Codex client path and uses the same local `/v1` surface for Antigravity model selection. For the one-file `~/.qwen/settings.json` configuration, router-specific key naming, model selection, and troubleshooting, see [`docs/qwen-code.md`](docs/qwen-code.md). For the exact endpoint/normalization contract, see [`docs/openai-compatible-api.md`](docs/openai-compatible-api.md).

## Codex compatibility

This project intentionally depends on advanced Codex configuration and authentication surfaces, including `chatgpt_base_url`, `openai_base_url`, and the file-backed `cli_auth_credentials_store`. These are interoperability points, not a promise that future Codex releases will keep identical transport behavior.

Current upstream references:

- [Codex configuration source](https://github.com/openai/codex/blob/main/codex-rs/core/src/config/mod.rs)
- [Codex configuration schema](https://github.com/openai/codex/blob/main/codex-rs/core/config.schema.json)
- [Codex app-server configuration tests](https://github.com/openai/codex/blob/main/codex-rs/app-server/tests/common/config.rs)

If a Codex update changes these surfaces, stop the router and restore the config backup before troubleshooting the new version.

## Account selection and failover

The first registered account becomes active unless an existing active profile is already present.

Automatic failover is intentionally reactive:

```text
account-1 request
  -> 429 usage_limit_reached
  -> suppress account-1 until the reported reset time
  -> choose the next registered account
  -> replay the request
```

Generic 429s, transport errors, login failures, and token-refresh failures do **not** rotate accounts. If every registered account is exhausted, the router stops rotating rather than cycling indefinitely.

This project does not bypass, combine, or extend a provider quota. It only selects among separately authenticated profiles after an upstream usage-limit signal.

## Common operations

Start or rebuild:

```bash
docker compose up -d --build
```

Stop:

```bash
docker compose down
```

Logs:

```bash
docker compose logs -f --tail=100 gpt-codex-router
```

List profiles from the running container:

```bash
docker compose exec gpt-codex-router gpt-codex-router auth list
```

Select a preferred profile:

```bash
docker compose exec gpt-codex-router gpt-codex-router auth use codex personal
```

The selected profile applies to new backend requests. An already-running stream stays bound to the credentials with which it started.

For normal post-install account maintenance, open `http://127.0.0.1:8317/admin`. Retrieve the administrator key with `docker compose exec -T gpt-codex-router gpt-codex-router admin-key`, then use **연결 확인**, **로그인**, or **다시 로그인** in the browser. The Docker image intentionally still does not include the interactive Codex CLI; the local host worker executes the official browser login without stopping the router.

The profile list itself is side-effect free: it reads local token metadata and the last explicit check result. `connected` means an explicit check for that exact profile reached the Codex model catalog with HTTP 200; an expired access token with a stored refresh token remains `access_expired_unverified` until refresh/check is actually attempted.

See [Docker account authentication management](docs/admin-account-management.md) for the API, state model, worker lease protocol, recovery rules, and live-verification checklist.

### Remove / restore

Stop the router:

```bash
docker compose down
```

To return Codex Desktop to the config that existed before the first setup run, restore the adjacent `config.toml.gpt-codex-router.bak` file. If setup created a brand-new config and no backup exists, remove the two router base-URL lines manually.

Delete the platform state directory only if you also want to remove the isolated local login profiles. Treat that directory as credential material:

```text
Windows: %APPDATA%\GPTCodexRouter
macOS:   ~/Library/Application Support/GPTCodexRouter
```

## Manual / developer build

For native development, use a currently supported Go toolchain. As of September 2026, Go 1.26 or Go 1.27 is recommended; the module's language version remains `go 1.23`. The normal Docker setup does not require a host Go installation.

```bash
make test
make vet
make build-all
```

Outputs:

```text
dist/gpt-codex-router-linux-amd64
dist/gpt-codex-router-darwin-arm64
dist/gpt-codex-router-darwin-amd64
dist/gpt-codex-router-windows-amd64.exe
```

Native mode is loopback-only by design:

```powershell
.\dist\gpt-codex-router-windows-amd64.exe serve
```

## Security model

GPT Codex Router handles bearer credentials for your ChatGPT/Codex sessions. It is therefore intentionally local-only.

- Native listeners must be loopback addresses. Loopback limits network exposure but does not authenticate other processes running as the local user, so the router is intended for a trusted single-user machine.
- Docker listens on `0.0.0.0:8317` **inside the container only**; Compose publishes it as `127.0.0.1:8317` on the host.
- `/backend-api` and `/backend-api/*` remain the Codex passthrough surface. Authenticated `/v1/*` routes are handled by the local OpenAI-compatible adapter and then enter the same credential/failover gateway.
- `/admin/*` uses a separate administrator key/session from the `/v1` client key; mutating browser calls require same-origin and CSRF validation.
- `/admin/worker/*` accepts only the separate host-worker key. Worker jobs contain no arbitrary command or path supplied by the browser.
- Inbound auth/cookie headers are removed before the selected profile credentials are attached upstream.
- OAuth refresh is constrained to the selected profile and rejects an account-identity change. Host login and refresh share a profile-scoped write lock, so unrelated profiles continue serving.
- The host worker discards raw Codex CLI stdout/stderr and administrator responses/job files never contain OAuth tokens, raw account IDs, authorization codes, or upstream response bodies.
- Real credentials are never needed by the automated test suite.

Read [SECURITY.md](SECURITY.md) before changing listener exposure, credential storage, proxy headers, or OAuth behavior.

## Non-goals

GPT Codex Router is not:

- an `api.openai.com/v1` API-key proxy;
- a public or LAN reverse proxy;
- a quota bypass mechanism;
- a replacement Codex client;
- an MSIX patcher or Desktop injector;
- a multi-user authentication server.

## Contributing

Bug reports and focused pull requests are welcome. Keep the project small, local-first, and security conservative. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).

## Observability

The router emits structured lifecycle events and distinguishes HTTP status from actual stream completion. For `/backend-api/codex/responses`, it also records **metadata-only Context Observability**: exact wire-body size, decoded context size, composition, bounded process-local opaque fingerprints, observed context reuse/growth, SSE event counts, and numeric token usage when ChatGPT supplies it. Codex may send the request body with `Content-Encoding: zstd`; the router decodes a bounded observer-only copy for analysis while forwarding and replaying the original compressed bytes unchanged. Prompt text, response text, source code, terminal/tool output contents, function arguments, and decoded payloads are not stored.

Generate a recent report from the normal Docker installation with:

```bash
docker compose exec gpt-codex-router \
  gpt-codex-router report --since 3h --timezone Asia/Seoul
```

For a native development binary, run `gpt-codex-router report ...` directly.

The report's `Context observability` section shows coverage, **request wire size**, **decoded context size**, context growth/reuse/amplification, structural composition, token usage, and replay inference. Stable lineage prefers explicit conversation/thread identifiers and can fall back to a process-local HMAC of `prompt_cache_key`; only the bounded lineage-source enum is logged, never the key itself. Composition separates tool outputs from tool definitions plus system, developer, reasoning, metadata, and residual other bytes. Growth/reuse/amplification are calculated from the decoded context representation rather than compressed wire bytes. `context_reuse_ratio` is structural payload reuse observed by the router; it is **not** the GPT prompt-cache hit rate. When upstream usage contains `cached_input_tokens`, the report shows the actual cached-token ratio separately. If upstream usage is absent, token usage is reported as `unavailable` rather than estimated from byte counts.

See [`docs/observability.md`](docs/observability.md) for the event contract, Context metric definitions, retention policy, coverage warnings, baseline rules, and rollback procedure.
