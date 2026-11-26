package lightprobe

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaValidationObjectAndField(t *testing.T) {
	scene := testScene(t)
	scene.Rooms[0].Bounds.Min.X = 100
	err := ValidateScene(scene)
	de := requireDomainError(t, err, ESchema)
	if de.ObjectID != scene.Rooms[0].ID || de.Field != "bounds.min" {
		t.Fatalf("unexpected error location %s/%s", de.ObjectID, de.Field)
	}

	scene = testScene(t)
	scene.Probes[10].Position = Vec3{X: 999, Y: 0, Z: 0}
	assertSchemaField(t, scene, scene.Probes[10].ID, "position")

	scene = testScene(t)
	scene.Probes[1].Position = scene.Probes[0].Position
	assertSchemaField(t, scene, scene.Probes[1].ID, "position")

	scene = testScene(t)
	scene.Probes[0].SH[0][4] = scene.Params.SHMaxFixed + 1
	assertSchemaField(t, scene, scene.Probes[0].ID, "sh[0][4]")

	scene = testScene(t)
	scene.Directives[1].Time = scene.Directives[0].Time - 1
	assertSchemaField(t, scene, scene.Directives[1].ID, "time")
}

func TestFixedReconstructionOrderIndependent(t *testing.T) {
	scene := testScene(t)
	probe := scene.Probes[0]
	normal, err := ReconstructProbe(probe, scene.Params)
	if err != nil {
		t.Fatal(err)
	}
	order := make([]int, 16*9)
	for i := range order {
		order[i] = len(order) - 1 - i
	}
	reversed, err := ReconstructProbeWithTermOrder(probe, scene.Params, order)
	if err != nil {
		t.Fatal(err)
	}
	if normal.RadianceFixed != reversed.RadianceFixed {
		t.Fatalf("fixed radiance changed with accumulation order: %v vs %v", normal.RadianceFixed, reversed.RadianceFixed)
	}
	if len(normal.Truncations) != 6 || normal.Truncations[0].Position == "" {
		t.Fatalf("truncation records not recorded per channel/stage: %+v", normal.Truncations)
	}
}

func TestQuantizationResidualMonotonic(t *testing.T) {
	scales := []int64{100, 1_000, 10_000, 100_000, 1_000_000}
	var previous float64 = 1
	for _, scale := range scales {
		params := DefaultParams()
		params.FixedScale = scale
		params.SHMinFixed = -4 * scale
		params.SHMaxFixed = 4 * scale
		scene, err := GenerateScene(DefaultSeed, params)
		if err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for _, p := range scene.Probes {
			r, err := ReconstructProbe(p, params)
			if err != nil {
				t.Fatal(err)
			}
			sum += r.SHQuantResidualMax
		}
		if sum > previous+1e-12 {
			t.Fatalf("residual did not tighten monotonically: scale=%d sum=%.9f previous=%.9f", scale, sum, previous)
		}
		previous = sum
	}
}

