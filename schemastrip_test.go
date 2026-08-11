package pxpipe

import (
	"bytes"
	"reflect"
	"testing"
)

func TestStripSchemaDescriptionsDoesNotMutateInput(t *testing.T) {
	schema := map[string]any{
		"title": "root docs",
		"type":  "object",
		"properties": map[string]any{
			"description": map[string]any{
				"type":        "string",
				"description": "field docs",
			},
			"count": map[string]any{"type": "integer"},
		},
		"required": []any{"description"},
	}
	inputBefore, err := jsonMarshal(schema)
	if err != nil {
		t.Fatal(err)
	}

	got := stripSchemaDescriptions(schema, 0)
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description": map[string]any{"type": "string"},
			"count":       map[string]any{"type": "integer"},
		},
		"required": []any{"description"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped schema = %#v, want %#v", got, want)
	}
	inputAfter, err := jsonMarshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(inputAfter) != string(inputBefore) {
		t.Fatalf("input schema mutated: %s", inputAfter)
	}
}

func TestStripSchemaDescriptionsInPlace(t *testing.T) {
	schema := map[string]any{
		"title": "root docs",
		"properties": map[string]any{
			"value": map[string]any{"type": "string", "description": "field docs"},
		},
	}

	stripSchemaDescriptionsInPlace(schema, 0)
	if _, exists := schema["title"]; exists {
		t.Fatal("root annotation survived in-place stripping")
	}
	value := schema["properties"].(map[string]any)["value"].(map[string]any)
	if _, exists := value["description"]; exists {
		t.Fatal("nested annotation survived in-place stripping")
	}
}

func TestSchemaStrippingWaitsForToolRewrite(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","tools":[{"name":"short","description":"tool docs","input_schema":{"type":"object","properties":{"value":{"type":"string","description":"field docs"}}}}],"messages":[{"role":"user","content":"@pxpipe pin be concise"}]}`)
	out, _ := TransformRequest(body, nil)
	if !bytes.Contains(out, []byte(`"description":"field docs"`)) {
		t.Fatalf("early serialized tool schema was stripped: %s", out)
	}
}
