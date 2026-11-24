package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"strata/internal/formats"
	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
	"strata/internal/store"
)

func (s *Service) Exact(req PreviewRequest) Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "exact", 0, nil))
	}
	camera, clip := s.frameInputs(req.Camera, req.Clip)
	result, err := s.exactSnapshot(snap, camera, clip, nil)
	if err != nil {
		env := errorEnvelope(err)
		if result != nil {
			env.Data = result
		}
		return env
	}
	return Envelope{OK: true, Data: result}
}

type commitProgress func(chunkID string, seq int) bool

func (s *Service) exactSnapshot(snap *snapshot, camera render.Camera, clip render.ClipPlane, progress commitProgress) (*ExactResult, error) {
	if err := camera.Validate(); err != nil {
		return nil, err
	}
	if err := clip.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	if existing := s.session.ExactByVersion[snap.Version]; existing != nil {
		exactMesh := existing
		s.mu.RUnlock()
		return s.renderExactComparison(snap, exactMesh, camera, clip, "frozen-reuse", nil, 0, 0)
	}
	s.mu.RUnlock()

	buildStart := time.Now()
	reasons := map[string]string{}
	for _, chunk := range MakeChunkIDs(snap) {
		reasons[chunk] = "exact-full"
	}
	exactMesh, err := s.builder.BuildIncremental(snap.CleanPoints, snap.Config, nil, reasons, snap.Version, "exact", 1)
	if err != nil {
		return nil, err
	}
	buildMs := float64(time.Since(buildStart).Microseconds()) / 1000
	runID := s.nextRunID("exact")
	if err := s.store.BeginRun(context.Background(), runID, "exact", snap.Version); err != nil {
		return nil, err
	}
	checkpointStart := time.Now()
	sequence := make([]string, 0, len(exactMesh.Chunks))
	for _, chunk := range exactMesh.Chunks {
		cp, err := checkpointFromChunk(exactMesh, chunk, snap, runID)
		if err != nil {
			_ = s.store.FinishRun(context.Background(), runID, "failed", "", err.Error())
			return nil, err
		}
		committed, err := s.store.CommitChunk(context.Background(), cp)
		if err != nil {
			_ = s.store.FinishRun(context.Background(), runID, "failed", "", err.Error())
			return nil, err
		}
		sequence = append(sequence, chunk.ID)
		if progress != nil && !progress(chunk.ID, committed.Seq) {
			_ = s.store.FinishRun(context.Background(), runID, "paused", exactMesh.ContentHash, "paused by user")
			return nil, model.NewError(model.ESTATE, "exact rebuild paused", "job.pause", 0, map[string]any{
				"runId": runID, "committed": len(sequence),
			})
		}
	}
	checkpointMs := float64(time.Since(checkpointStart).Microseconds()) / 1000
	if err := s.store.FinishRun(context.Background(), runID, "complete", exactMesh.ContentHash, ""); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.session.ExactByVersion[snap.Version] = exactMesh
	if s.session.Committed[snap.Version] == nil {
		s.session.Committed[snap.Version] = map[string]bool{}
	}
	for _, chunk := range exactMesh.Chunks {
		s.session.Committed[snap.Version][chunk.ID] = true
	}
	if snap.Version == s.session.ActiveVersion {
		s.session.MeshByVersion[snap.Version] = exactMesh
	}
	s.mu.Unlock()
	return s.renderExactComparison(snap, exactMesh, camera, clip, runID, sequence, buildMs, checkpointMs)
}

