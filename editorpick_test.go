package editorpick_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"editorpick"
	"editorpick/internal/samplegen"
	"editorpick/internal/verify"
)

func generateTestSample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := samplegen.Generate(dir, samplegen.DefaultSeed); err != nil {
		t.Fatalf("generate sample: %v", err)
	}
	return dir
}

func domainCode(t *testing.T, err error) string {
	t.Helper()
	var de *editorpick.DomainError
	if !errors.As(err, &de) {
		t.Fatalf("expected DomainError, got %T: %v", err, err)
	}
	return de.Code
}

func TestSampleVerificationReport(t *testing.T) {
	dir := generateTestSample(t)
	reportPath := filepath.Join(dir, "report.json")
	report, err := verify.Run(dir, reportPath)
	if err != nil {
		t.Fatalf("verify run: %v", err)
	}
	if !report.OK {
		t.Fatalf("verification failed: %s", report.FirstFailure)
	}
	if !report.DeterministicGenerate || !report.FileHashesEqual {
		t.Fatalf("generator is not deterministic or report sample is stale")
	}
	if !report.Concurrency.SerialSame || !report.Concurrency.DerivedSame {
		t.Fatalf("parallel verification failed: %+v", report.Concurrency)
	}
}

func TestSchemaCycleAndReferenceCodes(t *testing.T) {
	identity := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	precision := editorpick.DefaultPrecision()

	missingID := editorpick.SceneFile{
		Schema: editorpick.SceneSchemaV1, StructureVersion: editorpick.StructureVersion,
		Precision: precision, Nodes: []editorpick.NodeDTO{{Matrix: identity}},
	}
	_, err := editorpick.ParseScene(mustJSON(t, missingID))
	if code := domainCode(t, err); code != editorpick.ESchema {
		t.Fatalf("missing id code = %s", code)
	}
	if !strings.Contains(err.Error(), "nodes[0]") {
		t.Fatalf("error should identify nodes[0], got %v", err)
	}

	a, b := "a", "b"
	cycle := editorpick.SceneFile{
		Schema: editorpick.SceneSchemaV1, StructureVersion: editorpick.StructureVersion, Precision: precision,
		Nodes: []editorpick.NodeDTO{
			{ID: &a, ParentID: &b, Matrix: identity},
			{ID: &b, ParentID: &a, Matrix: identity},
		},
	}
	_, err = editorpick.ParseScene(mustJSON(t, cycle))
	if code := domainCode(t, err); code != editorpick.ECycle {
		t.Fatalf("cycle code = %s", code)
	}

	missingParent := "ghost"
	c := "c"
	ref := editorpick.SceneFile{
		Schema: editorpick.SceneSchemaV1, StructureVersion: editorpick.StructureVersion, Precision: precision,
		Nodes: []editorpick.NodeDTO{{ID: &c, ParentID: &missingParent, Matrix: identity}},
	}
	_, err = editorpick.ParseScene(mustJSON(t, ref))
	if code := domainCode(t, err); code != editorpick.ERef {
		t.Fatalf("parent ref code = %s", code)
	}

	badIndex := 42
	badGeom := editorpick.SceneFile{
		Schema: editorpick.SceneSchemaV1, StructureVersion: editorpick.StructureVersion, Precision: precision,
		Nodes: []editorpick.NodeDTO{{ID: &c, Matrix: identity, GeometryIndex: &badIndex}},
	}
	_, err = editorpick.ParseScene(mustJSON(t, badGeom))
	if code := domainCode(t, err); code != editorpick.ERef {
		t.Fatalf("geometry ref code = %s", code)
	}
}

