package pxpipe

import (
	"strconv"
	"testing"
)

var benchmarkJSONBytes []byte

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
	if got, want := string(jsStringify(m)), `{"first":1,"second":2,"third":3}`; got != want {
		t.Fatalf("jsStringify() = %s, want %s", got, want)
	}

	unordered := map[string]any{"second": 2, "first": 1}
	if got, want := string(jsStringify(unordered)), `{"first":1,"second":2}`; got != want {
		t.Fatalf("unordered jsStringify() = %s, want %s", got, want)
	}
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
