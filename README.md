# pxpipe-go

A Go port of the core of [pxpipe](https://github.com/teamchong/pxpipe). It
converts large system prompts, tool definitions, earlier conversation turns,
and tool results into dense PNG pages when the vision input is estimated to
cost fewer tokens than the source text.

The transform supports Anthropic Messages, OpenAI Chat Completions, and OpenAI
Responses request bodies. The Gemini/Google API surface, upstream dashboard,
and offline export CLI are not included.

## Interfaces

| Interface | Entry point | Use case |
|---|---|---|
| CLI wrapper | `pxpipe <executable>` | Run Claude Code, Claude Desktop, OpenCode, or Codex through a process-scoped proxy |
| Standalone proxy | `pxpipe serve` | Connect an existing client to a loopback reverse/forward proxy |
| Go package | `pxpipe.NewHandler` and transform functions | Embed the proxy or request transform in a Go server |

## Installation

pxpipe-go requires Go 1.26.3 or later.

```bash
go install github.com/evan-choi/pxpipe-go/cmd/pxpipe@v0.4.19
pxpipe --version
```

`go install` writes the binary to `go env GOBIN`, or to
`$(go env GOPATH)/bin` when `GOBIN` is unset. Add that directory to `PATH` if
needed.

Add the Go package to a module with:

```bash
go get github.com/evan-choi/pxpipe-go@v0.4.19
```

## Quick start

### CLI wrapper

```bash
pxpipe claude --model opus
pxpipe Claude.app
PXPIPE_MODELS=gpt-5.6-sol pxpipe opencode --model openai/gpt-5.6-sol
PXPIPE_MODELS=gpt-5.6-sol pxpipe codex --model gpt-5.6-sol
```

Arguments after the executable are passed to the child unchanged. Proxy and
local CA settings apply only to the child; pxpipe does not modify the system
trust store. A Codex provider must set `supports_websockets=false` so request
bodies use HTTP and can be transformed.

### Standalone proxy

```bash
pxpipe serve
pxpipe serve --port 8080
```

The default port is `47821`. The startup view prints ready-to-run Claude and
Codex commands. The Codex command includes the generated CA path and proxy
environment.

```bash
ANTHROPIC_BASE_URL=http://localhost:47821 claude
```

### Go reverse proxy

`NewHandler` forwards to the public Anthropic and OpenAI APIs by default.

```go
package main

import (
	"log"
	"net/http"

	pxpipe "github.com/evan-choi/pxpipe-go"
)

func main() {
	handler := pxpipe.NewHandler(pxpipe.HandlerOptions{})
	log.Fatal(http.ListenAndServe("127.0.0.1:47821", handler))
}
```

Point Claude Code at the handler:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:47821 claude
```

See the [usage reference](docs/usage.md) for custom upstreams, route mapping,
credential injection, and pure transforms.

## Model configuration

| Setting | Behavior |
|---|---|
| `PXPIPE_MODELS` | CSV list of model base IDs to transform; models outside the list pass through unchanged |
| `PXPIPE_GPT_PROFILES` | JSON profiles for GPT-compatible models not in the built-in table |
| `PXPIPE_RENDER_CACHE_BYTES` | Render cache capacity; defaults to `64 MiB`, and `0` disables it |

The library and CLI wrapper have a built-in allowlist of `claude-fable-5` and
`gemini-3.6-flash`; GPT models are opt-in. `pxpipe serve` accepts every valid
Anthropic and OpenAI model when `PXPIPE_MODELS` is empty.

## Documentation

- [Usage reference](docs/usage.md): CLI profiles, standalone proxy, Go handler,
  and pure transforms
- [Performance and compatibility](docs/performance.md): golden parity,
  benchmarks, and reproduction commands
- [Upstream port status](UPSTREAM.md): pinned revision, port scope, and fixture
  verification

## Performance

![pxpipe versus pxpipe-go benchmark: pxpipe-go is 72.6 to 282.2 times faster across four workloads and uses 73.4% less peak RSS](docs/benchmark-improvements.png)

On an Apple M1 Pro, the four measured workloads ran 72.6–282.2 times faster
than the TypeScript implementation, while peak RSS for the full suite was
73.4% lower. See [performance and compatibility](docs/performance.md) for the
measurement conditions.

## Verification

Golden fixtures come only from the pinned TypeScript implementation. Tests
compare text, gate decisions, page geometry, and decoded pixels.

```bash
go test ./...
```

## License

[MIT](LICENSE). Portions derived from
[teamchong/pxpipe](https://github.com/teamchong/pxpipe) (MIT).
