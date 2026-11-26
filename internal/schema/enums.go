package schema

import (
	"fmt"

	"screenrelay/internal/model"
)

func parseV1Scale(v any, path string) (model.ScaleTier, error) {
	n, err := asInt(v, path)
	if err != nil {
		return "", err
	}
	switch n {
	case 1:
		return model.Scale100, nil
	case 2:
		return model.Scale200, nil
	default:
		return "", model.SchemaError(path, "unsupported v1 zoom code %v", v)
	}
}

func v1ScaleCode(scale model.ScaleTier) (int64, bool) {
	switch scale {
	case model.Scale100:
		return 1, true
	case model.Scale200:
		return 2, true
	default:
		return 0, false
	}
}

func parseV1Chroma(v any, path string) (model.Chroma, error) {
	n, err := asInt(v, path)
	if err != nil {
		return "", err
	}
	switch n {
	case 0:
		return model.Chroma420, nil
	case 1:
		return model.Chroma444, nil
	default:
		return "", model.SchemaError(path, "unsupported v1 color code %v", v)
	}
}

func v1ChromaCode(chroma model.Chroma) (int64, bool) {
	switch chroma {
	case model.Chroma420:
		return 0, true
	case model.Chroma444:
		return 1, true
	default:
		return 0, false
	}
}

func parseV2Scale(v any, path string) (model.ScaleTier, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	switch s {
	case "x1":
		return model.Scale100, nil
	case "x1_25":
		return model.Scale125, nil
	case "x1_5":
		return model.Scale150, nil
	case "x2":
		return model.Scale200, nil
	default:
		return "", model.SchemaError(path, "unsupported v2 scale_tier %q", s)
	}
}

func v2ScaleCode(scale model.ScaleTier) (string, bool) {
	switch scale {
	case model.Scale100:
		return "x1", true
	case model.Scale125:
		return "x1_25", true
	case model.Scale150:
		return "x1_5", true
	case model.Scale200:
		return "x2", true
	default:
		return "", false
	}
}

func parseV2Chroma(v any, path string) (model.Chroma, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	switch s {
	case "420":
		return model.Chroma420, nil
	case "422":
		return model.Chroma422, nil
	case "444":
		return model.Chroma444, nil
	default:
		return "", model.SchemaError(path, "unsupported v2 chroma %q", s)
	}
}

func v2ChromaCode(chroma model.Chroma) (string, bool) {
	switch chroma {
	case model.Chroma420:
		return "420", true
	case model.Chroma422:
		return "422", true
	case model.Chroma444:
		return "444", true
	default:
		return "", false
	}
}

func parseV3Scale(v any, path string) (model.ScaleTier, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	tier := model.ScaleTier(s)
	if _, ok := tier.Percent(); !ok {
		return "", model.SchemaError(path, "unsupported scale tier %q", s)
	}
	return tier, nil
}

func parseV3Chroma(v any, path string) (model.Chroma, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	switch model.Chroma(s) {
	case model.Chroma420, model.Chroma422, model.Chroma444:
		return model.Chroma(s), nil
	default:
		return "", model.SchemaError(path, "unsupported chroma %q", s)
	}
}

func asEnumList[T any](raw any, path string, parse func(any, string) (T, error)) ([]T, error) {
	items, err := asSlice(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for i, item := range items {
		value, err := parse(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
