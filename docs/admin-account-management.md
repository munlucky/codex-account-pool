# Docker account authentication management

Status: implementation contract for the local single-user Docker deployment.

This document describes the administrator UI, profile check, and host-login worker introduced for Codex profiles. It does not treat a local auth file as proof that ChatGPT is reachable. A profile becomes `connected` only after the router performs an explicit Codex model-catalog request with that exact profile and receives HTTP 200.

## Trust boundaries

The normal listener remains host-loopback only through Compose:

```text
browser/admin UI ----                      > 127.0.0.1:8317 -> Docker router
Codex/OpenAI client --/
host login worker ---/
```

Three independent local credentials have different purposes:

| Credential | Scope | Storage |
| --- | --- | --- |
| `client-key` | `/v1/*` client API only | state root |
| `admin-key` | creates a browser administrator session | state root |
| `worker-key` | `/admin/worker/*` only | state root |

The browser never receives the worker key. The host worker never receives the administrator key. A `/v1` client key cannot enumerate profiles, start login, inspect jobs, or call worker endpoints.

After administrator-key login, the browser uses an HttpOnly, SameSite=Strict cookie. Mutating administrator calls require both a same-origin `Origin` and the per-session `X-CSRF-Token`. Administrator sessions are process-local and expire after one hour; restarting the container invalidates them.

## Profile states

`GET /admin/profiles` is local-state-only. It does not refresh OAuth or call ChatGPT.

| State | Meaning |
| --- | --- |
| `not_logged_in` | the registered profile has no usable ChatGPT OAuth auth file |
| `unverified` | local access credentials are present and not expired, but no current explicit upstream result exists |
| `access_expired_unverified` | the access token is expired, a refresh token exists, and refresh has not yet been attempted by an explicit check/request |
| `connected` | the most recent explicit check for the exact profile returned upstream HTTP 200 |
| `reauth_required` | refresh credentials are absent, an authoritative refresh rejection was observed, the auth file is invalid, or account identity continuity failed |
| `temporarily_unavailable` | network/TLS/timeout/upstream failure prevented verification |
| `checking` | one explicit check is running |
| `logging_in` | the official host login or its bounded server-side final verification is still in progress |

The original design listed no state for a locally valid, unexpired token that had never been checked. `unverified` is therefore an intentional contract addition: reporting that state as `connected` would incorrectly imply upstream evidence, while reporting it as unavailable would imply a failure that did not occur.

Every explicit check result carries its check time. Token expiry alone never produces `connected` or proves `reauth_required`.

## Administrator API

The first implementation supports `provider=codex`.

| Route | Authentication | Behavior |
| --- | --- | --- |
| `GET /admin/profiles` | administrator session | local profile/status snapshot; no upstream call |
| `POST /admin/profiles/{provider}/{id}/check` | session + Origin + CSRF | starts an exact-profile upstream check |
| `POST /admin/profiles/{provider}/{id}/login` | session + Origin + CSRF | queues new login or re-login |
| `GET /admin/jobs/{job_id}` | administrator session | returns bounded job state/error code |
| `POST /admin/jobs/{job_id}/cancel` | session + Origin + CSRF | cancels a check or invalidates a login lease |
| `POST /admin/worker/lease` | worker key | leases one queued login |
| `POST /admin/worker/jobs/{job_id}/heartbeat` | worker key | renews the login lease |
| `POST /admin/worker/jobs/{job_id}/status` | worker key | returns bounded persisted job state for restart reconciliation |
| `POST /admin/worker/jobs/{job_id}/complete` | worker key | reports only success or a fixed error code |
| `POST /admin/worker/diagnostics` | worker key | reports bounded Desktop routing state |

Responses and persisted administrator state contain profile IDs, selected state, token expiry metadata, refresh-token presence, bounded status/error codes, and timestamps. They do not contain OAuth tokens, raw ChatGPT account IDs, authorization codes, provider response bodies, CLI output, arbitrary paths, or shell strings.

## Exact-profile check and 401 behavior

An explicit check performs these steps without changing the active registry entry:

1. acquire credentials for the requested profile;
2. refresh that same profile when its access token is within the existing refresh window;
3. request the Codex model catalog through the existing gateway using that same pinned profile;
4. suppress quota-driven 429 profile failover for this pinned request, so the active registry entry cannot change;
5. store only the bounded result and check time.

For ordinary gateway traffic, an upstream 401 may force-refresh that same profile at most once and replay only a request that is explicitly safe for this mechanism: `GET`, `HEAD`, `OPTIONS`, or the known `POST /backend-api/codex/responses` path. Unknown mutating POST routes and other mutating methods are not replayed. If a confirmed 429 usage-limit failover moves the request to another profile, that second profile may likewise be force-refreshed at most once if it later returns 401. This remains separate from the existing confirmed subscription-quota 429 failover; a 401, refresh failure, transport failure, or generic 429 does not itself rotate accounts.