func (s *Service) renderExactComparison(snap *snapshot, exactMesh *model.Mesh, camera render.Camera, clip render.ClipPlane, runID string, sequence []string, buildMs, checkpointMs float64) (*ExactResult, error) {
	preview := s.previewFor(snap, camera, clip)
	exact, err := render.Render(exactMesh, camera, clip, render.RenderOptions{
		Mode: "exact", SampleRate: 1, FlatShade: false,
		FrameID: s.nextFrameID("E"), Version: snap.Version,
	})
	if err != nil {
		return nil, err
	}
	exact.Message = "exact: full-resolution smooth shading"
	s.storeFrame(exact)
	comparison, cmpErr := render.Compare(preview, exact)
	s.mu.Lock()
	s.session.LastExact = exact
	s.session.LastComparison = comparison
	s.session.Camera, s.session.Clip = camera, clip
	s.mu.Unlock()
	frozen := 0
	if s.session.Committed[snap.Version] != nil {
		frozen = len(s.session.Committed[snap.Version])
	}
	result := &ExactResult{
		RunID: runID, Version: snap.Version, Preview: FrameToDTOPtr(preview),
		Exact: FrameToDTOPtr(exact), Comparison: comparison,
		ChunkSequence: sequence, MeshHash: exactMesh.ContentHash,
		FrozenChunks: frozen, CheckpointMs: checkpointMs, BuildMs: buildMs,
	}
	if cmpErr != nil {
		return result, cmpErr
	}
	return result, nil
}

func (s *Service) previewFor(snap *snapshot, camera render.Camera, clip render.ClipPlane) *render.Frame {
	s.mu.RLock()
	preview := s.session.PreviewByVersion[snap.Version]
	mesh := s.session.MeshByVersion[snap.Version]
	s.mu.RUnlock()
	if preview != nil && sameCamera(preview.Camera, camera) && preview.Clip == clip {
		return preview
	}
	if mesh == nil {
		mesh = s.session.ExactByVersion[snap.Version]
	}
	if mesh == nil {
		return nil
	}
	frame, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "preview", SampleRate: snap.Config.PreviewSample,
		FlatShade: true, FrameID: s.nextFrameID("P"), Version: snap.Version,
	})
	if err != nil {
		return nil
	}
	s.storeFrame(frame)
	s.mu.Lock()
	s.session.PreviewByVersion[snap.Version] = frame
	s.session.LastPreview = frame
	s.mu.Unlock()
	return frame
}

func sameCamera(a, b render.Camera) bool {
	return a.Yaw == b.Yaw && a.Pitch == b.Pitch && a.Distance == b.Distance &&
		a.TargetX == b.TargetX && a.TargetY == b.TargetY && a.TargetZ == b.TargetZ &&
		a.FOV == b.FOV && a.Width == b.Width && a.Height == b.Height
}

func MakeChunkIDs(snap *snapshot) []string {
	bounds := pipeline.PointsBounds(snap.CleanPoints)
	defs := pipeline.MakeChunks(bounds, snap.Config.ChunkSize)
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.ID
	}
	return out
}

func checkpointFromChunk(mesh *model.Mesh, chunk model.ChunkMesh, snap *snapshot, runID string) (store.ChunkCheckpoint, error) {
	if chunk.VertexStart < 0 || chunk.VertexStart+chunk.VertexCount > len(mesh.Vertices) ||
		chunk.FaceStart < 0 || chunk.FaceStart+chunk.FaceCount > len(mesh.Faces) {
		return store.ChunkCheckpoint{}, model.NewError(model.ESTATE, "chunk range is invalid", "checkpoint."+chunk.ID, 0, nil)
	}
	vertices := append([]model.MeshVertex(nil), mesh.Vertices[chunk.VertexStart:chunk.VertexStart+chunk.VertexCount]...)
	faces := make([]model.MeshFace, chunk.FaceCount)
	for i := 0; i < chunk.FaceCount; i++ {
		f := mesh.Faces[chunk.FaceStart+i]
		f.A -= chunk.VertexStart
		f.B -= chunk.VertexStart
		f.C -= chunk.VertexStart
		faces[i] = f
	}
	hash := chunk.ContentHash
	if hash == "" {
		hash = model.HashBytes(model.EncodeMeshPayload(vertices, faces))
	}
	return store.ChunkCheckpoint{
		RunID: runID, Version: snap.Version, ChunkID: chunk.ID,
		ParamsHash: mesh.ParamsHash, ParamsVersion: snap.Version,
		Algorithm: model.AlgorithmVersion, ContentKey: chunk.ContentKey, ContentHash: hash,
		PointIndices: append([]int(nil), chunk.PointIndices...), Vertices: vertices, Faces: faces,
		Reason: chunk.Reason, Reused: chunk.Reused,
	}, nil
}

