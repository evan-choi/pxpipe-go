package pxpipe

import (
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
