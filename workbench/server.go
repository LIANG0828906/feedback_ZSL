// Package workbench provides the local HTTP UI and diagnostic API.
package workbench

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"

	"loudnessworkbench/apperr"
	"loudnessworkbench/checkpoint"
	"loudnessworkbench/dsp"
	"loudnessworkbench/engine"
	"loudnessworkbench/project"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	dataDir  string
	manifest *project.Manifest
	programs map[string]*project.Program
	version  string

	runMu   sync.Mutex
	stateMu sync.Mutex
	session *engine.Session
	store   *checkpoint.Store
	result  *engine.Result
	truth   *engine.TruthReport
	diff    *engine.DifferenceReport
	running bool
	lastErr *apperr.Error
}

func NewServer(dataDir, version string) (*Server, error) {
	manifest, err := project.LoadManifest(filepath.Join(dataDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	programs := map[string]*project.Program{}
	for _, candidate := range []string{project.SourceV1, project.SourceV2, project.SourceV3} {
		path := filepath.Join(dataDir, "projects", "program."+candidate+".json")
		program, err := project.LoadWithManifest(path, filepath.Join(dataDir, "manifest.json"))
		if err != nil {
			return nil, err
		}
		programs[candidate] = program
	}
	if version == "" {
		version = project.SourceV3
	}
	if programs[version] == nil {
		return nil, apperr.New(apperr.CodeParameter, "unknown project version").
			WithField("version").WithActual(version)
	}
	server := &Server{
		dataDir: dataDir, manifest: manifest, programs: programs, version: version,
	}
	if err := server.buildSession(version); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) buildSession(version string) error {
	storeDir := filepath.Join(s.dataDir, "checkpoints", "workbench", version)
	store := checkpoint.NewStore(storeDir, s.programs[version].ProjectID)
	session, err := engine.NewSession(s.programs[version], store)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.version = version
	s.session = session
	s.store = store
	s.result = nil
	s.truth = nil
	s.diff = nil
	s.lastErr = nil
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/interop", s.handleInterop)
	sub, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return withHeaders(mux)
}

type stateResponse struct {
	Manifest         manifestView             `json:"manifest"`
	Version          string                   `json:"version"`
	Versions         []string                 `json:"versions"`
	Chain            dsp.ChainConfig          `json:"chain"`
	Segments         []segmentView            `json:"segments"`
	Running          bool                     `json:"running"`
	Result           *resultView              `json:"result,omitempty"`
	Truth            *engine.TruthReport      `json:"truth,omitempty"`
	Diff             *engine.DifferenceReport `json:"diff,omitempty"`
	LastError        *apperr.Error            `json:"last_error,omitempty"`
	Interoperability []interopView            `json:"interoperability"`
}

type manifestView struct {
	ProgramID    string  `json:"program_id"`
	Seed         int64   `json:"seed"`
	SampleRateHz int     `json:"sample_rate_hz"`
	TargetLUFS   float64 `json:"target_lufs"`
}

type segmentView struct {
	Index                     int                 `json:"index"`
	ID                        string              `json:"id"`
	Channels                  int                 `json:"channels"`
	Frames                    int                 `json:"frames"`
	State                     engine.SegmentState `json:"state"`
	SourceMeasurement         *dsp.Measurement    `json:"source_measurement,omitempty"`
	OutputMeasurement         *dsp.Measurement    `json:"output_measurement,omitempty"`
	NormalizationGainDB       float64             `json:"normalization_gain_db"`
	CompressionMaxReductionDB float64             `json:"compression_max_reduction_db"`
	CompressionGainCurveDB    []float64           `json:"compression_gain_curve_db,omitempty"`
	Error                     *apperr.Error       `json:"error,omitempty"`
}

type resultView struct {
	Mode                     engine.Mode         `json:"mode"`
	MixHash                  string              `json:"mix_hash"`
	MixFrames                int                 `json:"mix_frames"`
	MixChannels              int                 `json:"mix_channels"`
	CompressedIntegratedLUFS float64             `json:"compressed_integrated_lufs"`
	SourceIntegratedLUFS     float64             `json:"source_integrated_lufs"`
	FinalProgramLoudnessLUFS float64             `json:"final_program_loudness_lufs"`
	GlobalGainDB             float64             `json:"global_gain_db"`
	ProgramTruePeakDBTP      float64             `json:"program_true_peak_dbtp"`
	NormalizationIterations  int                 `json:"normalization_iterations"`
	NormalizationConverged   bool                `json:"normalization_converged"`
	BufferPeakBytes          int64               `json:"buffer_peak_bytes"`
	Recovery                 engine.RecoveryInfo `json:"recovery"`
	Offsets                  []engine.MixSpan    `json:"mix_offsets"`
}

type interopView struct {
	Left          string   `json:"left"`
	Right         string   `json:"right"`
	Equivalent    bool     `json:"equivalent"`
	FirstMismatch string   `json:"first_mismatch,omitempty"`
	Trace         []string `json:"degradation_trace"`
}

func (s *Server) handleState(writer http.ResponseWriter, _ *http.Request) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	program := s.session.Program()
	statuses := s.session.Statuses()
	segments := make([]segmentView, len(statuses))
	for i, status := range statuses {
		segment := program.Segments[i]
		view := segmentView{
			Index: i, ID: status.ID, Channels: segment.Channels,
			Frames: segment.Frames(), State: status.State,
		}
		if s.result != nil && i < len(s.result.Segments) {
			result := s.result.Segments[i]
			view.SourceMeasurement = &result.SourceMeasurement
			view.OutputMeasurement = &result.OutputMeasurement
			view.NormalizationGainDB = result.NormalizationGainDB
			view.CompressionMaxReductionDB = result.CompressionMaxReductionDB
			view.CompressionGainCurveDB = result.CompressionGainCurveDB
			view.Error = result.Error
		}
		segments[i] = view
	}
	response := stateResponse{
		Manifest: manifestView{
			ProgramID: program.ProjectID, Seed: s.manifest.Seed,
			SampleRateHz: program.SampleRateHz, TargetLUFS: program.TargetLUFS,
		},
		Version:   s.version,
		Versions:  []string{project.SourceV1, project.SourceV2, project.SourceV3},
		Chain:     program.ChainConfig(),
		Segments:  segments,
		Running:   s.running,
		Truth:     s.truth,
		Diff:      s.diff,
		LastError: s.lastErr,
	}
	if s.result != nil {
		response.Result = &resultView{
			Mode: s.result.Mode, MixHash: s.result.MixHash, MixFrames: s.result.MixFrames,
			MixChannels:              s.result.MixChannels,
			CompressedIntegratedLUFS: s.result.CompressedIntegratedLUFS,
			SourceIntegratedLUFS:     s.result.SourceIntegratedLUFS,
			FinalProgramLoudnessLUFS: s.result.FinalProgramLoudnessLUFS,
			GlobalGainDB:             s.result.GlobalGainDB, ProgramTruePeakDBTP: s.result.ProgramTruePeakDBTP,
			NormalizationIterations: s.result.NormalizationIterations,
			NormalizationConverged:  s.result.NormalizationConverged,
			BufferPeakBytes:         s.result.BufferPeakBytes, Recovery: s.result.Recovery,
			Offsets: s.result.Offsets,
		}
	}
	response.Interoperability, _ = s.computeInterop()
	writeJSON(writer, http.StatusOK, response)
}

type runRequest struct {
	Mode                 engine.Mode `json:"mode"`
	MaxParallel          int         `json:"max_parallel"`
	PerWorkerBufferBytes int64       `json:"per_worker_buffer_bytes"`
}

func (s *Server) handleRun(writer http.ResponseWriter, request *http.Request) {
	var body runRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, err)
		return
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.stateMu.Lock()
	session := s.session
	program := session.Program()
	s.running = true
	s.lastErr = nil
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		s.running = false
		s.stateMu.Unlock()
	}()
	if body.PerWorkerBufferBytes == 0 {
		body.PerWorkerBufferBytes = 16 << 20
	}
	result, runErr := session.Run(request.Context(), engine.RunOptions{
		Mode: body.Mode, MaxParallel: body.MaxParallel,
		PerWorkerBufferBytes: body.PerWorkerBufferBytes,
	})
	s.stateMu.Lock()
	s.result = result
	if result != nil {
		s.truth, _ = engine.VerifyTruth(result, s.manifest)
	}
	s.lastErr = asError(runErr)
	s.stateMu.Unlock()
	if runErr != nil {
		writeError(writer, runErr)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "version": program.SourceVersion})
}

