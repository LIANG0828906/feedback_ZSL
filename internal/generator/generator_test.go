package generator_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
)

func TestGenerationIsDeterministic(t *testing.T) {
	cfg := generator.DefaultConfig(1042)
	first, err := generator.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash {
		t.Fatalf("same seed produced different content hashes: %s != %s",
			first.ContentHash, second.ContentHash)
	}
	if !reflect.DeepEqual(first.World, second.World) {
		t.Fatal("same seed produced different world data")
	}
	if !reflect.DeepEqual(first.Truth, second.Truth) {
		t.Fatal("same seed produced different truth data")
	}
	other, err := generator.Generate(generator.DefaultConfig(1043))
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash == other.ContentHash {
		t.Fatal("different seeds unexpectedly produced the same project")
	}
}

func TestTruthCoversEveryWindowAndChunk(t *testing.T) {
	project, err := generator.Generate(generator.DefaultConfig(1042))
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Truth.Samples) != len(project.World.Cameras) {
		t.Fatal("truth sample count does not match camera trajectory")
	}
	for i, sample := range project.Truth.Samples {
		if len(sample.Chunks) != len(project.World.Chunks) {
			t.Fatalf("window %d truth does not cover all chunks", i)
		}
		for j, chunk := range sample.Chunks {
			if chunk.Chunk != j {
				t.Fatalf("window %d truth chunk order unstable", i)
			}
		}
	}
	if len(project.Truth.KnownProjections) != len(project.World.Cameras)*len(project.World.Chunks) {
		t.Fatal("known projection count mismatch")
	}
}

func TestWriteAndLoadProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project")
	project, err := generator.GenerateToDir(dir, generator.DefaultConfig(1042))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := schema.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContentHash != project.ContentHash {
		t.Fatal("loaded project hash differs")
	}
	if !reflect.DeepEqual(loaded.World, project.World) {
		t.Fatal("loaded world differs")
	}
}

func TestInvalidQuantizationIsParameterError(t *testing.T) {
	cfg := generator.DefaultConfig(1042)
	cfg.Quantization.WorldFracBits = 1
	_, err := generator.Generate(cfg)
	if err == nil || err.Code != model.CodeParameters || err.Field != "quantization.world_frac_bits" {
		t.Fatalf("expected E_PARA at quantization.world_frac_bits, got %+v", err)
	}
}
