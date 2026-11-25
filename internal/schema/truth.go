package schema

import (
	"encoding/json"

	"combatjudge/internal/model"
)

func LoadTruth(data []byte) (*model.TruthFile, error) {
	var truth model.TruthFile
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, FieldPath: "$", SourceVersion: "truth-v1", Message: "truth JSON decode failed: " + err.Error()}
	}
	if truth.Schema == "" {
		return nil, model.NewError(model.CodeSchema, "$.schema", "truth-v1", "truth schema marker is required")
	}
	if truth.BattleID == "" {
		return nil, model.NewError(model.CodeSchema, "$.battle_id", "truth-v1", "truth battle_id is required")
	}
	if truth.ToleranceQ < 0 {
		return nil, model.NewError(model.CodeSchema, "$.tolerance_q", "truth-v1", "tolerance must be non-negative")
	}
	for i, sample := range truth.Samples {
		if sample.Frame <= 0 || sample.SweepID == "" {
			return nil, model.NewError(model.CodeSchema, "$.samples["+itoa(i)+"]", "truth-v1", "truth sample requires positive frame and sweep_id")
		}
	}
	return &truth, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
