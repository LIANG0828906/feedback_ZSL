package spatialpick

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSampleFilesAreDeterministicAndLoadAsPair(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	if _, _, err := WriteSample(dir1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteSample(dir2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scene.json", "truth.json"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s is not byte-deterministic", name)
		}
	}
	scene, truth, err := LoadSceneWithTruth(filepath.Join(dir1, "scene.json"), filepath.Join(dir1, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(truth.RayResults) != len(scene.RayQueries) {
		t.Fatal("scene/truth query pair mismatch")
	}
	if _, err := NewEngine(scene); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSampleLoadsAndMatchesTruth(t *testing.T) {
	scene, truth, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range truth.RayResults {
		got, qerr := engine.PickRay(RayQuery{ID: want.QueryID, Screen: screenQuery(scene, want.QueryID)})
		if qerr != nil && !IsCode(qerr, want.ErrorCode) {
			t.Fatalf("ray %s error %v, want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if qerr == nil && want.ErrorCode != "" {
			t.Fatalf("ray %s got success, truth=%s", want.QueryID, want.ErrorCode)
		}
		if got.ErrorCode != want.ErrorCode {
			t.Fatalf("ray %s code %q want %q ambiguities=%v truth=%v", want.QueryID, got.ErrorCode, want.ErrorCode, got.Ambiguities, want.Ambiguities)
		}
		if want.First == nil {
			if got.First != nil {
				t.Fatalf("ray %s got hit %+v, truth miss", want.QueryID, got.First)
			}
		} else if got.First == nil || got.First.NodeID != want.First.NodeID {
			t.Fatalf("ray %s got %+v want node %s", want.QueryID, got.First, want.First.NodeID)
		} else if math.Abs(got.First.T-want.First.T) > 2e-4 || got.First.Point.Sub(want.First.Point).Length() > 2e-3 {
			t.Fatalf("ray %s hit mismatch got=(%.9f,%v) want=(%.9f,%v)", want.QueryID, got.First.T, got.First.Point, want.First.T, want.First.Point)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("ray %s ambiguity mismatch got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
	}
	for _, want := range truth.BoxResults {
		got, qerr := engine.SelectBox(BoxQuery{ID: want.QueryID, Rect: boxQuery(scene, want.QueryID)})
		if qerr != nil && !IsCode(qerr, want.ErrorCode) {
			t.Fatalf("box %s error %v want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if got.ErrorCode != want.ErrorCode {
			t.Fatalf("box %s code %q want %q ambig=%v truth=%v", want.QueryID, got.ErrorCode, want.ErrorCode, got.Ambiguities, want.Ambiguities)
		}
		if !reflect.DeepEqual(got.Hits, want.Hits) {
			t.Fatalf("box %s hits got %v want %v", want.QueryID, got.Hits, want.Hits)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("box %s ambiguity mismatch got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
	}
	for _, want := range truth.PointResults {
		got, qerr := engine.ContainsPoint(PointQuery{ID: want.QueryID, Point: pointQuery(scene, want.QueryID)})
		if qerr != nil && !IsCode(qerr, want.ErrorCode) {
			t.Fatalf("point %s error %v want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if got.ErrorCode != want.ErrorCode {
			t.Fatalf("point %s code %q want %q ambig=%v truth=%v", want.QueryID, got.ErrorCode, want.ErrorCode, got.Ambiguities, want.Ambiguities)
		}
		if !reflect.DeepEqual(got.Hits, want.Hits) {
			t.Fatalf("point %s hits got %v want %v", want.QueryID, got.Hits, want.Hits)
		}
		if !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("point %s ambiguity mismatch got=%v want=%v", want.QueryID, got.Ambiguities, want.Ambiguities)
		}
	}
}

func TestGeneratedEditSequenceMatchesTruth(t *testing.T) {
	scene, truth, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	for i, batch := range scene.EditSequence {
		if _, err := engine.ApplyPivotBatch(batch); err != nil {
			t.Fatalf("batch %s: %v", batch.ID, err)
		}
		want := truth.OperationResults[i]
		for _, nm := range want.Matrices {
			d, err := engine.GetDerived(nm.NodeID, 0)
			if err != nil {
				t.Fatal(err)
			}
			for k := 0; k < 16; k++ {
				if math.Abs(d.WorldFloat[k]-nm.Matrix[k]) > 5e-5 {
					t.Fatalf("batch %s node %s matrix[%d] got %.12f want %.12f", batch.ID, nm.NodeID, k, d.WorldFloat[k], nm.Matrix[k])
				}
			}
		}
	}
	for _, want := range truth.FinalRayResults {
		got, qerr := engine.PickRay(RayQuery{ID: want.QueryID, Screen: screenQuery(scene, want.QueryID)})
		if !errorCodeMatches(qerr, want.ErrorCode) {
			t.Fatalf("final ray %s error %v want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if got.ErrorCode != want.ErrorCode || !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("final ray %s ambiguity got=%v want=%v code=%s/%s", want.QueryID, got.Ambiguities, want.Ambiguities, got.ErrorCode, want.ErrorCode)
		}
		if want.First == nil {
			if got.First != nil {
				t.Fatalf("final ray %s got hit want miss", want.QueryID)
			}
		} else if got.First == nil || got.First.NodeID != want.First.NodeID {
			t.Fatalf("final ray %s got %+v want node %s", want.QueryID, got.First, want.First.NodeID)
		}
	}
	for _, want := range truth.FinalBoxResults {
		got, qerr := engine.SelectBox(BoxQuery{ID: want.QueryID, Rect: boxQuery(scene, want.QueryID)})
		if !errorCodeMatches(qerr, want.ErrorCode) {
			t.Fatalf("final box %s error %v want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if !reflect.DeepEqual(got.Hits, want.Hits) || !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("final box %s got hits=%v amb=%v want hits=%v amb=%v", want.QueryID, got.Hits, got.Ambiguities, want.Hits, want.Ambiguities)
		}
	}
	for _, want := range truth.FinalPointResults {
		got, qerr := engine.ContainsPoint(PointQuery{ID: want.QueryID, Point: pointQuery(scene, want.QueryID)})
		if !errorCodeMatches(qerr, want.ErrorCode) {
			t.Fatalf("final point %s error %v want %s", want.QueryID, qerr, want.ErrorCode)
		}
		if !reflect.DeepEqual(got.Hits, want.Hits) || !sameAmbiguityLocations(got.Ambiguities, want.Ambiguities) {
			t.Fatalf("final point %s got hits=%v amb=%v want hits=%v amb=%v", want.QueryID, got.Hits, got.Ambiguities, want.Hits, want.Ambiguities)
		}
	}
}

func TestThreePathsAgree(t *testing.T) {
	scene, _, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "checkpoint.snapshot")
	got, err := RunThreePaths(context.Background(), scene, 2, tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"ray-ambig-sphere"} {
		if len(got.Incremental.Rays) == 0 {
			t.Fatal("empty ray bundle")
		}
		found := false
		for _, r := range got.Incremental.Rays {
			if r.QueryID == code {
				found = true
				if r.ErrorCode != CodeAmbiguous {
					t.Fatalf("expected ambiguity position preserved for %s", code)
				}
			}
		}
		if !found {
			t.Fatalf("query %s missing", code)
		}
	}
}

func errorCodeMatches(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return IsCode(err, want)
}

func sameAmbiguityLocations(got, want []Ambiguity) bool {
	g := make([]string, len(got))
	w := make([]string, len(want))
	for i := range got {
		g[i] = got[i].QueryID + "/" + got[i].NodeID + "/" + got[i].GeometryID
	}
	for i := range want {
		w[i] = want[i].QueryID + "/" + want[i].NodeID + "/" + want[i].GeometryID
	}
	return reflect.DeepEqual(g, w)
}

func screenQuery(scene *Scene, id string) Vec2 {
	for _, q := range scene.RayQueries {
		if q.ID == id {
			return q.Screen
		}
	}
	return Vec2{}
}

func boxQuery(scene *Scene, id string) Rect {
	for _, q := range scene.BoxQueries {
		if q.ID == id {
			return q.Rect
		}
	}
	return Rect{}
}

func pointQuery(scene *Scene, id string) Vec3 {
	for _, q := range scene.PointQueries {
		if q.ID == id {
			return q.Point
		}
	}
	return Vec3{}
}
