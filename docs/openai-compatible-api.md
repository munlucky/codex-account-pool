# OpenAI-compatible local API

GPT Codex Router exposes a local OpenAI-compatible surface on the same loopback listener used by Codex Desktop:

```text
http://127.0.0.1:8317/v1
```

The compatibility surface is intentionally local-only and uses a router-local bearer key that is separate from every upstream OAuth credential.

## Endpoints

```text
POST /v1/responses
POST /v1/chat/completions
GET  /v1/models
```

All `/v1/*` requests require:

```text
Authorization: Bearer <router-client-key>
```

Retrieve the current key from the running container:

```bash
docker compose exec -T gpt-codex-router gpt-codex-router api-key
```

The key is created once under the configured router state root as `client-key` and is reused across ordinary rebuilds/restarts. It is never reused as an upstream provider credential.

## Provider routing

`/backend-api/*` remains the native Codex data plane. Provider selection exists only on `/v1/*` and is deliberately model-prefix based:

```text
gpt-* / o* / any other bare model ID
    -> Codex backend

google-antigravity/<model>
    -> Google Antigravity / Cloud Code Assist

other-provider/<model>
    -> 400 unsupported-provider error
```

A bare `gemini-*` ID is **not** inferred as Google Antigravity. This keeps routing explicit and prevents model-name collisions.

```text
OpenAI-compatible client
    |
    | Authorization: Bearer <client-key>
    v
/v1/* adapter
    |- authenticate local caller
    |- normalize to canonical Responses form
    |- strip local Authorization header
    |- resolve model provider
    |
    +--> Codex backend
    |      -> existing /backend-api/codex/* gateway
    |      -> selected Codex profile / Codex failover
    |      -> chatgpt.com
    |
    `--> Google Antigravity backend
           -> session-affine account pool
           -> selected google-antigravity credential snapshot
           -> Google OAuth refresh / project binding
           -> Cloud Code Assist
```

The Codex gateway is not generalized into a provider framework. Antigravity has a separate credential broker and CCA adapter behind the `/v1` dispatcher.

## Google Antigravity authentication

Before login, configure `GOOGLE_ANTIGRAVITY_CLIENT_ID` and, when required by that OAuth client, `GOOGLE_ANTIGRAVITY_CLIENT_SECRET` in the ignored local `.env` file. Real OAuth client credentials are intentionally not embedded in the repository.

Create the first Google profile interactively:

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth add google-antigravity google-1
```

The login flow uses authorization code + PKCE. The default callback is:

```text
http://127.0.0.1:51121/callback
```

When the router runs natively, the loopback callback can complete automatically. In Docker mode the router does not start the container-local loopback callback because the host browser cannot reach it through the current Compose mapping. After Google redirects to `127.0.0.1:51121`, copy the **complete redirected URL** from the browser address bar and paste it into the CLI. A raw authorization code is also accepted for environments that expose it separately.

After token exchange the router resolves the account's Cloud Code Assist project. The login is not committed as successful unless an access token, refresh token, expiry, and project binding are available. The project ID is stored only in that profile's private credential file, not in `registry.json`.

Multiple Google profiles are supported:

```powershell
gpt-codex-router auth add google-antigravity google-2
gpt-codex-router auth use google-antigravity google-2
gpt-codex-router auth status google-antigravity google-2
gpt-codex-router auth list
```

For Antigravity, `auth use` changes the preferred account for new conversations. Existing in-memory bindings retain their account, including an account selected by verified failover.

`auth list` and `auth status` do not print Google email, account IDs, project IDs, access tokens, or refresh tokens.

## `POST /v1/responses`

This is the preferred API for clients that support OpenAI Responses, including Qwen Code's `openai-responses` provider.

Common local rules:

- JSON must contain a non-empty `model`;
- string `input` is normalized to a user message with `input_text`;
- `store:true` is rejected locally; `store:false` is accepted;
- local `stream:false` is implemented by consuming canonical Responses SSE and assembling one completed Responses JSON object;
- local `stream:true` returns canonical Responses SSE.

### Codex models

Bare model IDs continue through the existing Codex adapter. It preserves the established subscription-backend compatibility behavior:

- upstream `store:false` and `stream:true` are enforced;
- `max_output_tokens` is removed because the ChatGPT Codex subscription route rejects it;
- router-controlled Codex `originator` and detected client-version headers are injected locally;
- profile selection, refresh, and confirmed Codex usage-limit failover remain owned by the existing Codex gateway.

The `max_output_tokens` behavior is therefore a Codex-specific compatibility trade-off. Do not depend on it as a hard generation cap on that route.

### Google Antigravity models

An ID such as:

```text
google-antigravity/gemini-3.8-flash
```

is stripped to the provider model ID only after routing. The Antigravity adapter converts the Responses request into the Cloud Code Assist request envelope and supports the first implementation scope required for agent clients:

