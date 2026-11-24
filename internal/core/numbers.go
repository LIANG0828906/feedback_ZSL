package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var ErrIntegerOverflow = errors.New("integer overflow")
var ErrInvalidRounding = errors.New("invalid rounding mode")

const RoundFloor = "floor"
const RoundTowardZero = "toward-zero"

func CheckedMul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 {
		return 0, ErrIntegerOverflow
	}
	result := a * b
	if result/b != a {
		return 0, ErrIntegerOverflow
	}
	return result, nil
}

func CheckedAdd(a, b int64) (int64, error) {
	result := a + b
	if (b > 0 && result < a) || (b < 0 && result > a) {
		return 0, ErrIntegerOverflow
	}
	return result, nil
}

func CheckedSub(a, b int64) (int64, error) {
	result := a - b
	if (b < 0 && result < a) || (b > 0 && result > a) {
		return 0, ErrIntegerOverflow
	}
	return result, nil
}

func FloorDiv(a, b int64) (int64, error) {
	if b == 0 {
		return 0, errors.New("division by zero")
	}
	if a == math.MinInt64 && b == -1 {
		return 0, ErrIntegerOverflow
	}
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q, nil
}

func TicksToUnixNanos(ticks, epochSeconds, tickNanos int64) (int64, error) {
	if tickNanos <= 0 {
		return 0, fmt.Errorf("tickNanos must be positive: %d", tickNanos)
	}
	delta, err := CheckedMul(ticks, tickNanos)
	if err != nil {
		return 0, err
	}
	epoch, err := CheckedMul(epochSeconds, int64(1_000_000_000))
	if err != nil {
		return 0, err
	}
	return CheckedAdd(epoch, delta)
}

func UnixNanosToTicks(nanos, epochSeconds, tickNanos int64, rounding string) (int64, error) {
	if tickNanos <= 0 {
		return 0, fmt.Errorf("tickNanos must be positive: %d", tickNanos)
	}
	epoch, err := CheckedMul(epochSeconds, int64(1_000_000_000))
	if err != nil {
		return 0, err
	}
	delta, err := CheckedSub(nanos, epoch)
	if err != nil {
		return 0, err
	}
	switch rounding {
	case RoundFloor:
		return FloorDiv(delta, tickNanos)
	case RoundTowardZero:
		return delta / tickNanos, nil
	default:
		return 0, fmt.Errorf("%w: %s", ErrInvalidRounding, rounding)
	}
}

func DecodeInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case uint64:
		if v > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		return parsed, err == nil
	case float64:
		if v != math.Trunc(v) || v < math.MinInt64 || v > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	default:
		return 0, false
	}
}

func DecodeBool(value any) (bool, bool) {
	v, ok := value.(bool)
	return v, ok
}

func DecodeStringList(value any) ([]string, bool) {
	switch list := value.(type) {
	case []string:
		return append([]string(nil), list...), true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	default:
		return nil, false
	}
}

func Lookup(root map[string]any, path string) (any, bool) {
	if path == "" {
		return root, true
	}
	parts := strings.Split(path, ".")
	var current any = root
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func Set(root map[string]any, path string, value any) {
	if path == "" {
		return
	}
	parts := strings.Split(path, ".")
	current := root
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
			return
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		current = next
	}
}

func Delete(root map[string]any, path string) bool {
	if path == "" {
		return false
	}
	parts := strings.Split(path, ".")
	current := root
	for i, part := range parts {
		if i == len(parts)-1 {
			if _, ok := current[part]; !ok {
				return false
			}
			delete(current, part)
			return true
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			return false
		}
		current = next
	}
	return false
}

func CloneMap(root map[string]any) map[string]any {
	out := make(map[string]any, len(root))
	for k, v := range root {
		switch value := v.(type) {
		case map[string]any:
			out[k] = CloneMap(value)
		case []any:
			list := make([]any, len(value))
			for i, item := range value {
				if nested, ok := item.(map[string]any); ok {
					list[i] = CloneMap(nested)
				} else {
					list[i] = item
				}
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}
