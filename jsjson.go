package pxpipe

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

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
	return appendJSValue(nil, v)
}

func appendJSValue(b []byte, v any) []byte {
	switch tv := v.(type) {
	case nil:
		return append(b, "null"...)
	case bool:
		if tv {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case string:
		return appendJSString(b, tv)
	case json.Number:
		if tv == "" {
			return append(b, '0')
		}
		return append(b, string(tv)...)
	case float64:
		nb, _ := json.Marshal(tv)
		return append(b, nb...)
	case int:
		nb, _ := json.Marshal(tv)
		return append(b, nb...)
	case []any:
		b = append(b, '[')
		for i, item := range tv {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSValue(b, item)
		}
		return append(b, ']')
	case []string:
		b = append(b, '[')
		for i, item := range tv {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSString(b, item)
		}
		return append(b, ']')
	case map[string]any:
		return appendJSObject(b, tv)
	default:
		nb, err := jsonAPI.Marshal(tv)
		if err != nil {
			return append(b, "null"...)
		}
		return append(b, nb...)
	}
}

func appendJSObjectPair(b []byte, key string, value any, comma bool) []byte {
	if comma {
		b = append(b, ',')
	}
	b = appendJSString(b, key)
	b = append(b, ':')
	return appendJSValue(b, value)
}

func appendJSObject(b []byte, m map[string]any) []byte {
	ordered := objKeyOrder(m)
	seen := make(map[string]struct{}, len(ordered))
	b = append(b, '{')
	first := true
	for _, k := range ordered {
		v, ok := m[k]
		if !ok {
			continue
		}
		seen[k] = struct{}{}
		b = appendJSObjectPair(b, k, v, !first)
		first = false
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
		b = appendJSObjectPair(b, k, m[k], !first)
		first = false
	}
	return append(b, '}')
}

func appendJSString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			if c < utf8.RuneSelf {
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			if r != utf8.RuneError || size > 1 {
				i += size
				continue
			}
			b = append(b, s[start:i]...)
			b = utf8.AppendRune(b, utf8.RuneError)
			i++
			start = i
			continue
		}

		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, `\b`...)
		case '\f':
			b = append(b, `\f`...)
		case '\n':
			b = append(b, `\n`...)
		case '\r':
			b = append(b, `\r`...)
		case '\t':
			b = append(b, `\t`...)
		default:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		i++
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