The 401 decision occurs inside the reverse-proxy transport before the upstream response is exposed to the downstream writer. A response that has already begun streaming to a client is therefore never replayed by this mechanism.

## Profile write serialization

OAuth refresh and host-side `codex login` can both replace a profile's `auth.json`. They share a profile-scoped filesystem lock under the profile's isolated `CODEX_HOME`.

The lock is per profile, not global. A login or refresh for `account-1` does not block normal credential use by `account-2`.

The host worker backs up an existing `auth.json` before login. The backup is a transient mode-0600 credential file inside that profile home and is never returned by an API or written to logs. If login fails, produces an invalid auth file, changes the raw ChatGPT account identity of an existing profile, or the persisted job later resolves to `failed/canceled`, the previous file is restored atomically. A restarted worker reconciles any surviving backup against the server's persisted job state: successful jobs delete the backup without rollback, failed/canceled jobs restore it, and unknown/in-progress jobs are left untouched rather than guessed.

## Host worker

The Docker image still does not contain Codex CLI. During the Docker build it also cross-compiles the small host-side executable contract for macOS arm64/amd64 and Windows amd64. Setup copies the matching executable **out of the just-built local container image** into `<state-root>/host-tools/`; the normal install therefore does not require a host Go toolchain or checked-in `dist/` binaries.

The extracted host binary runs:

```text
gpt-codex-router worker --server http://127.0.0.1:8317
```

It accepts only loopback server URLs. A leased job contains only a job ID, `codex`, a validated profile ID, a new-profile flag, and a lease ID. The worker itself constructs the only allowed login command:

```text
codex -c 'cli_auth_credentials_store="file"' login
```

It sets that profile's `CODEX_HOME`, removes `OPENAI_API_KEY`, `CODEX_API_KEY`, and `CODEX_ACCESS_TOKEN`, and discards Codex CLI stdout/stderr instead of persisting raw login output. The browser flow remains the official host Codex flow with its loopback callback.

The worker reports a heartbeat while login is in progress. Cancellation or an invalid lease cancels the child process. Worker and service restarts cannot leave a job indefinitely marked in progress:

- service restart converts persisted in-flight checks, active logins, and login finalization into `failed/service_restarted`;
- an expired worker lease becomes `failed/worker_restarted`; late heartbeat or completion requests cannot revive it;
- a queued job may be safely leased after restart because no login process had started.

A successful host login is not the final success condition. When the worker reports success, the server first atomically claims the still-valid login lease before any registration side effect. It then validates the resulting auth file, registers a new profile without changing an existing active profile, and performs the explicit upstream check asynchronously. The public job remains `logging_in` during this short finalization phase and becomes `succeeded` only when the check reaches `connected`. If cancellation or timeout won the race first, the completion is rejected and the worker restores the previous auth state rather than registering the late result.

## Desktop routing diagnostic

The host worker reads only the two relevant top-level entries from the host Codex config and reports one bounded value:

- `configured`: both expected loopback router URLs are active;
- `disabled`: the file is absent or the expected settings are absent/commented/different;
- `unknown`: the host config could not be inspected.

This diagnostic is separate from account authentication. A connected account can coexist with Desktop routing being disabled.

## Local files and recovery

Additional state files are stored beside the existing registry and OAuth state:

```text
admin-key
worker-key
admin-state.json
host-worker.pid
profiles/codex/<profile>/.gcr-login-backup.json  transient crash-recovery copy; present only while a login result is unsettled
```

They must remain local-user state and must not be committed or exported.

The management page itself never exposes the administrator key. In the normal Docker installation, retrieve it from the running router, which reads the same mounted state root:

```bash
docker compose exec -T gpt-codex-router gpt-codex-router admin-key
```

The extracted host-worker binary uses the same state root and therefore the same worker key, but the worker key is never shown in the browser or reused as an administrator credential.

Back up the whole state root before an upgrade if rollback must preserve profile credentials and administrator state. Rolling back code does not require changing `registry.json` or existing `profiles/codex/*/auth.json`; older binaries ignore the new administrator state files.

## Verification status

Automated tests cover credential separation, CSRF/origin checks, profile-scoped write serialization, exact-profile selection invariance, safe 401 single-refresh replay, persisted job restart handling, bounded terminal-job retention, new-profile registration without active-profile change, cancellation-versus-completion ordering, host-worker fixed command/environment, late-completion auth rollback, crash-backup reconciliation for failed and successful jobs, account-ID/JWT identity mismatch rollback, and non-loopback worker refusal.

A release is not considered fully live-verified until all of these are additionally exercised on the target host:

- macOS official browser login through the host worker;
- new profile and re-login followed by an actual upstream model-catalog HTTP 200;
- host worker and container restart during/around a login job;
- Codex Desktop with both router URLs actively configured;
- Windows host worker/browser login on a real Windows machine.

Windows support can be built and tested at the Go level before that final real-device check, but it must remain labeled unverified until the Windows live scenario succeeds.