func TestRayDeterminismAndAmbiguousBounds(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	qf, err := editorpick.LoadQueryFile(filepath.Join(dir, "queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}

	for _, q := range qf.Rays {
		first, err := engine.Raycast(q)
		if err != nil {
			t.Fatalf("ray %s: %v", q.ID, err)
		}
		second, err := engine.Raycast(q)
		if err != nil {
			t.Fatal(err)
		}
		firstJSON, _ := json.Marshal(first)
		secondJSON, _ := json.Marshal(second)
		if string(firstJSON) != string(secondJSON) {
			t.Fatalf("ray %s is not bitwise deterministic", q.ID)
		}
		if first.Status == editorpick.EAmbiguous {
			a := first.Ambiguity
			if a == nil || a.Code != editorpick.EAmbiguous {
				t.Fatalf("ray %s missing E_AMBIG details", q.ID)
			}
			if !(a.Lower.X <= a.Point.X && a.Point.X <= a.Upper.X &&
				a.Lower.Y <= a.Point.Y && a.Point.Y <= a.Upper.Y &&
				a.Lower.Z <= a.Point.Z && a.Point.Z <= a.Upper.Z) {
				t.Fatalf("ray %s ambiguity point outside bounds", q.ID)
			}
		}
	}

	parallel := editorpick.RaycastParallel(engine, qf.Rays)
	for i, outcome := range parallel {
		if outcome.Err != nil {
			t.Fatalf("parallel ray %s: %v", qf.Rays[i].ID, outcome.Err)
		}
		serial, _ := engine.Raycast(qf.Rays[i])
		a, _ := json.Marshal(outcome.Result)
		b, _ := json.Marshal(serial)
		if string(a) != string(b) {
			t.Fatalf("parallel ray %s differs", qf.Rays[i].ID)
		}
	}
}

func TestCameraScreenRayPicksObject(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	ray := editorpick.DefaultCamera().ScreenRay("pixel-center", 400, 300)
	result, err := engine.Raycast(ray)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "hit" || result.Hit == nil || result.Hit.NodeID != "sphere-node" {
		t.Fatalf("screen center picked %+v", result)
	}
}

func TestTransactionFailureRollsBack(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := engine.Derived("sphere-node")
	if !ok {
		t.Fatal("missing sphere derived")
	}
	beforeWorld := before.World

	_, err = engine.ApplyPivotBatch(editorpick.PivotBatch{
		Selection: []string{"sphere-node"}, Pivot: editorpick.Vec3{},
		Ops: []editorpick.PivotOp{{Type: "scale", Vector: editorpick.Vec3FromFloat(0, 1, 1)}},
	})
	if code := domainCode(t, err); code != editorpick.ETxn {
		t.Fatalf("zero scale code = %s", code)
	}
	after, _ := engine.Derived("sphere-node")
	if after.World != beforeWorld {
		t.Fatal("failed transaction changed committed world matrix")
	}

	_, err = engine.ApplyPivotBatch(editorpick.PivotBatch{Selection: []string{"missing"}, Ops: []editorpick.PivotOp{{Type: "translate"}}})
	if code := domainCode(t, err); code != editorpick.ETxn {
		t.Fatalf("missing selection code = %s", code)
	}
}

func TestSnapshotRejectsTampering(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := engine.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	snap, err := editorpick.LoadSnapshot(path, scene)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RestoreSnapshot(snap); err != nil {
		t.Fatal(err)
	}

	snap.Revision++
	data, _ := json.MarshalIndent(snap, "", " ")
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	if err := writeTestFile(tampered, data); err != nil {
		t.Fatal(err)
	}
	_, err = editorpick.LoadSnapshot(tampered, scene)
	if code := domainCode(t, err); code != editorpick.EHash {
		t.Fatalf("tampered snapshot code = %s", code)
	}
}

func TestGeometryAddRemoveInvalidation(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	center := editorpick.Vec3DTO{X: 0, Y: 0, Z: 0}
	report, err := engine.AttachGeometry("root", editorpick.GeometryDTO{Kind: "sphere", Center: &center, Radius: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	foundWorld := false
	for _, obj := range report.Objects {
		if obj.ObjectID == "world:root" && obj.Reason == "geometry-add" {
			foundWorld = true
		}
	}
	if !foundWorld {
		t.Fatalf("geometry add report missing world:root geometry-add: %+v", report.Objects)
	}
	if _, err := engine.RemoveGeometry("root"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverEngineRebuildsAfterHashMismatch(t *testing.T) {
	dir := generateTestSample(t)
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := editorpick.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(t.TempDir(), "snapshot.json")
	if err := engine.SaveSnapshot(validPath); err != nil {
		t.Fatal(err)
	}
	snap, err := editorpick.LoadSnapshot(validPath, scene)
	if err != nil {
		t.Fatal(err)
	}
	snap.StructureVersion = "future-version"
	data, _ := json.Marshal(snap)
	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	freshScene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	recovered, notice, err := editorpick.RecoverEngine(freshScene, badPath)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || notice == nil || !notice.Discarded || notice.Code != editorpick.EVersion {
		t.Fatalf("expected discarded E_VERSION recovery, got %+v", notice)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
