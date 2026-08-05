package pxpipe

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/bytedance/sonic/ast"
)

// Order-preserving JSON layer. The TS reference relies on JS object key
// insertion order surviving parse→stringify: tool schemas and tool_use args
// are re-serialized into IMAGED text, so key order changes pixels and cache
// hashes. Each decoded object carries its original key order under a sentinel
// key; jsStringify emits ordered keys first and any later-added keys after
// (sorted), matching JS insertion semantics for single additions.

const orderKey = "\x00keys"

func objKeyOrder(m map[string]any) []string {
	if ks, ok := m[orderKey].([]string); ok {
		return ks
	}
	return nil
}

func setObjKeyOrder(m map[string]any, keys []string) {
	m[orderKey] = keys
}

// parseOrderedJSON decodes body into the map[string]any / []any / string /
// json.Number tree the transform operates on, recording object key order.
func parseOrderedJSON(body []byte) (map[string]any, error) {
	p := ast.NewParser(string(body))
	node, err := p.Parse()
	if err != 0 {
		return nil, fmt.Errorf("json parse error: %v", err)
	}
	if loadErr := node.LoadAll(); loadErr != nil {
		return nil, loadErr
	}
	v, convErr := nodeToValue(&node)
	if convErr != nil {
		return nil, convErr
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("json body is not an object")
	}
	return m, nil
}

func nodeToValue(n *ast.Node) (any, error) {
	switch n.TypeSafe() {
	case ast.V_OBJECT:
		length, err := n.Len()
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, length+1)
		keys := make([]string, 0, length)
		for i := 0; i < length; i++ {
			pair := n.IndexPair(i)
			if pair == nil {
				break
			}
			child, err := nodeToValue(&pair.Value)
			if err != nil {
				return nil, err
			}
			// Duplicate keys: last one wins (JS), order keeps first sighting.
			if _, dup := m[pair.Key]; !dup {
				keys = append(keys, pair.Key)
			}
			m[pair.Key] = child
		}
		setObjKeyOrder(m, keys)
		return m, nil
	case ast.V_ARRAY:
		length, err := n.Len()
		if err != nil {
			return nil, err
		}
		arr := make([]any, 0, length)
		for i := 0; i < length; i++ {
			child := n.Index(i)
			if child == nil {
				break
			}
			cv, err := nodeToValue(child)
			if err != nil {
				return nil, err
			}
			arr = append(arr, cv)
		}
		return arr, nil
	case ast.V_STRING:
		return n.StrictString()
	case ast.V_NUMBER:
		num, err := n.Number()
		if err != nil {
			return nil, err
		}
		return num, nil
	case ast.V_TRUE:
		return true, nil
	case ast.V_FALSE:
		return false, nil
	case ast.V_NULL:
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported json node type %d", n.TypeSafe())
}

// jsStringify mirrors JSON.stringify: insertion-ordered object keys, JS string
// escaping (no HTML/2028 escaping), numbers emitted as decoded.
func jsStringify(v any) []byte {
	var b strings.Builder
	writeJSValue(&b, v)
	return []byte(b.String())
}

func writeJSValue(b *strings.Builder, v any) {
	switch tv := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if tv {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeJSString(b, tv)
	case json.Number:
		if tv == "" {
			b.WriteString("0")
		} else {
			b.WriteString(string(tv))
		}
	case float64:
		nb, _ := json.Marshal(tv)
		b.Write(nb)
	case int:
		nb, _ := json.Marshal(tv)
		b.Write(nb)
	case []any:
		b.WriteByte('[')
		for i, item := range tv {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSValue(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, item := range tv {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSString(b, item)
		}
		b.WriteByte(']')
	case map[string]any:
		writeJSObject(b, tv)
	default:
		nb, err := jsonAPI.Marshal(tv)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(nb)
	}
}

func writeJSObject(b *strings.Builder, m map[string]any) {
	ordered := objKeyOrder(m)
	seen := make(map[string]struct{}, len(ordered))
	b.WriteByte('{')
	first := true
	writePair := func(k string, v any) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		writeJSString(b, k)
		b.WriteByte(':')
		writeJSValue(b, v)
	}
	for _, k := range ordered {
		v, ok := m[k]
		if !ok {
			continue
		}
		seen[k] = struct{}{}
		writePair(k, v)
	}
	var extras []string
	for k := range m {
		if k == orderKey {
			continue
		}
		if _, done := seen[k]; !done {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		writePair(k, m[k])
	}
	b.WriteByte('}')
}

func writeJSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else if r >= 0x10000 || !utf16.IsSurrogate(r) {
				b.WriteRune(r)
			} else {
				// Lone surrogates escape as \uXXXX (well-formed JSON.stringify).
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
}
