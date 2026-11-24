package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"strata/internal/formats"
	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
	"strata/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generate(os.Args[2:])
	case "validate":
		validate(os.Args[2:])
	case "serve":
		fmt.Fprintln(os.Stderr, "serve is provided by the root web entrypoint: go run . -addr 127.0.0.1:4173")
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `strata <command>

  generate -seed 195 -out data/samples
  validate -dir data/samples -json artifacts/validation.json`)
}

func generate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	seed := fs.Int("seed", 195, "deterministic sample seed")
	out := fs.String("out", "data/samples", "sample output directory")
	_ = fs.Parse(args)
	manifest, err := generator.Generate(*seed, *out)
	if err != nil {
		fail(err)
	}
	printJSON(manifest)
}

func validate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	dir := fs.String("dir", "data/samples", "sample directory")
	jsonOut := fs.String("json", "artifacts/validation.json", "validation report path")
	_ = fs.Parse(args)
	manifest, err := generator.LoadManifest(*dir)
	if err != nil {
		fail(err)
	}
	cfg := pipeline.DefaultConfig()
	builder := pipeline.NewBuilder()
	versionReports := make([]map[string]any, 0, len(manifest.Files))
	var firstMesh *model.Mesh
	var firstClean []model.Point
	var firstGeometry *model.Geometry
	allVersionsPassed := true
	for _, file := range manifest.Files {
		geometry, err := formats.LoadFile(filepath.Join(*dir, file.File), file.VersionID)
		if err != nil {
			fail(err)
		}
		result, err := builder.Build(geometry, cfg, "exact", 1)
		if err != nil {
			fail(err)
		}
		if firstMesh == nil {
			firstMesh = result.Mesh
			firstClean = result.CleanPoints
			firstGeometry = geometry
		}
		crossMatch := firstMesh.ContentHash == result.Mesh.ContentHash
		featErr := featureError(result.Features, manifest.Truth.Features)
		boundsOK := result.Mesh.Bounds.Within(manifest.Truth.CleanBounds.Expanded(manifest.Truth.BoundsExpansion), 1e-9)
		passed := crossMatch && featErr <= manifest.Truth.FeatureTolerance && boundsOK &&
			len(result.Mesh.Faces) == manifest.Truth.ExpectedFaces
		allVersionsPassed = allVersionsPassed && passed
		versionReports = append(versionReports, map[string]any{
			"version": file.VersionID, "file": file.File,
			"points": len(geometry.Points), "pipelinePoints": len(result.CleanPoints),
			"faces": len(result.Mesh.Faces), "vertices": len(result.Mesh.Vertices),
			"expectedFaces": manifest.Truth.ExpectedFaces, "bounds": result.Mesh.Bounds,
			"featureError": featErr, "boundsWithinExpansion": boundsOK,
			"crossVersionVertexHashMatch": crossMatch,
			"meshHash":                    result.Mesh.ContentHash, "passed": passed,
		})
	}

	camera := render.DefaultCamera()
	clip := render.ClipPlane{Enabled: false, Axis: "z", KeepAbove: true}
	preview, err := render.Render(firstMesh, camera, clip, render.RenderOptions{
		Mode: "preview", SampleRate: cfg.PreviewSample, FlatShade: true, FrameID: "P000001", Version: 1,
	})
	if err != nil {
		fail(err)
	}
	exact, err := render.Render(firstMesh, camera, clip, render.RenderOptions{
		Mode: "exact", SampleRate: 1, FlatShade: false, FrameID: "E000002", Version: 1,
	})
	if err != nil {
		fail(err)
	}
	comparison, cmpErr := render.Compare(preview, exact)
	if cmpErr != nil {
		comparison = &render.Comparison{Passed: false}
	}

	clean := append([]model.Point(nil), firstClean...)
	clean[0].Z += 0.02
	reasons := pipeline.PlanInvalidation(firstMesh, "C00", "point-edit")
	incrementalMesh, err := pipeline.NewBuilder().BuildIncremental(clean, cfg, firstMesh, reasons, 2, "incremental", 1)
	if err != nil {
		fail(err)
	}
	rebuilt, reused := 0, 0
	for _, chunk := range incrementalMesh.Chunks {
		if chunk.Rebuilt {
			rebuilt++
		}
		if chunk.Reused {
			reused++
		}
	}
	incrementalPassed := rebuilt == 4 && reused == 8 && len(reasons) == 4

	pathResult, err := verifyPaths(firstClean, cfg)
	if err != nil {
		fail(err)
	}
	exportReport := verifyExports(firstGeometry, firstMesh)
	errorCodes := map[string]string{}
	if _, err := formats.Version("unknown/v999"); err != nil {
		errorCodes["unknownVersion"] = model.AsAppError(err).Code
	}
	if _, err := formats.LoadFile(filepath.Join(*dir, "missing-cloud.pcbin"), model.VersionPCBin); err != nil {
		errorCodes["decodeFailure"] = model.AsAppError(err).Code
	}
	badCfg := cfg
	badCfg.VoxelSize = 9
	if err := badCfg.Validate(); err != nil {
		errorCodes["invalidParameter"] = model.AsAppError(err).Code
	}
	if code := stateErrorCode(firstMesh); code != "" {
		errorCodes["frozenChunkMutation"] = code
	}
	divergent := *preview
	divergent.Mask = make([]byte, len(preview.Mask))
	if _, err := render.Compare(&divergent, exact); err != nil {
		errorCodes["previewDivergence"] = model.AsAppError(err).Code
	}
	allExportsPassed := true
	for _, item := range exportReport {
		allExportsPassed = allExportsPassed && item["passed"].(bool)
	}
	passed := allVersionsPassed && comparison.Passed && incrementalPassed && pathResult.Passed &&
		allExportsPassed && errorCodes["unknownVersion"] == model.EVER &&
		errorCodes["invalidParameter"] == model.EPARA && errorCodes["decodeFailure"] == model.EDECODE &&
		errorCodes["frozenChunkMutation"] == model.ESTATE && errorCodes["previewDivergence"] == model.EDIVERGE &&
		preview.LatencyMs < 50
	report := map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"seed":        manifest.Seed, "sampleDir": *dir,
		"versions":         versionReports,
		"previewLatencyMs": preview.LatencyMs,
		"exactLatencyMs":   exact.LatencyMs,
		"comparison":       comparison,
		"incremental": map[string]any{
			"rebuilt": rebuilt, "reused": reused, "total": len(incrementalMesh.Chunks),
			"affected": len(reasons), "passed": incrementalPassed,
		},
		"paths":           pathResult,
		"exports":         exportReport,
		"errorCodeChecks": errorCodes,
		"passed":          passed,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*jsonOut), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*jsonOut, append(data, '\n'), 0o644); err != nil {
		fail(err)
	}
	printJSON(map[string]any{
		"passed": passed, "versions": len(versionReports),
		"previewLatencyMs": preview.LatencyMs, "exactLatencyMs": exact.LatencyMs,
		"coverageDeviation":   comparison.CoverageDeviation,
		"bboxProjectionDelta": comparison.BBoxProjectionDelta,
		"normalHistogramL1":   comparison.NormalHistogramL1,
		"incremental":         map[string]int{"rebuilt": rebuilt, "reused": reused},
		"report":              *jsonOut,
	})
	if !passed {
		os.Exit(1)
	}
}

