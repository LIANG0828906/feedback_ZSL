package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"strata/internal/formats"
	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
	"strata/internal/store"
)

type snapshot struct {
	Version       int
	Reason        string
	SourceVersion string
	SourcePoints  []model.Point
	CleanPoints   []model.Point
	Config        pipeline.Config
	SnapshotHash  string
	CreatedAt     string
	Meta          model.SourceMeta
}

type sessionState struct {
	ActiveVersion    int
	Snapshots        map[int]*snapshot
	MeshByVersion    map[int]*model.Mesh
	ExactByVersion   map[int]*model.Mesh
	PreviewByVersion map[int]*render.Frame
	Committed        map[int]map[string]bool
	LastPreview      *render.Frame
	LastExact        *render.Frame
	LastComparison   *render.Comparison
	Camera           render.Camera
	Clip             render.ClipPlane
}

type Service struct {
	mu          sync.RWMutex
	root        string
	sampleDir   string
	artifactDir string
	dbPath      string
	store       *store.Store
	manifest    *generator.Manifest
	versions    []model.VersionInfo
	builder     *pipeline.Builder
	session     *sessionState
	frames      map[string]*render.Frame
	jobs        map[string]*jobState
	frameSeq    int
	runSeq      int
	versionSeq  int
	startedAt   string
}

type jobState struct {
	ID        string
	Mode      string
	Version   int
	Status    string
	Total     int
	Committed int
	Sequence  []string
	Error     string
	StartedAt string
	UpdatedAt string
	Pause     bool
	Cancel    bool
	Done      chan struct{}
	RunID     string
	Snap      *snapshot
	Camera    render.Camera
	Clip      render.ClipPlane
	Result    *ExactResult
}

func New(root string) (*Service, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "workspace", 0, nil)
	}
	s := &Service{
		root: abs, sampleDir: filepath.Join(abs, "data", "samples"),
		artifactDir: filepath.Join(abs, "artifacts"), dbPath: filepath.Join(abs, "data", "strata.db"),
		builder: pipeline.NewBuilder(), frames: map[string]*render.Frame{}, jobs: map[string]*jobState{},
		startedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := os.MkdirAll(s.sampleDir, 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "data.samples", 0, nil)
	}
	if err := os.MkdirAll(s.artifactDir, 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "artifacts", 0, nil)
	}
	manifest, err := generator.LoadManifest(s.sampleDir)
	if err != nil {
		manifest, err = generator.Generate(195, s.sampleDir)
		if err != nil {
			return nil, err
		}
	}
	s.manifest = manifest
	st, err := store.Open(s.dbPath)
	if err != nil {
		return nil, err
	}
	s.store = st
	if err := s.refreshVersionInfo(); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := s.initializeSession(); err != nil {
		_ = st.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Service) refreshVersionInfo() error {
	versions := formats.Versions()
	for i := range versions {
		var file string
		for _, f := range s.manifest.Files {
			if f.VersionID == versions[i].ID {
				file = f.File
				versions[i].FileBytes = f.Bytes
				break
			}
		}
		if file == "" {
			file = versions[i].File
		}
		versions[i].File = file
		if info, err := os.Stat(filepath.Join(s.sampleDir, file)); err == nil {
			versions[i].FileBytes = info.Size()
		}
	}
	s.versions = versions
	return nil
}

