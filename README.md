# GPT Codex Router

Local, Docker-first ChatGPT subscription profile routing for Codex Desktop and Codex CLI.

GPT Codex Router lets you sign in to multiple ChatGPT accounts through the **official Codex login flow**, keeps each account in an isolated `CODEX_HOME`, and routes Codex Desktop backend traffic through the selected profile. When ChatGPT returns a confirmed subscription usage-limit response, the router can move to the next registered profile and replay that request.

> **Unofficial project.** This repository is not affiliated with or endorsed by OpenAI. OpenAI, ChatGPT, GPT, and Codex names are used only to describe interoperability. Use only accounts you are authorized to use and follow the applicable product terms and limits.

## Why this exists

Codex Desktop normally uses one ChatGPT login context. GPT Codex Router adds a small local routing layer without modifying the Desktop app, MSIX package, renderer, or Codex binaries.

```text
Codex Desktop
   |
   | chatgpt_base_url + openai_base_url
   v
127.0.0.1:8317
   |
   v
GPT Codex Router (Docker)
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
git clone https://github.com/munlucky/codex-account-pool.git
cd gpt-codex-router
.\setup.cmd
```

macOS:

```bash
git clone https://github.com/munlucky/codex-account-pool.git
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
2. writes the host-state path to the ignored local `.env` used by Compose;
3. builds and starts the Docker container;
4. checks `http://127.0.0.1:8317/healthz`;
5. backs up and configures `~/.codex/config.toml` (or `%USERPROFILE%\.codex\config.toml` on Windows).

Then **fully restart Codex Desktop**.

For custom names, or to re-authenticate an existing profile, pass profile names explicitly while still completing each browser login interactively.

Windows:

```powershell
.\setup.ps1 -Profiles personal,work,backup
# Later, re-authenticate only one existing profile:
.\setup.ps1 -Profiles personal
```

macOS:

```bash
./setup.sh --profiles personal,work,backup
# Later, re-authenticate only one existing profile:
./setup.sh --profiles personal
```

If you downloaded a ZIP instead of cloning, extract it and run `setup.cmd` on Windows or `./setup.sh` on macOS from that folder. If the ZIP loses the executable bit on macOS, run `chmod +x setup.sh` once.

Rerun the platform setup script later to add more automatically named accounts. Existing profiles and the originally selected active profile are preserved.

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

A normal request produces credential-safe logs similar to:

```text
GPT Codex Router ChatGPT gateway listening on http://0.0.0.0:8317/backend-api (active codex profile: personal)
proxy personal POST 200
switch personal -> work quota
proxy work POST 200
```

Tokens, ChatGPT account IDs, prompt bodies, and provider error bodies are not logged.

## What setup changes

### Router state

Host state lives outside the repository:

```text
Windows: %APPDATA%\GPTCodexRouter
macOS:   ~/Library/Application Support/GPTCodexRouter
```

Each Codex login is isolated below `profiles/codex/<profile>/auth.json`. Docker bind-mounts the platform state directory as `/data`; credentials are not copied into the image or Docker build context.

The setup script writes only the selected host-state path to the ignored repository-local `.env` file so ordinary `docker compose ...` commands use the same mount later. The `.env` file does not contain ChatGPT tokens.

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

To add or refresh ChatGPT logins, rerun the host setup script. The Docker image intentionally does not include the interactive Codex CLI.

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

Go 1.23 or newer is required only when building the native binary directly.

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

- Native listeners must be loopback addresses.
- Docker listens on `0.0.0.0:8317` **inside the container only**; Compose publishes it as `127.0.0.1:8317` on the host.
- Only `/backend-api` and `/backend-api/*` are proxied.
- Inbound auth/cookie headers are removed before the selected profile credentials are attached upstream.
- OAuth refresh is constrained to the selected profile and rejects an account-identity change.
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
