# Contributing to GPT Codex Router

Thanks for contributing.

## Scope

GPT Codex Router is intentionally small. Changes should preserve these boundaries:

- local, single-user operation;
- official Codex login as the source of ChatGPT authentication;
- isolated Codex profile state;
- explicit, conservative usage-limit failover;
- no Desktop binary/package modification;
- no public/LAN credential-bearing proxy.

Avoid speculative frameworks, compatibility layers, or unrelated provider integrations unless there is a concrete interoperability requirement.

## Development

Use a currently supported Go toolchain for development. As of September 2026, Go 1.26 or Go 1.27 is recommended; `go.mod` retains `go 1.23` as the module language version.

```bash
make fmt
make test
make vet
make build-all
```

Docker validation:

```bash
docker compose config
docker build -t gpt-codex-router:dev .
```

Automated tests must use synthetic credentials and local test servers. Never commit real `auth.json`, access tokens, refresh tokens, cookies, account IDs, router `client-key` values, or exported credentials.

OpenAI-compatible adapter changes should include focused regression coverage for the affected contract. At minimum, preserve local bearer authentication, stripping of the local `Authorization` header before gateway injection, Codex-required `store:false`/upstream streaming behavior, bounded error responses, and SSE behavior for streaming clients. Qwen-shaped Responses requests are a primary interoperability case; do not add broad translation frameworks when one explicit compatibility rule is sufficient.

When changing `/v1` behavior, update `README.md`, `docs/openai-compatible-api.md`, `docs/qwen-code.md` when applicable, and `SECURITY.md` if the trust boundary or credential handling changes.

## Pull requests

Keep pull requests focused. Include:

- the problem being solved;
- the security/trust-boundary impact, if any;
- tests for behavioral changes;
- README or SECURITY updates when user-facing behavior changes.
