package quantforensics

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, schemaError("$", "invalid JSON: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaError("$", "root must be a JSON object")
	}
	return m, nil
}

func fieldPath(parent, key string) string {
	if parent == "" || parent == "$" {
		return "$." + key
	}
	return parent + "." + key
}

func arrayPath(parent string, i int) string { return fmt.Sprintf("%s[%d]", parent, i) }

func asObject(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaError(path, "required object is missing or has wrong type")
	}
	return m, nil
}

func requireObject(m map[string]any, key, parent string) (map[string]any, error) {
	p := fieldPath(parent, key)
	v, ok := m[key]
	if !ok || v == nil {
		return nil, schemaError(p, "required field is missing")
	}
	return asObject(v, p)
}

func requireArray(m map[string]any, key, parent string) ([]any, error) {
	p := fieldPath(parent, key)
	v, ok := m[key]
	if !ok || v == nil {
		return nil, schemaError(p, "required field is missing")
	}
	a, ok := v.([]any)
	if !ok {
		return nil, schemaError(p, "required array has wrong type")
	}
	return a, nil
}

func requireString(m map[string]any, key, parent string) (string, error) {
	p := fieldPath(parent, key)
	v, ok := m[key]
	if !ok || v == nil {
		return "", schemaError(p, "required field is missing")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", schemaError(p, "required non-empty string has wrong type")
	}
	return s, nil
}

func optionalString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func requireInt(m map[string]any, key, parent string) (int64, string, error) {
	p := fieldPath(parent, key)
	v, ok := m[key]
	if !ok || v == nil {
		return 0, p, schemaError(p, "required field is missing")
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, p, schemaError(p, "required integer has wrong type")
	}
	i, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr == nil && f == float64(int64(f)) {
			return int64(f), p, nil
		}
		return 0, p, schemaError(p, "value %q is not an integer", n.String())
	}
	return i, p, nil
}

func optionalIntDefault(m map[string]any, key, parent string, def int64) (int64, bool, string, error) {
	p := fieldPath(parent, key)
	v, ok := m[key]
	if !ok || v == nil {
		return def, false, p, nil
	}
	i, p2, err := requireInt(m, key, parent)
	return i, true, p2, err
}

func parseHexColor(s, path string) (RGB, error) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return RGB{}, schemaError(path, "color must be #rrggbb")
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return RGB{}, schemaError(path, "color must be #rrggbb: %v", err)
	}
	return RGB{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n)}, nil
}

func hexColor(c RGB) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func decodeFramePixels(s string, w, h int, path string) ([]RGB, string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, "", schemaError(path, "rgbBase64 is invalid base64: %v", err)
	}
	if len(raw) != w*h*3 {
		return nil, "", schemaError(path, "pixel byte length %d does not match %dx%d RGB (%d)", len(raw), w, h, w*h*3)
	}
	pixels := make([]RGB, w*h)
	for i := range pixels {
		pixels[i] = RGB{R: raw[i*3], G: raw[i*3+1], B: raw[i*3+2]}
	}
	return pixels, HashHex(raw), nil
}

func encodeFramePixels(pixels []RGB) string {
	raw := make([]byte, len(pixels)*3)
	for i, c := range pixels {
		raw[i*3] = c.R
		raw[i*3+1] = c.G
		raw[i*3+2] = c.B
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func kindFromString(s string) BlockKind {
	switch BlockKind(s) {
	case KindForeground, KindGradient, KindStroke, KindNoise, KindMixed:
		return BlockKind(s)
	default:
		return KindMixed
	}
}
