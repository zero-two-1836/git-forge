package main

// Small JSON and coercion helpers with JavaScript semantics, because the
// forge's wire behaviour was specified against JSON.stringify / JSON.parse
// and the loose typeof checks of the original handlers:
//   - strings escape exactly the characters JSON.stringify escapes,
//   - integral floats print without a decimal point,
//   - asString/asBool only accept the real JSON types, never coerce.

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// jsonString renders s as a JSON string literal, escaping only what
// JSON.stringify escapes: quote, backslash and the control characters.
func jsonString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		default:
			if r < 0x20 {
				b.WriteString("\\u")
				const digits = "0123456789abcdef"
				b.WriteByte(digits[(r>>12)&0xf])
				b.WriteByte(digits[(r>>8)&0xf])
				b.WriteByte(digits[(r>>4)&0xf])
				b.WriteByte(digits[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// marshalJSON serializes the loose value model (nil, bool, int, float64,
// string, []any, map[string]any) with map keys in sorted order so output is
// deterministic across calls.
func marshalJSON(v any) string {
	var b strings.Builder
	writeJSONValue(&b, v)
	return b.String()
}

func writeJSONValue(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(jsonNumber(t))
	case string:
		b.WriteString(jsonString(t))
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONValue(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(item))
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(k))
			b.WriteByte(':')
			writeJSONValue(b, t[k])
		}
		b.WriteByte('}')
	default:
		raw, _ := json.Marshal(t)
		b.Write(raw)
	}
}

// jsonNumber formats a float the way JSON.stringify does: integers without
// a fraction, others in shortest round-trip form.
func jsonNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// parseJSONBody decodes a request body; ok=false mirrors a JSON.parse throw.
func parseJSONBody(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// asLabelNames accepts the two label shapes the API takes: plain strings or
// {name} objects; anything else is dropped.
func asLabelNames(v any) []string {
	out := []string{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		} else if m, ok := item.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// parseIntJS mirrors Number.parseInt(s, 10): an optional sign, then a run of
// digits; no digits at all yields NaN, represented here as ok=false.
func parseIntJS(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	value := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		value = value*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		value = -value
	}
	return value, true
}

// parsePositiveInt accepts only an all-digits string with value > 0.
func parsePositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// queryInt mirrors the REST layer's queryInt: a missing or unparseable
// parameter yields the fallback; a parsed value is used as-is.
func queryInt(query map[string]string, name string, fallback int) int {
	raw, present := query[name]
	if !present {
		return fallback
	}
	value, ok := parseIntJS(raw)
	if !ok {
		return fallback
	}
	return value
}

// pageParams mirrors the REST pagination: per_page defaults to 30 and
// clamps to [1,100]; page defaults to 1 and clamps to >= 1. (A literal
// per_page=0 parses fine and clamps to 1 — it does not hit the default.)
func pageParams(query map[string]string) (int, int) {
	perPage := queryInt(query, "per_page", 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := queryInt(query, "page", 1)
	if page < 1 {
		page = 1
	}
	return perPage, page
}

func paginateSlice[T any](items []T, perPage, page int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
