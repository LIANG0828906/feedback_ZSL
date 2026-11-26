package schema_test

import (
	"strings"
	"testing"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/generator"
	"acousticfield/internal/schema"
)

func TestGeneratedSampleValid(t *testing.T) {
	level, _ := generator.Generate(generator.DefaultSeed)
	if err := schema.ValidateLevel(level); err != nil {
		t.Fatalf("generated sample must validate: %v", err)
	}
}

func TestSchemaErrorsIdentifyObjectAndField(t *testing.T) {
	t.Run("alpha", func(t *testing.T) {
		level, _ := generator.Generate(generator.DefaultSeed)
		level.Chunks[0].Bands[0].AlphaQ = 123
		err := schema.ValidateLevel(level)
		ae, ok := acoustic.AsError(err)
		if !ok || ae.Code != acoustic.CodeSchema {
			t.Fatalf("expected E_SCHEMA, got %v", err)
		}
		if ae.ObjectID != "C00" || !strings.Contains(ae.Field, "alpha_q") {
			t.Fatalf("unexpected object/field: %+v", ae)
		}
	})

	t.Run("normal", func(t *testing.T) {
		level, _ := generator.Generate(generator.DefaultSeed)
		level.Chunks[0].Surfaces[0].Normal = acoustic.Point{X: 1, Y: 0}
		err := schema.ValidateLevel(level)
		ae, _ := acoustic.AsError(err)
		if ae == nil || ae.Code != acoustic.CodeSchema || !strings.Contains(ae.Field, "normal_q") {
			t.Fatalf("expected normal E_SCHEMA, got %v", err)
		}
	})

	t.Run("overlap", func(t *testing.T) {
		level, _ := generator.Generate(generator.DefaultSeed)
		level.Chunks[1].Bounds.Min.X = level.Chunks[0].Bounds.Max.X - 1000
		err := schema.ValidateLevel(level)
		ae, _ := acoustic.AsError(err)
		if ae == nil || ae.Code != acoustic.CodeSchema || ae.Field != "bounds" {
			t.Fatalf("expected bounds E_SCHEMA, got %v", err)
		}
	})

	t.Run("missing id json", func(t *testing.T) {
		_, err := schema.LoadLevel([]byte(`{"schema_version":"acoustic-level-v1","band_ids":[0],"bounds":{"min":{"x_q":0,"y_q":0},"max":{"x_q":10000,"y_q":10000}},"chunks":[{}]}`))
		ae, _ := acoustic.AsError(err)
		if ae == nil || ae.Code != acoustic.CodeSchema || ae.Field != "id" {
			t.Fatalf("expected missing id E_SCHEMA, got %v", err)
		}
	})
}