func TestParallelismAndShuffledChunksAreDeterministic(t *testing.T) {
	scene := testScene(t)
	serial := map[string]ProbeResult{}
	for _, p := range scene.Probes {
		r, err := ReconstructProbe(p, scene.Params)
		if err != nil {
			t.Fatal(err)
		}
		serial[p.ID] = r
	}

	for _, workers := range []int{1, 2, 8} {
		results, stats, err := RunProbePool(scene.Probes, scene.Params, PoolConfig{
			Workers: workers, BufferLimit: 3, QueueBudget: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Completed != len(scene.Probes) || int(stats.PeakActiveWorkers) > workers {
			t.Fatalf("bad pool stats: %+v", stats)
		}
		if workers == 1 && stats.WaitCount == 0 {
			t.Fatalf("expected bounded queue waits under tiny budget")
		}
		assertSameProbeResults(t, serial, results)
	}

	shuffled := append([]Probe(nil), scene.Probes...)
	r := rand.New(rand.NewSource(99))
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	results, _, err := RunProbePool(shuffled, scene.Params, PoolConfig{Workers: 4, BufferLimit: 2, QueueBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertSameProbeResults(t, serial, results)
}

func TestTightenedToleranceDoesNotChangeMeasurement(t *testing.T) {
	scene := testScene(t)
	pr, err := RunPipeline(scene, RunOptions{Pool: PoolConfig{Workers: 2, BufferLimit: 4, QueueBudget: 4}})
	if err != nil {
		t.Fatal(err)
	}
	original := pr.QueryResults[0]
	tight := scene.Params
	tight.Tolerance = 1e-12
	tight.ParamVersion = "tight-test"
	expected := map[string][3]float64{}
	for _, truth := range scene.Truths {
		expected[truth.QueryID] = truth.Expected
	}
	tightReport := BuildReport(original, expected[original.Query.ID], pr.ProbeResults, tight)
	if tightReport.ActualErrorMax != original.Report.ActualErrorMax {
		t.Fatalf("measured error changed with tolerance: %.12e vs %.12e",
			tightReport.ActualErrorMax, original.Report.ActualErrorMax)
	}
	if !tightReport.Uncertain {
		t.Fatalf("extremely tight tolerance should mark query uncertain")
	}
	truth := expected[original.Query.ID]
	for ch := 0; ch < 3; ch++ {
		if truth[ch] < tightReport.Channels[ch].LowerBound || truth[ch] > tightReport.Channels[ch].UpperBound {
			t.Fatalf("truth outside uncertainty bounds channel %d", ch)
		}
	}
}

func TestDependencyInvalidationIsExact(t *testing.T) {
	report, err := RunAcceptance(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := report.Full.Graph
	probeID := "probe-room-b-020101"
	inv := g.InvalidateProbeRadiance(probeID)
	ids := invalidationIDSet(inv)
	for _, want := range []string{"radiance:" + probeID, "recon:" + probeID} {
		if !ids[want] {
			t.Fatalf("expected %s in invalidations %v", want, inv)
		}
	}
	if !hasPrefix(ids, "query:") || !hasPrefix(ids, "pixel:") {
		t.Fatalf("radiance edit should reach query and pixel: %v", inv)
	}
	if ids["radiance:probe-room-a-020101"] || ids["recon:probe-room-a-020101"] {
		t.Fatalf("unrelated room-a probe invalidated: %v", inv)
	}

	res_inv := g.InvalidateRoomResolution("room-c", "res-c-9x5x5-v2")
	resIDs := invalidationIDSet(res_inv)
	if !hasPrefix(resIDs, "recon:probe-room-c") || !hasPrefix(resIDs, "query:") {
		t.Fatalf("room-c resolution change missed room probes/queries")
	}
	if resIDs["radiance:probe-room-a-020101"] {
		t.Fatalf("room-c resolution change invalidated room-a radiance")
	}

	oldQueries := append([]Query(nil), report.Scene.Queries...)
	newQueries := append([]Query(nil), report.Scene.Queries...)
	newQueries[5].Position.X += 0.2
	diff := DiffQueryTrajectory(oldQueries, newQueries)
	if len(diff.Changed) != 1 {
		t.Fatalf("expected one changed query, got %+v", diff)
	}
	qInv := g.InvalidateQueryDiff(diff)
	qIDs := invalidationIDSet(qInv)
	if !qIDs["query:"+diff.Changed[0]] || !hasPrefix(qIDs, "pixel:") {
		t.Fatalf("query diff did not invalidate query/pixel: %v", qInv)
	}
	for id := range qIDs {
		if len(id) >= 6 && id[:6] == "recon:" {
			t.Fatalf("query trajectory change must not invalidate reconstruction %s", id)
		}
	}

	oldKeys := g.ReconcileCache(nil).NewKeys
	target := g.Nodes["radiance:"+probeID]
	target.ContentHash = "probe-content:edited-for-cache-test"
	g.RecomputeKeys(report.Scene)
	newKeys := g.ReconcileCache(oldKeys)
	if containsString(newKeys.Reused, "recon:probe-room-a-020101") == false {
		t.Fatalf("unaffected room-a cache key should be reused")
	}
	if containsString(newKeys.Rebuilt, "recon:"+probeID) == false {
		t.Fatalf("edited probe downstream key should rebuild")
	}
}

func TestStateMachineIllegalTransitionPreservesState(t *testing.T) {
	rp := NewRoomProgress("room-x")
	err := rp.Transition(StateCommitted)
	de := requireDomainError(t, err, EState)
	if de.ObjectID != "room-x" {
		t.Fatalf("bad object id %s", de.ObjectID)
	}
	if rp.State != StatePending || rp.Progress != 0 {
		t.Fatalf("illegal transition mutated progress: %+v", rp)
	}
	if err := rp.Transition(StateCollecting); err != nil {
		t.Fatal(err)
	}
	if err := rp.Transition(StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := rp.Transition(StateCollecting); err != nil {
		t.Fatalf("failed room should support retry: %v", err)
	}
}

func TestCheckpointCorruptionHashAndVersionBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.lpjson")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := RoomRecord{
		RoomID: "room-a", ParamVersion: DefaultParams().ParamVersion,
		AlgorithmVersion: AlgorithmVersion, ResolutionVersion: "res-a",
		ProbeDigests: map[string]string{"p": "d"}, DependencySet: []string{"x"},
	}
	rec.ContentHash = ComputeRoomHash(rec)
	if err := store.CommitRoom(DefaultSeed, rec); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{"room-a": "res-a"}
	valid, stale := reopened.VerifiedRooms(versions, DefaultParams().ParamVersion)
	if len(valid) != 1 || len(stale) != 0 {
		t.Fatalf("valid checkpoint not accepted: valid=%v stale=%v", valid, stale)
	}
	if _, stale := reopened.VerifiedRooms(versions, "different-param"); len(stale) != 1 || stale[0].Reason != "param-version-mismatch" {
		t.Fatalf("param mismatch should force whole-room rebuild: %+v", stale)
	}

	corruptHash := rec
	corruptHash.RoomID = "room-b"
	corruptHash.ResolutionVersion = "res-b"
	corruptHash.ContentHash = "room-hash:bad"
	if err := reopened.CommitRoom(DefaultSeed, corruptHash); err != nil {
		t.Fatal(err)
	}
	_, stale = reopened.VerifiedRooms(map[string]string{"room-a": "res-a", "room-b": "res-b"}, DefaultParams().ParamVersion)
	if len(stale) != 1 || stale[0].RoomID != "room-b" || stale[0].Reason != "content-hash-mismatch" {
		t.Fatalf("hash mismatch should identify room-b: %+v", stale)
	}

	corruptPath := filepath.Join(dir, "corrupt.lpjson")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenCheckpoint(corruptPath)
	requireDomainError(t, err, EDecode)
}

func TestRoomExceptionIsolationDecodeAndRecovery(t *testing.T) {
	scene := testScene(t)
	badProbe := "probe-room-a-000000"
	if err := CorruptSample(scene, badProbe); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "isolate.lpjson")
	pr, err := RunPipelineContext(context.Background(), scene, RunOptions{
		CheckpointPath: path, Pool: PoolConfig{Workers: 2, BufferLimit: 2, QueueBudget: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	badErr, ok := pr.RoomErrors["room-a"]
	if !ok || badErr.Code != EDecode {
		t.Fatalf("expected room-a E_DECODE, got %+v", pr.RoomErrors)
	}
	if _, ok := pr.RoomStates["room-b"]; !ok || pr.RoomStates["room-b"].State != StateCommitted {
		t.Fatalf("room-b should remain committed: %+v", pr.RoomStates)
	}
	if _, ok := pr.RoomErrors["room-b"]; ok {
		t.Fatalf("room-b polluted by room-a failure")
	}

	p := scene.ProbeByID(badProbe)
	if _, err := ApplyRadianceEdit(scene, badProbe, 0, p.Radiance[0]); err != nil {
		t.Fatal(err)
	}
	retry, err := RunPipeline(scene, RunOptions{CheckpointPath: path, Pool: PoolConfig{Workers: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.RoomErrors) != 0 {
		t.Fatalf("retry should recover room-a: %+v", retry.RoomErrors)
	}
	if !containsString(retry.RestoredRooms, "room-b") {
		t.Fatalf("committed room-b should be restored from checkpoint")
	}
}

func TestParameterInjectionGridOutOfBoundsIsEVal(t *testing.T) {
	scene := testScene(t)
	err := ValidateGridParameterInjection(scene.Rooms[0], [3]int{5, 3, 3}, Vec3{X: 1.75, Y: 0.5, Z: 0.5})
	de := requireDomainError(t, err, EVal)
	if de.Field == "" || de.ObjectID == "" {
		t.Fatalf("E_VAL should identify object/field: %v", de)
	}
	err = ValidateGridParameterInjection(scene.Rooms[0], [3]int{1, 3, 3}, Vec3{0.5, 0.5, 0.5})
	requireDomainError(t, err, EVal)
}

func TestDiffLocatesRoomCoordinateAndField(t *testing.T) {
	scene := testScene(t)
	first, err := RunPipeline(scene, RunOptions{Pool: PoolConfig{Workers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunPipeline(scene, RunOptions{Pool: PoolConfig{Workers: 4}})
	if err != nil {
		t.Fatal(err)
	}
	probeID := "probe-room-b-010101"
	r := second.ProbeResults[probeID]
	r.RadianceFixed[1] += 7
	second.ProbeResults[probeID] = r
	de := DiffPipelines(first, second)
	requireDomainError(t, de, EDiff)
	if de.ObjectID != "room-b" || de.Field == "" {
		t.Fatalf("E_DIFF should locate room and coordinate field: %+v", de)
	}
	if de.Field != "grid_coord=[1,1,1].radiance_fixed[G]" {
		t.Fatalf("unexpected diff field %q", de.Field)
	}
}

func testScene(t *testing.T) *Scene {
	t.Helper()
	s, err := GenerateScene(DefaultSeed, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireDomainError(t *testing.T, err error, code string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *Error, got %T %v", err, err)
	}
	if de.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, de.Code, err)
	}
	return de
}

func assertSchemaField(t *testing.T, s *Scene, objectID, field string) {
	t.Helper()
	de := requireDomainError(t, ValidateScene(s), ESchema)
	if de.ObjectID != objectID || de.Field != field {
		t.Fatalf("expected %s/%s, got %s/%s: %v", objectID, field, de.ObjectID, de.Field, de)
	}
}

func assertSameProbeResults(t *testing.T, want, got map[string]ProbeResult) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("result count want %d got %d", len(want), len(got))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if w.RadianceFixed != g.RadianceFixed {
			t.Fatalf("%s fixed radiance differs: %v vs %v", id, w.RadianceFixed, g.RadianceFixed)
		}
		if len(w.Truncations) != len(g.Truncations) {
			t.Fatalf("%s truncation records differ", id)
		}
		for i := range w.Truncations {
			if w.Truncations[i] != g.Truncations[i] {
				t.Fatalf("%s truncation %d differs: %+v vs %+v", id, i, w.Truncations[i], g.Truncations[i])
			}
		}
		if len(w.Distribution) != len(g.Distribution) {
			t.Fatalf("%s direction distribution length differs", id)
		}
		for i := range w.Distribution {
			if w.Distribution[i].RadianceFixed != g.Distribution[i].RadianceFixed {
				t.Fatalf("%s direction %d fixed distribution differs", id, i)
			}
		}
	}
}

func invalidationIDSet(in []Invalidation) map[string]bool {
	m := map[string]bool{}
	for _, x := range in {
		m[x.NodeID] = true
	}
	return m
}

func hasPrefix(m map[string]bool, prefix string) bool {
	for id := range m {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
