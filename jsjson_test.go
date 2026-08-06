package pxpipe

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

var benchmarkJSONBytes []byte
var benchmarkJSONString string

func TestParseOrderedJSONByteView(t *testing.T) {
	body := []byte(`{"first":1,"second":"value"}`)
	got, err := parseOrderedJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	order := objKeyOrder(got)
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("key order = %v", order)
	}
	if _, err := parseOrderedJSON(nil); err == nil {
		t.Fatal("empty body parsed successfully")
	}
}

func TestJSStringifyObjectOrderAndExtras(t *testing.T) {
	m, err := parseOrderedJSON([]byte(`{"first":1,"removed":2}`))
	if err != nil {
		t.Fatal(err)
	}
	delete(m, "removed")
	m["third"] = 3
	m["second"] = 2
	if got, want := jsStringifyString(m), `{"first":1,"second":2,"third":3}`; got != want {
		t.Fatalf("jsStringify() = %s, want %s", got, want)
	}

	unordered := map[string]any{"second": 2, "first": 1}
	if got, want := string(jsStringify(unordered)), `{"first":1,"second":2}`; got != want {
		t.Fatalf("unordered jsStringify() = %s, want %s", got, want)
	}
	if got, want := string(jsStringifyCap(unordered, 128)), `{"first":1,"second":2}`; got != want {
		t.Fatalf("preallocated jsStringify() = %s, want %s", got, want)
	}
}

func TestJSStringifyAnthropicImage(t *testing.T) {
	png := []byte{0, 1, 2, 253, 254, 255}
	block := makeImageBlock(png)
	got := string(jsStringify(block))
	want := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAEC/f7/"}}`
	if got != want {
		t.Fatalf("jsStringify(image) = %s, want %s", got, want)
	}
	messages := []any{map[string]any{"content": []any{block}}}
	if got, want := historyImageSha8(messages), sha8("AAEC/f7/"); got != want {
		t.Fatalf("historyImageSha8() = %s, want %s", got, want)
	}
}

func TestOpenAIJSONCapacity(t *testing.T) {
	for _, tc := range []struct {
		body, image, want int
	}{
		{100, 0, 100},
		{100, 3, 104},
		{-1, -1, 0},
		{maxOpenAIJSONPreallocBytes, 0, maxOpenAIJSONPreallocBytes},
		{0, maxOpenAIJSONPreallocBytes, maxOpenAIJSONPreallocBytes},
		{maxOpenAIJSONPreallocBytes - 1, 1, maxOpenAIJSONPreallocBytes},
	} {
		if got := openAIJSONCapacity(tc.body, tc.image); got != tc.want {
			t.Errorf("openAIJSONCapacity(%d, %d) = %d, want %d", tc.body, tc.image, got, tc.want)
		}
	}
}

func TestCachePrefixDigestJoinsSerializedParts(t *testing.T) {
	tool := map[string]any{"name": "lookup", "description": "도구"}
	setObjKeyOrder(tool, []string{"name", "description"})
	system := textBlock("시스템")
	history := textBlock(HistorySyntheticIntro)
	image := makeImageBlock([]byte{0, 1, 2, 253, 254, 255})
	tail := textBlock("끝")
	message := map[string]any{"role": "assistant", "content": []any{history, image, "raw", tail}}
	setObjKeyOrder(message, []string{"role", "content"})
	req := map[string]any{
		"tools":    []any{tool},
		"system":   []any{system},
		"messages": []any{message, map[string]any{"role": "user", "content": "ignored"}},
	}

	prefix := strings.Join([]string{
		string(jsStringify(tool)),
		string(jsStringify(system)),
		string(jsStringify(history)),
		string(jsStringify(image)),
		"raw",
		string(jsStringify(tail)),
	}, "\x00")
	wantSHA := sha8(prefix)
	wantBytes := u16len(prefix)
	gotSHA, gotBytes, ok := cachePrefixDigest(req)
	if !ok {
		t.Fatal("cachePrefixDigest() did not find history boundary")
	}
	if gotSHA != wantSHA {
		t.Fatalf("cachePrefixDigest() sha = %q, want %q", gotSHA, wantSHA)
	}
	if gotBytes != wantBytes {
		t.Fatalf("cachePrefixDigest() bytes = %d, want %d", gotBytes, wantBytes)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 20 {
				sha, bytes, ok := cachePrefixDigest(req)
				if !ok || sha != wantSHA || bytes != wantBytes {
					t.Errorf("concurrent cachePrefixDigest() = %q, %d, %v", sha, bytes, ok)
					return
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkAppendJSOrderedObject(b *testing.B) {
	m, err := parseOrderedJSON([]byte(`{"alpha":1,"nested":{"beta":"value","gamma":true},"items":[{"delta":null}]}`))
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkJSONBytes = appendJSValue(buf[:0], m)
	}
}

func BenchmarkAppendJSLargeOrderedObject(b *testing.B) {
	m := make(map[string]any, 65)
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = "key" + strconv.Itoa(i)
		m[keys[i]] = true
	}
	setObjKeyOrder(m, keys)
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkJSONBytes = appendJSValue(buf[:0], m)
	}
}

func BenchmarkJSStringifyString(b *testing.B) {
	m := make(map[string]any, 65)
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = "key" + strconv.Itoa(i)
		m[keys[i]] = true
	}
	setObjKeyOrder(m, keys)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkJSONString = jsStringifyString(m)
	}
}

func BenchmarkJSStringifyAnthropicImage(b *testing.B) {
	png := make([]byte, 256<<10)
	b.SetBytes(int64(len(png)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkJSONBytes = jsStringify(makeImageBlock(png))
	}
}
