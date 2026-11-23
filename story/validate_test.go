package story_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"storyworkbench/story"
)

const samplePath = "../samples/glass-meridian-20261124.json"

func readSample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	return data
}

func sampleObject(t *testing.T) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(readSample(t), &obj); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	return obj
}

func parseObject(t *testing.T, obj map[string]any) (*story.Project, error) {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal mutated sample: %v", err)
	}
	return story.ParseProject(data)
}

func findNode(t *testing.T, obj map[string]any, id string) map[string]any {
	t.Helper()
	nodes := obj["nodes"].([]any)
	for _, item := range nodes {
		node := item.(map[string]any)
		if node["id"] == id {
			return node
		}
	}
	t.Fatalf("node %s not found", id)
	return nil
}

func assertError(t *testing.T, err error, code, nodeID, fieldPart string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	appErr, ok := story.AsError(err)
	if !ok {
		t.Fatalf("expected *story.Error, got %T: %v", err, err)
	}
	if appErr.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, appErr.Code, err)
	}
	if nodeID != "" && appErr.NodeID != nodeID {
		t.Fatalf("expected node %s, got %s (%v)", nodeID, appErr.NodeID, err)
	}
	if fieldPart != "" && !strings.Contains(appErr.Field, fieldPart) {
		t.Fatalf("expected field containing %s, got %s (%v)", fieldPart, appErr.Field, err)
	}
}

func TestLoadSampleAndContentHashStable(t *testing.T) {
	project, err := story.LoadProject(samplePath)
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	if project.SchemaVersion != story.ProjectSchemaVersion {
		t.Fatalf("unexpected schema version: %d", project.SchemaVersion)
	}
	if project.Truth == nil || project.Truth.ShotCount == 0 {
		t.Fatal("sample truth is missing")
	}
	hash1 := story.ContentHash(project)
	clone, err := story.CloneProject(project)
	if err != nil {
		t.Fatalf("clone project: %v", err)
	}
	if hash1 != story.ContentHash(clone) {
		t.Fatal("content hash is not stable across clone")
	}
	topo, err := story.TopologicalOrder(project)
	if err != nil {
		t.Fatalf("topological order: %v", err)
	}
	if len(topo) != len(project.Nodes) {
		t.Fatalf("topological order has %d nodes, want %d", len(topo), len(project.Nodes))
	}
}

func TestMissingShotDurationIsSchemaError(t *testing.T) {
	obj := sampleObject(t)
	node := findNode(t, obj, "arrival")
	shot := node["shots"].([]any)[0].(map[string]any)
	delete(shot, "duration_ms")
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodeSchema, "arrival", "duration_ms")
}

func TestFractionalDurationIsParameterError(t *testing.T) {
	obj := sampleObject(t)
	node := findNode(t, obj, "arrival")
	shot := node["shots"].([]any)[0].(map[string]any)
	shot["duration_ms"] = json.Number("10.5")
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodePara, "arrival", "duration_ms")
}

func TestNegativeDurationIsParameterError(t *testing.T) {
	obj := sampleObject(t)
	node := findNode(t, obj, "arrival")
	shot := node["shots"].([]any)[0].(map[string]any)
	shot["duration_ms"] = json.Number("-1")
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodePara, "arrival", "duration_ms")
}

func TestGraphCycleIsCycleError(t *testing.T) {
	obj := sampleObject(t)
	arrival := findNode(t, obj, "arrival")
	arrival["join"] = []any{"dawn"}
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodeCycle, "", "join")
}

func TestMonotonicChoiceTimestamps(t *testing.T) {
	obj := sampleObject(t)
	choices := obj["choices"].([]any)
	second := choices[1].(map[string]any)
	second["at_ms"] = json.Number("0")
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodePara, "whisper", "at_ms")
}

func TestMissingChoiceTargetIsSchemaError(t *testing.T) {
	obj := sampleObject(t)
	choices := obj["choices"].([]any)
	first := choices[0].(map[string]any)
	first["target"] = "does-not-exist"
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodeSchema, "does-not-exist", "target")
}

func TestDuplicateNodeIDIsSchemaError(t *testing.T) {
	obj := sampleObject(t)
	nodes := obj["nodes"].([]any)
	first := nodes[0].(map[string]any)
	second := nodes[1].(map[string]any)
	second["id"] = first["id"]
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodeSchema, "arrival", "id")
}

func TestUnknownCharacterReferenceIsSchemaError(t *testing.T) {
	obj := sampleObject(t)
	node := findNode(t, obj, "whisper")
	event := node["events"].([]any)[0].(map[string]any)
	event["kind"] = "set_character"
	event["character"] = "nobody"
	_, err := parseObject(t, obj)
	assertError(t, err, story.CodeSchema, "whisper", "character")
}

func TestLoadMissingFile(t *testing.T) {
	_, err := story.LoadProject(filepath.Join(t.TempDir(), "missing.json"))
	assertError(t, err, story.CodeSchema, "", "file")
}