func (s *Service) initializeSession() error {
	activeVersion := model.VersionPLYASCII
	geometry, err := formats.LoadFile(filepath.Join(s.sampleDir, fileForVersion(s.versions, activeVersion)), activeVersion)
	if err != nil {
		return err
	}
	model.CanonicalizePoints(geometry.Points)
	result, err := s.builder.Build(geometry, pipeline.DefaultConfig(), "exact", 1)
	if err != nil {
		return err
	}
	version := 1
	latest, err := s.store.LatestVersion(context.Background())
	if err != nil {
		return err
	}
	clean := result.CleanPoints
	cfg := pipeline.DefaultConfig()
	reason := "initial-load"
	if latest != nil && latest.SourceVersion == activeVersion && len(latest.Points) > 0 {
		if err := json.Unmarshal(latest.ParamsJSON, &cfg); err == nil && cfg.Validate() == nil {
			version = latest.Version + 1
			clean = append([]model.Point(nil), latest.Points...)
			model.CanonicalizePoints(clean)
			reason = "restore:" + latest.Reason
			mesh, err := s.builder.BuildIncremental(clean, cfg, nil, nil, version, "exact", 1)
			if err == nil {
				result.Mesh = mesh
			}
		}
	}
	s.versionSeq = version
	snap := &snapshot{
		Version: version, Reason: reason, SourceVersion: activeVersion,
		SourcePoints: append([]model.Point(nil), geometry.Points...),
		CleanPoints:  append([]model.Point(nil), clean...),
		Config:       cfg, CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: geometry.Meta,
	}
	snap.SnapshotHash = snapshotHash(snap)
	s.session = &sessionState{
		ActiveVersion:    version,
		Snapshots:        map[int]*snapshot{version: snap},
		MeshByVersion:    map[int]*model.Mesh{version: result.Mesh},
		ExactByVersion:   map[int]*model.Mesh{},
		PreviewByVersion: map[int]*render.Frame{},
		Committed:        map[int]map[string]bool{},
		Camera:           render.DefaultCamera(), Clip: render.ClipPlane{Axis: "z", KeepAbove: true},
	}
	paramsJSON, _ := json.Marshal(cfg)
	if err := s.store.SaveVersion(context.Background(), store.VersionSnapshot{
		Version: version, Reason: reason, SnapshotHash: snap.SnapshotHash,
		SourceVersion: activeVersion, PointCount: len(clean), ParamsJSON: paramsJSON, Points: clean,
	}); err != nil {
		return err
	}
	return nil
}

func fileForVersion(versions []model.VersionInfo, id string) string {
	for _, v := range versions {
		if v.ID == id {
			return v.File
		}
	}
	return ""
}

func snapshotHash(snap *snapshot) string {
	data, _ := json.Marshal(struct {
		V      int
		S      string
		C      pipeline.Config
		Points []model.Point
	}{snap.Version, snap.SourceVersion, snap.Config, snap.CleanPoints})
	return model.HashBytes(data)
}

func (s *Service) currentLocked() (*snapshot, *model.Mesh) {
	if s.session == nil {
		return nil, nil
	}
	snap := s.session.Snapshots[s.session.ActiveVersion]
	if snap == nil {
		return nil, nil
	}
	return snap, s.session.MeshByVersion[snap.Version]
}

func (s *Service) latestSnapshot() (*snapshot, *model.Mesh) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentLocked()
}

func (s *Service) nextFrameID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frameSeq++
	return fmt.Sprintf("%s%06d", prefix, s.frameSeq)
}

func (s *Service) nextRunID(mode string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSeq++
	return fmt.Sprintf("run-%s-%d-%04d", mode, time.Now().UTC().UnixMilli(), s.runSeq)
}

func (s *Service) storeFrame(frame *render.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames[frame.ID] = frame
}

func (s *Service) FramePNG(id string) ([]byte, bool) {
	s.mu.RLock()
	frame := s.frames[id]
	s.mu.RUnlock()
	if frame == nil || frame.RGBA == nil {
		return nil, false
	}
	// The renderer already produced a base64 PNG; decode lazily for the HTTP
	// route so interactive action responses stay small.
	data, err := decodeBase64(frame.ImageBase64)
	if err != nil {
		return nil, false
	}
	return data, true
}

func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty image")
	}
	return base64.StdEncoding.DecodeString(s)
}

func (s *Service) Bootstrap() Envelope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.session == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "workspace is not initialized", "workspace", 0, nil))
	}
	snap, mesh := s.currentLocked()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active snapshot", "workspace", 0, nil))
	}
	meta := snap.Meta
	if meta.VersionID == "" {
		geometry, err := formats.LoadFile(filepath.Join(s.sampleDir, fileForVersion(s.versions, snap.SourceVersion)), snap.SourceVersion)
		if err != nil {
			return errorEnvelope(err)
		}
		meta = geometry.Meta
	}
	meshDTO := MeshToDTO(mesh)
	stats := StatsDTO{
		SourcePoints: len(snap.SourcePoints), PipelinePoints: len(snap.CleanPoints),
		Outliers: len(snap.SourcePoints) - len(snap.CleanPoints),
		Vertices: len(mesh.Vertices), Faces: len(mesh.Faces), Chunks: len(mesh.Chunks),
	}
	var lastPreview, lastExact *FrameDTO
	if s.session.LastPreview != nil {
		dto := FrameToDTO(s.session.LastPreview)
		lastPreview = &dto
		stats.PreviewLatency = s.session.LastPreview.LatencyMs
	}
	if s.session.LastExact != nil {
		dto := FrameToDTO(s.session.LastExact)
		lastExact = &dto
		stats.ExactLatency = s.session.LastExact.LatencyMs
	}
	workspace := WorkspaceDTO{
		Product: "STRATA", DatasetName: "RIDGE TERRAIN", Seed: s.manifest.Seed,
		ActiveVersion: snap.SourceVersion, Version: snap.Version, Ready: true,
		Versions: s.versions, Source: sourceDTO(meta), Truth: s.manifest.Truth,
		Stats: stats, Params: snap.Config, Mesh: meshDTO, Chunks: meshDTO.Chunks,
		Camera: s.session.Camera, Clip: s.session.Clip,
		LastComparison: s.session.LastComparison, LastPreview: lastPreview, LastExact: lastExact,
		Cache: map[string]any{
			"chunkCount": len(mesh.Chunks),
			"algorithm":  model.AlgorithmVersion,
		},
	}
	return Envelope{OK: true, Data: workspace}
}

