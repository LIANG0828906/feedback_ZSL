package schema

import (
	"encoding/json"

	"presentkernel/internal/domain"
)

type LoadResult struct {
	Project *domain.Project         `json:"project"`
	Trace   *domain.ConversionTrace `json:"trace"`
}

type ExportResult struct {
	Version string                  `json:"version"`
	Data    []byte                  `json:"-"`
	Trace   *domain.ConversionTrace `json:"trace"`
	Lossy   []domain.LossyIssue     `json:"lossy"`
}

func addField(t *domain.ConversionTrace, path, source, target, converter string, old, next any, truncated bool, remainder int64) {
	t.Fields = append(t.Fields, domain.FieldTrace{
		Path: path, SourceField: source, TargetField: target, Converter: converter,
		OldValue: old, NewValue: next, Truncated: truncated, Remainder: remainder,
	})
}

func addFill(t *domain.ConversionTrace, path string, defaultValue any, source string) {
	t.Fills = append(t.Fills, domain.DefaultFill{Path: path, OldValue: nil, DefaultValue: defaultValue, Source: source})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func decodeStrict(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