- text user/assistant history;
- system/developer instructions;
- function tools;
- function calls and function-call outputs;
- `tool_choice`;
- `temperature` and `top_p`;
- `max_output_tokens`;
- Responses reasoning effort to the provider's model-specific thinking controls;
- stable process-local session IDs;
- streaming text/function-call output;
- usage metadata including reasoning tokens when supplied upstream.

Image input is not part of the current Antigravity adapter contract and is rejected explicitly instead of being silently dropped.

The upstream CCA stream is translated back to canonical Responses events such as:

```text
response.created
response.output_item.added
response.content_part.added
response.output_text.delta
response.function_call_arguments.delta
response.output_item.done
response.completed
```

Malformed or prematurely truncated CCA streams are not treated as completed generations.

### Account and thought-signature continuity

Account selection lives in a credential-free pool, separate from the HTTP adapter. Every request reloads one selected profile's current token/project snapshot. Requests in the same conversation serialize through the end of the stream; other conversations remain independent. The pool holds at most 1024 conversations, with 30-minute idle expiry and idle-entry eviction. It never evicts an in-flight lease.

Use a unique conversation identifier in `X-Client-Thread-Id`, `X-OpenAI-Conversation-Id`, or `X-Codex-Parent-Thread-Id`. A Responses `prompt_cache_key` can also identify a conversation, but must be unique per conversation, not shared merely because prompts use the same cache template. Explicit headers take precedence over `prompt_cache_key`.

Headerless clients receive opaque router-generated `call_id` values. Echo each returned `call_id`, function name, and arguments unchanged in subsequent tool history. A bounded in-memory registry recovers the original conversation and restores the provider's original wire call ID and signature. Identical initial prompts in different conversations do not share identity. Headerless requests without tool history are independent requests; use an explicit identifier to preserve account affinity across plain-text turns.

The cache holds up to 1024 signature entries and 1024 call handles, expiring after 30 minutes. Handles bind the function name and canonical argument digest. Invalid, expired, mixed-session, or altered handles fail locally. Restart, expiry, or eviction can make tool continuation unavailable. Start a new conversation when `session_continuity_unavailable` is returned; the router does not invent signatures or replay them under another account.

For an exact Antigravity profile, send `X-AI-Account: google-2`. The local header is removed before dispatch and never sent upstream. Unknown or wrong-provider profiles fail without fallback. A selector conflicting with existing tool history fails continuity validation. A pinned request never fails over. Codex rejects this unsupported header; its existing gateway behavior remains unchanged.

Provider reasoning/tool continuity uses a bounded process-memory replay cache. Its key includes Google profile, wire model, session, function name, and canonical arguments. Only signatures that match the provider signature shape are retained; synthetic OpenAI item/call IDs are rejected as signatures.

Properties:

```text
memory only
bounded entry count
TTL + LRU eviction
profile isolated
no raw prompt storage
cleared on process exit
```

A signature-related upstream 400 clears that session's signature and call-handle state rather than recycling it across later turns.

### Google tool schema transformation

Function `parameters` are transformed without mutating the caller's schema. Local `$ref` pointers through `$defs` or `definitions` are inlined, including escaped JSON Pointer segments. String `const` becomes a singleton string `enum`; conflicting type/enum constraints fail locally. Object properties retain their literal names, including `$ref` and `uniqueItems`.

`$schema`, `$id`, unused definitions, `default`, and `examples` are removed. `uniqueItems` is deliberately omitted for wire compatibility; tool executors must enforce uniqueness. Objects, arrays, supported scalar types, nullable unions, `anyOf`, boolean/schema `additionalProperties`, string enums, bounds, required fields, and supported annotations are retained. Non-string enum/const, general type unions, unsupported composition/keywords, unresolved/external/cyclic references, and conflicting reference siblings return local `400 invalid_request_error` with `invalid_tool_schema` in the message.

