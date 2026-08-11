# Usage reference

## CLI wrapper

Run a supported client directly. Arguments after the executable are passed to
the child unchanged.

```bash
pxpipe claude --model opus
pxpipe Claude.app
PXPIPE_MODELS=gpt-5.6-sol pxpipe opencode --model openai/gpt-5.6-sol
PXPIPE_MODELS=gpt-5.6-sol pxpipe codex --model gpt-5.6-sol
pxpipe another-cli --its-child-flag
```

| Client | Transformed request paths | Notes |
|---|---|---|
| Claude Code and Claude Desktop | Paths ending in `/messages` | Preserves HTTP(S) `ANTHROPIC_BASE_URL` overrides |
| OpenCode | Paths ending in `/messages`, `/chat/completions`, or `/responses` | Disables the experimental WebSocket transport |
| Codex | Paths ending in `/responses` | The provider must set `supports_websockets=false` |
| Other executables | All three path forms | Uses the protocol-neutral profile |

Use `--` when the executable is literally named `serve` or `help`:

```bash
pxpipe -- <executable> [args...]
```

### Process and network scope

- Listeners use kernel-assigned `127.0.0.1` ports.
- The child retains its terminal streams and exit status.
- Proxy and CA environment variables apply only to the child. The system trust
  store is not modified.
- Matching HTTPS requests are decrypted with a local CA so pxpipe can inspect
  the path and body. Other requests retain their original destination.
- Provider domains, schemes, authorities, paths, and queries are preserved, so
  custom base URLs and localhost intermediaries continue to work.
- Anthropic base URLs normally omit `/v1`; Claude appends `/v1/messages`.
- If `ANTHROPIC_UNIX_SOCKET` is already set, pxpipe gives the child a separate
  process-scoped Unix socket and relays to the original socket without exposing
  its path to the child.

When the child exits, pxpipe writes the estimated input usage without the
transform, the provider-reported usage with the transform, and the percentage
change to stderr. It prints `token usage unavailable` when the response lacks
enough usage data.

Launcher startup and process-scoped CA/proxy injection have been smoke-tested
with installed Claude Code, OpenCode, and Codex releases. Local Codex routing,
transformation, and upstream restoration are integration-tested; live OpenCode
and Codex inference still need provider end-to-end validation.

### Claude Desktop

On macOS, `Claude.app` is matched case-insensitively and searched for in
`~/Applications` and `/Applications`. Its bundle executable receives
process-scoped `ANTHROPIC_UNIX_SOCKET`, `NODE_EXTRA_CA_CERTS`, and
`SSL_CERT_FILE` values. Fully quit a running Claude process before launching it
through pxpipe so it inherits the new environment.

## Standalone proxy

Start a loopback reverse/forward proxy:

```bash
pxpipe serve
pxpipe serve --port 8080
```

The default port is `47821`. The startup view prints ready-to-run Claude and
Codex commands. Select `[copy]` in a terminal to copy a complete command over
OSC 52.

Set `ANTHROPIC_BASE_URL` for Claude:

```bash
ANTHROPIC_BASE_URL=http://localhost:47821 claude
```

Use the generated Codex command as printed. It sets the generated CA path,
uppercase and lowercase proxy variables, and clears `NO_PROXY` and `no_proxy`.
It does not replace `model_provider` or the provider base URL, so ChatGPT login,
OpenAI API keys, custom provider credentials, and localhost intermediaries keep
their original destination and authorization. The active provider must set
`supports_websockets=false`.

The terminal view shows the 20 most recent requests with status, endpoint,
model, text/image mode, cache hits, and token estimates. Anthropic usage weights
cache reads at `0.1x` and cache creation at `1.25x`. OpenAI token columns remain
`-` until OpenAI response usage parsing is available. Redirected output emits
one row per request instead of redrawing the view.

## Go reverse proxy

`NewHandler` returns an `http.Handler`. If no upstreams are provided, it uses
`https://api.anthropic.com` and `https://api.openai.com`.

```go
package main

import (
	"log"
	"net/http"

	pxpipe "github.com/evan-choi/pxpipe-go"
)

func main() {
	handler := pxpipe.NewHandler(pxpipe.HandlerOptions{
		OnResult: func(r *http.Request, result *pxpipe.TransformResult) {
			log.Printf("%s applied=%v reason=%s images=%d",
				r.URL.Path, result.Applied, result.Reason, result.Info.ImageCount)
		},
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:47821", handler))
}
```

### Default routing