func stateErrorCode(mesh *model.Mesh) string {
	if mesh == nil || len(mesh.Chunks) == 0 {
		return ""
	}
	dir, err := os.MkdirTemp("", "strata-state-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		return ""
	}
	defer db.Close()
	if err := db.BeginRun(context.Background(), "state", "exact", 1); err != nil {
		return ""
	}
	cp := checkpoint(mesh, mesh.Chunks[0], "state")
	if _, err := db.CommitChunk(context.Background(), cp); err != nil {
		return ""
	}
	if _, err := db.CommitChunk(context.Background(), cp); err != nil {
		return model.AsAppError(err).Code
	}
	return ""
}

func verifyPaths(clean []model.Point, cfg pipeline.Config) (*servicePathResult, error) {
	dir, err := os.MkdirTemp("", "strata-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	db, err := store.Open(filepath.Join(dir, "verify.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	res := &servicePathResult{}
	fullMesh, err := pipeline.NewBuilder().BuildIncremental(clean, cfg, nil, nil, 1, "full", 1)
	if err != nil {
		return nil, err
	}
	res.FullSequence, err = commitPath(db, fullMesh, "verify-full")
	if err != nil {
		return nil, err
	}
	incMesh, err := pipeline.NewBuilder().BuildIncremental(clean, cfg, fullMesh, map[string]string{}, 1, "incremental", 1)
	if err != nil {
		return nil, err
	}
	res.IncrementalSequence, err = commitPath(db, incMesh, "verify-incremental")
	if err != nil {
		return nil, err
	}
	resumeMesh, err := pipeline.NewBuilder().BuildIncremental(clean, cfg, fullMesh, map[string]string{}, 1, "resume", 1)
	if err != nil {
		return nil, err
	}
	runID := "verify-resume"
	if err := db.BeginRun(context.Background(), runID, "resume", 1); err != nil {
		return nil, err
	}
	for i := 0; i < len(resumeMesh.Chunks)/2; i++ {
		if _, err := db.CommitChunk(context.Background(), checkpoint(resumeMesh, resumeMesh.Chunks[i], runID)); err != nil {
			return nil, err
		}
	}
	existing, err := db.LoadCheckpoints(context.Background(), runID)
	if err != nil {
		return nil, err
	}
	committed := map[string]bool{}
	for _, cp := range existing {
		committed[cp.ChunkID] = true
		res.ResumeSequence = append(res.ResumeSequence, cp.ChunkID)
	}
	for _, chunk := range resumeMesh.Chunks {
		if committed[chunk.ID] {
			continue
		}
		if _, err := db.CommitChunk(context.Background(), checkpoint(resumeMesh, chunk, runID)); err != nil {
			return nil, err
		}
		res.ResumeSequence = append(res.ResumeSequence, chunk.ID)
	}
	if err := db.FinishRun(context.Background(), runID, "complete", resumeMesh.ContentHash, ""); err != nil {
		return nil, err
	}
	previewMesh, err := pipeline.NewBuilder().BuildIncremental(clean, cfg, fullMesh, map[string]string{}, 1, "preview", cfg.PreviewSample)
	if err != nil {
		return nil, err
	}
	res.PreviewSequence, err = commitPath(db, previewMesh, "verify-preview")
	if err != nil {
		return nil, err
	}
	res.FullMeshHash = fullMesh.ContentHash
	res.IncrementalMeshHash = incMesh.ContentHash
	res.ResumeMeshHash = resumeMesh.ContentHash
	res.PreviewMeshHash = previewMesh.ContentHash
	res.Passed = res.FullMeshHash == res.IncrementalMeshHash &&
		res.FullMeshHash == res.ResumeMeshHash && res.FullMeshHash == res.PreviewMeshHash &&
		equalStrings(res.FullSequence, res.IncrementalSequence) &&
		equalStrings(res.FullSequence, res.ResumeSequence) &&
		equalStrings(res.FullSequence, res.PreviewSequence)
	if !res.Passed {
		res.FirstDifferingChunk = firstSequenceDiff(res.FullSequence, res.ResumeSequence)
	}
	return res, nil
}

type servicePathResult struct {
	Passed              bool     `json:"passed"`
	FullSequence        []string `json:"fullSequence"`
	IncrementalSequence []string `json:"incrementalSequence"`
	ResumeSequence      []string `json:"resumeSequence"`
	PreviewSequence     []string `json:"previewSequence"`
	FullMeshHash        string   `json:"fullMeshHash"`
	IncrementalMeshHash string   `json:"incrementalMeshHash"`
	ResumeMeshHash      string   `json:"resumeMeshHash"`
	PreviewMeshHash     string   `json:"previewMeshHash"`
	FirstDifferingChunk string   `json:"firstDifferingChunk,omitempty"`
}

func commitPath(db *store.Store, mesh *model.Mesh, runID string) ([]string, error) {
	if err := db.BeginRun(context.Background(), runID, "verify", 1); err != nil {
		return nil, err
	}
	seq := make([]string, 0, len(mesh.Chunks))
	for _, chunk := range mesh.Chunks {
		if _, err := db.CommitChunk(context.Background(), checkpoint(mesh, chunk, runID)); err != nil {
			return nil, err
		}
		seq = append(seq, chunk.ID)
	}
	if err := db.FinishRun(context.Background(), runID, "complete", mesh.ContentHash, ""); err != nil {
		return nil, err
	}
	return seq, nil
}

func checkpoint(mesh *model.Mesh, chunk model.ChunkMesh, runID string) store.ChunkCheckpoint {
	vertices := append([]model.MeshVertex(nil), mesh.Vertices[chunk.VertexStart:chunk.VertexStart+chunk.VertexCount]...)
	faces := make([]model.MeshFace, chunk.FaceCount)
	for i := 0; i < chunk.FaceCount; i++ {
		f := mesh.Faces[chunk.FaceStart+i]
		f.A -= chunk.VertexStart
		f.B -= chunk.VertexStart
		f.C -= chunk.VertexStart
		faces[i] = f
	}
	return store.ChunkCheckpoint{
		RunID: runID, Version: 1, ChunkID: chunk.ID, ParamsHash: mesh.ParamsHash,
		ParamsVersion: 1, Algorithm: model.AlgorithmVersion, ContentKey: chunk.ContentKey,
		ContentHash: chunk.ContentHash, PointIndices: chunk.PointIndices,
		Vertices: vertices, Faces: faces, Reason: chunk.Reason, Reused: chunk.Reused,
	}
}

func verifyExports(cloud *model.Geometry, mesh *model.Mesh) []map[string]any {
	meshGeometry := &model.Geometry{Points: make([]model.Point, len(mesh.Vertices)), Faces: make([]model.Face, len(mesh.Faces))}
	for i, v := range mesh.Vertices {
		meshGeometry.Points[i] = model.Point{
			X: v.Position.X, Y: v.Position.Y, Z: v.Position.Z,
			NX: v.Normal.X, NY: v.Normal.Y, NZ: v.Normal.Z,
			R: v.Color[0], G: v.Color[1], B: v.Color[2], Intensity: v.Intensity,
		}
	}
	for i, f := range mesh.Faces {
		meshGeometry.Faces[i] = model.Face{A: f.A, B: f.B, C: f.C}
	}
	outDir := "artifacts/verify-exports"
	_ = os.MkdirAll(outDir, 0o755)
	var report []map[string]any
	for _, version := range formats.Versions() {
		for _, kind := range []string{"cloud", "mesh"} {
			source := cloud
			if kind == "mesh" {
				source = meshGeometry
			}
			ext := ".ply"
			if version.ID == model.VersionPCBin {
				ext = ".pcbin"
			}
			slug := strings.ReplaceAll(version.ID, "/", "-")
			path := filepath.Join(outDir, kind+"-"+slug+ext)
			reportData, err := formats.WriteFile(path, version.ID, source, kind == "mesh")
			if err != nil {
				report = append(report, map[string]any{"versionId": version.ID, "kind": kind, "passed": false, "error": err.Error()})
				continue
			}
			passed := reportData.ReadbackPoints == len(source.Points) &&
				reportData.ReadbackFaces == len(source.Faces)
			report = append(report, map[string]any{
				"versionId": version.ID, "kind": kind, "file": path,
				"pointCount": len(source.Points), "readbackPoints": reportData.ReadbackPoints,
				"faceCount": len(source.Faces), "readbackFaces": reportData.ReadbackFaces,
				"warnings": len(reportData.Warnings), "downgradeTrace": reportData.DowngradeTrace,
				"passed": passed,
			})
		}
	}
	return report
}

func featureError(a, b model.FeatureSet) float64 {
	maxErr := 0.0
	pairs := [][2]model.Vec3{
		{a.Peak, b.Peak}, {a.Trough, b.Trough}, {a.Ridge, b.Ridge},
		{a.MinCorner, b.MinCorner}, {a.MaxCorner, b.MaxCorner},
	}
	for _, pair := range pairs {
		d := pair[0].Sub(pair[1]).Length()
		if d > maxErr {
			maxErr = d
		}
	}
	return maxErr
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

func firstSequenceDiff(a, b []string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i] + "!=" + b[i]
		}
	}
	if len(a) != len(b) {
		return "sequence-length"
	}
	return ""
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

func fail(err error) {
	appErr := model.AsAppError(err)
	data, _ := json.MarshalIndent(appErr, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
	os.Exit(1)
}