func sourceDTO(meta model.SourceMeta) SourceDTO {
	return SourceDTO{
		VersionID: meta.VersionID, VersionNumber: meta.VersionNumber,
		DisplayName: meta.DisplayName, Format: meta.Format, Encoding: meta.Encoding,
		File: meta.File, FileBytes: meta.FileBytes, PointCount: meta.PointCount,
		FaceCount: meta.FaceCount, Bounds: meta.Bounds, FieldOrder: meta.FieldOrder,
		OptionalAttributes: meta.OptionalAttributes, Fields: meta.Fields,
		Quantization: meta.Quantization, HeaderMeta: meta.HeaderMeta,
		HasNormals: meta.HasNormals, HasColors: meta.HasColors, HasIntensity: meta.HasIntensity,
	}
}

func MeshToDTO(mesh *model.Mesh) MeshDTO {
	dto := MeshDTO{
		Positions: make([]float64, 0, len(mesh.Vertices)*3),
		Normals:   make([]float64, 0, len(mesh.Vertices)*3),
		Colors:    make([]int, 0, len(mesh.Vertices)*3),
		Indices:   make([]int, 0, len(mesh.Faces)*3),
		Bounds:    mesh.Bounds, ContentHash: mesh.ContentHash, Algorithm: mesh.Algorithm,
		SampleRate: mesh.SampleRate, BuildMode: mesh.BuildMode,
	}
	for _, v := range mesh.Vertices {
		dto.Positions = append(dto.Positions, v.Position.X, v.Position.Y, v.Position.Z)
		dto.Normals = append(dto.Normals, v.Normal.X, v.Normal.Y, v.Normal.Z)
		dto.Colors = append(dto.Colors, int(v.Color[0]), int(v.Color[1]), int(v.Color[2]))
	}
	for _, f := range mesh.Faces {
		dto.Indices = append(dto.Indices, f.A, f.B, f.C)
	}
	for _, c := range mesh.Chunks {
		dto.Chunks = append(dto.Chunks, chunkDTO(c, false))
	}
	return dto
}

func chunkDTO(c model.ChunkMesh, cacheHit bool) ChunkDTO {
	return ChunkDTO{
		ID: c.ID, Row: c.Row, Col: c.Col, Bounds: c.Bounds,
		VertexStart: c.VertexStart, VertexCount: c.VertexCount,
		FaceStart: c.FaceStart, FaceCount: c.FaceCount, PointCount: len(c.PointIndices),
		Reason: c.Reason, Rebuilt: c.Rebuilt, Reused: c.Reused, CacheHit: cacheHit,
		ContentHash: c.ContentHash, Error: c.Error,
	}
}

func FrameToDTO(frame *render.Frame) FrameDTO {
	if frame == nil {
		return FrameDTO{}
	}
	return FrameDTO{
		ID: frame.ID, Version: frame.Version, Mode: frame.Mode,
		AlgorithmMode: frame.AlgorithmMode, RenderMode: frame.RenderMode,
		SampleRate: frame.SampleRate, Width: frame.Width, Height: frame.Height,
		Camera: frame.Camera, Clip: frame.Clip, Coverage: frame.Coverage,
		BBoxProjection: frame.BBoxProjection, NormalHistogram: frame.NormalHistogram,
		LatencyMs: frame.LatencyMs, WithinBudget: frame.WithinBudget,
		ImageURL: "/api/frame/" + frame.ID + ".png",
	}
}

func errorEnvelope(err error) Envelope {
	return Envelope{OK: false, Error: model.AsAppError(err)}
}

