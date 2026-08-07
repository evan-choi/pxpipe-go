# Upstream port status

The TypeScript reference implementation is pinned as the `pxpipe/` submodule.
Initialize it with:

```bash
git submodule update --init --recursive
```

## Revisions

- Last verified Go port baseline: `a9b975917719cd6436b8e7e952aa32b611ec9d31`
  (2026-08-06)
- Current reference submodule: `a9b975917719cd6436b8e7e952aa32b611ec9d31`
  (2026-08-06)

The baseline advances only after equivalent behavior and regenerated fixtures
are verified against the pinned reference.

## Ported after 508fc9d

- `2a071a8`: per-turn Anthropic billing headers are excluded from imaged slab
  content at any line position.
- `6654d40`: the shared 100-image wire limit includes caller and nested
  tool-result images.
- `6654d40`: history uses adaptive image budgets, sticky freeze grids, cache
  accounting feedback, and batched oversized user prompts.
- `6654d40`: history image hashes select the synthetic history message.
- `6654d40`: component cache-prefix and image-budget diagnostics are exposed.

Response accounting intentionally does not mark a cache dead for transient
5xx or unrelated 4xx responses. It records successful usage only when usage
was parsed, and treats provider 413 or a size-related 400 as conclusive
rejection signals.

## Verification after 508fc9d

The upstream test changes through `a9b9759` are mapped as follows:

- `tests/render.test.ts`, `tests/history.test.ts`,
  `tests/history-grid-e2e.test.ts`, and `tests/native-image-cap.test.ts` map to
  `upstream_history_image_parity_test.go`.
- `tests/cache-bust-attribution.test.ts`, `tests/cache-liveness.test.ts`, and
  the transform telemetry behind `tests/context-map.test.ts` map to
  `upstream_cache_liveness_parity_test.go`.
- The runtime invariants from `tests/scripts/smoke-collapse.mjs` map to
  `upstream_smoke_parity_test.go`: 400-to-410-turn growth, the 100-image cap,
  the serialized-size bound, survival after an upstream 500, and an exactly
  100-image caller-saturated request.
- Regenerated Go golden fixtures verify the new telemetry and cache-prefix
  hashes without changing the fixture inputs or rendered PNG output.

Dashboard HTML, dashboard host labels, and context-map wording do not apply
because this repository has no dashboard. Cloudflare Worker caller-secret
authentication does not apply to `NewHandler`; an embedding server is
responsible for ingress authentication when it injects provider credentials.
The CLI binds its process-scoped proxy listeners to loopback.

The Node process launcher, `/health` polling, fixed-port orchestration, and log
assertions in the smoke script are deployment-specific and are not ported. Its
older assertion that a transient 500 should trigger denser repacking is also
not ported because the final cache-liveness behavior in `6654d40` keeps 5xx
responses neutral.

Security policy, CI, Worker, dashboard, and offline-export implementation
changes in the same upstream range otherwise do not apply to this library
port.
