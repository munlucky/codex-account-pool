# Qwen Code integration

For `google-antigravity/*`, Qwen must echo the router-returned opaque `call_id` with the unchanged function name and arguments. The router recovers the original conversation/account even when Qwen sends no conversation header. It never groups conversations by their first prompt text. Antigravity continuity is persisted under the router state root and survives a normal container/router restart. The default idle window is seven days with a 30-day absolute cap; if continuity is genuinely outside retention or a current handle is altered, start a new conversation when `session_continuity_unavailable` is returned.

Clients may supply a unique `X-Client-Thread-Id` per conversation to retain account affinity across plain-text turns as well. Do not hardcode one shared ID for concurrent sessions. The local regression suite covers headerless Responses tool continuation, Chat translation, account changes, and signature restoration with a fake upstream; it is not a fresh credentialed Qwen/Google live test.

Qwen Code can use GPT Codex Router through its `openai-responses` provider.

This path has been validated against the router with Qwen Code 0.23.3 on Windows using a ChatGPT/Codex profile and `gpt-5.6-luna`.

```text
Qwen Code
  -> openai-responses
  -> http://127.0.0.1:8317/v1/responses
  -> GPT Codex Router compatibility adapter
  -> selected ChatGPT/Codex profile
  -> chatgpt.com/backend-api/codex/responses
```

Use `openai-responses`, not the legacy `openai` Chat Completions provider, when possible. It maps directly to the router's primary `/v1/responses` compatibility path and preserves Responses tool/reasoning semantics more accurately.

## Prerequisites

Start/rebuild the router first:

```powershell
cd C:\dev\ai-auth-proxy
docker compose up -d --build
```

Verify health:

```powershell
Invoke-WebRequest http://127.0.0.1:8317/healthz
```

Verify the local Responses API independently of Qwen before changing Qwen settings:

```powershell
$key = (docker compose exec -T gpt-codex-router gpt-codex-router api-key).Trim()
$body = @{
  model = "gpt-5.6-luna"
  input = "Reply with exactly: pong"
  stream = $false
} | ConvertTo-Json

curl.exe http://127.0.0.1:8317/v1/responses `
  -H "Authorization: Bearer $key" `
  -H "Content-Type: application/json" `
  -d $body
```

A working router returns a completed Responses object containing `pong`.

## One-file `settings.json` configuration

Qwen Code supports `modelProviders` and the `env` object in `~/.qwen/settings.json`. If you intentionally want the router key stored in that file, use a dedicated variable name instead of the generic `OPENAI_API_KEY`.

Recommended user-scope file on Windows:

```text
C:\Users\<user>\.qwen\settings.json
```

Merge the fields below into the existing file rather than blindly replacing it. Preserve unrelated settings such as UI preferences, MCP configuration, and `tools.workflowsEnabled`.

Example:

```json
{
  "$version": 4,
  "env": {
    "GPT_CODEX_ROUTER_API_KEY": "gcr_REPLACE_WITH_CURRENT_ROUTER_KEY"
  },
  "modelProviders": {
    "openai-responses": [
      {
        "id": "gpt-5.6-luna",
        "name": "GPT-5.6 Luna (Codex Account Pool)",
        "envKey": "GPT_CODEX_ROUTER_API_KEY",
        "baseUrl": "http://127.0.0.1:8317/v1",
        "generationConfig": {
          "timeout": 120000
        }
      }
    ]
  },
  "security": {
    "auth": {
      "selectedType": "openai-responses"
    }
  },
  "model": {
    "name": "gpt-5.6-luna"
  }
}
```

Retrieve the value to place in `GPT_CODEX_ROUTER_API_KEY` with:

```powershell
cd C:\dev\ai-auth-proxy
docker compose exec -T gpt-codex-router gpt-codex-router api-key
```

Do not put `/responses` in `baseUrl`. Qwen appends the Responses route itself, so the correct value is:

```text
http://127.0.0.1:8317/v1
```

### Why use `GPT_CODEX_ROUTER_API_KEY`?

Qwen resolves `modelProviders[].envKey` from its environment. Its generic OpenAI credentials often use `OPENAI_API_KEY`, which may already exist in a shell, system environment, `.env` file, another development tool, or an older Qwen configuration.

Using a router-specific name isolates this configuration:

```text
settings.json env.GPT_CODEX_ROUTER_API_KEY
                    |
                    v
modelProviders.openai-responses[].envKey
                    |
                    v
Authorization: Bearer <router client-key>
```

Qwen's `settings.json` `env` values are fallback environment values. A shell or `.env` value with the **same variable name** can take precedence. Using the dedicated name dramatically reduces accidental collisions, but if you have already defined `GPT_CODEX_ROUTER_API_KEY` elsewhere, remove or update that higher-priority value.

The key is stored in plain text in `settings.json`. Treat that file as credential material and do not commit or publish it.

## Selecting another model

List the models currently visible through the Codex catalog:

```powershell
$key = (docker compose exec -T gpt-codex-router gpt-codex-router api-key).Trim()
curl.exe http://127.0.0.1:8317/v1/models `
  -H "Authorization: Bearer $key"