func FrameToDTOPtr(frame *render.Frame) *FrameDTO {
	if frame == nil {
		return nil
	}
	dto := FrameToDTO(frame)
	return &dto
}

func (s *Service) EditAndIncremental(req EditRequest) Envelope {
	if math.Abs(req.DeltaZ) > 1 {
		return errorEnvelope(model.NewError(model.EPARA, "deltaZ must be in [-1, 1]", "edit.deltaZ", 0, map[string]any{"value": req.DeltaZ}))
	}
	start := time.Now()
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "edit", 0, nil))
	}
	if req.BaseVersion != 0 && req.BaseVersion != snap.Version {
		return errorEnvelope(model.NewError(model.ESTATE, "edit targets a frozen or stale snapshot", "version", 0, map[string]any{
			"baseVersion": req.BaseVersion, "activeVersion": snap.Version,
		}))
	}
	if req.Index < 0 || req.Index >= len(snap.CleanPoints) {
		return errorEnvelope(model.NewError(model.EPARA, "point index is outside the pipeline point set", "edit.index", 0, map[string]any{
			"value": req.Index, "count": len(snap.CleanPoints),
		}))
	}
	chunkID := ""
	for _, c := range mesh.Chunks {
		for _, idx := range c.PointIndices {
			if idx == req.Index {
				chunkID = c.ID
				break
			}
		}
		if chunkID != "" {
			break
		}
	}
	if chunkID == "" {
		return errorEnvelope(model.NewError(model.EPARA, "point index does not belong to any chunk", "edit.index", 0, nil))
	}
	newClean := append([]model.Point(nil), snap.CleanPoints...)
	newSource := append([]model.Point(nil), snap.SourcePoints...)
	newClean[req.Index].Z += req.DeltaZ
	// Reflect the edit in the internal source model for export, using the
	// nearest source point to the pre-edit clean position.
	oldPos := snap.CleanPoints[req.Index].Position()
	best, bestD := -1, math.Inf(1)
	for i, p := range newSource {
		d := p.Position().Sub(oldPos).Dot(p.Position().Sub(oldPos))
		if d < bestD {
			best, bestD = i, d
		}
	}
	if best >= 0 {
		newSource[best].Z += req.DeltaZ
	}
	cfg := cloneConfig(snap.Config)
	reasons := pipeline.PlanInvalidation(mesh, chunkID, "point-edit")
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	newSnap := &snapshot{
		Version: version, Reason: fmt.Sprintf("edit-point:%d:%+.3f", req.Index, req.DeltaZ),
		SourceVersion: snap.SourceVersion, SourcePoints: newSource, CleanPoints: newClean,
		Config: cfg, CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: snap.Meta,
	}
	newSnap.SnapshotHash = snapshotHash(newSnap)
	s.mu.Unlock()
	newMesh, err := s.builder.BuildIncremental(newClean, cfg, mesh, reasons, version, "incremental", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.session.Snapshots[version] = newSnap
	s.session.MeshByVersion[version] = newMesh
	s.session.ActiveVersion = version
	if req.Camera != nil {
		s.session.Camera = *req.Camera
	}
	if req.Clip != nil {
		s.session.Clip = *req.Clip
	}
	s.mu.Unlock()
	if err := s.saveSnapshot(newSnap); err != nil {
		return errorEnvelope(err)
	}
	manifest := s.chunkManifest(newMesh, version)
	inc := IncrementalResult{
		Version: version, Changed: len(reasons), Manifest: manifest,
		MeshHash: newMesh.ContentHash, ElapsedMs: float64(time.Since(start).Microseconds()) / 1000,
	}
	return s.withWorkspace(map[string]any{"incremental": inc, "edit": map[string]any{
		"index": req.Index, "deltaZ": req.DeltaZ, "chunkId": chunkID,
		"invalidatedBy": reasons,
	}})
}

