#!/bin/sh

set -eu

usage() {
  printf '%s\n' "usage: BENCH_GOMAXPROCS=N tools/bench-linux.sh OUTPUT_DIR" >&2
  exit 2
}

[ "$#" -eq 1 ] || usage
[ "$(uname -s)" = "Linux" ] || {
  printf '%s\n' "error: benchmarks must run on native Linux" >&2
  exit 1
}
[ -f /etc/alpine-release ] || {
  printf '%s\n' "error: benchmarks must run in Alpine Linux" >&2
  exit 1
}
case "$(uname -a)" in
  *[Ll]inuxkit*|*[Mm]icrosoft*)
    [ "${BENCH_ALLOW_DEV_VM:-0}" = "1" ] || {
      printf '%s\n' "error: Docker Desktop, LinuxKit, and WSL are not production benchmark hosts" >&2
      exit 1
    }
    ;;
esac
[ -n "${BENCH_GOMAXPROCS:-}" ] || {
  printf '%s\n' "error: BENCH_GOMAXPROCS must match the pod CPU limit" >&2
  exit 1
}

case "$BENCH_GOMAXPROCS" in
  *[!0-9]*|'') usage ;;
esac
[ "$BENCH_GOMAXPROCS" -gt 0 ] || usage

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=$1
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
cd "$root"

goarch=$(go env GOARCH)
machine=$(uname -m)
case "$machine:$goarch" in
  x86_64:amd64|aarch64:arm64) ;;
  *)
    printf '%s\n' "error: uname architecture $machine does not match Go architecture $goarch" >&2
    exit 1
    ;;
esac

expected_go=${BENCH_GO_VERSION:-go1.26.5}
actual_go=$(go env GOVERSION)
[ "$actual_go" = "$expected_go" ] || {
  printf '%s\n' "error: Go version $actual_go does not match $expected_go" >&2
  exit 1
}

export CGO_ENABLED=0
export GOMAXPROCS=$BENCH_GOMAXPROCS
export GOTOOLCHAIN=local

count=${BENCH_COUNT:-10}
benchtime=${BENCH_TIME:-3s}
train_time=${BENCH_PGO_TRAIN_TIME:-10s}

sequential='^(BenchmarkTransformBigClaudeCode|BenchmarkTransformAnthropicMessages|BenchmarkCachePrefixDigest|BenchmarkHistoryImageSha8|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses|BenchmarkClassifyResponsesPairs|BenchmarkAnthropicResponseScannerJSON|BenchmarkAnthropicResponseScannerSSE)$'
parallel='^(BenchmarkTransformBigClaudeCodeParallel|BenchmarkRenderDensePageParallel|BenchmarkTransformOpenAIChatParallel|BenchmarkTransformOpenAIResponsesParallel)$'
representative='^(BenchmarkTransformBigClaudeCode|BenchmarkRenderDensePage|BenchmarkTransformOpenAIChat|BenchmarkTransformOpenAIResponses|BenchmarkAnthropicResponseScannerSSE)$'

{
  date -u '+utc=%Y-%m-%dT%H:%M:%SZ'
  if command -v git >/dev/null 2>&1; then
    printf 'git_commit='
    git rev-parse HEAD
    printf 'git_dirty='
    if [ -n "$(git status --porcelain)" ]; then printf 'true\n'; else printf 'false\n'; fi
  else
    printf 'git_commit=unavailable\n'
    printf 'git_dirty=unavailable\n'
  fi
  printf 'alpine='
  cat /etc/alpine-release
  uname -a
  go version
  go env GOOS GOARCH GOAMD64 GOARM64 CGO_ENABLED GOTOOLCHAIN GOVERSION
  printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"
  printf 'GOMEMLIMIT=%s\n' "${GOMEMLIMIT:-unset}"
  printf 'GOGC=%s\n' "${GOGC:-unset}"
  printf 'BENCH_COUNT=%s\n' "$count"
  printf 'BENCH_TIME=%s\n' "$benchtime"
  printf 'BENCH_ALLOW_DEV_VM=%s\n' "${BENCH_ALLOW_DEV_VM:-0}"
} >"$output/environment.txt"

go test -mod=readonly -pgo=off -run '^$' -bench "$sequential" \
  -benchtime "$benchtime" -benchmem -count "$count" . >"$output/sequential.txt"

go test -mod=readonly -pgo=off -run '^$' -bench "$parallel" \
  -benchtime "$benchtime" -benchmem -count "$count" . >"$output/parallel.txt"

if [ "${BENCH_PGO:-0}" = "1" ]; then
  off_binary="$output/pxpipe-pgo-off.test"
  on_binary="$output/pxpipe-pgo-on.test"
  profile="$output/training.pprof"

  go test -mod=readonly -pgo=off -c -o "$off_binary" .
  "$off_binary" -test.run '^$' -test.bench "$representative" \
    -test.benchtime "$train_time" -test.count=1 -test.cpuprofile "$profile" \
    >"$output/pgo-training.txt"
  go test -mod=readonly -pgo="$profile" -c -o "$on_binary" .

  "$off_binary" -test.run '^$' -test.bench "$representative" \
    -test.benchtime "$benchtime" -test.benchmem -test.count "$count" \
    >"$output/pgo-off.txt"
  "$on_binary" -test.run '^$' -test.bench "$representative" \
    -test.benchtime "$benchtime" -test.benchmem -test.count "$count" \
    >"$output/pgo-on.txt"

  if command -v benchstat >/dev/null 2>&1; then
    benchstat "$output/pgo-off.txt" "$output/pgo-on.txt" >"$output/pgo-benchstat.txt"
  fi
  rm -f "$off_binary" "$on_binary"
fi

printf '%s\n' "benchmark results: $output"
