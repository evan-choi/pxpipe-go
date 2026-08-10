# Upstream port status

The TypeScript reference implementation is pinned as the `pxpipe/` submodule.
Initialize it with:

```bash
git submodule update --init --recursive
```

## Revisions

- Last verified Go port baseline: `c5fc2a8f3b2864cf72aa1aa79a7e9e72a04ff37a`
  (2026-08-09)
- Current reference submodule: `c5fc2a8f3b2864cf72aa1aa79a7e9e72a04ff37a`
  (2026-08-09)
- Upstream `origin/main` last checked: 2026-08-11; no newer commit.

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

## Ported after a9b9759

- `3556db5`: PNG scanline filtering is applied without changing decoded
  pixels. The Go encoder selects the smaller filter result for narrow
  grayscale pages and keeps the measured fast path for wide pages.
- `3556db5` and `2053cc2`: rendered pages are cached by exact render inputs,
  with the retained bytes bounded by `PXPIPE_RENDER_CACHE_BYTES`.
- `c3f553c`: history collapses before tool-result imaging, preserving original
  tool-result text in the history image.
- `2cad7bf`: `total_tokens` blocks are classified as dynamic.
- `f677330`: transformable request bodies are read through a 16 MiB limit and
  rejected with protocol-specific 413 responses before transformation.
- `dbdb6fc`: OpenAI routing uses an explicit credential policy. Anthropic
  credentials are never forwarded to OpenAI, ChatGPT OAuth JWTs are preserved,
  and configured OpenAI keys replace remaining bearer credentials.
- `cb80dcc`: caller and generated images share an 18 MiB decoded-image budget.
  The corresponding byte telemetry is included in golden comparisons.
- `25d1e7c` and `5865e8e`: the per-turn billing line is removed from cacheable
  body content and forwarded as `X-Anthropic-Billing-Header`.
- `74d21ca`: non-Fable Claude history uses the legible font profile. The Go
  planner derives page capacity from the selected font and height so rendered
  history remains inside the 100-image wire limit.
- `6ddda91`: invalid `charsPerToken` values fall back to the calibrated value
  of 3. The regular transform option default remains 4, matching upstream.

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

## Verification after a9b9759

Golden data must be regenerated only by running the TypeScript implementation
from `pxpipe@c5fc2a8` with Bun:

```bash
cd pxpipe
pnpm install --frozen-lockfile
bun run ../tools/gen-fixtures.ts
bun run ../tools/gen-fixtures-openai.ts
bun run ../tools/dump-gpt-profiles.ts > ../testdata/openai/profiles.json
```

Do not generate fixtures from the Go port or invoke the TypeScript generators
through `tsx`.

The fixture generator omits raw `imagePngs` and `firstImagePng` telemetry from
`info.json`; rendered fixture PNGs and provider-facing JSON remain direct
upstream outputs. Go tests compare decoded PNG pixels because Bun and Go use
different deflate implementations.

The new behavior maps to these Go checks:

- PNG filtering and lossless output: `render/png_simd_test.go` and
  `render/golden_test.go`.
- Render-cache keys, isolation, eviction, and concurrent hits:
  `render/cache_test.go`.
- Transform order, dynamic tags, billing headers, and history geometry:
  `transform_upstream_test.go`, `handler_test.go`, and
  `upstream_history_image_parity_test.go`.
- Credential routing and request limits: `handler_routing_test.go` and
  `handler_test.go`.
- Decoded image budgets: `image_byte_budget_test.go`.
- TypeScript output parity: `golden_transform_test.go` and
  `golden_openai_test.go`.

## Not ported after a9b9759

- Node request-timing telemetry and Node response scanning from `3556db5` and
  `67b2866` do not apply to the Go library API.
- The Node restart scripts, process launcher changes, and Node version target
  from `462ffd4`, `f22fbdb`, and `27ef83a` do not apply.
- Eval harness changes, the offline `cpt-fit.ts` analyzer, dashboard work, and
  offline CLI stats are not part of the Go runtime port.
- Node dependency updates, release metadata, community routing documentation,
  `CONTRIBUTING.md`, and the pull-request template have no Go counterpart.
