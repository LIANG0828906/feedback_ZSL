package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"spatialpick"
)

func main() {
	dataDir := flag.String("data", "data", "directory containing scene.json and truth.json")
	snapshotPath := flag.String("snapshot", filepath.Join("data", "editor.snapshot"), "snapshot file used by the resume path")
	checkpoint := flag.Int("checkpoint", 2, "number of edit batches persisted before restart")
	flag.Parse()

	scenePath := filepath.Join(*dataDir, "scene.json")
	truthPath := filepath.Join(*dataDir, "truth.json")
	if _, err := os.Stat(scenePath); err != nil {
		fail("missing %s; run: go run ./cmd/scenegen -out %s", scenePath, *dataDir)
	}
	if _, err := os.Stat(truthPath); err != nil {
		fail("missing %s; run: go run ./cmd/scenegen -out %s", truthPath, *dataDir)
	}
	scene, truth, err := spatialpick.LoadSceneWithTruth(scenePath, truthPath)
	if err != nil {
		fail("load and validate sample: %v", err)
	}
	fmt.Printf("loaded scene=%s nodes=%d geometries=%d precision=%d hash=%s\n",
		scene.SceneVersion, len(scene.Nodes), len(scene.Geometries), scene.Precision, short(scene.ContentHash))

	engine, err := spatialpick.NewEngine(scene)
	if err != nil {
		fail("build engine: %v", err)
	}
	baseChecks, baseAmbig := checkRayTruth(engine, scene, truth.RayResults, true)
	boxChecks, boxAmbig := checkBoxTruth(engine, scene, truth.BoxResults, true)
	pointChecks, pointAmbig := checkPointTruth(engine, scene, truth.PointResults, true)
	fmt.Printf("base truth: rays=%d boxes=%d points=%d ambiguities=%d\n",
		baseChecks, boxChecks, pointChecks, baseAmbig+boxAmbig+pointAmbig)

	maxMatrixDelta := 0.0
	for i, batch := range scene.EditSequence {
		plan, err := engine.PlanPivotBatch(batch)
		if err != nil {
			fail("plan batch %s: %v", batch.ID, err)
		}
		if _, err := engine.CommitBatch(plan); err != nil {
			fail("commit batch %s: %v", batch.ID, err)
		}
		want := truth.OperationResults[i]
		for _, expected := range want.Matrices {
			derived, err := engine.GetDerived(expected.NodeID, 0)
			if err != nil {
				fail("read %s after %s: %v", expected.NodeID, batch.ID, err)
			}
			for k := 0; k < 16; k++ {
				maxMatrixDelta = math.Max(maxMatrixDelta, math.Abs(derived.WorldFloat[k]-expected.Matrix[k]))
			}
		}
		fmt.Printf("batch %s: invalidated caches=%d index_entries=%d revision=%d\n",
			batch.ID, len(plan.Invalidation.CacheItems), len(plan.Invalidation.IndexItems), engine.Revision())
	}
	if maxMatrixDelta > 5e-5 {
		fail("world matrix truth delta %.9g exceeds fixed tolerance", maxMatrixDelta)
	}
	finalRays, finalRayAmbig := checkRayTruth(engine, scene, truth.FinalRayResults, false)
	finalBoxes, finalBoxAmbig := checkBoxTruth(engine, scene, truth.FinalBoxResults, false)
	finalPoints, finalPointAmbig := checkPointTruth(engine, scene, truth.FinalPointResults, false)
	fmt.Printf("edited truth: matrices max_delta=%.3g rays=%d boxes=%d points=%d ambiguities=%d\n",
		maxMatrixDelta, finalRays, finalBoxes, finalPoints, finalRayAmbig+finalBoxAmbig+finalPointAmbig)

	three, err := spatialpick.RunThreePaths(context.Background(), scene, *checkpoint, *snapshotPath)
	if err != nil {
		fail("three-path verification: %v", err)
	}
	fmt.Printf("three paths agree: checkpoint=%d revision=%d ambiguous_queries=%d\n",
		three.Checkpoint, three.Incremental.Revision, countAmbigQueries(&three.Incremental))

	if err := engine.SaveSnapshot(context.Background(), *snapshotPath); err != nil {
		fail("save snapshot: %v", err)
	}
	restored, status, err := spatialpick.OpenEngineWithSnapshot(context.Background(), scene, *snapshotPath)
	if err != nil || !status.Loaded {
		fail("restart load: status=%+v err=%v", status, err)
	}
	left, right := engine.Bundle(), restored.Bundle()
	if err := spatialpick.CompareBundles(&left, &right); err != nil {
		fail("restored state differs: %v", err)
	}
	fmt.Printf("snapshot restored: revision=%d index_version=%d bytes=%d\n",
		restored.Revision(), restored.IndexVersion(), fileSize(*snapshotPath))
	fmt.Println("OK")
}

