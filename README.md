# pxpipe-go

Go port of the core of [pxpipe](https://github.com/teamchong/pxpipe): a
token-saving transform for the Anthropic Messages API that renders bulky
context (system prompt, tool docs, old history, large tool results) into dense
PNG pages, cutting input tokens by reading them through the vision channel.

This port is a **library**: embed it into your own Go server either as an
`http.Handler` reverse proxy or as pure functions over request bodies. It
covers the Anthropic Messages surface **and** the OpenAI Chat Completions /
Responses surfaces (the GPT path, including the o200k-exact profitability
gate and GPT history imaging). It deliberately omits the TS project's
dashboard, savings measurement, `warp` MITM mode, offline export CLI, and the
Gemini/Google surface.

## Install

```bash
go get github.com/evan-choi/pxpipe-go
```

## Use as an embedded reverse proxy

```go
package main

import (
    "log"
    "net/http"
    "net/url"

    pxpipe "github.com/evan-choi/pxpipe-go"
)

func main() {
    anthropic, _ := url.Parse("https://api.anthropic.com")
    openAI, _ := url.Parse("https://api.openai.com")
    h := pxpipe.NewHandler(pxpipe.HandlerOptions{
        AnthropicUpstream: anthropic,
        OpenAIUpstream:    openAI,
        OnResult: func(r *http.Request, res *pxpipe.TransformResult) {
            log.Printf("%s applied=%v reason=%s images=%d",
                r.URL.Path, res.Applied, res.Reason, res.Info.ImageCount)
        },
    })
    log.Fatal(http.ListenAndServe("127.0.0.1:47821", h))
}
```

Then point Claude Code at it:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:47821 claude
```

Canonical `/v1/messages` requests go to `AnthropicUpstream`; canonical
`/v1/chat/completions`, `/v1/responses`, and `/v1/responses/*` requests go to
`OpenAIUpstream`. `/v1/models` uses the request's auth style to choose between
them. Both upstreams have the public API defaults shown above.

Provider-prefixed paths (`/anthropic/*`, `/openai/*`,
`/google-ai-studio/*`, `/compat/*`) keep their full path and go to
`AnthropicUpstream`, which lets an API gateway route them. Supported POST
bodies are still transformed by wire shape; `count_tokens`, unknown routes,
and all responses (including SSE) pass through unchanged.

Set `APIKey` or `AuthToken`/`AuthTokenFunc` for Anthropic credentials and
`OpenAIAPIKey` for OpenAI. Direct OpenAI requests never receive `x-api-key` or
`anthropic-*` headers.

The handler defaults to a 5-minute response-header timeout, a 2-minute stream
idle timeout, and a 1-minute identical-request hold. Set
`UpstreamHeadersTimeout`, `UpstreamIdleTimeout`, or `DuplicateHold` to a
duration pointer; a pointer to zero disables that guard. `TransformFunc` can
return live transform options per request and takes precedence over the static
`Transform` value.

A turn containing only `@pxpipe pin` or `@pxpipe unpin` is answered locally in
the caller's Anthropic Messages, Chat Completions, or Responses wire format.
Set `x-pxpipe-bypass: 1` to forward it instead.

### Custom routes

Set `ProtocolOf` to map your own paths to wire protocols; return
`pxpipe.ProtocolNone` to pass a request through, or fall back to
`pxpipe.DefaultProtocolOf` for the built-in rules:

```go
h := pxpipe.NewHandler(pxpipe.HandlerOptions{
    AnthropicUpstream: anthropic,
    OpenAIUpstream:    openAI,
    ProtocolOf: func(path string) pxpipe.Protocol {
        switch path {
        case "/api/llm/claude":
            return pxpipe.ProtocolAnthropicMessages
        case "/api/llm/gpt/chat":
            return pxpipe.ProtocolOpenAIChat
        case "/api/llm/gpt/responses":
            return pxpipe.ProtocolOpenAIResponses
        }
        return pxpipe.DefaultProtocolOf(path)
    },
    RewritePath: func(path string, _ pxpipe.Protocol) string {
        switch path {
        case "/api/llm/claude":
            return "/v1/messages"
        case "/api/llm/gpt/chat":
            return "/v1/chat/completions"
        case "/api/llm/gpt/responses":
            return "/v1/responses"
        }
        return path
    },
})
```

`ProtocolOf` chooses the request-body transform. `RewritePath` chooses the
outbound API path, which also controls direct Anthropic/OpenAI routing.

## Use as a pure transform

```go
res := pxpipe.TransformAnthropicMessages(pxpipe.TransformInput{
    Body:  bodyBytes,          // the JSON request body
    Model: "claude-fable-5",  // resolved model id (gates applicability)
})
if res.Applied {
    forward(res.Body)
} else {
    forward(bodyBytes) // res.Reason explains why (below_min_chars, …)
}
```

For the OpenAI surfaces:

```go
body, info := pxpipe.TransformOpenAIChatCompletions(bodyBytes, nil)
body, info  = pxpipe.TransformOpenAIResponses(bodyBytes, nil)
```

Per-model GPT render/pricing profiles (gpt-5.x, o-series, Grok, …) resolve via
`pxpipe.ResolveGptProfile`; unknown models can be declared without a code
change through the `PXPIPE_GPT_PROFILES` env JSON, exactly as upstream.

Or render arbitrary text with the same model-specific geometry and style:

```go
out, _ := pxpipe.RenderTextToImages(text, pxpipe.RenderOptions{
    Model: "gpt-5.6-sol",
    Reflow: true,
})
for _, page := range out.Pages { os.WriteFile("page.png", page.PNG, 0o644) }
```

## Model scope

Same semantics as upstream pxpipe: the built-in allowlist is
`claude-fable-5` plus `gemini-3.6-flash`; GPT models are opt-in. Override with
the `PXPIPE_MODELS` env CSV (e.g. `PXPIPE_MODELS=claude-fable-5,gpt-5.6-sol`)
or at runtime via `pxpipe.SetAllowedModelBases`. Unlisted models pass through
untransformed — that is the escape hatch for byte-exact work.

## Fidelity

The port is verified against golden fixtures generated by the TypeScript
reference implementation (`testdata/`, tooling in `tools/`):

- 17 render cases pass **pixel-exact** (PNG bytes differ only by deflate
  implementation; decoded pixels are identical).
- 9 Anthropic transform cases match on every text output, hash, gate
  verdict, page count, and page dimensions; image blocks are compared by
  decoded pixels.
- 15 OpenAI (Chat + Responses) cases match on every text output, hash, gate
  verdict (including exact o200k token counts), history-collapse plan, page
  count, and page dimensions; the embedded o200k tokenizer reproduces
  gpt-tokenizer counts exactly.
- GPT model-profile resolution (27 ids incl. env overrides and misresolution
  guards) matches the TS table verbatim.
- TS UTF-16 string-length semantics are reproduced for every char gate and
  telemetry counter; JSON object key order is preserved through parse →
  imaged-text serialization so rendered tool schemas match the reference
  byte-for-byte.

Known deviations:

- PNG byte streams (and therefore `imageBytes`, `historyImageSha`,
  `cachePrefixSha8`) differ from TS — deflate output is implementation
  specific. They remain deterministic per input, which is what prompt-cache
  stability requires.
- Keys *added* by the transform to objects it did not create serialize after
  the object's original keys in sorted order (TS appends in insertion order).
- No Gemini/Google surface, Messages↔OpenAI bridges, or savings
  measurement.

## Performance

![pxpipe versus pxpipe-go benchmark: pxpipe-go is 7.2 to 46.4 times faster across four workloads and uses 59.2% less peak RSS](docs/benchmark-improvements.png)

Measured on an Apple M1 Pro with Node 26.5.0 and Go 1.26.3. Values are medians
of five runs with three timed iterations per run, comparing `pxpipe@508fc9d`
with `pxpipe-go@406cce7` (2026-08-06). Actual latency varies with CPU
architecture and request content.

| benchmark | pxpipe time/op | pxpipe-go time/op | speedup |
|---|---:|---:|---:|
| TransformBigClaudeCode | 156.20 ms | 21.66 ms | 7.2× |
| RenderDensePage | 391.20 ms | 8.43 ms | 46.4× |
| TransformOpenAIChat | 119.10 ms | 7.77 ms | 15.3× |
| TransformOpenAIResponses | 640.00 ms | 44.15 ms | 14.5× |

Peak RSS was measured over the full four-benchmark suite in a fresh process
for each run:

| implementation | median peak RSS | relative to pxpipe |
|---|---:|---:|
| pxpipe | 305.22 MiB | baseline |
| pxpipe-go | 124.66 MiB | 59.2% lower |

Go's `-benchmem` output reports the following allocation volume per operation:

| benchmark | B/op | allocs/op |
|---|---:|---:|
| TransformBigClaudeCode | 4,896,602 | 3,855 |
| RenderDensePage | 803,208 | 73 |
| TransformOpenAIChat | 1,243,930 | 1,779 |
| TransformOpenAIResponses | 3,436,256 | 2,351 |

Raw GC counts are not compared because V8 and Go use different collectors and
event semantics. Peak RSS is the cross-runtime memory metric; `B/op` and
`allocs/op` identify allocation pressure within the Go implementation.

Reproduce the comparison with:

```bash
for benchmark_run in 1 2 3 4 5; do
  (cd ../pxpipe && pnpm exec tsx ../pxpipe-go/tools/bench-ts.ts)
done

go test -run '^$' \
  -bench '^(BenchmarkTransformBigClaudeCode|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses)$' \
  -benchtime=3x -benchmem -count=5 .
```

On macOS, reproduce peak RSS by running each suite in a fresh process five
times and taking the median `maximum resident set size` value (reported in
bytes). Build the Go benchmark binary first so the measurement excludes the
compiler and `go` command:

```bash
for benchmark_run in 1 2 3 4 5; do
  (cd ../pxpipe &&
    /usr/bin/time -l pnpm exec tsx ../pxpipe-go/tools/bench-ts.ts >/dev/null)
done

go test -c -o /tmp/pxpipe-go-bench.test .
for benchmark_run in 1 2 3 4 5; do
  /usr/bin/time -l /tmp/pxpipe-go-bench.test -test.run '^$' \
    -test.bench '^(BenchmarkTransformBigClaudeCode|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses)$' \
    -test.benchtime=3x -test.count=1 >/dev/null
done
```

Linux ARM64 profiles show PNG rendering and zlib as the largest remaining CPU
cost. The renderer uses zlib level 6; framebuffers and encoders are pooled.

## Regenerating fixtures

Requires the TS repo checked out as a sibling `../pxpipe`:

```bash
cd ../pxpipe
pnpm exec tsx ../pxpipe-go/tools/dump-atlas.ts
pnpm exec tsx ../pxpipe-go/tools/gen-fixtures.ts
pnpm exec tsx ../pxpipe-go/tools/gen-fixtures-openai.ts
pnpm exec tsx ../pxpipe-go/tools/dump-gpt-profiles.ts > ../pxpipe-go/testdata/openai/profiles.json
```

## License

MIT. Portions derived from [teamchong/pxpipe](https://github.com/teamchong/pxpipe) (MIT).