func (s *Service) Dispatch(action string, payload json.RawMessage) Envelope {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	switch action {
	case "bootstrap", "state":
		return s.Bootstrap()
	case "load":
		var req struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.LoadVersion(req.Version)
	case "load_path":
		var req struct {
			Path      string `json:"path"`
			VersionID string `json:"versionId"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.LoadPath(req.Path, req.VersionID)
	case "preview":
		var req PreviewRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.Preview(req)
	case "exact":
		var req PreviewRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.Exact(req)
	case "edit_point":
		var req EditRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.EditAndIncremental(req)
	case "incremental":
		var req EditRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		if req.Index == 0 && req.DeltaZ == 0 {
			req.Index, req.DeltaZ = s.defaultEdit()
		}
		return s.EditAndIncremental(req)
	case "update_params":
		var req ParamsRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.UpdateParams(req)
	case "chunk_param":
		var req ChunkParamRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.UpdateChunkParam(req)
	case "retry_chunk":
		var req struct {
			ChunkID string `json:"chunkId"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.RetryChunk(req.ChunkID)
	case "export":
		var req ExportRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return errorEnvelope(model.NewError(model.EPARA, err.Error(), "payload", 0, nil))
		}
		return s.Export(req)
	case "verify_paths":
		return s.VerifyPaths()
	case "start_job":
		var req PreviewRequest
		_ = json.Unmarshal(payload, &req)
		return s.StartExactJob(req)
	case "pause":
		return s.PauseJob()
	case "resume":
		var req struct {
			JobID string `json:"jobId"`
		}
		_ = json.Unmarshal(payload, &req)
		return s.ResumeJob(req.JobID)
	case "restart_resume":
		var req struct {
			RunID string `json:"runId"`
		}
		_ = json.Unmarshal(payload, &req)
		return s.RestartResume(req.RunID)
	case "jobs":
		return s.ListJobs()
	case "schema":
		return s.Schema()
	default:
		return errorEnvelope(model.NewError(model.ESTATE, "unknown action "+action, "action", 0, nil))
	}
}

func (s *Service) LoadVersion(versionID string) Envelope {
	if versionID == "" {
		return errorEnvelope(model.NewError(model.EVER, "version id is required", "version", 0, nil))
	}
	if _, err := formats.Version(versionID); err != nil {
		return errorEnvelope(err)
	}
	path := filepath.Join(s.sampleDir, fileForVersion(s.versions, versionID))
	geometry, err := formats.LoadFile(path, versionID)
	if err != nil {
		return errorEnvelope(err)
	}
	model.CanonicalizePoints(geometry.Points)
	cfg := pipeline.DefaultConfig()
	result, err := s.builder.Build(geometry, cfg, "exact", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	snap := &snapshot{
		Version: version, Reason: "load:" + versionID, SourceVersion: versionID,
		SourcePoints: append([]model.Point(nil), geometry.Points...),
		CleanPoints:  append([]model.Point(nil), result.CleanPoints...),
		Config:       cfg, CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: geometry.Meta,
	}
	snap.SnapshotHash = snapshotHash(snap)
	s.session.Snapshots[version] = snap
	s.session.MeshByVersion[version] = result.Mesh
	s.session.ActiveVersion = version
	s.session.LastComparison = nil
	s.session.LastPreview = nil
	s.session.LastExact = nil
	camera := s.session.Camera
	clip := s.session.Clip
	s.mu.Unlock()
	paramsJSON, _ := json.Marshal(cfg)
	if err := s.store.SaveVersion(context.Background(), store.VersionSnapshot{
		Version: version, Reason: snap.Reason, SnapshotHash: snap.SnapshotHash,
		SourceVersion: versionID, PointCount: len(result.CleanPoints), ParamsJSON: paramsJSON, Points: result.CleanPoints,
	}); err != nil {
		return errorEnvelope(err)
	}
	data := s.Bootstrap()
	if data.OK {
		if ws, ok := data.Data.(WorkspaceDTO); ok {
			ws.Camera, ws.Clip = camera, clip
			data.Data = ws
		}
	}
	return data
}

func (s *Service) LoadPath(path, versionID string) Envelope {
	if path == "" {
		return errorEnvelope(model.NewError(model.EVER, "file path is required", "file.path", 0, nil))
	}
	if versionID == "" {
		inferred, err := formats.InferVersion(path)
		if err != nil {
			return errorEnvelope(err)
		}
		versionID = inferred
	}
	if _, err := formats.Version(versionID); err != nil {
		return errorEnvelope(err)
	}
	geometry, err := formats.LoadFile(path, versionID)
	if err != nil {
		return errorEnvelope(err)
	}
	geometry.Meta.File = path
	geometry.Meta.DisplayName = "Local " + geometry.Meta.DisplayName
	model.CanonicalizePoints(geometry.Points)
	cfg := pipeline.DefaultConfig()
	result, err := s.builder.Build(geometry, cfg, "exact", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.versionSeq++
	version := s.versionSeq
	snap := &snapshot{
		Version: version, Reason: "load-path:" + filepath.Base(path), SourceVersion: versionID,
		SourcePoints: append([]model.Point(nil), geometry.Points...),
		CleanPoints:  append([]model.Point(nil), result.CleanPoints...),
		Config:       cfg, CreatedAt: time.Now().UTC().Format(time.RFC3339), Meta: geometry.Meta,
	}
	snap.SnapshotHash = snapshotHash(snap)
	s.session.Snapshots[version] = snap
	s.session.MeshByVersion[version] = result.Mesh
	s.session.ActiveVersion = version
	s.session.LastComparison = nil
	s.session.LastPreview = nil
	s.session.LastExact = nil
	s.mu.Unlock()
	if err := s.saveSnapshot(snap); err != nil {
		return errorEnvelope(err)
	}
	return s.Bootstrap()
}

func (s *Service) Preview(req PreviewRequest) Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "preview", 0, nil))
	}
	camera, clip := s.frameInputs(req.Camera, req.Clip)
	return s.renderPreview(snap, mesh, camera, clip)
}

