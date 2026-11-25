package schema

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"maintenanceorchestrator/internal/domain"
)

func decodeVersion(data []byte, dst any) error {
	if err := rejectUnknownFields(data, reflect.TypeOf(dst), "$"); err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return domain.NewError(domain.ESchemaUnknownFieldCombo, "$", "schema decode failed: %v", err)
	}
	return nil
}

func rejectUnknownFields(data []byte, t reflect.Type, path string) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return domain.NewError(domain.ESchemaUnknownFieldCombo, path, "invalid JSON: %v", err)
	}
	return walkUnknown(t, value, path)
}

func walkUnknown(t reflect.Type, value any, path string) error {
	if value == nil {
		return nil
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		elem := t.Elem()
		for i, item := range items {
			if err := walkUnknown(elem, item, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case reflect.Struct:
		obj, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(t)
		for key, raw := range obj {
			field, ok := fields[key]
			fieldPath := domain.JoinPath(path, key)
			if !ok {
				return domain.NewError(domain.ESchemaUnknownFieldCombo, fieldPath,
					"field is not recognized by schema version")
			}
			if err := walkUnknown(field.Type, raw, fieldPath); err != nil {
				return err
			}
		}
	case reflect.Map:
		// DTOs use maps only for extension-free payloads; no unknown-key validation.
		return nil
	}
	return nil
}

func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := f.Name
		if tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] != "" {
				name = parts[0]
			}
		}
		out[name] = f
	}
	return out
}
