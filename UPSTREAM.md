# Upstream port status

The TypeScript reference implementation is pinned as the `pxpipe/` submodule.
Initialize it with:

```bash
git submodule update --init --recursive
```

## Revisions

- Last verified Go port baseline: `508fc9dd1c1ee4d75473fcb8e1be885da07b0776`
  (2026-08-04)
- Current reference submodule: `a9b975917719cd6436b8e7e952aa32b611ec9d31`
  (2026-08-06)

The baseline is derived from source and golden-fixture parity, the initial Go
port history, and the benchmark reference recorded in `README.md`. Future
ports should update the baseline only after equivalent behavior and fixtures
are verified.

## Pending runtime ports

The following upstream changes after the baseline are not yet represented in
the Go implementation:

- `2a071a8`: exclude the per-turn Anthropic billing header from imaged slab
  content regardless of its line position.
- `6654d40`: enforce the shared 100-image Anthropic wire limit, including
  caller-provided and nested tool-result images.
- `6654d40`: adapt history image budgets and freeze grids, and batch oversized
  historical user prompts.
- `6654d40`: feed provider cache accounting into sticky per-session history
  planning.
- `6654d40`: hash the synthetic history message rather than assuming it is the
  first transformed message.
- `6654d40`: expose component-level cache-prefix and image-budget diagnostics.

Security policy, CI, dashboard, Worker, and offline-export changes in the same
upstream range do not apply to this library port.
