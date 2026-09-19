# Account selection and adapter boundaries

The September 2026 change preserves `/v1`, explicit `provider/model` routing, and separate provider credentials. It addresses three concrete defects: every Antigravity turn restarted at ACTIVE, schema sanitization deleted referenced types, and a diagnostic change reflected raw upstream error bodies.

```text
/v1 authentication and normalization
  -> ProviderRouter registry -> RouteTarget(provider, model, optional selector)
  -> Antigravity account coordinator
       -> accountpool: session lease, ID selection, quota observations
       -> antigravityauth: selected profile's fresh credential snapshot
  -> Antigravity wire adapter
       -> bounded Google schema transformation
       -> profile/model/session/call-bound signature replay
       -> HTTP/SSE translation
```

`accountpool` contains no access/refresh tokens, project IDs, or provider wire parsing. `antigravity/accounts.go` adapts the provider's registry and quota observations to that policy. `client.go` owns HTTP transport, same-account refresh, request/response conversion, and lifecycle events. Codex remains behind its existing gateway; this change does not reinterpret its established failover rules.

## Execution strategy and invariants

1. Preserve existing edits, explicit provider routing, and provider credential storage. Replace the router's fixed backend fields with a registry and a small RouteTarget.
2. Serialize each conversation through a bounded lease. Selection is exact selector, existing binding, preferred account, then a recently verified usable candidate during model quota cooldown. A deleted/unusable bound account fails visibly; it does not silently change signature authority.
3. Allow one alternate only after verified exhaustion, before streaming and without tool history or an exact selector. All quota probes and candidate credential loads share a five-second deadline. Successful alternate selection persists across turns; later ACTIVE changes or quota-cache expiry do not move an existing binding.
4. Keep signature and account continuity together. Explicit IDs are supported; headerless tool clients use opaque call handles verified against profile/model/session/function/argument digest. The original upstream call ID and Gemini step position are restored on replay. Continuity state is persisted under the managed router state root, so a normal router/container restart restores the same Google profile, wire call IDs, and thought signatures. Current-turn tool calls remain strict; missing records from already completed older turns do not invalidate a fresh turn.
5. Replace deletion-based sanitization with bounded, non-mutating schema transformation. Resolve local references, convert string const, preserve literal property names, and reject unsupported semantics with a local error. Unique-items enforcement is an explicit wire-compatibility exception and remains the tool executor's responsibility.
6. Return fixed error categories instead of raw provider bodies. Verify actual API handler paths with a fake HTTP/SSE upstream, including headerless clients, then obtain Kernel command receipts.

## Acceptance evidence map

| Acceptance | Implementation | Regression evidence |
|---|---|---|
| AC-1 strategy and boundaries | This document and API/security docs | Source/contract inspection |
| AC-2 independent account policy and continuity | accountpool, accounts.go, broker.go, session/replay | pool tests; API failover, auth-use, fresh snapshot, expiry, headerless and Chat tests |
| AC-3 registry and selector | openaiapi/backend.go; authenticated request context | third-provider catalog test, unsupported routes/selectors, exact-account API test |
| AC-4 schema preservation | antigravity/schema.go | local/escaped refs, const, literal property names, cyclic/external/unknown constraints, expansion limits, pre-upstream rejection |
| AC-5 safe upstream errors | antigravity/client.go | injected token/project/email absent from API responses and lifecycle logs |
| AC-6 integration and build | complete Go package suite | Kernel-executed go:test, go:vet, go:build requested at final settlement |

The Windows test suite previously asserted POSIX permission bits for credential and local-key files. The assertions now apply on Unix, while persistence/round-trip tests still run everywhere. This does not constitute Windows ACL verification. This machine has no enabled CGO compiler for the race detector; concurrency is exercised by deterministic lease tests, without claiming race-detector evidence.

## Operational limits

- Antigravity continuity is persisted at `<state-root>/antigravity/continuity.json` with atomic replacement and mode `0600` on Unix. Continuity uses a seven-day sliding idle window capped by 30-day absolute retention. Capacity pressure evicts an idle session as a unit instead of deleting one call from an otherwise valid session. The in-memory account lease and quota observations remain bounded; quota observations expire after one minute.
- Headerless plain-text turns without tool history are independent. Clients requiring affinity there must send a unique conversation ID. Shared prompt-cache template keys are unsuitable conversation IDs.
- No live Google inference, credential rotation, service restart, deployment, commit, or push is part of this change. Fake-upstream tests are not credentialed live acceptance.
- The schema adapter uses a documented bounded subset. Non-string enum/const, unsupported composition, and invalid references fail explicitly rather than losing meaning.

The initial Kernel successor was blocked because its September 9 final workspace fingerprint differed from the current checkout. On explicit user recovery instruction, the installed workspace-recovery contract acknowledged the observed current fingerprint and issued a fresh run without rewriting old completed evidence or modifying user files. Product work began only after `next` issued the allowed work unit. Runtime-specific IDs and completion receipts remain in the Kernel runtime, not in repository source.
