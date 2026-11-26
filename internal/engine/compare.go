package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func CompareResults(a, b *RunResult) string {
	if a == nil || b == nil {
		return fmt.Sprintf("nil result: a=%v b=%v", a == nil, b == nil)
	}
	return diffValue(reflect.ValueOf(a), reflect.ValueOf(b), "$")
}

func diffValue(va, vb reflect.Value, path string) string {
	if !va.IsValid() || !vb.IsValid() {
		if va.IsValid() == vb.IsValid() {
			return ""
		}
		return fmt.Sprintf("%s: validity differs", path)
	}
	for va.Kind() == reflect.Interface || va.Kind() == reflect.Pointer {
		if va.IsNil() {
			if vb.Kind() == reflect.Pointer || vb.Kind() == reflect.Interface {
				if vb.IsNil() {
					return ""
				}
			}
			return fmt.Sprintf("%s: nil vs %v", path, vb.Type())
		}
		if vb.Kind() == reflect.Pointer || vb.Kind() == reflect.Interface {
			if vb.IsNil() {
				return fmt.Sprintf("%s: %v vs nil", path, va.Type())
			}
		}
		va = va.Elem()
		vb = vb.Elem()
	}
	if va.Type() != vb.Type() {
		return fmt.Sprintf("%s: type %s != %s", path, va.Type(), vb.Type())
	}

	switch va.Kind() {
	case reflect.Struct:
		typ := va.Type()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			nextPath := path + "." + field.Name
			if tag := field.Tag.Get("json"); tag != "" && tag != "-" {
				name := strings.Split(tag, ",")[0]
				if name != "" {
					nextPath = path + "." + name
				}
			}
			if mismatch := diffValue(va.Field(i), vb.Field(i), nextPath); mismatch != "" {
				return mismatch
			}
		}
	case reflect.Slice, reflect.Array:
		if va.Len() != vb.Len() {
			return fmt.Sprintf("%s: length %d != %d", path, va.Len(), vb.Len())
		}
		for i := 0; i < va.Len(); i++ {
			if mismatch := diffValue(va.Index(i), vb.Index(i), fmt.Sprintf("%s[%d]", path, i)); mismatch != "" {
				return mismatch
			}
		}
	case reflect.Map:
		if va.Len() != vb.Len() {
			return fmt.Sprintf("%s: map length %d != %d", path, va.Len(), vb.Len())
		}
		keys := va.MapKeys()
		keyStrings := make([]string, len(keys))
		keyByString := map[string]reflect.Value{}
		for i, key := range keys {
			keyStrings[i] = fmt.Sprint(key.Interface())
			keyByString[keyStrings[i]] = key
		}
		sort.Strings(keyStrings)
		for _, ks := range keyStrings {
			key := keyByString[ks]
			av := va.MapIndex(key)
			bv := vb.MapIndex(key)
			if !bv.IsValid() {
				return fmt.Sprintf("%s.%s: missing in second result", path, ks)
			}
			if mismatch := diffValue(av, bv, path+"."+ks); mismatch != "" {
				return mismatch
			}
		}
	default:
		if !reflect.DeepEqual(va.Interface(), vb.Interface()) {
			return fmt.Sprintf("%s: %v != %v", path, va.Interface(), vb.Interface())
		}
	}
	return ""
}

func FirstMismatch(a, b *RunResult) (index int, path string, message string) {
	msg := CompareResults(a, b)
	if msg == "" {
		return -1, "", ""
	}
	for i := 0; i < len(a.Windows) && i < len(b.Windows); i++ {
		windowPath := fmt.Sprintf("$.windows[%d]", i)
		if detail := CompareResults(&RunResult{Windows: []WindowResult{a.Windows[i]}}, &RunResult{Windows: []WindowResult{b.Windows[i]}}); detail != "" {
			return i, windowPath, detail
		}
	}
	return -1, "$", msg
}