func cloneConfig(cfg pipeline.Config) pipeline.Config {
	out := cfg
	if cfg.ChunkBias != nil {
		out.ChunkBias = map[string]float64{}
		for k, v := range cfg.ChunkBias {
			out.ChunkBias[k] = v
		}
	}
	return out
}

func (s *Service) chunkManifest(mesh *model.Mesh, version int) ChunkManifest {
	m := ChunkManifest{Version: version, Total: len(mesh.Chunks), Sequence: make([]string, 0, len(mesh.Chunks))}
	for _, c := range mesh.Chunks {
		cacheHit := false
		if c.ContentKey != "" {
			cached, err := s.store.LoadCachedChunk(context.Background(), c.ContentKey)
			cacheHit = err == nil && cached != nil
		}
		if c.Reused {
			m.Reused++
		}
		if c.Rebuilt {
			m.Rebuilt++
		}
		if cacheHit {
			m.CacheHits++
		}
		if c.Reason != "" && c.Reason != "unchanged" {
			m.Affected++
		}
		m.Items = append(m.Items, chunkDTO(c, cacheHit))
		m.Sequence = append(m.Sequence, c.ID)
	}
	return m
}

func (s *Service) saveSnapshot(snap *snapshot) error {
	paramsJSON, _ := json.Marshal(snap.Config)
	return s.store.SaveVersion(context.Background(), store.VersionSnapshot{
		Version: snap.Version, Reason: snap.Reason, SnapshotHash: snap.SnapshotHash,
		SourceVersion: snap.SourceVersion, PointCount: len(snap.CleanPoints),
		ParamsJSON: paramsJSON, Points: snap.CleanPoints,
	})
}

func (s *Service) withWorkspace(extra map[string]any) Envelope {
	ws := s.Bootstrap()
	if !ws.OK {
		return ws
	}
	extra["workspace"] = ws.Data
	return Envelope{OK: true, Data: extra}
}

func (s *Service) UpdateParams(req ParamsRequest) Envelope {
	if err := req.Params.Validate(); err != nil {
		return errorEnvelope(err)
	}
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "params", 0, nil))
	}
	start := time.Now()
	oldCfg := snap.Config
	newCfg := cloneConfig(req.Params)
	newClean := append([]model.Point(nil), snap.CleanPoints...)
	reasons := map[string]string{}
	full := oldCfg.ChunkSize != newCfg.ChunkSize || oldCfg.ResampleSpacing != newCfg.ResampleSpacing
	if !full {
		keysChanged := oldCfg.VoxelSize != newCfg.VoxelSize || oldCfg.OutlierSigma != newCfg.OutlierSigma ||
			oldCfg.NormalK != newCfg.NormalK || oldCfg.MaxWorkers != newCfg.MaxWorkers ||
			oldCfg.VertexBufferLimit != newCfg.VertexBufferLimit
		if keysChanged {
			reasons = pipeline.PlanAll(mesh, "parameter-change")
		} else if !equalFloatMaps(oldCfg.ChunkBias, newCfg.ChunkBias) {
			for id := range unionFloatKeys(oldCfg.ChunkBias, newCfg.ChunkBias) {
				inv := pipeline.PlanInvalidation(mesh, id, "chunk-parameter-change")
				for k, v := range inv {
					reasons[k] = v
				}
			}
		}
	}
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	newSnap := &snapshot{
		Version: version, Reason: "parameter-snapshot", SourceVersion: snap.SourceVersion,
		SourcePoints: append([]model.Point(nil), snap.SourcePoints...),
		CleanPoints:  newClean, Config: newCfg, CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: snap.Meta,
	}
	newSnap.SnapshotHash = snapshotHash(newSnap)
	s.mu.Unlock()
	var newMesh *model.Mesh
	var err error
	if oldCfg.PreviewSample != newCfg.PreviewSample && len(reasons) == 0 && !full {
		newMesh = mesh
		newMesh.ParamsVersion = version
	} else {
		if full {
			newMesh, err = s.builder.BuildIncremental(newClean, newCfg, nil, nil, version, "full-parameter", 1)
		} else {
			newMesh, err = s.builder.BuildIncremental(newClean, newCfg, mesh, reasons, version, "parameter", 1)
		}
		if err != nil {
			return errorEnvelope(err)
		}
	}
	s.mu.Lock()
	s.session.Snapshots[version] = newSnap
	s.session.MeshByVersion[version] = newMesh
	s.session.ActiveVersion = version
	if req.Camera != nil {
		s.session.Camera = *req.Camera
	}
	if req.Clip != nil {
		s.session.Clip = *req.Clip
	}
	s.mu.Unlock()
	if err := s.saveSnapshot(newSnap); err != nil {
		return errorEnvelope(err)
	}
	manifest := s.chunkManifest(newMesh, version)
	inc := IncrementalResult{
		Version: version, Changed: len(reasons), Manifest: manifest,
		MeshHash: newMesh.ContentHash, ElapsedMs: float64(time.Since(start).Microseconds()) / 1000,
	}
	return s.withWorkspace(map[string]any{"incremental": inc})
}

