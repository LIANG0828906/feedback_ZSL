package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"example.com/vectorcore"
	"example.com/vectorcore/internal/sampletruth"
)

type report struct {
	SceneID       string       `json:"sceneId"`
	SourceVersion int          `json:"sourceVersion"`
	Match         bool         `json:"match"`
	Tolerance     int          `json:"pixelTolerance"`
	MaxDelta      int          `json:"maxPixelDelta"`
	FirstMismatch *mismatch    `json:"firstMismatch,omitempty"`
	Samples       []sampleInfo `json:"samples"`
	Errors        []string     `json:"errors,omitempty"`
}

type mismatch struct {
	TimeIndex int    `json:"timeIndex"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	PathID    string `json:"pathId,omitempty"`
	Reason    string `json:"reason"`
}

type sampleInfo struct {
	TimeIndex   int               `json:"timeIndex"`
	CommitSeq   int64             `json:"commitSeq"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	PNG         string            `json:"png"`
	PixelSHA256 string            `json:"pixelSha256"`
	MaxDelta    int               `json:"maxPixelDelta"`
	Match       bool              `json:"match"`
	BBox        vectorcore.Bounds `json:"bbox"`
}

func main() {
	scenePath := flag.String("scene", "samples/batch-01/scene.v3.json", "scene JSON")
	truthPath := flag.String("truth", "samples/batch-01/truth.v3.json", "truth JSON")
	outDir := flag.String("out", "out/rendered", "output directory")
	dbPath := flag.String("db", "out/checkpoint.db", "SQLite checkpoint path; empty disables persistence")
	fresh := flag.Bool("fresh", false, "remove an existing checkpoint before rendering")
	workers := flag.Int("workers", 0, "override max workers (0 uses truth/default config)")
	pixelTolerance := flag.Int("pixel-tolerance", 0, "accepted per-channel pixel delta")
	flag.Parse()

	if err := run(*scenePath, *truthPath, *outDir, *dbPath, *fresh, *workers, *pixelTolerance); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(scenePath, truthPath, outDir, dbPath string, fresh bool, workers, pixelTolerance int) error {
	scene, err := vectorcore.LoadSceneFile(scenePath)
	if err != nil {
		return fmt.Errorf("load scene: %w", err)
	}
	truth, err := sampletruth.Load(truthPath)
	if err != nil {
		return fmt.Errorf("load truth: %w", err)
	}
	cfg := truth.Config.ToConfig()
	if workers > 0 {
		cfg.MaxWorkers = workers
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	engine, err := vectorcore.NewEngine(scene, cfg)
	if err != nil {
		return err
	}
	var store *vectorcore.CheckpointStore
	if dbPath != "" {
		if fresh {
			_ = os.Remove(dbPath)
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
		}
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return err
		}
		store, err = vectorcore.OpenCheckpoint(dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := engine.LoadCheckpoint(context.Background(), store); err != nil {
			return fmt.Errorf("resume checkpoint: %w", err)
		}
	}
	timeline, err := engine.RenderAll(context.Background())
	if err != nil {
		return err
	}
	rep := report{SceneID: scene.ID, SourceVersion: scene.SourceVersion, Match: true, Tolerance: pixelTolerance}
	truthByIndex := make(map[int]sampletruth.Sample, len(truth.Samples))
	for _, s := range truth.Samples {
		truthByIndex[s.TimeIndex] = s
	}
	truthDir := filepath.Dir(truthPath)
	for _, tr := range timeline.Times {
		sample, ok := truthByIndex[tr.TimeIndex]
		if !ok {
			rep.Match = false
			rep.Errors = append(rep.Errors, fmt.Sprintf("truth missing time %d", tr.TimeIndex))
			continue
		}
		info := sampleInfo{TimeIndex: tr.TimeIndex, CommitSeq: tr.CommitSeq, Width: tr.Bitmap.Width,
			Height: tr.Bitmap.Height, BBox: tr.Viewport.Bounds, Match: true}
		pngName := fmt.Sprintf("time-%02d.png", tr.TimeIndex)
		if err := writePNG(filepath.Join(outDir, pngName), tr.Bitmap); err != nil {
			return err
		}
		info.PNG = pngName
		sum := sha256.Sum256(tr.Bitmap.Pixels)
		info.PixelSHA256 = hex.EncodeToString(sum[:])
		if sample.BitmapWidth != 0 {
			x, y, maxDelta, ok, err := sampletruth.CompareFile(tr.Bitmap, filepath.Join(truthDir, sample.PixelFile))
			if err != nil {
				return err
			}
			info.MaxDelta = maxDelta
			if maxDelta > rep.MaxDelta {
				rep.MaxDelta = maxDelta
			}
			if !ok || maxDelta > pixelTolerance {
				info.Match = false
				rep.Match = false
				if rep.FirstMismatch == nil {
					rep.FirstMismatch = &mismatch{TimeIndex: tr.TimeIndex, X: x, Y: y,
						PathID: compositeOwner(tr), Reason: fmt.Sprintf("pixel delta %d", maxDelta)}
				}
			}
			if tr.Bitmap.Width != sample.BitmapWidth || tr.Bitmap.Height != sample.BitmapHeight ||
				tr.Viewport.Bounds != sample.BBox {
				info.Match = false
				rep.Match = false
				rep.Errors = append(rep.Errors, fmt.Sprintf("time %d viewport metadata differs", tr.TimeIndex))
			}
		}
		for _, pt := range sample.Paths {
			pf := findPath(tr, pt.PathID)
			if pf == nil {
				rep.Match = false
				rep.Errors = append(rep.Errors, fmt.Sprintf("time %d missing path %s", tr.TimeIndex, pt.PathID))
				continue
			}
			if pf.AnalyticArea != pt.AnalyticArea || pf.Bounds != pt.BBox {
				rep.Match = false
				rep.Errors = append(rep.Errors, fmt.Sprintf("time %d path %s area/bbox differs", tr.TimeIndex, pt.PathID))
			}
			for _, cs := range pt.CurveSamples {
				points, err := vectorcore.SampleSegment(pf.Contour, cs.Segment, 4)
				if err != nil || !reflect.DeepEqual(points, cs.Points) {
					rep.Match = false
					rep.Errors = append(rep.Errors, fmt.Sprintf("time %d path %s segment %d samples differ", tr.TimeIndex, pt.PathID, cs.Segment))
					break
				}
			}
		}
		rep.Samples = append(rep.Samples, info)
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	data = append(data, '\n')
	reportPath := filepath.Join(outDir, "diff-report.json")
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		return err
	}
	if !rep.Match {
		return fmt.Errorf("%s: bitmap/truth mismatch; report=%s", vectorcore.CodeDiff, reportPath)
	}
	fmt.Printf("rendered %d samples, match=true, maxPixelDelta=%d, report=%s\n", len(rep.Samples), rep.MaxDelta, reportPath)
	return nil
}

func compositeOwner(tr *vectorcore.TimeResult) string {
	if len(tr.Paths) == 0 {
		return ""
	}
	return tr.Paths[len(tr.Paths)-1].PathID
}

func findPath(tr *vectorcore.TimeResult, id string) *vectorcore.PathFrame {
	for i := range tr.Paths {
		if tr.Paths[i].PathID == id {
			return &tr.Paths[i]
		}
	}
	return nil
}

func writePNG(path string, bitmap *vectorcore.Bitmap) error {
	if bitmap == nil || bitmap.Width <= 0 || bitmap.Height <= 0 {
		return fmt.Errorf("invalid bitmap")
	}
	img := &image.NRGBA{Pix: bitmap.Pixels, Stride: bitmap.Width * 4, Rect: image.Rect(0, 0, bitmap.Width, bitmap.Height)}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("encode %s: %w", strings.TrimSpace(path), err)
	}
	return nil
}