```

Add another entry to `modelProviders.openai-responses`, for example:

```json
{
  "id": "gpt-5.6-sol",
  "name": "GPT-5.6 Sol (Codex Account Pool)",
  "envKey": "GPT_CODEX_ROUTER_API_KEY",
  "baseUrl": "http://127.0.0.1:8317/v1",
  "generationConfig": {
    "timeout": 120000,
    "reasoning": {
      "effort": "high"
    }
  }
}
```

Then select it through Qwen's `/model` command or change `model.name`.

A model being listed does not guarantee that every account/profile has identical entitlement. Upstream account/product policy remains authoritative.

## Smoke test

Fully exit any existing Qwen process after changing `settings.json`, then start a fresh process:

```powershell
qwen -p "Reply with exactly: pong"
```

Expected output:

```text
pong
```

After the smoke test succeeds, normal Qwen skills/workflows use the same provider path. For example, a project workflow command does not require a separate router configuration unless that workflow explicitly overrides model/provider credentials itself.

## Why the router normalizes Qwen requests

Qwen's `openai-responses` provider follows the public OpenAI Responses shape, while the ChatGPT Codex subscription backend has a narrower request contract.

The router currently handles the known differences required by Qwen:

- Qwen can send `max_output_tokens`; the router removes it because the Codex subscription backend rejects it;
- Qwen expects streaming Responses; the router always requests upstream streaming;
- successful local streaming responses are labeled `Content-Type: text/event-stream`, even when the backend omitted that header;
- the Codex backend requires `store:false`;
- OpenAI string input can be converted into the Codex item-list form;
- the router injects the Codex client compatibility headers required by the backend.

These are protocol-compatibility rules only. They do not change profile selection, OAuth ownership, or quota behavior.

## Troubleshooting

### `401 Invalid local router API key`

Example:

```text
Responses API error 401: {"error":{"message":"Invalid local router API key.","type":"invalid_api_key"}}
```

This means Qwen reached the router successfully, but the bearer key Qwen sent does not equal the current `<state-root>/client-key`.

Check the current router key source:

```powershell
cd C:\dev\ai-auth-proxy
docker compose exec -T gpt-codex-router gpt-codex-router api-key
```

Then ensure the exact value is stored under the variable named by the selected model's `envKey`.

If you use `GPT_CODEX_ROUTER_API_KEY`, also verify that an old value with that exact name is not present in the launching shell, system environment, project `.qwen/.env`, project `.env`, `~/.qwen/.env`, or `~/.env`. Qwen gives those sources higher priority than `settings.json.env`.

Do not troubleshoot this error by changing ChatGPT OAuth profiles: the 401 happens before the request enters the OAuth-backed gateway.

### `Unsupported parameter: max_output_tokens`

This means an old router image is still running. Current `/v1/responses` removes that field before sending the request to the Codex backend.

Rebuild:

```powershell
cd C:\dev\ai-auth-proxy
docker compose up -d --build
```

Then retry Qwen.

### `Responses API returned non-SSE content-type: <missing>`

This also indicates an old router build. Current streaming `/v1/responses` guarantees `Content-Type: text/event-stream` on successful responses.

Rebuild with:

```powershell
cd C:\dev\ai-auth-proxy
docker compose up -d --build
```

### Qwen uses an unexpected model or URL

Check for higher-priority Qwen CLI flags and environment variables. For a model selected through `modelProviders`, the selected provider entry supplies the model ID, `envKey`, and `baseUrl`; generic CLI/environment overrides can still affect resolution depending on how Qwen is launched.

Prefer user-scope `~/.qwen/settings.json` for the shared router configuration and avoid duplicating the same provider definition in project-scope Qwen settings.

### Key changed after reinstall/state change

Normal Docker rebuilds do not rotate `client-key`. If you deliberately replaced the router state root or deleted `client-key`, retrieve the new key and update `settings.json`.

The repository-local `.env` controls which host state root Docker mounts. Do not casually rerun setup into a different state root or delete an existing state directory when troubleshooting a key mismatch.

## Security notes

Storing the key directly in `settings.json` is convenient but means the key exists in clear text. The router is still loopback-only, but any process/user able to read that file can authenticate to the local `/v1` surface.

The router key is only local authorization. It is never forwarded to ChatGPT; the selected profile's OAuth credentials are attached later by the gateway.

See [`../SECURITY.md`](../SECURITY.md) and [`openai-compatible-api.md`](openai-compatible-api.md) for the trust boundary and protocol contract.
## Upstream Qwen references

Qwen Code's current configuration reference documents `modelProviders`, the `openai-responses` provider, custom `envKey` names, and the `settings.json` `env` fallback:

- [Qwen Code model providers](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/model-providers/)
- [Qwen Code authentication configuration](https://github.com/QwenLM/qwen-code/blob/main/docs/users/configuration/auth.md)

The router documentation describes the compatibility behavior implemented in this repository; Qwen's upstream documentation remains authoritative for future Qwen configuration changes.