func (s *Server) handleConfig(writer http.ResponseWriter, request *http.Request) {
	var chain dsp.ChainConfig
	if err := decodeJSON(request, &chain); err != nil {
		writeError(writer, err)
		return
	}
	s.stateMu.Lock()
	session := s.session
	s.stateMu.Unlock()
	if err := session.UpdateChain(chain); err != nil {
		writeError(writer, err)
		return
	}
	s.stateMu.Lock()
	s.result = nil
	s.truth = nil
	s.diff = nil
	s.lastErr = nil
	s.stateMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleVersion(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, err)
		return
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.programs[body.Version] == nil {
		writeError(writer, apperr.New(apperr.CodeParameter, "unknown version").WithField("version").WithActual(body.Version))
		return
	}
	if err := s.buildSession(body.Version); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "version": body.Version})
}

func (s *Server) handleReset(writer http.ResponseWriter, request *http.Request) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.stateMu.Lock()
	version := s.version
	s.stateMu.Unlock()
	if err := s.buildSession(version); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleVerify(writer http.ResponseWriter, request *http.Request) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.stateMu.Lock()
	program := s.session.Program()
	version := s.version
	s.running = true
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		s.running = false
		s.stateMu.Unlock()
	}()
	checkRoot := filepath.Join(s.dataDir, "checkpoints", "workbench-verify", version)
	fullStore := checkpoint.NewStore(filepath.Join(checkRoot, "full"), program.ProjectID)
	fullSession, err := engine.NewSession(program, fullStore)
	if err != nil {
		writeError(writer, err)
		return
	}
	full, err := fullSession.Run(request.Context(), engine.RunOptions{Mode: engine.ModeFull, MaxParallel: 4, PerWorkerBufferBytes: 16 << 20})
	if err != nil {
		writeError(writer, err)
		return
	}
	realtimeStore := checkpoint.NewStore(filepath.Join(checkRoot, "realtime"), program.ProjectID)
	realtimeSession, err := engine.NewSession(program, realtimeStore)
	if err != nil {
		writeError(writer, err)
		return
	}
	realtime, err := realtimeSession.Run(request.Context(), engine.RunOptions{Mode: engine.ModeRealtime, PerWorkerBufferBytes: 16 << 20})
	if err != nil {
		writeError(writer, err)
		return
	}
	resumeStore := checkpoint.NewStore(filepath.Join(checkRoot, "resume"), program.ProjectID)
	snapshot, err := fullStore.Load()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := resumeStore.Save(*snapshot); err != nil {
		writeError(writer, err)
		return
	}
	resumeSession, err := engine.NewSession(program, resumeStore)
	if err != nil {
		writeError(writer, err)
		return
	}
	resumed, err := resumeSession.Run(request.Context(), engine.RunOptions{Mode: engine.ModeResume, MaxParallel: 4, PerWorkerBufferBytes: 16 << 20})
	if err != nil {
		writeError(writer, err)
		return
	}
	diffFullRealtime, err := engine.CompareResults(full, realtime)
	if err != nil {
		s.stateMu.Lock()
		s.diff = diffFullRealtime
		s.lastErr = asError(err)
		s.stateMu.Unlock()
		writeError(writer, err)
		return
	}
	diffFullResume, err := engine.CompareResults(full, resumed)
	s.stateMu.Lock()
	s.diff = diffFullResume
	s.result = full
	s.truth, _ = engine.VerifyTruth(full, s.manifest)
	s.lastErr = asError(err)
	s.stateMu.Unlock()
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"ok": true,
		"comparisons": []map[string]any{
			{"left": "full", "right": "realtime", "report": diffFullRealtime},
			{"left": "full", "right": "resume", "report": diffFullResume},
		},
	})
}

