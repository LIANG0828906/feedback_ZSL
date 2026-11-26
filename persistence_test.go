package maskqa

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointMismatchForcesRebuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ckpt")
	wb, err := NewWorkbench(mustGenerate(t, 0), dir, DefaultSchedulerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wb.InitializeFull(); err != nil {
		t.Fatal(err)
	}
	if err := wb.Close(); err != nil {
		t.Fatal(err)
	}

	commits := filepath.Join(dir, "commits")
	entries, err := os.ReadDir(commits)
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(commits, entries[0].Name())
	payload, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	record["content_hash"] = "sha256:deliberately-bad"
	bad, _ := json.MarshalIndent(record, "", "  ")
	if err := os.WriteFile(first, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	store, recovery, err := OpenCheckpointStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if recovery.Action != RecoveryRebuild || recovery.Code == "" {
		t.Fatalf("expected rebuild code, got %+v", recovery)
	}
	if len(store.Records()) != 0 {
		t.Fatal("corrupt checkpoint was not reset for full rebuild")
	}

	versionDir := filepath.Join(t.TempDir(), "version-ckpt")
	wb2, err := NewWorkbench(mustGenerate(t, 0), versionDir, DefaultSchedulerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wb2.InitializeFull(); err != nil {
		t.Fatal(err)
	}
	if err := wb2.Close(); err != nil {
		t.Fatal(err)
	}
	versionEntries, _ := os.ReadDir(filepath.Join(versionDir, "commits"))
	versionFile := filepath.Join(versionDir, "commits", versionEntries[0].Name())
	versionPayload, _ := os.ReadFile(versionFile)
	var versionRecord map[string]any
	_ = json.Unmarshal(versionPayload, &versionRecord)
	versionRecord["algorithm_version"] = "future-algorithm-v999"
	versionBad, _ := json.MarshalIndent(versionRecord, "", "  ")
	_ = os.WriteFile(versionFile, versionBad, 0o644)
	_, recovery, err = OpenCheckpointStore(versionDir)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Action != RecoveryRebuild || recovery.Code != "E_CKPT_VERSION_HASH" {
		t.Fatalf("expected version/hash rebuild, got %+v", recovery)
	}
}

func TestCheckpointForceKillSubprocess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kill")
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointKillHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "MASKQA_KILL_HELPER_DIR="+dir)
	var helperOutput bytes.Buffer
	cmd.Stdout = &helperOutput
	cmd.Stderr = &helperOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	expectedHashBytes, err := os.ReadFile(ready + ".hash")
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not become ready: %v\noutput:\n%s", err, helperOutput.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	store, recovery, err := OpenCheckpointStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if recovery.Action != RecoveryRecovered || recovery.LastSequence != 3 {
		t.Fatalf("unexpected kill recovery: %+v", recovery)
	}
	coordBytes, err := os.ReadFile(filepath.Join(dir, "ready.coord"))
	if err != nil {
		t.Fatal(err)
	}
	var coord TileCoord
	if err := json.Unmarshal(coordBytes, &coord); err != nil {
		t.Fatal(err)
	}
	record, ok := store.LatestTile(coord)
	if !ok {
		t.Fatal("missing committed tile 0,0")
	}
	if got := ContentDigest(record.TileAfter); got != string(expectedHashBytes) {
		t.Fatalf("committed pixels changed: hash %s want %s", got, string(expectedHashBytes))
	}
}

func TestCheckpointKillHelper(t *testing.T) {
	dir := os.Getenv("MASKQA_KILL_HELPER_DIR")
	if dir == "" {
		t.Skip("helper only runs when environment variable is set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	project := mustGenerate(t, 0)
	store, _, err := OpenCheckpointStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ordered := OrderStrokes(project.Strokes)
	var tile Tile
	var computation *TileComputation
	for _, candidateTile := range Tiles(project.Width, project.Height, project.TileSize) {
		candidate, err := computeTile(project, candidateTile, ordered, make([]byte, candidateTile.PixelCount))
		if err != nil {
			t.Fatal(err)
		}
		if len(candidate.records) >= 3 {
			tile = candidateTile
			computation = candidate
			break
		}
	}
	if computation == nil {
		t.Fatal("no tile with at least three stroke records")
	}
	var third StrokeCommitRecord
	for i := 0; i < 3; i++ {
		third, err = store.CommitStroke(computation.records[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ready.hash"), []byte(ContentDigest(third.TileAfter)), 0o644); err != nil {
		t.Fatal(err)
	}
	coordData, _ := json.Marshal(tile.Coord)
	if err := os.WriteFile(filepath.Join(dir, "ready.coord"), coordData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {}
}