func checkRayTruth(engine *spatialpick.Engine, scene *spatialpick.Scene, truths []spatialpick.RayTruth, verbose bool) (int, int) {
	ambiguous := 0
	for _, want := range truths {
		query := findRay(scene, want.QueryID)
		got, err := engine.PickRay(query)
		if !sameCode(err, want.ErrorCode) {
			fail("ray %s code: got %v want %s", want.QueryID, err, want.ErrorCode)
		}
		if got.ErrorCode != want.ErrorCode {
			fail("ray %s result code mismatch", want.QueryID)
		}
		if want.First == nil {
			if got.First != nil {
				fail("ray %s unexpected hit %+v", want.QueryID, got.First)
			}
		} else if got.First == nil || got.First.NodeID != want.First.NodeID || math.Abs(got.First.T-want.First.T) > 2e-4 {
			fail("ray %s hit got=%+v want=%+v", want.QueryID, got.First, want.First)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			fail("ray %s ambiguity locations got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
		if want.ErrorCode == spatialpick.CodeAmbiguous {
			ambiguous++
		}
		if verbose {
			hit := "miss"
			if got.First != nil {
				hit = fmt.Sprintf("%s t=%.6f", got.First.NodeID, got.First.T)
			}
			fmt.Printf("  ray %-20s %-8s %s\n", want.QueryID, codeOrOK(want.ErrorCode), hit)
		}
	}
	return len(truths), ambiguous
}

func checkBoxTruth(engine *spatialpick.Engine, scene *spatialpick.Scene, truths []spatialpick.BoxTruth, verbose bool) (int, int) {
	ambiguous := 0
	for _, want := range truths {
		query := findBox(scene, want.QueryID)
		got, err := engine.SelectBox(query)
		if !sameCode(err, want.ErrorCode) || got.ErrorCode != want.ErrorCode {
			fail("box %s code: got=%v/%s want=%s", want.QueryID, err, got.ErrorCode, want.ErrorCode)
		}
		if !equalStrings(got.Hits, want.Hits) {
			fail("box %s hits got=%v want=%v", want.QueryID, got.Hits, want.Hits)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			fail("box %s ambiguity locations got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
		if want.ErrorCode == spatialpick.CodeAmbiguous {
			ambiguous++
		}
		if verbose {
			fmt.Printf("  box %-20s %-8s hits=%v\n", want.QueryID, codeOrOK(want.ErrorCode), got.Hits)
		}
	}
	return len(truths), ambiguous
}

func checkPointTruth(engine *spatialpick.Engine, scene *spatialpick.Scene, truths []spatialpick.PointTruth, verbose bool) (int, int) {
	ambiguous := 0
	for _, want := range truths {
		query := findPoint(scene, want.QueryID)
		got, err := engine.ContainsPoint(query)
		if !sameCode(err, want.ErrorCode) || got.ErrorCode != want.ErrorCode {
			fail("point %s code: got=%v/%s want=%s", want.QueryID, err, got.ErrorCode, want.ErrorCode)
		}
		if !equalStrings(got.Hits, want.Hits) {
			fail("point %s hits got=%v want=%v", want.QueryID, got.Hits, want.Hits)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			fail("point %s ambiguity locations got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
		if want.ErrorCode == spatialpick.CodeAmbiguous {
			ambiguous++
		}
		if verbose {
			fmt.Printf("  point %-18s %-8s hits=%v\n", want.QueryID, codeOrOK(want.ErrorCode), got.Hits)
		}
	}
	return len(truths), ambiguous
}

func sameCode(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return spatialpick.IsCode(err, want)
}

func sameAmbiguityLocations(got, want []spatialpick.Ambiguity) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].QueryID != want[i].QueryID || got[i].NodeID != want[i].NodeID ||
			got[i].GeometryID != want[i].GeometryID || got[i].Depth != want[i].Depth {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countAmbigQueries(b *spatialpick.ResultBundle) int {
	n := 0
	for _, r := range b.Rays {
		if r.ErrorCode == spatialpick.CodeAmbiguous {
			n++
		}
	}
	for _, r := range b.Boxes {
		if r.ErrorCode == spatialpick.CodeAmbiguous {
			n++
		}
	}
	for _, r := range b.Points {
		if r.ErrorCode == spatialpick.CodeAmbiguous {
			n++
		}
	}
	return n
}

func findRay(scene *spatialpick.Scene, id string) spatialpick.RayQuery {
	for _, q := range scene.RayQueries {
		if q.ID == id {
			return q
		}
	}
	fail("scene has no ray query %s", id)
	return spatialpick.RayQuery{}
}

func findBox(scene *spatialpick.Scene, id string) spatialpick.BoxQuery {
	for _, q := range scene.BoxQueries {
		if q.ID == id {
			return q
		}
	}
	fail("scene has no box query %s", id)
	return spatialpick.BoxQuery{}
}

func findPoint(scene *spatialpick.Scene, id string) spatialpick.PointQuery {
	for _, q := range scene.PointQueries {
		if q.ID == id {
			return q
		}
	}
	fail("scene has no point query %s", id)
	return spatialpick.PointQuery{}
}

func codeOrOK(code string) string {
	if code == "" {
		return "OK"
	}
	return code
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
