package project_test

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"terrain-bake/internal/testutil"
	"terrain-bake/project"
)

func loadBase(t *testing.T) (*project.Project, string) {
	t.Helper()
	dir := testutil.CopyProject(t)
	p, e := project.Load(dir)
	if e != nil {
		t.Fatalf("load project: %v", e)
	}
	return p, dir
}

func mutateManifest(t *testing.T, dir string, fn func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	var m map[string]any
	testutil.ReadJSON(t, path, &m)
	fn(m)
	testutil.WriteJSON(t, path, m)
}

func requireCode(t *testing.T, err *project.Error, code, block, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if err.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, err.Code, err)
	}
	if block != "" && err.BlockID != block {
		t.Fatalf("expected block %s, got %s (%v)", block, err.BlockID, err)
	}
	if field != "" && err.Field != field {
		t.Fatalf("expected field %s, got %s (%v)", field, err.Field, err)
	}
}

func TestLoadValidProject(t *testing.T) {
	p, _ := loadBase(t)
	if p.ContentHash == "" {
		t.Fatal("content hash is empty")
	}
	if len(p.BlockOrder) != 6 {
		t.Fatalf("expected 6 blocks, got %d", len(p.BlockOrder))
	}
	if len(p.Manifest.Seams) != 5 {
		t.Fatalf("expected 5 seams, got %d", len(p.Manifest.Seams))
	}
	if len(p.Manifest.Materials) != 3 {
		t.Fatalf("expected 3 material layers, got %d", len(p.Manifest.Materials))
	}
	if p.TruthStale || len(p.Edits) != 0 {
		t.Fatal("base project should not contain edits")
	}
}

func TestMissingTopLevelFieldIsSchemaError(t *testing.T) {
	dir := testutil.CopyProject(t)
	mutateManifest(t, dir, func(m map[string]any) {
		delete(m, "seed")
	})
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeSchema, "", "seed")
}

func TestMissingBlockFieldReportsBlockAndField(t *testing.T) {
	dir := testutil.CopyProject(t)
	mutateManifest(t, dir, func(m map[string]any) {
		block := m["blocks"].([]any)[0].(map[string]any)
		scatter := block["scatter"].(map[string]any)
		delete(scatter, "seed")
	})
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeSchema, "B_0_0", "scatter.seed")
}

func TestBlockGridSizeMismatchIsSizeError(t *testing.T) {
	dir := testutil.CopyProject(t)
	mutateManifest(t, dir, func(m map[string]any) {
		block := m["blocks"].([]any)[0].(map[string]any)
		height := block["height"].(map[string]any)
		height["cols"] = 23.0
	})
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeSize, "B_0_0", "height.cols")
}

func TestMaskSizeMismatchIsSizeError(t *testing.T) {
	dir := testutil.CopyProject(t)
	mutateManifest(t, dir, func(m map[string]any) {
		block := m["blocks"].([]any)[0].(map[string]any)
		mask := block["passability"].(map[string]any)
		mask["rows"] = 15.0
	})
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeSize, "B_0_0", "passability.rows")
}

func TestMaterialLayerMismatchIsSchemaError(t *testing.T) {
	dir := testutil.CopyProject(t)
	mutateManifest(t, dir, func(m map[string]any) {
		block := m["blocks"].([]any)[0].(map[string]any)
		block["material"].(map[string]any)["layers"] = 2.0
	})
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeSchema, "B_0_0", "material.layers")
}

func TestWeightOutOfRange(t *testing.T) {
	dir := testutil.CopyProject(t)
	path := filepath.Join(dir, "blocks", "B_0_0", "material.u16")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	quant := uint16(10000)
	binary.LittleEndian.PutUint16(data[:2], quant+1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeWeight, "B_0_0", "material.weights")
}

func TestWeightSumMismatch(t *testing.T) {
	dir := testutil.CopyProject(t)
	path := filepath.Join(dir, "blocks", "B_0_0", "material.u16")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w := binary.LittleEndian.Uint16(data[:2])
	binary.LittleEndian.PutUint16(data[:2], w-1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeWeight, "B_0_0", "material.weights")
}

func TestCorruptManifestIsDecodeError(t *testing.T) {
	dir := testutil.CopyProject(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schemaVersion":`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeDecode, "", "manifest.json")
}

func TestCorruptTruthIsDecodeError(t *testing.T) {
	dir := testutil.CopyProject(t)
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), []byte(`{"schemaVersion":1,`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeDecode, "", "truthFile")
}

func TestTruncatedBinaryResourceIsDecodeError(t *testing.T) {
	dir := testutil.CopyProject(t)
	path := filepath.Join(dir, "blocks", "B_0_0", "height.f64")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-3], 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := project.Load(dir)
	requireCode(t, e, project.CodeDecode, "B_0_0", "height.file")
}

func TestEditOverlayReplaysAndRebalances(t *testing.T) {
	p, dir := loadBase(t)
	oldHash := p.ContentHash
	if e := p.ApplyMaterialEdit("B_0_0", 4, 5, 1, 2500); e != nil {
		t.Fatalf("apply material edit: %v", e)
	}
	if e := p.ApplyHeightEdit("B_0_0", 6, 7, 3.25); e != nil {
		t.Fatalf("apply height edit: %v", e)
	}
	if p.ContentHash == oldHash {
		t.Fatal("content hash did not change")
	}
	if e := project.WriteEdits(dir, p.Edits); e != nil {
		t.Fatalf("write edits: %v", e)
	}
	reloaded, e := project.Load(dir)
	if e != nil {
		t.Fatalf("reload edited project: %v", e)
	}
	if !reloaded.TruthStale || len(reloaded.Edits) != 2 {
		t.Fatalf("edit metadata not restored: stale=%v edits=%d", reloaded.TruthStale, len(reloaded.Edits))
	}
	if reloaded.ContentHash != p.ContentHash {
		t.Fatalf("content hash mismatch after replay: %s != %s", reloaded.ContentHash, p.ContentHash)
	}
	bd := reloaded.Blocks["B_0_0"]
	base := (5*bd.Def.Material.Cols + 4) * bd.Def.Material.Layers
	sum := 0
	for layer := 0; layer < bd.Def.Material.Layers; layer++ {
		sum += int(bd.Weights[base+layer])
	}
	if sum != bd.Def.Material.QuantMax {
		t.Fatalf("rebalanced weights sum to %d", sum)
	}
	if bd.Height[7*bd.Def.Grid.Cols+6] != 3.25 {
		t.Fatalf("height edit not replayed")
	}
	data, err := json.Marshal(project.EditFile{SchemaVersion: 1, Edits: reloaded.Edits})
	if err != nil || len(data) == 0 {
		t.Fatal("edits should be JSON encodable")
	}
}
