# OpenAI-compatible local API

GPT Codex Router exposes a local OpenAI-compatible surface on the same loopback listener used by Codex Desktop.

```text
http://127.0.0.1:8317/v1
```

The compatibility surface is intentionally local-only and uses a router-local bearer key that is separate from all ChatGPT/Codex OAuth credentials.

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

The key is created once under the configured router state root as `client-key` and is reused across ordinary container rebuilds/restarts. It changes only if that state is deliberately replaced or the key is deliberately rotated.

The local bearer header is stripped before the request enters the ChatGPT backend gateway. The gateway injects only the OAuth credentials belonging to the selected Codex profile.

## Request path

```text
OpenAI-compatible client
    |
    | Authorization: Bearer <client-key>
    v
/v1/* adapter
    |- authenticate local caller
    |- normalize client protocol where required
    |- strip local Authorization header
    v
/backend-api/codex/* gateway
    |- choose active Codex profile
    |- refresh that profile when required
    |- perform narrow usage-limit failover when allowed
    |- inject selected ChatGPT/Codex credentials
    v
chatgpt.com/backend-api/codex/*
```

The existing Codex backend gateway remains authoritative for profile selection, OAuth refresh, retry/failover, and observability.

## `POST /v1/responses`

This is the preferred API for clients that support the OpenAI Responses protocol, including Qwen Code's `openai-responses` provider.

The ChatGPT Codex subscription backend does not accept every field accepted by the public OpenAI Responses API. The local adapter therefore applies the smallest known compatibility normalization before forwarding the request.

Current normalization:

- OpenAI string `input` is converted to a Codex message-item list using `input_text` content.
- `store` is forced to `false`; explicit `store:true` is rejected locally with HTTP 400.
- upstream `stream` is always forced to `true` because the Codex backend requires streaming;
- when the local caller requested `stream:false` or omitted `stream`, the router consumes the upstream SSE stream and reconstructs one completed JSON Responses object;
- when the local caller requested `stream:true`, the router forwards the SSE body and guarantees `Content-Type: text/event-stream` plus `Cache-Control: no-cache` on successful responses;
- `max_output_tokens` is currently removed before forwarding because the Codex subscription backend rejects that parameter and exposes no equivalent field through this route;
- router-controlled Codex compatibility headers such as `originator` and the detected Codex client `version` are injected locally.

The `max_output_tokens` behavior is an explicit compatibility trade-off: callers may send the field, but the router does **not** currently enforce that output limit upstream. Do not depend on it as a hard generation cap when using this subscription-backed route.

The non-streaming aggregator reconstructs output from events including `response.output_item.done` and `response.completed`; it does not treat the initial HTTP 200 alone as generation completion.

### Example

```bash
KEY="$(docker compose exec -T gpt-codex-router gpt-codex-router api-key)"

curl http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer ${KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-luna","input":"Reply with exactly: pong","stream":false}'
```

On PowerShell:

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

## `POST /v1/chat/completions`

The Chat Completions endpoint is a compatibility adapter for clients that cannot use Responses directly.

It translates the supported Chat Completions subset into one upstream Responses request and translates the result back to Chat Completions JSON/SSE.

Supported request families include:

- text messages;
- image URL content;
- function tools and function calls;
- `tool_choice` and `parallel_tool_calls`;
- streaming and `stream_options.include_usage`;
- `temperature` and `top_p`;
- `reasoning_effort`;
- text / JSON-object / JSON-schema response formats;
- `max_completion_tokens` / `max_tokens` translation.

Unknown Chat Completions fields are rejected with HTTP 400 instead of being silently discarded. The `user` field is currently accepted by the request parser for compatibility but is not forwarded to the Codex backend; do not rely on it for attribution or routing.

Important backend caveat: `max_completion_tokens` and `max_tokens` are translated to Responses `max_output_tokens`. The current ChatGPT Codex subscription backend may reject that field. Clients that can use Responses should prefer `/v1/responses`, where the router has a dedicated compatibility rule for this mismatch.

## `GET /v1/models`

The model endpoint reads the Codex client version from managed router state and requests the same Codex model-catalog route used by the compatibility layer:

```text
/backend-api/codex/models?client_version=<detected-version>
```

Setup writes the installed `codex --version` value to:

```text
<state-root>/codex-client-version
```

If that file is missing, the router can recover a version from an existing Codex `models_cache.json`. If no usable version can be resolved, `/v1/models` returns a local configuration error while the rest of the router can continue serving traffic.

Example:

```bash
KEY="$(docker compose exec -T gpt-codex-router gpt-codex-router api-key)"
curl http://127.0.0.1:8317/v1/models -H "Authorization: Bearer ${KEY}"
```

The returned identifiers are normalized into an OpenAI-style model list. A model appearing in the list means it is visible through the current Codex catalog contract; it does not override subscription eligibility or upstream product policy.

## Error behavior

Local compatibility errors use JSON error objects. Common cases include:

- `401 invalid_api_key`: the bearer key does not match the current router `client-key`;
- `400 invalid_request_error`: malformed JSON, `store:true`, or an unsupported Chat Completions request shape;
- `503 configuration_error`: model listing cannot resolve a Codex client version;
- upstream 4xx/5xx: returned through the same backend gateway after local authentication and normalization.

Do not expose raw upstream HTML challenge pages or credential material to clients. `/v1/models` converts upstream catalog failures into bounded JSON errors.

## Observability

Compatibility traffic uses the same request lifecycle logger as native Codex traffic. `api_surface` distinguishes the local client entry point:

```text
openai_responses
openai_chat_completions
openai_models
```

`route_template` still records the backend route actually used, for example `/backend-api/codex/responses`.

For `/v1/responses` and `/v1/chat/completions`, Context Observability sees the normalized/transcoded Responses payload that is actually sent to the backend, not the caller's original local request body. See [`observability.md`](observability.md).

## Security boundary

The `/v1` key authenticates only local callers to the router. It is not an OpenAI API key and is never sent to ChatGPT.

The listener must remain host-loopback-only. The presence of a local bearer key does not make the service appropriate for LAN, internet, or multi-user exposure. See [`../SECURITY.md`](../SECURITY.md).
