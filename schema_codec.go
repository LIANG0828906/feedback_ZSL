package maskqa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/draw"
	"image/png"
)

// LoadProject detects a schema version and loads the unified model.
func LoadProject(data []byte) (*LoadResult, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, decodeError("$", "invalid JSON: %v", err)
	}
	version, field, err := detectVersion(envelope)
	if err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		return loadV1(data)
	case SchemaV2:
		return loadV2(data)
	case SchemaV3:
		return loadV3(data)
	default:
		return nil, &Error{Code: CodeDecode, Field: field, Message: "unknown schema version", Actual: version}
	}
}

func detectVersion(envelope map[string]json.RawMessage) (string, string, error) {
	for _, candidate := range []struct{ key, value string }{
		{"schema", SchemaV1}, {"schemaVersion", SchemaV2}, {"schema_version", SchemaV3},
	} {
		raw, ok := envelope[candidate.key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", candidate.key, decodeError(candidate.key, "schema version must be a string")
		}
		switch value {
		case SchemaV1, SchemaV2, SchemaV3:
			return value, candidate.key, nil
		default:
			return value, candidate.key, &Error{
				Code: CodeDecode, Field: candidate.key,
				Message: "unknown schema version", Actual: value,
			}
		}
	}
	return "", "schema_version", decodeError("schema_version", "required field missing: no supported schema key")
}

func requiredValue[T any](value *T, field string) (T, error) {
	var zero T
	if value == nil {
		return zero, decodeError(field, "required field missing")
	}
	return *value, nil
}

func decodeGray(field string, width, height *int, encoded string) (*image.Gray, error) {
	w, err := requiredValue(width, field+".width")
	if err != nil {
		return nil, err
	}
	h, err := requiredValue(height, field+".height")
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 {
		return nil, decodeError(field, "width and height must be positive")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, decodeError(field+".gray8_base64", "invalid base64: %v", err)
	}
	if len(raw) != w*h {
		return nil, decodeError(field, "pixel payload length %d does not match %dx%d", len(raw), w, h)
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	copy(img.Pix, raw)
	return img, nil
}

func encodeGray(img *image.Gray) (int, int, string) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	tight := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			tight[y*w+x] = img.GrayAt(x, y).Y
		}
	}
	return w, h, base64.StdEncoding.EncodeToString(tight)
}

func decodePNG(field, encoded string) (*image.RGBA, error) {
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, decodeError(field, "invalid icon base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, decodeError(field, "invalid PNG: %v", err)
	}
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	return rgba, nil
}

func encodePNG(img *image.RGBA) (string, error) {
	if img == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func defaultTolerancesIfNil(in *Tolerances) Tolerances {
	if in == nil {
		return DefaultTolerances()
	}
	out := *in
	def := DefaultTolerances()
	if out.AreaRelPermyriad == 0 {
		out.AreaRelPermyriad = def.AreaRelPermyriad
	}
	if out.CentroidMilliPx == 0 {
		out.CentroidMilliPx = def.CentroidMilliPx
	}
	if out.BBoxPx == 0 {
		out.BBoxPx = def.BBoxPx
	}
	if out.DirectionScorePermyriad == 0 {
		out.DirectionScorePermyriad = def.DirectionScorePermyriad
	}
	return out
}

func addDefaultTrace(traces *[]FieldTrace, version, path, sourceField, targetField string, defaultValue any, note string) {
	*traces = append(*traces, FieldTrace{
		SourceVersion: version, Path: path, SourceField: sourceField,
		TargetField: targetField, Rule: "default_when_missing",
		OldValue: "<missing>", NewValue: defaultValue, DefaultValue: defaultValue, Note: note,
	})
}

func addFieldTrace(traces *[]FieldTrace, version, path, sourceField, targetField, rule string, oldValue, newValue any) {
	*traces = append(*traces, FieldTrace{
		SourceVersion: version, Path: path, SourceField: sourceField,
		TargetField: targetField, Rule: rule, OldValue: oldValue, NewValue: newValue,
	})
}
