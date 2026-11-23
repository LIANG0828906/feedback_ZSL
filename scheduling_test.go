package terrainbake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestParallelAndSerialIdenticalStableOrder(t *testing.T) {
	p, _ := GenerateProject(11)
	run := func(workers int) (map[string]*BlockOutput, *BakeResult) {
		opts := DefaultEngineOptions(t.TempDir())
		opts.MaxWorkers = workers
		e, err := NewEngine(p, opts)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.FullBake(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Outputs()
		if err != nil {
			t.Fatal(err)
		}
		return out, res
	}
	serialOut, serial := run(1)
	parallelOut, parallel := run(4)
	if _, err := CompareSnapshots(serialOut, parallelOut, serial.SeamStats, parallel.SeamStats, serial.CommitSequence, parallel.CommitSequence); err != nil {
		t.Fatalf("parallel differs from serial: %v", err)
	}
	if len(serial.CommitSequence) != len(p.Blocks) {
		t.Fatalf("commit sequence length %d want %d", len(serial.CommitSequence), len(p.Blocks))
	}
	for i := range serial.CommitSequence {
		if serial.CommitSequence[i].BlockID != parallel.CommitSequence[i].BlockID {
			t.Fatalf("unstable commit order at %d", i)
		}
	}
}

func TestParallelAndSerialSameEditSequence(t *testing.T) {
	p, _ := GenerateProject(111)
	edits := []Edit{
		{Kind: EditHeight, BlockID: "tl", X: 9, Y: 7, Value: 6.75},
		{Kind: EditMaterial, MaterialID: "rock", Field: "rotation_deg", Value: 41},
	}
	run := func(workers int) (map[string]*BlockOutput, *BakeResult) {
		opts := DefaultEngineOptions(t.TempDir())
		opts.MaxWorkers = workers
		e, err := NewEngine(p, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.FullBake(context.Background()); err != nil {
			t.Fatal(err)
		}
		res, err := e.IncrementalBake(context.Background(), edits)
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Outputs()
		if err != nil {
			t.Fatal(err)
		}
		return out, res
	}
	serialOut, serial := run(1)
	parallelOut, parallel := run(4)
	if _, err := CompareSnapshots(serialOut, parallelOut, serial.SeamStats, parallel.SeamStats, serial.CommitSequence, parallel.CommitSequence); err != nil {
		t.Fatalf("edit-sequence parallel mismatch: %v", err)
	}
}

func TestIncrementalReusesUnaffectedBlock(t *testing.T) {
	p, _ := GenerateProject(12)
	isolateBR(p)
	opts := DefaultEngineOptions(t.TempDir())
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.FullBake(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := e.CheckpointManifest()
	beforeBR := before.Blocks["br"]
	res, err := e.IncrementalBake(context.Background(), []Edit{
		{Kind: EditHeight, BlockID: "tl", X: 5, Y: 5, Value: 9.25},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(res.Reused, "br") {
		t.Fatalf("br should be reused: reused=%v baked=%v", res.Reused, res.Baked)
	}
	if containsString(res.Baked, "br") {
		t.Fatalf("br should not be baked: %v", res.Baked)
	}
	after := e.CheckpointManifest()
	if after.Blocks["br"].NormalDigest != beforeBR.NormalDigest || after.Blocks["br"].DataDigest != beforeBR.DataDigest {
		t.Fatal("reused block cache was rewritten")
	}
	foundTL, foundTR := false, false
	for _, item := range res.Invalidation {
		if item.BlockID == "tl" {
			foundTL = true
		}
		if item.BlockID == "tr" && len(item.Chain) >= 2 {
			foundTR = true
		}
	}
	if !foundTL || !foundTR {
		t.Fatalf("invalidation chain missing expected entries: %+v", res.Invalidation)
	}
}

func TestMemoryBudgetQueuesAndStillCompletes(t *testing.T) {
	p, _ := GenerateProject(13)
	referenceOut, referenceRes := fullOutputs(t, p, func(o *EngineOptions) {
		o.MaxWorkers = 1
		o.NormalMapMemoryBytes = 64 * 1024 * 1024
	})
	opts := DefaultEngineOptions(t.TempDir())
	opts.MaxWorkers = 4
	opts.NormalMapMemoryBytes = 10000
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.FullBake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Outputs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSnapshots(referenceOut, out, referenceRes.SeamStats, res.SeamStats, referenceRes.CommitSequence, res.CommitSequence); err != nil {
		t.Fatal(err)
	}
	if len(res.Queue) == 0 {
		t.Fatal("expected normal map budget queue events")
	}
	found := false
	for _, q := range res.Queue {
		if q.Reason == "normal_map_budget_wait" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing memory queue reason: %+v", res.Queue)
	}
}

func TestThreadPixelBudgetExhaustion(t *testing.T) {
	p, _ := GenerateProject(14)
	opts := DefaultEngineOptions(t.TempDir())
	opts.MaxThreadPixels = 4
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.FullBake(context.Background())
	assertBiz(t, err, CodeResource, "", "max_thread_pixels")
}

func TestPauseAndResume(t *testing.T) {
	p, _ := GenerateProject(15)
	edits := []Edit{{Kind: EditHeight, BlockID: "tl", X: 4, Y: 4, Value: 8.5}}

	edited, err := cloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEdits(edited, BuildDependencyGraph(edited), edits); err != nil {
		t.Fatal(err)
	}
	referenceOut, referenceRes := fullOutputs(t, edited, func(o *EngineOptions) {
		o.MaxWorkers = 3
	})

	store := t.TempDir()
	opts := DefaultEngineOptions(store)
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.FullBake(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.opts.PauseAfter = 1
	paused, err := e.IncrementalBake(context.Background(), edits)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || len(paused.Baked) != 1 {
		t.Fatalf("expected one committed block before pause: %+v", paused)
	}
	pendingProject, err := e.ProjectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	m := e.CheckpointManifest()
	if m.Complete || len(m.Pending) == 0 {
		t.Fatalf("pause checkpoint is not durable: %+v", m)
	}

	restartOpts := DefaultEngineOptions(store)
	restarted, err := NewEngine(pendingProject, restartOpts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.snapshotReset {
		t.Fatalf("resume unexpectedly reset snapshot: %s", restarted.snapshotResetWhy)
	}
	resumed, err := restarted.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Paused {
		t.Fatal("resume did not finish")
	}
	out, err := restarted.Outputs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSnapshots(referenceOut, out, referenceRes.SeamStats, resumed.SeamStats, referenceRes.CommitSequence, resumed.CommitSequence); err != nil {
		t.Fatalf("resume mismatch: %v", err)
	}
}

func TestVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	p, _ := GenerateProject(16)
	store := t.TempDir()
	e, err := NewEngine(p, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.FullBake(context.Background()); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	data, err := os.ReadFile(filepath.Join(store, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.AlgorithmVersion = "old-algorithm"
	cp := m.Blocks["tl"]
	cp.AlgorithmVersion = "old-algorithm"
	m.Blocks["tl"] = cp
	data, _ = json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(store, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewEngine(p, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.snapshotReset {
		t.Fatal("version mismatch should invalidate snapshot")
	}
	res, err := restarted.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.SnapshotReset || len(res.Baked) != len(p.Blocks) {
		t.Fatalf("expected full rebake after reset: %+v", res)
	}
	if mm := restarted.CheckpointManifest(); !mm.Complete {
		t.Fatal("rebuilt checkpoint is not complete")
	}
}

func TestGlobalContentHashMismatchInvalidatesWholeSnapshot(t *testing.T) {
	p, _ := GenerateProject(161)
	store := t.TempDir()
	e, err := NewEngine(p, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.FullBake(context.Background()); err != nil {
		t.Fatal(err)
	}
	edited, _ := cloneProject(p)
	edited.Blocks[0].Elevation[0] += 0.75
	restarted, err := NewEngine(edited, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.snapshotReset {
		t.Fatal("global content hash mismatch should invalidate snapshot")
	}
	res, err := restarted.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.SnapshotReset || len(res.Baked) != len(edited.Blocks) {
		t.Fatalf("expected complete rebuild, got %+v", res)
	}
}

func TestPerBlockVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	p, _ := GenerateProject(162)
	store := t.TempDir()
	e, err := NewEngine(p, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.FullBake(context.Background()); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	data, _ := os.ReadFile(filepath.Join(store, "manifest.json"))
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	cp := m.Blocks["bl"]
	cp.ParamsVersion = "stale-params"
	m.Blocks["bl"] = cp
	data, _ = json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(store, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(p, DefaultEngineOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.snapshotReset {
		t.Fatal("per-block version mismatch should invalidate snapshot")
	}
}

func TestFailureIsolationAndRetry(t *testing.T) {
	p, _ := GenerateProject(17)
	referenceOut, _ := fullOutputs(t, p, func(o *EngineOptions) {
		o.MaxWorkers = 3
	})
	opts := DefaultEngineOptions(t.TempDir())
	opts.MaxWorkers = 2
	opts.FaultInjector = func(blockID string, attempt int) error {
		if blockID == "tl" && attempt == 1 {
			return fmt.Errorf("injected PNG write failure")
		}
		return nil
	}
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := e.FullBake(context.Background())
	if err == nil {
		t.Fatal("expected injected bake error")
	}
	be, ok := AsError(err)
	if !ok || be.Code != CodeBake {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed == nil || failed.Failed["tl"] == "" {
		t.Fatalf("failed block not reported: %+v", failed)
	}
	if containsString(failed.Baked, "tl") {
		t.Fatal("failed block was committed")
	}
	m := e.CheckpointManifest()
	if m.Failures["tl"] == "" {
		t.Fatal("failure status was not persisted in checkpoint")
	}
	for _, id := range []string{"tr", "bl", "br"} {
		cp, ok := m.Blocks[id]
		if !ok {
			t.Fatalf("successful block %s was not committed", id)
		}
		got, err := e.store.LoadBlock(cp)
		if err != nil {
			t.Fatal(err)
		}
		want := referenceOut[id]
		if got.NormalDigest != want.NormalDigest || got.SlopeDigest != want.SlopeDigest {
			t.Fatalf("successful block %s was polluted by tl failure", id)
		}
	}
	retried, err := e.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if retried.Paused || len(retried.Baked) != 1 || retried.Baked[0] != "tl" {
		t.Fatalf("retry did not commit only the failed block: %+v", retried)
	}
	if m := e.CheckpointManifest(); len(m.Failures) != 0 || !m.Complete {
		t.Fatalf("retry did not clear failure state: %+v", m)
	}
	out, err := e.Outputs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSnapshots(referenceOut, out, nil, nil, refCommitIDs(referenceOut), retried.CommitSequence); err != nil {
		t.Fatal(err)
	}
}

func refCommitIDs(out map[string]*BlockOutput) []CommitEvent {
	var ids []string
	for id := range out {
		ids = append(ids, id)
	}
	sortStrings(ids)
	var events []CommitEvent
	for _, id := range ids {
		events = append(events, CommitEvent{BlockID: id, Kind: "baked"})
	}
	return events
}

func TestSeamConstraintFailureIsStructured(t *testing.T) {
	p, _ := GenerateProject(18)
	for i := range p.Seams {
		p.Seams[i].MaxAngleDeg = 1e-9
		p.Seams[i].MaxIterations = 1
	}
	e, err := NewEngine(p, DefaultEngineOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.FullBake(context.Background())
	if err == nil {
		t.Fatal("expected E_SEAM")
	}
	be, ok := AsError(err)
	if !ok || be.Code != CodeSeam {
		t.Fatalf("unexpected error: %v", err)
	}
	if be.BlockA == "" || be.BlockB == "" || be.Angle <= be.Limit {
		t.Fatalf("seam error lacks pair/measured angle: %+v", be)
	}
}

func TestDiffLocatesFirstPixel(t *testing.T) {
	p, _ := GenerateProject(19)
	out, res := fullOutputs(t, p, nil)
	clone := make(map[string]*BlockOutput, len(out))
	for id, v := range out {
		cp := *v
		cp.Normals = append([]Vec3(nil), v.Normals...)
		cp.SlopeDeg = append([]float64(nil), v.SlopeDeg...)
		clone[id] = &cp
	}
	bad := clone["tl"]
	bad.Normals[2*bad.Width+3] = Vec3{X: 0.123, Y: -0.456, Z: 0.881}
	_, err := CompareSnapshots(out, clone, res.SeamStats, res.SeamStats, res.CommitSequence, res.CommitSequence)
	if err == nil {
		t.Fatal("expected E_DIFF")
	}
	be, ok := AsError(err)
	if !ok || be.Code != CodeDiff || be.BlockID != "tl" || be.X != 3 || be.Y != 2 {
		t.Fatalf("wrong diff diagnosis: %+v", be)
	}
}

func fullOutputs(t *testing.T, p *Project, mutate func(*EngineOptions)) (map[string]*BlockOutput, *BakeResult) {
	t.Helper()
	opts := DefaultEngineOptions(t.TempDir())
	if mutate != nil {
		mutate(&opts)
	}
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.FullBake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Outputs()
	if err != nil {
		t.Fatal(err)
	}
	return out, res
}

func isolateBR(p *Project) {
	var seams []Seam
	kept := map[string]bool{}
	for _, s := range p.Seams {
		if s.BlockA == "br" || s.BlockB == "br" {
			continue
		}
		seams = append(seams, s)
		kept[s.ID] = true
	}
	p.Seams = seams
	var st []TruthSeam
	for _, t := range p.SeamTruth {
		if kept[t.SeamID] {
			st = append(st, t)
		}
	}
	p.SeamTruth = st
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
