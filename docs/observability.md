# Observability

The router records a bounded JSONL request lifecycle and keeps HTTP status separate from transport completion.

## Event contract

Each proxied request emits one `request_start` and one `request_end`. Intermediate events can include `upstream_attempt`, `account_switch`, and `transport_fallback`.

Important fields are `request_id`, `route_template`, `transport`, `peer_class`, `profile_ref`, `status_code`, `status_origin`, `outcome`, `semantic_outcome`, `auth_ms`, `upstream_headers_ms`, `first_body_ms`, `gateway_total_ms`, and `response_bytes`.

`profile_ref` is a process-local pseudonymous reference. Dynamic paths are reduced to bounded route templates. The logger does not persist request or response bodies or query strings.

For a streaming request, `status_code=200` is not proof that delivery completed. Inspect `outcome` and `semantic_outcome` as separate dimensions.

## Context Observability

Context Observability applies to `POST /backend-api/codex/responses`. It starts from the exact byte slice already buffered for quota-aware replay and never re-marshals or rewrites the upstream request. If those wire bytes are Zstd encoded, a bounded observer-only copy is decoded for JSON structure analysis while the original compressed bytes remain the source for upstream transmission and failover replay. Responses stream observation is incremental and does not collect the full response body; the exact Responses POST route enables the stream parser even when the upstream Content-Type is not labeled `text/event-stream`.

Persisted request metadata can include:

- `request_bytes`, the exact encoded wire-body size, and a process-local opaque `body_ref`;
- `context_bytes`, the decoded JSON size used only for structural analysis when analysis succeeds;
- `instructions_bytes`, `user_bytes`, `assistant_bytes`, `tool_output_bytes`, `tool_definition_bytes`, `system_bytes`, `developer_bytes`, `reasoning_bytes`, `metadata_bytes`, and residual `other_input_bytes`, all measured from the decoded context representation;
- bounded item counts and a bounded `lineage_source` enum;
- `previous_request_bytes`, `context_growth_bytes`, `reused_context_bytes`, `novel_context_bytes`, `context_reuse_ratio`, and `context_amplification_ratio` when a safe lineage is available;
- SSE/output/tool-call counts;
- numeric `input_tokens`, `cached_input_tokens`, `output_tokens`, `reasoning_tokens`, and `total_tokens` when those values are supplied upstream.

`body_ref`, internal item references, and lineage references use HMAC-SHA256 with a random process-local secret and are truncated before logging. Explicit conversation/thread identifiers are preferred for lineage; when they are absent, `prompt_cache_key` can be used only through that process-local HMAC. The raw `prompt_cache_key` is never logged or retained. These references exist only to compare opaque content within one process lifetime; they are not stable content hashes or user/account identifiers.

The router does **not** persist prompt/response/reasoning text, source code, file contents, terminal/tool-output contents, function arguments, credentials, cookies, raw account identifiers, complete headers, query strings, raw traffic, or the transient decoded request payload. Unknown fields are ignored by the allowlisted structural analyzer.

### Coverage and interpretation

The report distinguishes `analyzed`, `skipped`, and `unobserved` Responses POST requests. Skip reasons such as `malformed_json`, `decode_error`, `unsupported_encoding`, or `oversized_payload` are coverage information, not proxy failures. Context analysis is fail-open: telemetry failure never makes an otherwise valid proxied request fail.

The report prints `Request wire size` separately from `Decoded context size`. Coverage also shows whether a safe lineage key was available, whether a predecessor delta was available, and the bounded lineage-source counts. The first request in a lineage can have a lineage key but no delta; later requests sharing the same safe lineage can produce growth/reuse/amplification metrics. `context_reuse_ratio` is the fraction of the current decoded context represented by structurally identical observed items from the previous safe lineage. `context_amplification_ratio` compares total decoded context size with newly observed decoded context bytes. These are router-side payload measurements; **neither value is a GPT prompt-cache hit rate**.

When the upstream response provides `cached_input_tokens`, the report calculates `cache_ratio = cached_input_tokens / input_tokens`. That numeric upstream usage is the authoritative cache signal. If usage is absent, the report prints `Token usage: unavailable`; it does not estimate tokens from byte counts.

### Replay and duplicate inference

Existing lifecycle events are also correlated by `request_id` to report Responses request count, upstream attempts, quota-triggered account switches/replays, successful 2xx upstream attempts, and duplicate successful generations. A normal usage-limit sequence such as `429 -> account_switch -> 200` is one replay with one successful generation. More than one 2xx `upstream_attempt` for the same request ID is reported as `duplicate_successful_generation` and should be investigated as a router defect.

## Storage

Runtime data is written below the configured state root:

- `observability/events.jsonl`: rotated at 20 MiB with five files retained.
- `observability/daily/YYYY-MM-DD.json`: daily aggregates retained for 30 days.

The logger uses a bounded asynchronous queue. Queue overflow is surfaced later with `logger_health` and `dropped_events`; reports containing dropped events are incomplete evidence.

Docker Compose also limits stdout JSON logs to 20 MiB x five files. Docker builds resolve the current Git HEAD from minimal repository metadata included in the build context, so `service_commit` identifies the source revision even when `docker compose up --build` is run directly. An explicit `COMMIT` build arg still overrides this resolution when needed.

## Reports

```sh
gpt-codex-router report --since 3h --timezone Asia/Seoul
```

Use `--from` and `--to` for an explicit interval, or `--stdin` for JSONL supplied from another source.

The report includes coverage, request counts, status/origin counts, route/status counts, bounded error categories, transport outcomes, semantic stream outcomes, attempts, fallbacks, incomplete request pairs, dropped events, and p50/p95/p99 latency metrics.

Accepted WebSocket upgrades (`101 Switching Protocols`) that are still open are reported as `active_upgrades`; they are not counted as unfinished requests and do not trigger an incomplete-coverage warning by themselves. If a later `startup` event proves the process restarted before an upgrade emitted its closing event, it is reported as `interrupted_upgrades` and does trigger a coverage warning instead of being mistaken for a live connection.

Small samples and genuinely incomplete coverage are explicitly warned about and should not be used alone to declare an optimization successful.

## Baseline and rollback

Compare the same route and transport before and after one change at a time. Treat expected Responses WebSocket 426 fallback and client cancellation separately from service failures.

Before replacing a running container, record its image ID, command, bind mount, published port, and logging configuration. Reuse the same state root and do not run two routers against the same profile state. After deployment verify `/healthz`, the data mount, loopback-only host publication, log rotation, and a real request with correlated start/attempt/end events. If streaming behavior regresses, recreate the previous image with the same runtime mapping.
