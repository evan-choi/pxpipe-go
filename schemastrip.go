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
	if depth > schemaStripMaxDepth {
		return node
	}
	if _, isArr := node.([]any); isArr {
		return node
	}
	obj, ok := asMap(node)
	if !ok {
		return node
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if _, strip := schemaStripKeys[k]; strip {
			continue
		}
		if k == "format" {
			if s, isStr := v.(string); isStr && u16len(s) > schemaFormatMaxLen {
				continue
			}
		}
		if _, verbatim := schemaVerbatimKeys[k]; verbatim {
			out[k] = v
			continue
		}
		if _, named := schemaNamedSubschemaKeys[k]; named {
			if vm, isMap := asMap(v); isMap {
				nested := make(map[string]any, len(vm))
				for pk, pv := range vm {
					nested[pk] = stripSchemaDescriptions(pv, depth+1)
				}
				out[k] = nested
				continue
			}
		}
		if _, comp := schemaCompositionKeys[k]; comp {
			if va, isArr := asArr(v); isArr {
				mapped := make([]any, len(va))
				for i, sub := range va {
					mapped[i] = stripSchemaDescriptions(sub, depth+1)
				}
				out[k] = mapped
				continue
			}
		}
		if _, single := schemaSingleSubschemaKeys[k]; single {
			switch tv := v.(type) {
			case bool:
				out[k] = tv
			case []any:
				mapped := make([]any, len(tv))
				for i, sub := range tv {
					mapped[i] = stripSchemaDescriptions(sub, depth+1)
				}
				out[k] = mapped
			default:
				out[k] = stripSchemaDescriptions(v, depth+1)
			}
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			out[k] = stripSchemaDescriptions(v, depth+1)
		default:
			out[k] = v
		}
	}
	return out
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
