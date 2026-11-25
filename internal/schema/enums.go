package schema

import "obsproj/internal/model"

var v1ScaleStrings = map[string]model.Scale{
	"1x":   model.Scale100,
	"1.5x": model.Scale150,
	"2x":   model.Scale200,
}

var v1StringScales = map[model.Scale]string{
	model.Scale100: "1x",
	model.Scale150: "1.5x",
	model.Scale200: "2x",
}

var v1ChromaStrings = map[string]model.Chroma{
	"4:2:0": model.Chroma420,
	"4:2:2": model.Chroma422,
}

var v1StringChromas = map[model.Chroma]string{
	model.Chroma420: "4:2:0",
	model.Chroma422: "4:2:2",
}

var v2ScaleValues = map[int64]model.Scale{
	100: model.Scale100,
	125: model.Scale125,
	150: model.Scale150,
	200: model.Scale200,
}

var v2ChromaValues = map[int64]model.Chroma{
	1: model.Chroma420,
	2: model.Chroma422,
	3: model.Chroma444,
}

var v2IntChromas = map[model.Chroma]int64{
	model.Chroma420: 1,
	model.Chroma422: 2,
	model.Chroma444: 3,
}

var v3ScaleOrdinals = []model.Scale{
	model.Scale100,
	model.Scale125,
	model.Scale150,
	model.Scale200,
}

var v3ScaleOrdinal = map[model.Scale]int64{
	model.Scale100: 0,
	model.Scale125: 1,
	model.Scale150: 2,
	model.Scale200: 3,
}

var v3ChromaOrdinals = []model.Chroma{
	model.Chroma420,
	model.Chroma422,
	model.Chroma444,
}

var v3ChromaOrdinal = map[model.Chroma]int64{
	model.Chroma420: 0,
	model.Chroma422: 1,
	model.Chroma444: 2,
}

var v1SupportedScales = map[model.Scale]bool{
	model.Scale100: true,
	model.Scale150: true,
	model.Scale200: true,
}

var v1SupportedChromas = map[model.Chroma]bool{
	model.Chroma420: true,
	model.Chroma422: true,
}

func v2ScaleFromInt(v int64, version, field string) (model.Scale, *model.KernelError) {
	scale, ok := v2ScaleValues[v]
	if !ok {
		return 0, schemaError(version, field, field, "unknown scale encoding")
	}
	return scale, nil
}

func v2ChromaFromInt(v int64, version, field string) (model.Chroma, *model.KernelError) {
	chroma, ok := v2ChromaValues[v]
	if !ok {
		return 0, schemaError(version, field, field, "unknown chroma encoding")
	}
	return chroma, nil
}

func v3ScaleFromOrdinal(v int64, version, field string) (model.Scale, *model.KernelError) {
	if v < 0 || v >= int64(len(v3ScaleOrdinals)) {
		return 0, schemaError(version, field, field, "scale ordinal out of range")
	}
	return v3ScaleOrdinals[v], nil
}

func v3ChromaFromOrdinal(v int64, version, field string) (model.Chroma, *model.KernelError) {
	if v < 0 || v >= int64(len(v3ChromaOrdinals)) {
		return 0, schemaError(version, field, field, "chroma ordinal out of range")
	}
	return v3ChromaOrdinals[v], nil
}
