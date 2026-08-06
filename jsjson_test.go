package pxpipe

import "testing"

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
