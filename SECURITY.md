# Security Policy

## Trust boundary

GPT Codex Router is a **local-only, single-user ChatGPT subscription gateway**. It handles Codex OAuth credentials, so its security model depends on keeping the HTTP listener on the same trusted machine as the user. Loopback binding limits network exposure; it is not authentication between local processes.

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

The router reads the selected profile's Codex `auth.json` only to authenticate ChatGPT backend requests and maintain that same profile's OAuth refresh lifecycle. A separate router-local `client-key` is stored at the state root and is used only to authenticate `/v1/*` callers; it is never used as upstream ChatGPT authentication. The state root also stores `codex-client-version`, which is non-secret compatibility metadata used by `/v1/models`. Existing installations may point the repository-local Compose `.env` at a legacy/custom state root; treat that selected root as the credential boundary and do not silently migrate it.

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

- the platform state directory is bind-mounted at `/data`, and the managed `profiles/` state path is excluded from the supplied Docker build context; keep standalone credential files such as `auth.json` out of the source checkout rather than relying on filename-based ignore rules;
- the runtime process runs as a non-root user;
- Linux capabilities are dropped;
- `no-new-privileges` is enabled;
- `/healthz` is unauthenticated but contains no credential data and never contacts ChatGPT.

Only one router process should actively use a profile directory at a time. The setup script is intended to perform interactive login before starting/restarting the container.

## Upstream request sanitization

`/backend-api` and `/backend-api/*` remain the Codex passthrough routes. `/v1/responses`, `/v1/chat/completions`, and `/v1/models` are local compatibility routes and require `Authorization: Bearer <client-key>`. The compatibility layer removes that local authorization header before handing a request to the backend gateway. Before any upstream request is sent, the gateway removes inbound authentication-bearing headers and attaches only the credentials selected from local profile state.

The local `GET /healthz` route is handled without reading profile credentials or contacting ChatGPT.

## Local OpenAI-compatible API

The `/v1/*` surface is intended for local development clients only. It does not turn the router into a network service: the same loopback-only host boundary applies. The local client key is generated from cryptographically random bytes, stored outside the repository, and should be treated as a bearer secret.

`/v1/responses` performs local authentication/path adaptation and injects router-controlled Codex compatibility headers before entering the existing gateway. Because the ChatGPT Codex backend requires item-list input, `store:false`, and `stream:true`, the adapter normalizes OpenAI string input, injects `store:false`, and always streams upstream. Non-streaming local callers receive a reconstructed completed JSON response assembled from upstream SSE events. Successful streaming callers receive a normalized `Content-Type: text/event-stream` header even when the backend omits it. Explicit `store:true` is rejected rather than silently changing that caller-requested semantic. The adapter currently removes `max_output_tokens` because the subscription backend rejects that field and exposes no equivalent through this route; therefore the local adapter does not provide a hard upstream output-token cap for that field.

`/v1/chat/completions` translates a bounded supported subset into Responses requests and translates Responses output back to Chat Completions using the same backend-required `store:false`/`stream:true` contract. Unsupported request fields are rejected rather than silently discarded. Chat `max_completion_tokens`/`max_tokens` currently map to Responses `max_output_tokens`, which the backend may reject; clients that support Responses should prefer `/v1/responses`. `/v1/models` resolves the installed Codex client version from managed state (falling back to an existing Codex model cache), requests `/backend-api/codex/models?client_version=<version>`, and normalizes only the returned model identifiers. Caller-supplied `originator`, `version`, and `client_version` values are not trusted as the upstream Codex identity contract.

Do not publish the listener to a LAN or the public internet even when `/v1/*` authentication is enabled. The client key is defense in depth for local callers; it does not change the product's single-user trust model.

If a client such as Qwen Code stores the router key directly in its own settings file, that settings file becomes local credential material. For example, Qwen's `settings.json` `env` object stores values in clear text. Do not commit, sync publicly, or attach such a file to bug reports. Prefer a router-specific variable name (for example `GPT_CODEX_ROUTER_API_KEY`) when multiple OpenAI-compatible tools share the machine so an unrelated `OPENAI_API_KEY` does not accidentally select the wrong credential.

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

Native CLI login/status/run operations use an isolated `CODEX_HOME` and remove known API-key/token override variables before launching Codex. Both platform setup scripts apply the same isolation when invoking the official login command.

## Context Observability data policy

Context Observability is a metadata analyzer, not a traffic-capture facility. It reads the already replayable `/backend-api/codex/responses` wire bytes without rewriting them and observes Responses output incrementally without buffering the complete response for telemetry. When the request uses `Content-Encoding: zstd`, only a bounded observer-side copy is decompressed for structural analysis; the compressed bytes forwarded upstream and used for quota replay are never replaced by the decoded representation.

It may persist only bounded structural metadata such as wire/context byte counts, structural category byte counts, a bounded lineage-source enum, item counts, numeric token usage supplied by the upstream response, ratios, lifecycle counters, and process-local opaque content references. Request/body, item, and lineage references are truncated HMAC-SHA256 values keyed by a random process secret. Stable lineage may use `prompt_cache_key` only after this process-local HMAC transformation; the raw key is never retained or emitted. References are intentionally unstable across process restarts and must not be treated as durable content hashes, account identifiers, or user-tracking identifiers. The transient decoded context bytes themselves are never written to logs or daily summaries.

Context Observability must never persist:

- raw prompt, system, developer, or assistant text;
- raw response or reasoning text;
- source code or file contents;
- terminal output or tool-result contents;
- function/tool arguments;
- raw `prompt_cache_key` or other session/cache-affinity identifiers;
- OAuth/access/refresh/ID tokens, cookies, or raw ChatGPT account IDs;
- complete HTTP headers, query strings, or raw request/response traffic.

Structural `context_reuse_ratio` measures repeated payload bytes/items observed by the router. It is not a provider prompt-cache hit rate. Only numeric upstream usage such as `cached_input_tokens` is used for actual cache reporting, and missing token usage is reported as unavailable rather than inferred from bytes.

Context analysis is fail-open with respect to the data plane: malformed JSON, invalid/unsupported content encoding, oversized decoded payloads, or other unsupported telemetry input may reduce observability coverage but must not fail an otherwise valid proxied request. Decoder work is bounded; observer decode failures are telemetry failures only.

## Testing and repository hygiene

Automated tests must use synthetic credentials and local test servers. Never commit or attach real:

- `auth.json` files;
- access/refresh/ID tokens;
- cookies;
- ChatGPT account IDs;
- environment files containing credentials;
- private proxy logs containing request data;
- router `client-key` values or client settings files that embed that key.

Managed runtime profile directories, common credential exports, environment files, `*.log` files, and build output are ignored by Git. A standalone file named `auth.json` outside the managed profile directory is credential material and must not be placed in the source checkout. Always inspect `git status` and the staged diff before publishing.

## Reporting a vulnerability

Open a GitHub security advisory for issues involving credential disclosure, cross-profile credential writes, listener exposure, inbound-auth trust, OAuth account identity changes, or unintended profile switching. Do not include live credentials in reports.
