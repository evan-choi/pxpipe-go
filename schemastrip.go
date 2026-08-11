package pxpipe

// Structure-aware JSON-Schema annotation stripper: annotation keywords are
// stripped only at schema-node level; keys inside properties/$defs are user
// property names and must survive (see the TS module docstring for the
// required:["description"] failure this prevents).

const schemaStripMaxDepth = 20

var schemaStripKeys = map[string]struct{}{
	"description": {}, "title": {}, "examples": {}, "default": {}, "$id": {}, "$comment": {},
}

var schemaCompositionKeys = map[string]struct{}{
	"oneOf": {}, "anyOf": {}, "allOf": {},
}

var schemaNamedSubschemaKeys = map[string]struct{}{
	"properties": {}, "patternProperties": {}, "definitions": {}, "$defs": {},
}

var schemaSingleSubschemaKeys = map[string]struct{}{
	"items": {}, "additionalProperties": {}, "not": {}, "contains": {}, "propertyNames": {},
	"unevaluatedItems": {}, "unevaluatedProperties": {}, "if": {}, "then": {}, "else": {},
}

var schemaVerbatimKeys = map[string]struct{}{
	"$schema": {}, "required": {}, "enum": {}, "const": {}, "type": {}, "$ref": {},
	"minimum": {}, "maximum": {}, "exclusiveMinimum": {}, "exclusiveMaximum": {},
	"minLength": {}, "maxLength": {}, "minItems": {}, "maxItems": {},
	"minProperties": {}, "maxProperties": {}, "multipleOf": {}, "uniqueItems": {}, "pattern": {},
}

const schemaFormatMaxLen = 32

func stripSchemaDescriptions(node any, depth int) any {
	stripped, _ := stripSchemaDescriptionsChanged(node, depth, true)
	return stripped
}

func stripSchemaDescriptionsInPlace(node any, depth int) {
	_, _ = stripSchemaDescriptionsChanged(node, depth, false)
}

func stripSchemaArray(values []any, depth int, copyOnWrite bool) ([]any, bool) {
	out := values
	changed := false
	for i, value := range values {
		stripped, childChanged := stripSchemaDescriptionsChanged(value, depth, copyOnWrite)
		if !childChanged {
			continue
		}
		if !changed && copyOnWrite {
			out = append([]any(nil), values...)
		}
		changed = true
		out[i] = stripped
	}
	return out, changed
}

func stripSchemaMapValues(values map[string]any, depth int, copyOnWrite bool) (map[string]any, bool) {
	out := values
	changed := false
	for key, value := range values {
		stripped, childChanged := stripSchemaDescriptionsChanged(value, depth, copyOnWrite)
		if !childChanged {
			continue
		}
		if !changed && copyOnWrite {
			out = cloneMap(values)
		}
		changed = true
		out[key] = stripped
	}
	return out, changed
}

func stripSchemaDescriptionsChanged(node any, depth int, copyOnWrite bool) (any, bool) {
	if depth > schemaStripMaxDepth {
		return node, false
	}
	if _, isArr := node.([]any); isArr {
		return node, false
	}
	obj, ok := asMap(node)
	if !ok {
		return node, false
	}
	out := obj
	changed := false
	for k, v := range obj {
		if _, strip := schemaStripKeys[k]; strip {
			if !changed && copyOnWrite {
				out = cloneMap(obj)
			}
			changed = true
			delete(out, k)
			continue
		}
		if k == "format" {
			if s, isStr := v.(string); isStr && u16len(s) > schemaFormatMaxLen {
				if !changed && copyOnWrite {
					out = cloneMap(obj)
				}
				changed = true
				delete(out, k)
				continue
			}
		}
		if _, verbatim := schemaVerbatimKeys[k]; verbatim {
			continue
		}
		if _, named := schemaNamedSubschemaKeys[k]; named {
			if vm, isMap := asMap(v); isMap {
				if nested, childChanged := stripSchemaMapValues(vm, depth+1, copyOnWrite); childChanged {
					if !changed && copyOnWrite {
						out = cloneMap(obj)
					}
					changed = true
					out[k] = nested
				}
				continue
			}
		}
		if _, comp := schemaCompositionKeys[k]; comp {
			if va, isArr := asArr(v); isArr {
				if mapped, childChanged := stripSchemaArray(va, depth+1, copyOnWrite); childChanged {
					if !changed && copyOnWrite {
						out = cloneMap(obj)
					}
					changed = true
					out[k] = mapped
				}
				continue
			}
		}
		if _, single := schemaSingleSubschemaKeys[k]; single {
			switch tv := v.(type) {
			case bool:
			case []any:
				if mapped, childChanged := stripSchemaArray(tv, depth+1, copyOnWrite); childChanged {
					if !changed && copyOnWrite {
						out = cloneMap(obj)
					}
					changed = true
					out[k] = mapped
				}
			default:
				if stripped, childChanged := stripSchemaDescriptionsChanged(v, depth+1, copyOnWrite); childChanged {
					if !changed && copyOnWrite {
						out = cloneMap(obj)
					}
					changed = true
					out[k] = stripped
				}
			}
			continue
		}
		if _, isMap := v.(map[string]any); isMap {
			if stripped, childChanged := stripSchemaDescriptionsChanged(v, depth+1, copyOnWrite); childChanged {
				if !changed && copyOnWrite {
					out = cloneMap(obj)
				}
				changed = true
				out[k] = stripped
			}
		}
	}
	if !changed {
		return obj, false
	}
	return out, true
}

var schemaStructuralKeys = []string{
	"properties", "patternProperties", "oneOf", "anyOf", "allOf", "items", "$ref", "enum", "const",
}

func schemaHasStructure(schema map[string]any) bool {
	for _, k := range schemaStructuralKeys {
		if _, ok := schema[k]; ok {
			return true
		}
	}
	return false
}
