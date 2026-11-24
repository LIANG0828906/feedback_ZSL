package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"cadlab/internal/engine"
	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/render"
	"cadlab/internal/sample"
	"cadlab/internal/sched"
)

type report struct {
	GeneratedAt      string           `json:"generatedAt"`
	Seed             int64            `json:"seed"`
	AlgorithmVersion string           `json:"algorithmVersion"`
	Passed           bool             `json:"passed"`
	Sample           sampleReport     `json:"sample"`
	Preview          frameReport      `json:"preview"`
	Exact            frameReport      `json:"exact"`
	Edits            []editReport     `json:"edits"`
	Resume           resumeReport     `json:"resume"`
	Checkpoint       checkpointReport `json:"checkpoint"`
	Queue            queueReport      `json:"queue"`
	Profile          profileReport    `json:"profile"`
	Anomalies        []anomalyReport  `json:"anomalies"`
}

type sampleReport struct {
	Patches        int     `json:"patches"`
	Meshes         int     `json:"meshes"`
	TruthSamples   int     `json:"truthSamples"`
	MaxAxisError   float64 `json:"maxAxisError"`
	MaxNormalDeg   float64 `json:"maxNormalAngleDeg"`
	Tolerance      float64 `json:"tolerance"`
	AngleTolerance float64 `json:"angleToleranceDeg"`
}

type frameReport struct {
	Version     int64   `json:"version"`
	Path        string  `json:"path"`
	LOD         int     `json:"lod"`
	DurationMS  float64 `json:"durationMs"`
	EncodeMS    float64 `json:"pngEncodeMs"`
	TotalMS     float64 `json:"totalMs"`
	Tiles       int     `json:"tiles"`
	CacheHits   int     `json:"cacheHits"`
	ContentHash string  `json:"contentHash"`
	Image       string  `json:"image,omitempty"`
}

type editReport struct {
	Kind           string `json:"kind"`
	PatchID        int    `json:"patchId"`
	Version        int64  `json:"version"`
	Invalidated    int    `json:"invalidatedTiles"`
	Reused         int    `json:"reusedTiles"`
	CacheHits      int    `json:"cacheHits"`
	PixelEqual     bool   `json:"pixelEqual"`
	CoverageEqual  bool   `json:"coverageEqual"`
	ShadingEqual   bool   `json:"shadingEqual"`
	DiffDiagnostic string `json:"diffDiagnostic,omitempty"`
}

type resumeReport struct {
	RestoredVersion int64  `json:"restoredVersion"`
	LoadedTiles     int    `json:"loadedTiles"`
	PixelEqual      bool   `json:"pixelEqual"`
	Diagnostic      string `json:"diagnostic,omitempty"`
}

type checkpointReport struct {
	WriteCount int   `json:"writeCount"`
	Ordered    bool  `json:"ordered"`
	Order      []int `json:"order"`
	DBBytes    int64 `json:"dbBytes"`
}

type queueReport struct {
	PeakParallel int            `json:"peakParallel"`
	PeakPixelMB  float64        `json:"peakPixelBufferMB"`
	PeakMeshMB   float64        `json:"peakMeshBufferMB"`
	QueueRecords int            `json:"queueRecords"`
	ReasonCounts map[string]int `json:"reasonCounts"`
}

type profileReport struct {
	Version    int64 `json:"version"`
	Locked     bool  `json:"locked"`
	Segments   int   `json:"segments"`
	HasWorldUV bool  `json:"hasWorldAndParamReadings"`
}

type anomalyReport struct {
	Kind   string `json:"kind"`
	Code   string `json:"code"`
	Patch  int    `json:"patchId"`
	Tile   int    `json:"tileId"`
	Passed bool   `json:"passed"`
}

