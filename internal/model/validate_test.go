package model_test

import (
	"encoding/json"
	"testing"

	"retargetlab/internal/generator"
	"retargetlab/internal/model"
)

func generatedWorkspace(t *testing.T) model.Workspace {
	t.Helper()
	ws, err := generator.Generate(generator.Options{Seed: 7, FrameCount: 16, PhaseSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func workspaceJSONMap(t *testing.T, ws model.Workspace) map[string]any {
	t.Helper()
	b, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func decodeMap(t *testing.T, root map[string]any) (model.Workspace, error) {
	t.Helper()
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return model.DecodeWorkspace(b)
}

func TestDecodeMissingJointFieldReturnsSchemaError(t *testing.T) {
	ws := generatedWorkspace(t)
	root := workspaceJSONMap(t, ws)
	source := root["source"].(map[string]any)
	joints := source["joints"].([]any)
	delete(joints[2].(map[string]any), "localOffset")
	_, err := decodeMap(t, root)
	if !model.IsCode(err, model.E_SCHEMA) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
}

func TestCycleReturnsCycleErrorWithJoint(t *testing.T) {
	ws := generatedWorkspace(t)
	ws.Source.Joints[0].Parent = 1
	err := model.ValidateSkeleton(ws.Source)
	if !model.IsCode(err, model.E_CYCLE) {
		t.Fatalf("expected E_CYCLE, got %v", err)
	}
	var ce *model.CodedError
	if !errorsAs(err, &ce) || ce.Joint == "" {
		t.Fatalf("cycle error does not identify a joint: %+v", ce)
	}
}

func TestFrameRotationCountReturnsFrameError(t *testing.T) {
	ws := generatedWorkspace(t)
	ws.Clip.Frames[5].LocalRotations = ws.Clip.Frames[5].LocalRotations[:len(ws.Clip.Frames[5].LocalRotations)-1]
	err := model.ValidateClip(ws.Source, ws.Clip)
	if !model.IsCode(err, model.E_FRAME) {
		t.Fatalf("expected E_FRAME, got %v", err)
	}
	var ce *model.CodedError
	if !errorsAs(err, &ce) || !ce.HasFrame || ce.Frame != 5 {
		t.Fatalf("frame error does not identify frame 5: %+v", ce)
	}
}

func TestMappingUnknownJointAndIllegalFallback(t *testing.T) {
	ws := generatedWorkspace(t)
	target := ws.Targets[0].Skeleton
	bad := ws.Mapping.Clone()
	bad.Rules[0].Source = "not_a_joint"
	if err := model.ValidateMapping(ws.Source, target, bad); !model.IsCode(err, model.E_SCHEMA) {
		t.Fatalf("unknown source joint was accepted: %v", err)
	}
	bad = ws.Mapping.Clone()
	for i := range bad.Missing {
		if bad.Missing[i].Policy == model.MissingInheritParent {
			bad.Missing[i].Policy = "teleport"
		}
	}
	if err := model.ValidateMapping(ws.Source, target, bad); !model.IsCode(err, model.E_SCHEMA) {
		t.Fatalf("illegal fallback policy was accepted: %v", err)
	}
}

func TestGeneratedWorkspaceIncludesOneToManyAndFallback(t *testing.T) {
	ws := generatedWorkspace(t)
	foundMany := false
	for _, r := range ws.Mapping.Rules {
		if len(r.Targets) > 1 {
			foundMany = true
		}
	}
	if !foundMany || len(ws.Mapping.Missing) == 0 {
		t.Fatalf("sample does not exercise one-to-many and fallback mappings")
	}
	if err := model.ValidateWorkspace(ws); err != nil {
		t.Fatal(err)
	}
}

// Keep the test independent from errors.As imports in older reader snippets.
func errorsAs(err error, target any) bool {
	switch t := target.(type) {
	case **model.CodedError:
		if ce, ok := err.(*model.CodedError); ok {
			*t = ce
			return true
		}
	}
	return false
}
