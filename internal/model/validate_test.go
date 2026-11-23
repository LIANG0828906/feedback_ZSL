package model_test

import (
	"encoding/json"
	"errors"
	"testing"

	"inputkernel/internal/model"
	"inputkernel/internal/samplegen"
)

func TestSchemaConflictsAreLocated(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Scenario)
		field  string
	}{
		{
			name: "duplicate axis",
			mutate: func(s *model.Scenario) {
				s.Devices[0].Axes[1].ID = s.Devices[0].Axes[0].ID
			},
			field: "$.devices[0].axes[1].id",
		},
		{
			name: "non-monotonic time",
			mutate: func(s *model.Scenario) {
				s.Events[2].TimeMS = s.Events[1].TimeMS - 1
			},
			field: "$.events[2].time_ms",
		},
		{
			name: "axis out of range",
			mutate: func(s *model.Scenario) {
				s.Events[0].Value = 999999
			},
			field: "$.events[0].value",
		},
		{
			name: "unknown button",
			mutate: func(s *model.Scenario) {
				for i := range s.Events {
					if s.Events[i].Kind == model.EventButtonDown {
						s.Events[i].Button = "NO_SUCH_BUTTON"
						return
					}
				}
			},
			field: "$.events[1].button",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scenario := generatedScenario(t, "dual_stick")
			tt.mutate(scenario)
			err := model.ValidateScenario(scenario)
			assertCodeField(t, err, model.CodeSchema, tt.field)
		})
	}
}

func TestMissingFieldReturnsSchemaError(t *testing.T) {
	scenario := generatedScenario(t, "dual_stick")
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "name")
	data, _ = json.Marshal(raw)
	_, err = model.DecodeScenario(data)
	assertCodeField(t, err, model.CodeSchema, "$.name")
}

func TestInvertedTimingParametersReturnPara(t *testing.T) {
	scenario := generatedScenario(t, "dual_stick")
	for i := range scenario.Bindings {
		if scenario.Bindings[i].Type == model.BindingDoublePress {
			scenario.Bindings[i].Timing.MinGapMS = scenario.Bindings[i].Timing.WindowMS + 1
		}
	}
	err := model.ValidateScenario(scenario)
	assertCodeField(t, err, model.CodeParam, "bindings[6].timing.min_gap_ms")
}

func generatedScenario(t *testing.T, id string) *model.Scenario {
	t.Helper()
	scenarios, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		if scenario.ID == id {
			data, _ := json.Marshal(scenario)
			var clone model.Scenario
			_ = json.Unmarshal(data, &clone)
			return &clone
		}
	}
	t.Fatalf("scenario %s not found", id)
	return nil
}

func assertCodeField(t *testing.T, err error, code, field string) {
	t.Helper()
	var kernelErr *model.Error
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected *model.Error, got %T %v", err, err)
	}
	if kernelErr.Code != code || kernelErr.Field != field {
		t.Fatalf("expected %s at %s, got %+v", code, field, kernelErr)
	}
}
