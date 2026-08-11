package pxpipe

import (
	"github.com/bytedance/sonic"
)

// jsonAPI keeps numbers lossless (json.Number) on decode and emits
// deterministic (sorted-key) output on encode. Key order in outbound bodies is
// normalized rather than insertion-preserving; output is deterministic per
// input, which is what prompt-cache stability requires.
var jsonAPI = sonic.Config{
	UseNumber:   true,
	SortMapKeys: true,
}.Froze()

func jsonMarshal(v any) ([]byte, error)   { return jsonAPI.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return jsonAPI.Unmarshal(b, v) }

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asArr(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

func getStr(m map[string]any, k string) (string, bool) {
	s, ok := m[k].(string)
	return s, ok
}

func blockType(v any) string {
	if m, ok := asMap(v); ok {
		if t, ok := getStr(m, "type"); ok {
			return t
		}
	}
	return ""
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func textBlock(text string) map[string]any {
	m := map[string]any{"type": "text", "text": text}
	setObjKeyOrder(m, []string{"type", "text"})
	return m
}
