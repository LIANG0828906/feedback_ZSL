package terrainbake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointReuseAndTamperInvalidatesWholeBlock(t *testing.T) {
	p := testProject(t)
	dir := t.TempDir()
	e, err := NewEngine(p, DefaultEngineConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.WrittenBlocks) != len(p.Blocks) {
		t.Fatalf("initial writes = %d", len(first.WrittenBlocks))
	}
	path := filepath.Join(dir, checkpointFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("checkpoint is not atomically valid JSON: %v", err)
	}

	reopened, err := NewEngine(p, DefaultEngineConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reopened.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.ReusedCount != len(p.Blocks) || len(second.WrittenBlocks) != 0 {
		t.Fatalf("restart reuse = %d written=%d", second.ReusedCount, len(second.WrittenBlocks))
	}
	if err := CompareBakeResults(first, second); err != nil {
		t.Fatalf("restart differs: %v", err)
	}

	records := check["records"].(map[string]any)
	b02 := records["B02"].(map[string]any)
	b02["vertex_digest"] = "tampered"
	tampered, _ := json.MarshalIndent(check, "", "  ")
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	thirdEngine, err := NewEngine(p, DefaultEngineConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	third, err := thirdEngine.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.ReusedCount != len(p.Blocks)-1 {
		t.Fatalf("tamper reused = %d want %d", third.ReusedCount, len(p.Blocks)-1)
	}
	if len(third.WrittenBlocks) != 1 || third.WrittenBlocks[0] != "B02" {
		t.Fatalf("tampered block rewrite = %v", third.WrittenBlocks)
	}
	if err := CompareBakeResults(first, third); err != nil {
		t.Fatalf("recomputed block differs after tamper: %v", err)
	}
}

func TestCheckpointAlgorithmVersionMismatchRecomputes(t *testing.T) {
	p := testProject(t)
	dir := t.TempDir()
	e, err := NewEngine(p, DefaultEngineConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bake(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, checkpointFileName)
	data, _ := os.ReadFile(path)
	var check map[string]any
	_ = json.Unmarshal(data, &check)
	rec := check["records"].(map[string]any)["B04"].(map[string]any)
	rec["algorithm_version"] = "old-algorithm"
	data, _ = json.MarshalIndent(check, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
	e2, err := NewEngine(p, DefaultEngineConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e2.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.WrittenBlocks) != 1 || r.WrittenBlocks[0] != "B04" {
		t.Fatalf("algorithm mismatch writes = %v", r.WrittenBlocks)
	}
}