func (s *Service) frameInputs(camera *render.Camera, clip *render.ClipPlane) (render.Camera, render.ClipPlane) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.session.Camera
	p := s.session.Clip
	if camera != nil {
		c = *camera
	}
	if clip != nil {
		p = *clip
	}
	return c, p
}

func (s *Service) renderPreview(snap *snapshot, mesh *model.Mesh, camera render.Camera, clip render.ClipPlane) Envelope {
	frame, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "preview", SampleRate: snap.Config.PreviewSample,
		FlatShade: true, FrameID: s.nextFrameID("P"), Version: snap.Version,
	})
	if err != nil {
		return errorEnvelope(err)
	}
	frame.Message = "preview: topology-preserving flat shading; camera and clip shared with exact"
	s.storeFrame(frame)
	s.mu.Lock()
	s.session.PreviewByVersion[snap.Version] = frame
	s.session.LastPreview = frame
	s.session.Camera, s.session.Clip = camera, clip
	s.mu.Unlock()
	data := map[string]any{
		"frame": FrameToDTO(frame), "version": snap.Version,
		"previewSampleRate": snap.Config.PreviewSample, "algorithmMode": frame.AlgorithmMode,
		"withinBudget": frame.WithinBudget,
	}
	return Envelope{OK: true, Data: data}
}

func (s *Service) Schema() Envelope {
	snap, _ := s.latestSnapshot()
	if snap == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active snapshot", "schema", 0, nil))
	}
	geometry, err := formats.LoadFile(filepath.Join(s.sampleDir, fileForVersion(s.versions, snap.SourceVersion)), snap.SourceVersion)
	if err != nil {
		return errorEnvelope(err)
	}
	return Envelope{OK: true, Data: map[string]any{
		"versionId": geometry.Meta.VersionID, "fieldOrder": geometry.Meta.FieldOrder,
		"optionalAttributes": geometry.Meta.OptionalAttributes, "fields": geometry.Meta.Fields,
		"quantization": geometry.Meta.Quantization, "headerMeta": geometry.Meta.HeaderMeta,
	}}
}

func (s *Service) defaultEdit() (int, float64) {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil || len(snap.CleanPoints) == 0 {
		return 0, 0.02
	}
	for _, c := range mesh.Chunks {
		if c.ID == "C00" && len(c.PointIndices) > 0 {
			return c.PointIndices[len(c.PointIndices)/2], 0.02
		}
	}
	return len(snap.CleanPoints) / 2, 0.02
}

func (s *Service) ListJobs() Envelope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobDTO, 0, len(s.jobs))
	keys := make([]string, 0, len(s.jobs))
	for k := range s.jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, jobToDTO(s.jobs[k]))
	}
	return Envelope{OK: true, Data: map[string]any{"jobs": out}}
}

func jobToDTO(j *jobState) JobDTO {
	return JobDTO{
		ID: j.ID, RunID: j.RunID, Mode: j.Mode, Version: j.Version, Status: j.Status,
		Committed: j.Committed, Total: j.Total, Sequence: append([]string(nil), j.Sequence...),
		Error: j.Error, StartedAt: j.StartedAt, UpdatedAt: j.UpdatedAt,
	}
}