| Request | Destination |
|---|---|
| `/v1/messages` | `AnthropicUpstream` |
| `/v1/chat/completions`, `/v1/responses`, and `/v1/responses/*` | `OpenAIUpstream` |
| `/v1/models` | Selected from the inbound authentication style |
| `/anthropic/*`, `/openai/*`, `/google-ai-studio/*`, and `/compat/*` | Full path forwarded to `AnthropicUpstream` |

Supported POST bodies are transformed according to their wire shape.
`count_tokens`, unknown routes, all responses, and SSE streams pass through
unchanged.

Set `APIKey`, `AuthToken`, or `AuthTokenFunc` for Anthropic credentials and
`OpenAIAPIKey` for OpenAI. Direct OpenAI requests never receive `x-api-key` or
`anthropic-*` headers. An embedding server that injects provider credentials is
responsible for ingress authentication.

The handler has these default guards:

- Response header timeout: 5 minutes
- Stream idle timeout: 2 minutes
- Identical-request hold: 1 minute

Set `UpstreamHeadersTimeout`, `UpstreamIdleTimeout`, or `DuplicateHold` to a
duration pointer to change a guard. A pointer to zero disables that guard.
`TransformFunc` supplies live transform options per request and takes
precedence over the static `Transform` value. `OnResponseComplete` runs after a
response body reaches a clean EOF and includes Anthropic JSON/SSE usage when
available.

A turn containing only `@pxpipe pin` or `@pxpipe unpin` is answered locally in
the caller's Anthropic Messages, Chat Completions, or Responses format. Set
`x-pxpipe-bypass: 1` to forward it instead.

### Custom routes

`ProtocolOf` selects the request-body protocol. `RewritePath` selects the
outbound API path.

```go
handler := pxpipe.NewHandler(pxpipe.HandlerOptions{
	ProtocolOf: func(path string) pxpipe.Protocol {
		switch path {
		case "/api/llm/claude":
			return pxpipe.ProtocolAnthropicMessages
		case "/api/llm/gpt/chat":
			return pxpipe.ProtocolOpenAIChat
		case "/api/llm/gpt/responses":
			return pxpipe.ProtocolOpenAIResponses
		default:
			return pxpipe.DefaultProtocolOf(path)
		}
	},
	RewritePath: func(path string, _ pxpipe.Protocol) string {
		switch path {
		case "/api/llm/claude":
			return "/v1/messages"
		case "/api/llm/gpt/chat":
			return "/v1/chat/completions"
		case "/api/llm/gpt/responses":
			return "/v1/responses"
		default:
			return path
		}
	},
})
```

`UpstreamFor` may return an HTTP(S) upstream for each request and protocol. A
non-nil result takes precedence over `AnthropicUpstream` and `OpenAIUpstream`.

## Pure transforms

Transform an Anthropic Messages body with model gating:

```go
result := pxpipe.TransformAnthropicMessages(pxpipe.TransformInput{
	Body:  bodyBytes,
	Model: "claude-fable-5",
})
if result.Applied {
	forward(result.Body)
} else {
	forward(bodyBytes) // result.Reason records why it was not applied.
}
```

Use the matching function for an OpenAI request body:

```go
body, info := pxpipe.TransformOpenAIChatCompletions(bodyBytes, nil)
body, info = pxpipe.TransformOpenAIResponses(bodyBytes, nil)
```

Render arbitrary text with the same model-specific geometry and style:

```go
output, err := pxpipe.RenderTextToImages(text, pxpipe.RenderOptions{
	Model:  "gpt-5.6-sol",
	Reflow: true,
})
```

## Model scope and environment

The library and CLI wrapper have a built-in allowlist of `claude-fable-5` and
`gemini-3.6-flash`. Opt GPT models in through `PXPIPE_MODELS` or
`pxpipe.SetAllowedModelBases`. Models outside the list pass through without
byte changes.

Standalone `pxpipe serve` accepts every valid Anthropic and OpenAI model when
`PXPIPE_MODELS` is unset or blank. Set it to restrict the server to an explicit
list.

| Environment variable | Purpose |
|---|---|
| `PXPIPE_MODELS` | CSV list of allowed model base IDs |
| `PXPIPE_GPT_PROFILES` | JSON profiles for GPT-compatible models not in the built-in table |
| `PXPIPE_RENDER_CACHE_BYTES` | Exact render-input cache limit; defaults to `64 MiB`, and `0` disables it |

The `PXPIPE_GPT_PROFILES` schema matches upstream. Call
`pxpipe.ResolveGptProfile` to inspect the resolved runtime profile.
