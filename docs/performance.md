# Performance and compatibility

## TypeScript reference parity

Golden fixtures are generated only by the pinned TypeScript implementation in
`pxpipe/`.

- Decoded pixels match in 17 render cases.
- Text, hashes, gate decisions, page counts, and page dimensions match in 9
  Anthropic transform cases. Image blocks are compared by decoded pixels.
- Text, hashes, exact o200k gate decisions, history-collapse plans, page counts,
  and page dimensions match in 15 OpenAI Chat and Responses cases. The embedded
  tokenizer reproduces gpt-tokenizer o200k counts exactly.
- The TypeScript table matches for 27 GPT model-profile IDs, environment
  overrides, and misresolution guards.
- TypeScript UTF-16 string-length semantics and JSON object key order are
  reproduced for character gates, telemetry counters, and rendered tool
  schemas.

### Known differences

- Go and Bun use different deflate implementations, so PNG byte streams and
  the resulting `imageBytes`, `historyImageSha`, and `cachePrefixSha8` values
  differ. Decoded pixels match, and output is deterministic for a given runtime
  and input.
- Keys added to an existing object by the transform are sorted after its
  original keys. TypeScript appends them in insertion order.
- The Gemini/Google surface, Messages-to-OpenAI bridges, and savings
  measurement are not included.

The pinned revision and test mapping are recorded in
[UPSTREAM.md](../UPSTREAM.md).

## Benchmark snapshot

Measurements used an Apple M1 Pro running macOS 26.5.2, Bun 1.3.14, and Go
1.26.5. Go used the machine-default `GOMAXPROCS=10` with PGO disabled. The
compared revisions were `pxpipe@c5fc2a8` and `pxpipe-go@c3e0e50`; measurements
were taken on 2026-08-11.

Values are medians of five runs. Each pxpipe run performs two warmups and three
timed iterations, then reports their mean. Each pxpipe-go run uses a one-second
benchmark window. Actual latency varies with CPU architecture, available cores,
and request content.

| Benchmark | pxpipe time/op | pxpipe-go time/op | Speedup |
|---|---:|---:|---:|
| TransformBigClaudeCode | 52.20 ms | 0.72 ms | 72.6× |
| RenderDensePage | 12.50 ms | 0.14 ms | 91.7× |
| TransformOpenAIChat | 52.40 ms | 0.28 ms | 190.5× |
| TransformOpenAIResponses | 132.50 ms | 0.47 ms | 282.2× |

Peak RSS was measured by running the full four-benchmark suite in a fresh
process for each implementation.

| Implementation | Median peak RSS | Relative to pxpipe |
|---|---:|---:|
| pxpipe | 405.00 MiB | baseline |
| pxpipe-go | 107.70 MiB | 73.4% lower |

Go `-benchmem` results:

| Benchmark | B/op | allocs/op |
|---|---:|---:|
| TransformBigClaudeCode | 1,165,232 | 1,813 |
| RenderDensePage | 2,736 | 25 |
| TransformOpenAIChat | 633,320 | 993 |
| TransformOpenAIResponses | 1,069,497 | 1,043 |

Raw GC counts are not compared because V8 and Go use different collectors and
event semantics. Peak RSS is the cross-runtime memory metric; `B/op` and
`allocs/op` describe allocation pressure within Go.

## Reproduce on macOS

Install the submodule dependencies, then run the latency and allocation
comparison from the repository root:

```bash
for benchmark_run in 1 2 3 4 5; do
  (cd pxpipe && bun run ../tools/bench-ts.ts)
done

GOTOOLCHAIN=go1.26.5 go test -pgo=off -run '^$' \
  -bench '^(BenchmarkTransformBigClaudeCode|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses)$' \
  -benchtime=1s -benchmem -count=5 .
```

Measure peak RSS with `maximum resident set size` from macOS
`/usr/bin/time -l`. Build the Go benchmark binary first so the measurement
excludes the compiler and `go` command:

```bash
for benchmark_run in 1 2 3 4 5; do
  (cd pxpipe &&
    /usr/bin/time -l bun run ../tools/bench-ts.ts >/dev/null)
done

GOTOOLCHAIN=go1.26.5 go test -pgo=off \
  -c -o /tmp/pxpipe-go-bench.test .
for benchmark_run in 1 2 3 4 5; do
  /usr/bin/time -l /tmp/pxpipe-go-bench.test -test.run '^$' \
    -test.bench '^(BenchmarkTransformBigClaudeCode|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses)$' \
    -test.benchtime=1s -test.count=1 >/dev/null
done
```

The current macOS hot-cache profile puts the cache-prefix hit path at 0.6% of
CPU, with negligible mutex contention. The optimized high-cardinality Responses
path fell from 12.04 ms/op to 7.02 ms/op. Level-6 PNG compression accounts for
58.0% of CPU and uncached o200k counting for 7.2%.

## Production Linux validation

In a Docker Desktop Alpine ARM64 A/B run, `GOGC=400` with
`GOMEMLIMIT=192MiB` was 5.6% faster by parallel geomean than the default GC
settings and used about 219 MiB of peak cgroup memory. Remeasure this candidate
on the production EKS instance family before using it as a deployment setting.

Run the native Linux harness in an Alpine pod on the same instance family and
architecture as production. It rejects macOS, non-Alpine environments,
architecture mismatches, Docker Desktop, LinuxKit, and WSL.

```bash
BENCH_GOMAXPROCS=4 tools/bench-linux.sh /results/current
BENCH_GOMAXPROCS=4 BENCH_PGO=1 tools/bench-linux.sh /results/current-pgo
```

Set `BENCH_GOMAXPROCS` to the pod's integer CPU limit. Run the first command
from baseline and candidate checkouts, then compare `sequential.txt` and
`parallel.txt` with `benchstat`. PGO profiles depend on the workload and
architecture; do not deploy a profile trained on a developer machine or an
emulated CPU.

## Regenerate fixtures

Initialize the pinned TypeScript reference and run every generator with Bun:

```bash
git submodule update --init --recursive
cd pxpipe
pnpm install --frozen-lockfile
bun run ../tools/dump-atlas.ts
bun run ../tools/gen-fixtures.ts
bun run ../tools/gen-fixtures-openai.ts
bun run ../tools/dump-gpt-profiles.ts > ../testdata/openai/profiles.json
```

Do not generate golden data from the Go port or run these generators through
`tsx`.
