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

Requires Go 1.23 or newer.

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

Automated tests must use synthetic credentials and local test servers. Never commit real `auth.json`, access tokens, refresh tokens, cookies, account IDs, or exported credentials.

## Pull requests

Keep pull requests focused. Include:

- the problem being solved;
- the security/trust-boundary impact, if any;
- tests for behavioral changes;
- README or SECURITY updates when user-facing behavior changes.
