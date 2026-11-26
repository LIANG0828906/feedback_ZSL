package schema

import (
	"encoding/base64"
	"fmt"

	"maskqc/model"
)

func decodeRawMask(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 mask: %w", err)
	}
	return data, nil
}

func encodeRawMask(mask []byte) string {
	return base64.StdEncoding.EncodeToString(mask)
}

func maskObject(mask []byte) map[string]any {
	return map[string]any{
		"encoding": "raw8-base64",
		"data":     encodeRawMask(mask),
	}
}

func addIntTrace(report *model.ConversionReport, field, objectID string, from, to model.SchemaVersion, a, b, rr, result int64, rounding string) {
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: field, ObjectID: objectID, From: string(from), To: string(to),
		OperandA: a, OperandB: b, Quotient: 0, Remainder: rr, Result: result,
		Rounding: rounding, Source: from, Target: to,
	})
}

func addScaleTrace(report *model.ConversionReport, field, objectID string, from, to model.SchemaVersion, a, b, quotient, remainder, result int64, rounding string) {
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: field, ObjectID: objectID, From: string(from), To: string(to),
		OperandA: a, OperandB: b, Quotient: quotient, Remainder: remainder, Result: result,
		Rounding: rounding, Source: from, Target: to,
	})
}

func safeAdd(a, b int64) (int64, bool) {
	result := a + b
	if (b > 0 && result < a) || (b < 0 && result > a) {
		return 0, false
	}
	return result, true
}

func safeSub(a, b int64) (int64, bool) {
	if b == -1<<63 {
		if a >= 0 {
			return 0, false
		}
		return a - b, true
	}
	return safeAdd(a, -b)
}

func safeMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	result := a * b
	if result/b != a {
		return 0, false
	}
	return result, true
}
