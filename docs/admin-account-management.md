# Docker account authentication management

This document describes the local administrator UI and the container-owned Codex device-code login flow. The router does not treat a local `auth.json` file as proof that ChatGPT is reachable. A profile becomes `connected` only after an explicit request for that exact profile reaches the Codex model catalog with HTTP 200.

## Architecture

```text
browser /admin
      |
      | administrator session + CSRF
      v
127.0.0.1:8317 -> Docker gpt-codex-router
                    |-- account registry / gateway / refresh
                    |-- administrator job state
                    `-- Codex app-server
                          |
                          | account/login/start
                          | type=chatgptDeviceCode
                          v
                       ChatGPT

host state root -----------------------> /data
  profiles/codex/<profile>/auth.json
```

There is no continuously running host login worker. Windows/macOS only provide Docker Desktop and the browser used to approve the device code. The official Codex runtime used for login is bundled in the Docker image and runs with the selected profile's isolated `CODEX_HOME`.

## Local credential boundaries

Two router-local credentials are used:

| Credential | Purpose | Storage |
| --- | --- | --- |
| `client-key` | authenticates `/v1/*` clients | state root |
| `admin-key` | creates a browser administrator session | state root |

They are not interchangeable and neither is sent upstream as ChatGPT authentication. The browser session cookie is HttpOnly and SameSite Strict. Mutating administrator requests also require same-origin validation and a CSRF token.

Existing installations may still contain obsolete `worker-key`, `host-worker.pid`, or `host-tools/` files created by an earlier implementation. Current versions do not read them. Setup may stop a verifiably running obsolete worker as migration cleanup but does not require or reinstall one.

## Profile state

`GET /admin/profiles` is local-state-only. It does not refresh OAuth or call ChatGPT.

Possible profile states:

| State | Meaning |
| --- | --- |
| `not_logged_in` | no usable local Codex auth exists |
| `unverified` | local auth exists but has not been explicitly checked |
| `access_expired_unverified` | access token is expired; a refresh token exists but no check has run yet |
| `connected` | an explicit exact-profile check reached the Codex model catalog with HTTP 200 |
| `reauth_required` | the stored auth can no longer be refreshed/used |
| `temporarily_unavailable` | transport, timeout, TLS, provider 5xx, or other retryable failure |
| `checking` | exact-profile upstream check is running |
| `logging_in` | Docker-owned device login or its short finalization phase is running |

A profile-list refresh never mutates the selected account and never performs OAuth refresh.

## Administrator API

| Endpoint | Authentication | Purpose |
| --- | --- | --- |
| `GET /admin` | none | static local administrator shell; contains no profile credentials |
| `POST /admin/session` | administrator key + same origin | creates the browser session |
| `GET /admin/session` | administrator session | obtains the CSRF token/session expiry |
| `GET /admin/profiles` | administrator session | reads local profile/status snapshot |
| `POST /admin/profiles/{provider}/{id}/check` | session + Origin + CSRF | starts an exact-profile upstream check |
| `POST /admin/profiles/{provider}/{id}/login` | session + Origin + CSRF | starts new login or re-login inside Docker |
| `GET /admin/jobs/{job_id}` | administrator session | returns bounded job state, device challenge, and fixed error code |
| `POST /admin/jobs/{job_id}/cancel` | session + Origin + CSRF | cancels a check or active device login |

The previous `/admin/worker/*` surface has been removed.

## Device-code login

For each login job the router starts the bundled Codex app-server with:

```text
CODEX_HOME=/data/profiles/codex/<profile>
cli_auth_credentials_store="file"
```

Known API-key/token override environment variables are removed. The router initializes the app-server and requests:

```json
{
  "method": "account/login/start",
  "params": { "type": "chatgptDeviceCode" }
}
```

The job exposes only the bounded values required for user approval:

- HTTPS verification URL;
- device user code.

OAuth access/refresh/ID tokens, raw ChatGPT account IDs, authorization codes, app-server stderr, and provider error bodies are never returned by the administrator API.

The browser opens the verification URL normally on the host. No container-to-host localhost OAuth callback is required. When Codex emits `account/login/completed`, the router validates the resulting file-backed auth state and then performs an exact-profile upstream check.

If device-code authentication is disabled by the upstream account or organization, the job fails with the fixed `device_auth_unavailable` error instead of falling back to a hidden host daemon or another OAuth flow.

## Login safety and rollback

Before re-login the router acquires the same profile-scoped auth write lock used by OAuth refresh and writes a transient recovery file:

```text
profiles/codex/<profile>/.gcr-login-backup.json
```

This file contains the previous `auth.json` bytes and is credential material. On Unix it is written mode `0600`, stays inside the profile home, is never returned by an API, and is deleted when the login reaches a settled result.

The previous auth is restored when:

- the device login fails or is canceled;
- the login times out;
- Codex produces an invalid auth file;
- re-login resolves to a different raw ChatGPT account identity;
- the post-login exact-profile connection check fails.

A new profile is registered only after the new auth passes local validation. Registering it does not change an already selected active profile. If final verification fails, the new profile registration is rolled back.

If the container restarts while a login is unsettled, the persisted job is marked `failed/service_restarted` on startup. The router then reconciles any surviving `.gcr-login-backup.json`: failed/canceled jobs restore the old auth; successful jobs remove an obsolete backup; unknown states are not guessed.

## Exact-profile checks

Administrator checks pin the requested profile directly. They never change the active profile and do not perform usage-limit failover.

A pinned 401 may perform the same bounded same-profile refresh used by ordinary gateway traffic, but the request stays pinned to that account. Refresh failures are classified conservatively:

- definitive invalid/revoked refresh state -> `reauth_required`;
- generic 400, transport/TLS/timeout, or 5xx -> `temporarily_unavailable`.

Provider response bodies are not reflected into administrator errors.

## State root and upgrades

Compose bind-mounts the selected host state root at `/data`. The repository-local ignored `.env` is the single host-side source of the bind-mount path. Setup preserves an existing `GPT_CODEX_ROUTER_STATE_ROOT` value, including legacy/custom roots, rather than silently creating a second credential store.

Typical state:

```text
registry.json
client-key
admin-key
admin-state.json
observability/
profiles/codex/<profile>/auth.json
profiles/codex/<profile>/.gcr-login-backup.json   # only while recovery is unsettled
```

The Docker image carries the Codex runtime version used for device login and model-catalog compatibility. The host does not need a Codex CLI merely to operate the router.

Retrieve the administrator key with:

```bash
docker compose exec -T gpt-codex-router gpt-codex-router admin-key
```

Then open:

```text
http://127.0.0.1:8317/admin
```

## Automated verification

Automated tests cover:

- administrator session, Origin, and CSRF enforcement;
- removal of the old worker Bearer-authentication surface;
- container device-code app-server handshake and bounded challenge extraction;
- stripping API-key/token override environment variables from the Codex login child;
- device-auth-disabled classification;
- profile-scoped auth write serialization;
- re-login account-identity mismatch rollback;
- new-profile registration without active-profile change;
- cancellation and container-restart backup reconciliation;
- exact-profile selection invariance and safe same-profile 401 refresh;
- bounded terminal-job retention.

## Live verification checklist

On a real Windows/macOS Docker Desktop installation verify:

- `docker compose up -d --build` builds the image including the pinned Codex runtime;
- `/healthz` returns `ok` and `/admin` loads;
- an existing custom/legacy `.env` state root remains unchanged after setup;
- an existing profile still passes **연결 확인** and the selected profile does not change;
- adding a new Codex profile displays an HTTPS verification URL and device code without any host worker process;
- approving the code results in `succeeded` / `connected` and preserves the previous active profile;
- **다시 로그인** keeps the old auth if the operation is canceled or intentionally fails;
- restarting the container during an unsettled re-login restores the prior auth from the transient backup;
- Codex Desktop works after both base URLs point to the loopback router.