Expansion is limited to depth 32, 4096 schema nodes, and 256 KiB of transformed JSON. These are adapter limits, not guarantees that an upstream model accepts every schema. The subset follows the [Google Schema representation](https://googleapis.github.io/dotnet-genai/api/Google.GenAI.Types.Schema.html); the Antigravity transport still has its own upstream acceptance behavior.

### 401 and 429 behavior

Antigravity authentication failures and quota responses intentionally use different rules:

```text
401
 -> refresh the same profile once
 -> retry the same profile once
 -> otherwise require login again
```

A generic 429 is **not** an account-rotation signal. Before any alternate profile is tried, the router probes `fetchAvailableModels` with the failed profile's own access-token/project pair and requires the exact wire model to report zero remaining quota. It then requires another registered profile's same wire model to report positive remaining quota. Only then is the original request replayed once with that alternate profile's token and project as one credential snapshot. Unknown, missing, or still-positive quota evidence returns the original 429 without rotation.

The probe sequence has one five-second deadline. A successful alternate becomes the conversation account for subsequent turns. Pinned selectors and requests with existing tool history never switch accounts. Streaming output is never retried. Model-specific quota observations have a bounded one-minute cache: new conversations may prefer a recently verified usable account while the preferred account is exhausted. Existing bindings do not move when that cache expires.

## Example Responses calls

Codex:

```bash
KEY="$(docker compose exec -T gpt-codex-router gpt-codex-router api-key)"

curl http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer ${KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-luna","input":"Reply with exactly: pong","stream":false}'
```

Antigravity:

```bash
curl http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer ${KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"google-antigravity/gemini-3.8-flash","input":"Reply with exactly: pong","stream":false}'
```

PowerShell:

```powershell
$key = (docker compose exec -T gpt-codex-router gpt-codex-router api-key).Trim()
$body = @{
  model = "google-antigravity/gemini-3.8-flash"
  input = "Reply with exactly: pong"
  stream = $false
} | ConvertTo-Json

curl.exe http://127.0.0.1:8317/v1/responses `
  -H "Authorization: Bearer $key" `
  -H "Content-Type: application/json" `
  -d $body
```

## `POST /v1/chat/completions`

Chat Completions is a compatibility adapter for clients that cannot use Responses directly. It translates the supported Chat request into canonical Responses first and then uses the same provider router. There is no separate Antigravity Chat transport.

The existing Chat translator supports text messages, function tools/calls, `tool_choice`, streaming usage, temperature/top-p, reasoning effort, response-format translation, and token-limit translation. Some Chat features can translate to Responses items that a specific backend does not support; backend-specific validation still applies. In particular, Antigravity image input is currently rejected.

Unknown Chat Completions fields are rejected with HTTP 400 rather than silently discarded.

## `GET /v1/models`

The model endpoint merges independently obtained provider catalogs.

Codex models come from:

```text
/backend-api/codex/models?client_version=<detected-version>
```

using the managed `codex-client-version` metadata or an existing Codex model cache fallback.

Antigravity models are discovered lazily with:

```text
POST /v1internal:fetchAvailableModels
```

The discovery call happens on model-list demand, not server startup. It uses a short timeout, caches successful results in memory, collapses provider wire variants into stable public picker IDs where required, and falls back to a bounded static Antigravity catalog when live discovery is unavailable. A catalog failure from one provider does not suppress models obtained from the other provider.

Antigravity entries are always namespaced:

```json
{
  "id": "google-antigravity/gemini-3.8-flash",
  "object": "model",
  "owned_by": "google-antigravity"
}
```

A model appearing in the list is a routing/catalog statement, not a guarantee that the current subscription/profile is entitled to use it.

## Server startup without a profile

The HTTP server no longer requires an active Codex profile before listening. This allows health checks and Antigravity-only local `/v1` use:

```text
/healthz                         -> available when the router is healthy
/backend-api/* without Codex    -> Codex auth unavailable
Antigravity model without login -> account_unavailable error
```

Live model discovery is never a startup dependency.

## Error behavior

Local compatibility errors use JSON error objects. Common cases include:

- `401 invalid_api_key`: the bearer key does not match the router `client-key`;
- `400 invalid_request_error`: malformed JSON, `store:true`, unsupported request shape, or unsupported provider prefix;
- `503 account_unavailable`: the selected Antigravity account is unavailable, or no recently verified usable account remains during quota cooldown;
- `503 session_continuity_unavailable`: conversation/account affinity is missing, expired, or conflicts with a selector;
- `400 invalid_request_error` with `session_continuity_unavailable`: a bound Gemini conversation is missing a valid tool signature;
- bounded upstream 4xx/5xx errors: no raw provider body, token, project ID, or HTML challenge page is reflected to the caller.

## Observability

Compatibility traffic uses the same structured lifecycle sink. Important dimensions include:

```text
api_surface = openai_responses | openai_chat_completions | openai_models
provider    = google-antigravity   (for Antigravity lifecycle events)
route_template = /backend-api/... or /v1internal:streamGenerateContent
```

Codex Context Observability remains attached to the Codex Responses data plane. Antigravity currently records lifecycle/upstream timing and status metadata without persisting raw prompt/response bodies, tool arguments, thought signatures, OAuth values, Google account identity, or project IDs.

See [`observability.md`](observability.md).

## Security boundary

The `/v1` key authenticates only local callers to the router. It is not an OpenAI or Google API key and is never sent upstream. Provider access/refresh tokens are loaded from the selected local profile and injected by the corresponding broker after local authentication.

The listener must remain host-loopback-only. The presence of a local bearer key does not make the service appropriate for LAN, internet, or multi-user exposure. See [`../SECURITY.md`](../SECURITY.md).