func (s *Server) handleInterop(writer http.ResponseWriter, _ *http.Request) {
	interop, err := s.computeInterop()
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, interop)
}

func (s *Server) computeInterop() ([]interopView, error) {
	versions := []string{project.SourceV1, project.SourceV2, project.SourceV3}
	var out []interopView
	for i := 0; i < len(versions); i++ {
		for j := i + 1; j < len(versions); j++ {
			left, right := versions[i], versions[j]
			report, err := project.ComparePrograms(s.programs[left], s.programs[right])
			view := interopView{Left: left, Right: right, Equivalent: err == nil, Trace: report.Trace}
			if err != nil {
				if e, ok := apperr.As(err); ok {
					view.FirstMismatch = e.Path
				}
			}
			out = append(out, view)
		}
	}
	return out, nil
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return apperr.Wrap(apperr.CodeParameter, "invalid JSON request", err)
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(writer http.ResponseWriter, err error) {
	code := apperr.Code(err)
	status := http.StatusInternalServerError
	switch code {
	case apperr.CodeParameter, apperr.CodeVersion:
		status = http.StatusBadRequest
	case apperr.CodeSchema, apperr.CodeDecode:
		status = http.StatusUnprocessableEntity
	case apperr.CodeState:
		status = http.StatusConflict
	case apperr.CodeClip, apperr.CodeDivergence, apperr.CodeLossy:
		status = http.StatusPreconditionFailed
	case apperr.CodeCancelled:
		status = 499
	}
	if appError, ok := apperr.As(err); ok {
		writeJSON(writer, status, appError)
		return
	}
	writeJSON(writer, status, apperr.New(apperr.CodeInternal, err.Error()))
}

func asError(err error) *apperr.Error {
	if err == nil {
		return nil
	}
	if appError, ok := apperr.As(err); ok {
		return appError
	}
	return apperr.Wrap(apperr.CodeInternal, err.Error(), err)
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}
