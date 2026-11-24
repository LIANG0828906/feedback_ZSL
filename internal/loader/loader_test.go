package loader

import (
	"encoding/json"
	"math"
	"testing"

	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/werr"
)

func TestParseValidManifest(t *testing.T) {
	data := manifestJSON(t)
	manifest, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(manifest.Samples) == 0 {
		t.Fatal("no samples")
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Run("missing field", func(t *testing.T) {
		root := manifestMap(t)
		sample := root["samples"].([]any)[0].(map[string]any)
		delete(sample, "sampleRate")
		_, err := Parse(marshalMap(t, root))
		assertCode(t, err, werr.CodeDecode)
		if err.Field != "samples[0].sampleRate" {
			t.Fatalf("field = %q", err.Field)
		}
	})
	t.Run("point count", func(t *testing.T) {
		root := manifestMap(t)
		sample := root["samples"].([]any)[0].(map[string]any)
		channel := sample["channels"].([]any)[0].(map[string]any)
		channel["samples"] = append(channel["samples"].([]any), 0.0)
		_, err := Parse(marshalMap(t, root))
		assertCode(t, err, werr.CodeDecode)
	})
	t.Run("rate", func(t *testing.T) {
		root := manifestMap(t)
		sample := root["samples"].([]any)[0].(map[string]any)
		sample["sampleRate"] = 4000
		_, err := Parse(marshalMap(t, root))
		assertCode(t, err, werr.CodeRate)
		if err.Actual != 4000 || err.Min == nil || *err.Min != 8000 || err.Max == nil || *err.Max != 192000 {
			t.Fatalf("rate context missing: actual=%v min=%v max=%v", err.Actual, err.Min, err.Max)
		}
	})
	t.Run("order", func(t *testing.T) {
		root := manifestMap(t)
		sample := root["samples"].([]any)[0].(map[string]any)
		channel := sample["channels"].([]any)[0].(map[string]any)
		truth := channel["truth"].(map[string]any)
		truth["reflections"] = []any{map[string]any{"arrival": 1, "energy": 0.01}}
		_, err := Parse(marshalMap(t, root))
		assertCode(t, err, werr.CodeOrder)
	})
	t.Run("duplicate channel", func(t *testing.T) {
		root := manifestMap(t)
		sample := root["samples"].([]any)[0].(map[string]any)
		layout := sample["layout"].(map[string]any)
		channels := layout["channels"].([]any)
		channels[1] = channels[0]
		_, err := Parse(marshalMap(t, root))
		assertCode(t, err, werr.CodeChan)
	})
}

func manifestJSON(t *testing.T) []byte {
	t.Helper()
	data, err := generator.Marshal(generator.Generate(generator.Options{Seed: 7, Samples: 1, Prefix: "loader"}))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func manifestMap(t *testing.T) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(manifestJSON(t), &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func marshalMap(t *testing.T, root map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertCode(t *testing.T, err *werr.Error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if err.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, err.Code, err)
	}
}

func TestFiniteTruth(t *testing.T) {
	manifest, err := Parse(manifestJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range manifest.Samples {
		for _, ch := range sample.Channels {
			if math.IsNaN(ch.Truth.DRRDB) || math.IsInf(ch.Truth.DRRDB, 0) {
				t.Fatalf("non-finite DRR for %s", ch.ID)
			}
		}
	}
}
