package schema

import "strconv"

func path(parts ...any) string {
	out := "$"
	for _, part := range parts {
		switch v := part.(type) {
		case int:
			out += "[" + strconv.Itoa(v) + "]"
		case string:
			if v == "" {
				continue
			}
			if out == "$" {
				out += "." + v
			} else {
				out += "." + v
			}
		default:
			out += "." + toString(v)
		}
	}
	return out
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}
