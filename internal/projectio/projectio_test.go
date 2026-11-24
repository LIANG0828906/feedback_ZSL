package projectio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/sample"
)

func TestGeneratedProjectValidatesAndRoundTrips(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	if err := Validate(project); err != nil {
		t.Fatalf("generated project should validate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "project.json")
	if err := Save(path, project); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Seed != project.Seed || len(loaded.Blocks) != len(project.Blocks) || len(loaded.Seams) != len(project.Seams) {
		t.Fatal("round trip changed project structure")
	}
}

func TestSchemaErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Project)
		code   string
		field  string
		block  int
	}{
		{
			name: "missing schema",
			mutate: func(p *model.Project) {
				p.SchemaVersion = ""
			},
			code:  model.ErrSchema,
			field: "schemaVersion",
			block: -1,
		},
		{
			name: "height size",
			mutate: func(p *model.Project) {
				p.Blocks[0].Heights = p.Blocks[0].Heights[:len(p.Blocks[0].Heights)-1]
			},
			code:  model.ErrSize,
			field: "heights",
			block: 1,
		},
		{
			name: "missing heights",
			mutate: func(p *model.Project) {
				p.Blocks[0].Heights = nil
			},
			code:  model.ErrSchema,
			field: "heights",
			block: 1,
		},
		{
			name: "weight range",
			mutate: func(p *model.Project) {
				p.Blocks[0].Weights[0][0] = 1001
			},
			code:  model.ErrWeight,
			field: "weights[0][0]",
			block: 1,
		},
		{
			name: "missing seam block",
			mutate: func(p *model.Project) {
				p.Seams[0].BlockB = 99
			},
			code:  model.ErrSchema,
			field: "seams[0].blockB",
			block: 99,
		},
		{
			name: "edge length",
			mutate: func(p *model.Project) {
				block := &p.Blocks[1]
				block.Height++
				lastHeight := block.Heights[len(block.Heights)-1]
				lastWeights := append([]int(nil), block.Weights[len(block.Weights)-1]...)
				for i := 0; i < block.Width; i++ {
					block.Heights = append(block.Heights, lastHeight)
					block.Weights = append(block.Weights, append([]int(nil), lastWeights...))
				}
			},
			code:  model.ErrSize,
			field: "seams[0].edgeSamples",
			block: 1,
		},
		{
			name: "invalid operator",
			mutate: func(p *model.Project) {
				p.Parameters.DifferenceOperator = "invalid"
			},
			code:  model.ErrPara,
			field: "parameters.differenceOperator",
			block: -1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := sample.Generate(sample.DefaultSeed)
			test.mutate(project)
			err := Validate(project)
			var typed *model.Error
			if !errors.As(err, &typed) {
				t.Fatalf("expected structured model error, got %v", err)
			}
			if typed.Code != test.code || typed.Field != test.field || typed.BlockID != test.block {
				t.Fatalf("got %s block=%d field=%s, want %s block=%d field=%s",
					typed.Code, typed.BlockID, typed.Field, test.code, test.block, test.field)
			}
		})
	}
}

func TestMalformedJSONReturnsSchemaError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.ErrSchema {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
}

func TestDecimalWeightReturnsWeightError(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	blocks := raw["blocks"].([]any)
	weights := blocks[0].(map[string]any)["weights"].([]any)
	weights[0].([]any)[0] = 0.5
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decimal-weight.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.ErrWeight || typed.BlockID != 1 {
		t.Fatalf("expected E_WEIGHT for block 1, got %v", err)
	}
}
