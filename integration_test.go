package vectorcore_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"example.com/vectorcore"
	"example.com/vectorcore/internal/samplegen"
	"example.com/vectorcore/internal/sampletruth"
)

func TestGeneratedSamplesAllVersions(t *testing.T) {
	cfg := vectorcore.DefaultConfig()
	cfg.Grid = 2
	cfg.Tolerance = 512
	cfg.MaxWorkers = 2
	dir := t.TempDir()
	manifest, err := samplegen.Generate(samplegen.Options{Seed: 424242, Batches: 1, OutDir: dir, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Batches) != 1 {
		t.Fatalf("batch count = %d", len(manifest.Batches))
	}
	var baseline *vectorcore.TimelineResult
	for version := 1; version <= 3; version++ {
		batch := manifest.Batches[0]
		scenePath := filepath.Join(dir, batch.Scenes[versionKey(version)])
		truthPath := filepath.Join(dir, batch.Truth[versionKey(version)])
		scene, err := vectorcore.LoadSceneFile(scenePath)
		if err != nil {
			t.Fatalf("v%d scene: %v", version, err)
		}
		if scene.SourceVersion != version {
			t.Fatalf("v%d source version = %d", version, scene.SourceVersion)
		}
		truth, err := sampletruth.Load(truthPath)
		if err != nil {
			t.Fatalf("v%d truth: %v", version, err)
		}
		renderCfg := truth.Config.ToConfig()
		renderCfg.MaxWorkers = 2
		engine, err := vectorcore.NewEngine(scene, renderCfg)
		if err != nil {
			t.Fatalf("v%d engine: %v", version, err)
		}
		timeline, err := engine.RenderAll(context.Background())
		if err != nil {
			t.Fatalf("v%d render: %v", version, err)
		}
		if len(timeline.Times) != len(truth.Samples) {
			t.Fatalf("v%d time count %d != truth %d", version, len(timeline.Times), len(truth.Samples))
		}
		truthDir := filepath.Dir(truthPath)
		for ti, sample := range truth.Samples {
			tr := timeline.Times[ti]
			if tr.TimeIndex != sample.TimeIndex || tr.TimeTicks != sample.TimeTicks {
				t.Fatalf("v%d time %d metadata differs", version, ti)
			}
			x, y, maxDelta, ok, err := sampletruth.CompareFile(tr.Bitmap, filepath.Join(truthDir, sample.PixelFile))
			if err != nil {
				t.Fatalf("v%d time %d pixels: %v", version, ti, err)
			}
			if !ok || maxDelta != 0 {
				t.Fatalf("v%d time %d pixel mismatch at (%d,%d), delta=%d", version, ti, x, y, maxDelta)
			}
			if tr.Bitmap.Width != sample.BitmapWidth || tr.Bitmap.Height != sample.BitmapHeight ||
				tr.Viewport.Bounds != sample.BBox {
				t.Fatalf("v%d time %d bbox/dimensions differ", version, ti)
			}
			for _, pt := range sample.Paths {
				var pf *vectorcore.PathFrame
				for pi := range tr.Paths {
					if tr.Paths[pi].PathID == pt.PathID {
						pf = &tr.Paths[pi]
						break
					}
				}
				if pf == nil {
					t.Fatalf("v%d time %d missing path %s", version, ti, pt.PathID)
				}
				if pf.AnalyticArea != pt.AnalyticArea || pf.Bounds != pt.BBox {
					t.Fatalf("v%d time %d path %s area/bbox differs", version, ti, pt.PathID)
				}
				for _, cs := range pt.CurveSamples {
					points, err := vectorcore.SampleSegment(pf.Contour, cs.Segment, 4)
					if err != nil || !reflect.DeepEqual(points, cs.Points) {
						t.Fatalf("v%d time %d path %s segment %d truth mismatch", version, ti, pt.PathID, cs.Segment)
					}
				}
			}
			if sha := sampletruth.PixelHash(tr.Bitmap.Pixels); sha != sample.PixelSHA256 {
				t.Fatalf("v%d time %d pixel hash differs", version, ti)
			}
		}
		if baseline == nil {
			baseline = timeline
		} else {
			if diff, err := vectorcore.CompareTimelines(baseline, timeline); err != nil {
				t.Fatalf("version timeline mismatch: %+v %v", diff, err)
			}
		}
	}
}

func versionKey(version int) string {
	if version == 1 {
		return "v1"
	}
	if version == 2 {
		return "v2"
	}
	return "v3"
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