func main() {
	outPath := flag.String("out", "artifacts/verification.json", "verification report path")
	flag.Parse()
	ctx := context.Background()
	ds := sample.Generate(sample.DefaultSeed)
	start := time.Now()
	temp, err := os.MkdirTemp("", "cadlab-verify-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(temp)
	opts := engine.DefaultOptions(temp)
	opts.DBPath = filepath.Join(temp, "workbench.sqlite")
	rep := report{
		GeneratedAt: start.UTC().Format(time.RFC3339),
		Seed:        ds.Seed, AlgorithmVersion: render.AlgorithmVersion,
		Sample: measureSample(ds),
	}
	e, err := engine.New(ds, opts)
	if err != nil {
		log.Fatal(err)
	}
	preview, err := e.RenderPreview(ctx)
	if err != nil {
		log.Fatal(err)
	}
	rep.Preview = frameReportOf(preview)
	rep.Preview.EncodeMS = measurePNG(preview)
	rep.Preview.TotalMS = rep.Preview.DurationMS + rep.Preview.EncodeMS
	exact, err := e.RenderExact(ctx)
	if err != nil {
		log.Fatal(err)
	}
	rep.Exact = frameReportOf(exact)
	rep.Exact.EncodeMS = measurePNG(exact)
	rep.Exact.TotalMS = rep.Exact.DurationMS + rep.Exact.EncodeMS
	outDir := filepath.Dir(*outPath)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	previewPNG := filepath.Join(outDir, "preview-frame.png")
	exactPNG := filepath.Join(outDir, "exact-frame.png")
	if err := savePNG(previewPNG, preview); err != nil {
		log.Fatal(err)
	}
	if err := savePNG(exactPNG, exact); err != nil {
		log.Fatal(err)
	}
	rep.Preview.Image = previewPNG
	rep.Exact.Image = exactPNG
	if d := e.ValidatePathSnapshot(preview, exact); d != nil {
		log.Fatalf("path snapshot mismatch: %v", d)
	}
	rep.Checkpoint.Order = e.CheckpointWriteOrder()
	rep.Checkpoint.Ordered = ordered(rep.Checkpoint.Order)
	rep.Checkpoint.WriteCount = len(rep.Checkpoint.Order)

	material := e.SceneState().Patches[1].Material
	material.BaseColor = model.Color{R: 224, G: 68, B: 52}
	material.Specular = 0.82
	out, err := e.ApplyMaterial(ctx, 2, material)
	if err != nil {
		log.Fatal(err)
	}
	rep.Edits = append(rep.Edits, editReportFrom("material", 2, out))

	cp := e.SceneState().Patches[0].Source.Control[0]
	cp.Position.Y += 0.22
	out, err = e.ApplyControlPoint(ctx, 1, cp.Row, cp.Col, cp.Position)
	if err != nil {
		log.Fatal(err)
	}
	rep.Edits = append(rep.Edits, editReportFrom("controlPoint", 1, out))

	out, err = e.ApplyDensity(ctx, 3, 24)
	if err != nil {
		log.Fatal(err)
	}
	rep.Edits = append(rep.Edits, editReportFrom("density", 3, out))
	versionBeforeRestart := e.SceneState().Version
	queue := e.LastQueue()
	rep.Queue = queueReportOf(queue)
	if err := e.Close(); err != nil {
		log.Fatal(err)
	}

	restarted, err := engine.New(ds, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer restarted.Close()
	resume, err := restarted.Resume(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if resume.Diagnostic != nil || resume.Frame == nil {
		log.Fatalf("resume failed: %+v", resume)
	}
	full, err := restarted.RenderExact(ctx)
	if err != nil {
		log.Fatal(err)
	}
	diff := render.CompareFrames(resume.Frame, full)
	rep.Resume = resumeReport{
		RestoredVersion: restarted.SceneState().Version,
		LoadedTiles:     resume.LoadedTiles,
		PixelEqual:      diff == nil,
	}
	if diff != nil {
		rep.Resume.Diagnostic = diff.Error()
	}
	rep.Checkpoint.Order = restarted.CheckpointWriteOrder()
	rep.Checkpoint.Ordered = ordered(rep.Checkpoint.Order)
	rep.Checkpoint.WriteCount = len(rep.Checkpoint.Order)
	if info, err := os.Stat(opts.DBPath); err == nil {
		rep.Checkpoint.DBBytes = info.Size()
	}
	cmp, err := restarted.ComparePaths()
	if err != nil {
		log.Fatal(err)
	}
	if cmp.FullVsResume == nil || !cmp.FullVsResume.Equal {
		log.Fatalf("three-path resume comparison failed: %+v", cmp)
	}

	profile, err := restarted.Profile(ctx, model.Vec3{Y: 1}, 0)
	if err != nil {
		log.Fatal(err)
	}
	rep.Profile = profileReport{Version: profile.Version, Locked: profile.Locked, Segments: len(profile.Segments)}
	if len(profile.Segments) > 0 {
		seg := profile.Segments[0]
		rep.Profile.HasWorldUV = seg.A.World.IsFinite() && seg.B.World.IsFinite() &&
			!math.IsNaN(seg.A.Param.U) && !math.IsNaN(seg.A.Param.V)
	}
	for _, tc := range []struct{ kind, code string }{
		{"missing-control-point", model.CodeGeom},
		{"knot-nonmonotonic", model.CodeKnot},
		{"density-out-of-range", model.CodePara},
		{"tile-stage-failure", model.CodeStage},
	} {
		res, err := restarted.InjectAnomaly(ctx, tc.kind, 5)
		if err != nil {
			log.Fatal(err)
		}
		ar := anomalyReport{Kind: tc.kind, Tile: res.TileID, Patch: res.PatchID}
		if res.Diagnostic != nil {
			ar.Code = res.Diagnostic.Code
			ar.Tile = res.Diagnostic.TileID
			ar.Patch = res.Diagnostic.PatchID
		}
		ar.Passed = ar.Code == tc.code
		rep.Anomalies = append(rep.Anomalies, ar)
	}
	rep.Passed = verdict(rep, versionBeforeRestart)
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*outPath, raw, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verification %s in %s\n", passText(rep.Passed), time.Since(start).Round(time.Millisecond))
	fmt.Printf("report: %s\n", *outPath)
	fmt.Printf("preview %.2fms (PNG total %.2fms) · exact %.2fms · edits %d · resume %d tiles · anomalies %d\n",
		rep.Preview.DurationMS, rep.Preview.TotalMS, rep.Exact.DurationMS,
		len(rep.Edits), rep.Resume.LoadedTiles, len(rep.Anomalies))
	if !rep.Passed {
		os.Exit(1)
	}
}

func measureSample(ds model.Dataset) sampleReport {
	out := sampleReport{
		Patches: len(ds.Patches), Meshes: len(ds.Meshes), TruthSamples: len(ds.Truth.Samples),
		Tolerance: ds.Tolerance, AngleTolerance: ds.AngleTol,
	}
	for _, p := range ds.Patches {
		for _, tr := range ds.Truth.Samples {
			if tr.PatchID != p.ID {
				continue
			}
			ev := geom.Evaluate(p, tr.U, tr.V)
			out.MaxAxisError = math.Max(out.MaxAxisError, math.Abs(ev.Position.X-tr.Position.X))
			out.MaxAxisError = math.Max(out.MaxAxisError, math.Abs(ev.Position.Y-tr.Position.Y))
			out.MaxAxisError = math.Max(out.MaxAxisError, math.Abs(ev.Position.Z-tr.Position.Z))
			dot := math.Max(-1, math.Min(1, ev.Normal.Dot(tr.Normal)))
			angle := math.Acos(dot) * 180 / math.Pi
			if angle > out.MaxNormalDeg {
				out.MaxNormalDeg = angle
			}
		}
	}
	return out
}

func frameReportOf(f *render.Frame) frameReport {
	return frameReport{
		Version: f.Version, Path: f.Path, LOD: f.LOD,
		DurationMS: float64(f.Duration.Microseconds()) / 1000,
		Tiles:      len(f.Tiles), CacheHits: f.CacheHits, ContentHash: f.ContentHash,
	}
}

func savePNG(path string, frame *render.Frame) error {
	if frame == nil || frame.Image == nil {
		return fmt.Errorf("frame image unavailable")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, frame.Image); err != nil {
		return err
	}
	return f.Sync()
}

func measurePNG(frame *render.Frame) float64 {
	start := time.Now()
	_ = frame.PNGBase64()
	return float64(time.Since(start).Microseconds()) / 1000
}

func editReportFrom(kind string, patchID int, out *engine.EditOutcome) editReport {
	r := editReport{
		Kind: kind, PatchID: patchID, Version: out.Frame.Version,
		Invalidated: len(out.Invalidation.InvalidatedTiles), Reused: len(out.Invalidation.ReusedTiles),
		CacheHits:  out.Frame.CacheHits,
		PixelEqual: render.CompareFrames(out.Frame, out.Reference) == nil,
	}
	if out.Comparison != nil {
		r.CoverageEqual = out.Comparison.CoverageEqual
		r.ShadingEqual = out.Comparison.ShadingEqual
		if out.Comparison.FullVsIncremental.Diagnostic != nil {
			r.DiffDiagnostic = out.Comparison.FullVsIncremental.Diagnostic.Error()
		}
	}
	return r
}

func queueReportOf(q sched.Metrics) queueReport {
	out := queueReport{
		PeakParallel: q.PeakParallel,
		PeakPixelMB:  float64(q.PeakPixelBytes) / (1 << 20),
		PeakMeshMB:   float64(q.PeakMeshBytes) / (1 << 20),
		QueueRecords: len(q.Queue),
		ReasonCounts: map[string]int{},
	}
	for _, rec := range q.Queue {
		out.ReasonCounts[rec.Reason]++
	}
	return out
}

func ordered(ids []int) bool {
	for i, id := range ids {
		if id != i {
			return false
		}
	}
	return true
}

func verdict(r report, expectedVersion int64) bool {
	if r.Sample.MaxAxisError > r.Sample.Tolerance || r.Sample.MaxNormalDeg > r.Sample.AngleTolerance {
		return false
	}
	if r.Preview.TotalMS > 50 || r.Preview.Version != r.Exact.Version {
		return false
	}
	if !r.Checkpoint.Ordered || !r.Resume.PixelEqual || r.Resume.RestoredVersion != expectedVersion {
		return false
	}
	for _, e := range r.Edits {
		if e.Invalidated == 0 || e.Reused == 0 || !e.PixelEqual || !e.CoverageEqual || !e.ShadingEqual {
			return false
		}
	}
	for _, a := range r.Anomalies {
		if !a.Passed {
			return false
		}
	}
	return r.Profile.Locked && r.Profile.Segments > 0
}

func passText(v bool) string {
	if v {
		return "PASSED"
	}
	return "FAILED"
}