func equalFloatMaps(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func unionFloatKeys(a, b map[string]float64) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func (s *Service) UpdateChunkParam(req ChunkParamRequest) Envelope {
	if req.Key != "bias" && req.Key != "chunkBias" {
		return errorEnvelope(model.NewError(model.EPARA, "only the local chunk bias parameter is supported", "chunk.key", 0, nil))
	}
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "params", 0, nil))
	}
	found := false
	for _, c := range mesh.Chunks {
		if c.ID == req.ChunkID {
			found = true
			break
		}
	}
	if !found {
		return errorEnvelope(model.NewError(model.EPARA, "unknown chunk id", "chunk.id", 0, map[string]any{"value": req.ChunkID}))
	}
	cfg := cloneConfig(snap.Config)
	if cfg.ChunkBias == nil {
		cfg.ChunkBias = map[string]float64{}
	}
	cfg.ChunkBias[req.ChunkID] = req.Value
	if err := cfg.Validate(); err != nil {
		return errorEnvelope(err)
	}
	reasons := pipeline.PlanInvalidation(mesh, req.ChunkID, "chunk-parameter-change")
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	newSnap := &snapshot{
		Version: version, Reason: "chunk-param:" + req.ChunkID, SourceVersion: snap.SourceVersion,
		SourcePoints: append([]model.Point(nil), snap.SourcePoints...),
		CleanPoints:  append([]model.Point(nil), snap.CleanPoints...), Config: cfg,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: snap.Meta,
	}
	newSnap.SnapshotHash = snapshotHash(newSnap)
	s.mu.Unlock()
	newMesh, err := s.builder.BuildIncremental(newSnap.CleanPoints, cfg, mesh, reasons, version, "incremental-param", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.session.Snapshots[version] = newSnap
	s.session.MeshByVersion[version] = newMesh
	s.session.ActiveVersion = version
	s.mu.Unlock()
	if err := s.saveSnapshot(newSnap); err != nil {
		return errorEnvelope(err)
	}
	manifest := s.chunkManifest(newMesh, version)
	return s.withWorkspace(map[string]any{"incremental": IncrementalResult{
		Version: version, Changed: len(reasons), Manifest: manifest, MeshHash: newMesh.ContentHash,
	}})
}

