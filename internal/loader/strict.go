package loader

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"samplingkernel/internal/errs"
)

func rejectUnknownFields(raw any, dtoType reflect.Type, version, path string) error {
	if dtoType.Kind() == reflect.Pointer {
		dtoType = dtoType.Elem()
	}

	value := reflect.ValueOf(raw)
	switch value.Kind() {
	case reflect.Map:
		if dtoType.Kind() != reflect.Struct {
			return nil
		}
		allowed := map[string]reflect.StructField{}
		for i := 0; i < dtoType.NumField(); i++ {
			field := dtoType.Field(i)
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				name = field.Name
			}
			allowed[name] = field
		}

		iter := value.MapRange()
		for iter.Next() {
			key, ok := iter.Key().Interface().(string)
			if !ok {
				continue
			}
			field, ok := allowed[key]
			childPath := path + "." + key
			if !ok {
				return errs.New(errs.SchemaUnknownField, "field is not declared by schema "+version).
					WithVersion(version).AtPath(childPath)
			}
			fieldType := field.Type
			if err := rejectUnknownFields(iter.Value().Interface(), fieldType, version, childPath); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if dtoType.Kind() != reflect.Slice && dtoType.Kind() != reflect.Array {
			return nil
		}
		elemType := dtoType.Elem()
		for i := 0; i < value.Len(); i++ {
			if err := rejectUnknownFields(value.Index(i).Interface(), elemType, version, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func unmarshalStrict(data []byte, dto any, version string) error {
	var raw any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return errs.Wrap(errs.SchemaInvalidValue, "invalid JSON", err).WithVersion(version).AtPath("$")
	}
	if err := rejectUnknownFields(raw, reflect.TypeOf(dto), version, "$"); err != nil {
		return err
	}
	if err := json.Unmarshal(data, dto); err != nil {
		return errs.Wrap(errs.SchemaInvalidValue, "type conversion failed", err).WithVersion(version).AtPath("$")
	}
	return nil
}
