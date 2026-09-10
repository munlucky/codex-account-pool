# Observability

The router records a bounded JSONL request lifecycle and keeps HTTP status separate from transport completion.

## Event contract

Each proxied request emits one `request_start` and one `request_end`. Intermediate events can include `upstream_attempt`, `account_switch`, and `transport_fallback`.

Important fields are `request_id`, `route_template`, `transport`, `peer_class`, `profile_ref`, `status_code`, `status_origin`, `outcome`, `semantic_outcome`, `auth_ms`, `upstream_headers_ms`, `first_body_ms`, `gateway_total_ms`, and `response_bytes`.

`profile_ref` is a process-local pseudonymous reference. Dynamic paths are reduced to bounded route templates. The logger does not persist request or response bodies or query strings.

For a streaming request, `status_code=200` is not proof that delivery completed. Inspect `outcome` and `semantic_outcome` as separate dimensions.

## Storage

Runtime data is written below the configured state root:

- `observability/events.jsonl`: rotated at 20 MiB with five files retained.
- `observability/daily/YYYY-MM-DD.json`: daily aggregates retained for 30 days.

The logger uses a bounded asynchronous queue. Queue overflow is surfaced later with `logger_health` and `dropped_events`; reports containing dropped events are incomplete evidence.

Docker Compose also limits stdout JSON logs to 20 MiB x five files.

## Reports

```sh
gpt-codex-router report --since 3h --timezone Asia/Seoul
```

Use `--from` and `--to` for an explicit interval, or `--stdin` for JSONL supplied from another source.

The report includes coverage, request counts, status/origin counts, route/status counts, bounded error categories, transport outcomes, semantic stream outcomes, attempts, fallbacks, incomplete request pairs, dropped events, and p50/p95/p99 latency metrics.

Small samples and incomplete coverage are explicitly warned about and should not be used alone to declare an optimization successful.

## Baseline and rollback

Compare the same route and transport before and after one change at a time. Treat expected Responses WebSocket 426 fallback and client cancellation separately from service failures.

Before replacing a running container, record its image ID, command, bind mount, published port, and logging configuration. Reuse the same state root and do not run two routers against the same profile state. After deployment verify `/healthz`, the data mount, loopback-only host publication, log rotation, and a real request with correlated start/attempt/end events. If streaming behavior regresses, recreate the previous image with the same runtime mapping.
