package project_test

import (
	"encoding/json"
	"strings"
	"testing"

	"terrainbake/generator"
	"terrainbake/model"
	"terrainbake/project"
)

func generatedJSON(t *testing.T) (model.SampleBundle, []byte) {
	t.Helper()
	bundle, err := generator.Generate(77)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := generator.MarshalBundle(bundle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bundle, data
}

func TestGeneratedBundleLoads(t *testing.T) {
	bundle, data := generatedJSON(t)
	loaded, err := project.LoadBundle(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Truth.Points) != len(bundle.Truth.Points) {
		t.Fatalf("truth points=%d", len(loaded.Truth.Points))
	}
	if model.ProjectIdentity(loaded.Project) != model.ProjectIdentity(bundle.Project) {
		t.Fatal("project identity changed during load")
	}
}

func TestGeneratorIsDeterministic(t *testing.T) {
	a, err := generator.Generate(991)
	if err != nil {
		t.Fatalf("generate a: %v", err)
	}
	b, err := generator.Generate(991)
	if err != nil {
		t.Fatalf("generate b: %v", err)
	}
	ja, _ := generator.MarshalBundle(a)
	jb, _ := generator.MarshalBundle(b)
	if string(ja) != string(jb) {
		t.Fatal("same seed produced different samples")
	}
	c, _ := generator.Generate(992)
	jc, _ := generator.MarshalBundle(c)
	if string(ja) == string(jc) {
		t.Fatal("different seed produced identical samples")
	}
}

func TestMissingChunkFieldReturnsDecodeWithPath(t *testing.T) {
	_, data := generatedJSON(t)
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	projectMap := generic["project"].(map[string]any)
	chunks := projectMap["chunks"].([]any)
	first := chunks[0].(map[string]any)
	delete(first, "heights")
	mutated, _ := json.Marshal(generic)
	_, err := project.LoadBundle(mutated)
	coded, ok := model.AsCoded(err)
	if !ok || coded.Code != model.CodeDecode {
		t.Fatalf("want E_DECODE, got %v", err)
	}
	if coded.ChunkID == "" || !strings.Contains(coded.Field, "heights") {
		t.Fatalf("missing chunk/field context: %+v", coded)
	}
}

func TestTruncatedSampleReturnsDecode(t *testing.T) {
	_, data := generatedJSON(t)
	truncated := data[:len(data)/3]
	_, err := project.LoadBundle(truncated)
	coded, ok := model.AsCoded(err)
	if !ok || coded.Code != model.CodeDecode {
		t.Fatalf("want E_DECODE, got %v", err)
	}
	if coded.Field == "" {
		t.Fatalf("truncated error has no field path: %+v", coded)
	}
}

func TestMissingTruthFieldReturnsDecodeWithChunkAndPath(t *testing.T) {
	_, data := generatedJSON(t)
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	truth := generic["truth"].(map[string]any)
	points := truth["points"].([]any)
	first := points[0].(map[string]any)
	delete(first, "sky_visibility")
	mutated, _ := json.Marshal(generic)
	_, err := project.LoadBundle(mutated)
	coded, ok := model.AsCoded(err)
	if !ok || coded.Code != model.CodeDecode || coded.ChunkID == "" || !strings.Contains(coded.Field, "sky_visibility") {
		t.Fatalf("unexpected truth decode error: %+v", err)
	}
}

func TestSemanticValidationCodes(t *testing.T) {
	bundle, _ := generatedJSON(t)

	t.Run("height count", func(t *testing.T) {
		p := cloneProject(t, bundle.Project)
		p.Chunks[0].Heights = p.Chunks[0].Heights[:len(p.Chunks[0].Heights)-1]
		err := project.ValidateProject(&p)
		coded, _ := model.AsCoded(err)
		if model.CodeOf(err) != model.CodeValue || coded.ChunkID != p.Chunks[0].ID || !strings.Contains(coded.Field, "heights") {
			t.Fatalf("unexpected error: %+v", err)
		}
	})

	t.Run("occluder reference", func(t *testing.T) {
		p := cloneProject(t, bundle.Project)
		p.Chunks[0].Occluders = []string{"missing-pillar"}
		err := project.ValidateProject(&p)
		coded, _ := model.AsCoded(err)
		if model.CodeOf(err) != model.CodeValue || coded.ChunkID != p.Chunks[0].ID || !strings.Contains(coded.Field, "occluders") {
			t.Fatalf("unexpected error: %+v", err)
		}
	})

	t.Run("sun monotonic", func(t *testing.T) {
		p := cloneProject(t, bundle.Project)
		p.Sun[2].AzimuthDeg = p.Sun[0].AzimuthDeg - 1
		err := project.ValidateProject(&p)
		coded, _ := model.AsCoded(err)
		if model.CodeOf(err) != model.CodeValue || !strings.Contains(coded.Field, "azimuth") {
			t.Fatalf("unexpected error: %+v", err)
		}
	})

	t.Run("shared edge", func(t *testing.T) {
		p := cloneProject(t, bundle.Project)
		c := &p.Chunks[0]
		c.Heights[c.VertexIndex(c.ResX-1, 1)] += 2
		err := project.ValidateProject(&p)
		coded, _ := model.AsCoded(err)
		if model.CodeOf(err) != model.CodeSeam || coded.EdgeID == "" {
			t.Fatalf("unexpected error: %+v", err)
		}
	})
}

func cloneProject(t *testing.T, p model.Project) model.Project {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("clone marshal: %v", err)
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("clone unmarshal: %v", err)
	}
	return out
}