func (s *Service) RetryChunk(chunkID string) Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "retry", 0, nil))
	}
	found := false
	failed := false
	for _, chunk := range mesh.Chunks {
		if chunk.ID == chunkID {
			found = true
			failed = chunk.Error != ""
			break
		}
	}
	if !found {
		return errorEnvelope(model.NewError(model.EPARA, "unknown chunk id", "retry.chunkId", 0, map[string]any{"value": chunkID}))
	}
	if !failed {
		return errorEnvelope(model.NewError(model.ESTATE, "chunk has no failed result to retry", "retry.chunkId", 0, nil))
	}
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	newSnap := &snapshot{
		Version: version, Reason: "retry:" + chunkID, SourceVersion: snap.SourceVersion,
		SourcePoints: append([]model.Point(nil), snap.SourcePoints...),
		CleanPoints:  append([]model.Point(nil), snap.CleanPoints...),
		Config:       cloneConfig(snap.Config), CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: snap.Meta,
	}
	newSnap.SnapshotHash = snapshotHash(newSnap)
	s.mu.Unlock()
	newMesh, err := s.builder.BuildIncremental(newSnap.CleanPoints, newSnap.Config, mesh, map[string]string{chunkID: "retry-failed-chunk"}, version, "retry", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.session.Snapshots[version] = newSnap
	s.session.MeshByVersion[version] = newMesh
	s.session.ActiveVersion = version
	s.mu.Unlock()
	if err := s.saveSnapshot(newSnap); err != nil {
		return errorEnvelope(err)
	}
	return s.withWorkspace(map[string]any{"incremental": IncrementalResult{
		Version: version, Changed: 1, Manifest: s.chunkManifest(newMesh, version), MeshHash: newMesh.ContentHash,
	}})
}

func (s *Service) Export(req ExportRequest) Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "export", 0, nil))
	}
	if _, err := formats.Version(req.VersionID); err != nil {
		return errorEnvelope(err)
	}
	kind := req.Kind
	if kind == "" {
		kind = "cloud"
	}
	if kind != "cloud" && kind != "mesh" {
		return errorEnvelope(model.NewError(model.EPARA, "export kind must be cloud or mesh", "export.kind", 0, nil))
	}
	var geometry *model.Geometry
	if kind == "mesh" {
		points := make([]model.Point, len(mesh.Vertices))
		for i, v := range mesh.Vertices {
			points[i] = model.Point{
				X: v.Position.X, Y: v.Position.Y, Z: v.Position.Z,
				NX: v.Normal.X, NY: v.Normal.Y, NZ: v.Normal.Z,
				R: v.Color[0], G: v.Color[1], B: v.Color[2], Intensity: v.Intensity,
			}
		}
		faces := make([]model.Face, len(mesh.Faces))
		for i, f := range mesh.Faces {
			faces[i] = model.Face{A: f.A, B: f.B, C: f.C}
		}
		geometry = &model.Geometry{Points: points, Faces: faces}
	} else {
		geometry = &model.Geometry{Points: append([]model.Point(nil), snap.SourcePoints...)}
	}
	ext := ".ply"
	slug := strings.ReplaceAll(req.VersionID, "/", "-")
	if req.VersionID == model.VersionPCBin {
		ext = ".pcbin"
	}
	name := fmt.Sprintf("%s-%s%s", kind, slug, ext)
	outDir := filepath.Join(s.artifactDir, "exports")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return errorEnvelope(model.NewError(model.EDECODE, err.Error(), "export.path", 0, nil))
	}
	outPath := filepath.Join(outDir, name)
	report, err := formats.WriteFile(outPath, req.VersionID, geometry, kind == "mesh")
	if err != nil {
		return errorEnvelope(err)
	}
	info, _ := os.Stat(outPath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	result := ExportResult{
		VersionID: req.VersionID, Kind: kind, Path: outPath, Bytes: size,
		PointCount: len(geometry.Points), ReadbackPoints: report.ReadbackPoints,
		FaceCount: len(geometry.Faces), ReadbackFaces: report.ReadbackFaces,
		Warnings: report.Warnings, DowngradeTrace: report.DowngradeTrace,
		RoundTrip: report.ReadbackPoints == len(geometry.Points) && report.ReadbackFaces == len(geometry.Faces),
	}
	return Envelope{OK: true, Data: result, Warnings: report.Warnings}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Keep imported packages available for the job/resume implementation in the
// same service package without duplicating helpers.
var _ = sort.Strings
