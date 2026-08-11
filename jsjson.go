package pxpipe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"unicode/utf8"
	"unsafe"

	"github.com/bytedance/sonic/ast"
	"github.com/evan-choi/pxpipe-go/render"
)

// Order-preserving JSON layer. The TS reference relies on JS object key
// insertion order surviving parse→stringify: tool schemas and tool_use args
// are re-serialized into IMAGED text, so key order changes pixels and cache
// hashes. Each decoded object carries its original key order under a sentinel
// key; jsStringify emits ordered keys first and any later-added keys after
// (sorted), matching JS insertion semantics for single additions.

const orderKey = "\x00keys"

type pngDataURL struct{ image *render.RenderedImage }
type pngBase64 []byte
type pngBase64Image struct{ image *render.RenderedImage }

type objectKeyOrder struct {
	small [4]string
	extra []string
	n     int
}

func (o *objectKeyOrder) append(key string) {
	if o.extra != nil {
		o.extra = append(o.extra, key)
		return
	}
	if o.n < len(o.small) {
		o.small[o.n] = key
		o.n++
		return
	}
	o.extra = make([]string, len(o.small), 8)
	copy(o.extra, o.small[:])
	o.extra = append(o.extra, key)
}

func (o *objectKeyOrder) slice() []string {
	if o == nil {
		return nil
	}
	if o.extra != nil {
		return o.extra
	}
	return o.small[:o.n]
}

func objKeyOrder(m map[string]any) []string {
	switch ks := m[orderKey].(type) {
	case []string:
		return ks
	case *objectKeyOrder:
		return ks.slice()
	}
	return nil
}

func setObjKeyOrder(m map[string]any, keys []string) {
	m[orderKey] = keys
}

// parseOrderedJSON decodes body into the map[string]any / []any / string /
// json.Number tree the transform operates on, recording object key order.
func parseOrderedJSON(body []byte) (map[string]any, error) {
	src := ""
	if len(body) != 0 {
		src = unsafe.String(unsafe.SliceData(body), len(body))
	}
	d := orderedJSONDecoder{stack: make([]orderedJSONFrame, 0, 16)}
	if err := ast.Preorder(src, &d, &orderedJSONVisitorOptions); err != nil {
		return nil, fmt.Errorf("json parse error: %v", err)
	}
	m, ok := d.root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("json body is not an object")
	}
	return m, nil
}

var orderedJSONVisitorOptions = ast.VisitorOptions{OnlyNumber: true}

type orderedJSONFrame struct {
	object map[string]any
	array  []any
	order  *objectKeyOrder
	key    string
	isObj  bool
}

type orderedJSONDecoder struct {
	stack []orderedJSONFrame
	root  any
}

func (d *orderedJSONDecoder) accept(v any) error {
	if len(d.stack) == 0 {
		d.root = v
		return nil
	}
	f := &d.stack[len(d.stack)-1]
	if !f.isObj {
		f.array = append(f.array, v)
		return nil
	}
	if f.object == nil {
		f.object = make(map[string]any, 8)
	}
	if _, duplicate := f.object[f.key]; !duplicate {
		if f.order == nil {
			f.order = &objectKeyOrder{}
		}
		f.order.append(f.key)
	}
	f.object[f.key] = v
	f.key = ""
	return nil
}

func (d *orderedJSONDecoder) OnNull() error           { return d.accept(nil) }
func (d *orderedJSONDecoder) OnBool(v bool) error     { return d.accept(v) }
func (d *orderedJSONDecoder) OnString(v string) error { return d.accept(v) }
func (d *orderedJSONDecoder) OnInt64(_ int64, n json.Number) error {
	return d.accept(n)
}
func (d *orderedJSONDecoder) OnFloat64(_ float64, n json.Number) error {
	return d.accept(n)
}
func (d *orderedJSONDecoder) OnObjectBegin(_ int) error {
	d.stack = append(d.stack, orderedJSONFrame{isObj: true})
	return nil
}
func (d *orderedJSONDecoder) OnObjectKey(key string) error {
	f := &d.stack[len(d.stack)-1]
	f.key = key
	return nil
}
func (d *orderedJSONDecoder) OnObjectEnd() error {
	n := len(d.stack) - 1
	f := d.stack[n]
	d.stack = d.stack[:n]
	if f.object == nil {
		f.object = make(map[string]any, 1)
	}
	f.object[orderKey] = f.order
	return d.accept(f.object)
}
func (d *orderedJSONDecoder) OnArrayBegin(capacity int) error {
	d.stack = append(d.stack, orderedJSONFrame{array: make([]any, 0, capacity)})
	return nil
}
func (d *orderedJSONDecoder) OnArrayEnd() error {
	n := len(d.stack) - 1
	f := d.stack[n]
	d.stack = d.stack[:n]
	return d.accept(f.array)
}

// jsStringify mirrors JSON.stringify: insertion-ordered object keys, JS string
// escaping (no HTML/2028 escaping), numbers emitted as decoded.
func jsStringify(v any) []byte {
	return appendJSValue(nil, v)
}

func jsStringifyCap(v any, capacity int) []byte {
	if capacity <= 0 {
		return jsStringify(v)
	}
	return appendJSValue(make([]byte, 0, capacity), v)
}

func jsStringifyString(v any) string {
	b := jsStringify(v)
	if len(b) == 0 {
		return ""
	}
	// jsStringify returns a fresh buffer that no caller mutates after this view.
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func jsStringifyU16Len(scratch *[]byte, v any) int {
	*scratch = appendJSValue((*scratch)[:0], v)
	return u16len(unsafe.String(unsafe.SliceData(*scratch), len(*scratch)))
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
	case pngDataURL:
		b = append(b, `"data:image/png;base64,`...)
		b = tv.image.AppendPNGBase64(b)
		return append(b, '"')
	case pngBase64:
		b = append(b, '"')
		b = base64.StdEncoding.AppendEncode(b, tv)
		return append(b, '"')
	case pngBase64Image:
		b = append(b, '"')
		b = tv.image.AppendPNGBase64Deferred(b)
		return append(b, '"')
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
	b = append(b, '{')
	first := true
	emitted := 0
	for _, k := range ordered {
		v, ok := m[k]
		if !ok {
			continue
		}
		b = appendJSObjectPair(b, k, v, !first)
		first = false
		emitted++
	}
	keyCount := len(m)
	if _, ok := m[orderKey]; ok {
		keyCount--
	}
	if emitted == keyCount {
		return append(b, '}')
	}
	var extras []string
	for k := range m {
		if k == orderKey {
			continue
		}
		if !slices.Contains(ordered, k) {
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

func hasZeroByte(word uint64) bool {
	return (word-0x0101010101010101)&^word&0x8080808080808080 != 0
}

func appendJSString(b []byte, s string) []byte {
	const (
		hex            = "0123456789abcdef"
		highBits       = uint64(0x8080808080808080)
		controlBits    = uint64(0xe0e0e0e0e0e0e0e0)
		quoteBytes     = uint64(0x2222222222222222)
		backslashBytes = uint64(0x5c5c5c5c5c5c5c5c)
	)
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		if len(s)-i >= 8 {
			word := *(*uint64)(unsafe.Pointer(unsafe.StringData(s[i:])))
			if word&highBits == 0 &&
				!hasZeroByte(word&controlBits) &&
				!hasZeroByte(word^quoteBytes) &&
				!hasZeroByte(word^backslashBytes) {
				i += 8
				continue
			}
		}
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
