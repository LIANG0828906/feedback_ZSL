package server

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"

	"audiofp"
)

//go:embed web/index.html
var indexHTML []byte

type Server struct {
	mat    *audiofp.Material
	params audiofp.Params
	limits audiofp.ResourceLimits
	dbPath string

	mu           sync.Mutex
	machine      *audiofp.WindowMachine
	result       *audiofp.AnnotationResult
	compare      *audiofp.DiffReport
	compareError *audiofp.Error
	notice       string
}

type Config struct {
	Material *audiofp.Material
	Params   audiofp.Params
	Limits   audiofp.ResourceLimits
	DBPath   string
}

func New(cfg Config) *Server {
	params := cfg.Params.Normalize()
	limits := cfg.Limits.Normalize()
	return &Server{
		mat:     cfg.Material,
		params:  params,
		limits:  limits,
		dbPath:  cfg.DBPath,
		machine: audiofp.NewFrameWindowMachine(cfg.Material.PCM.SampleCount, params),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/params", s.handleParams)
	mux.HandleFunc("POST /api/annotate", s.handleAnnotate)
	mux.HandleFunc("POST /api/resume", s.handleResume)
	mux.HandleFunc("POST /api/compare", s.handleCompare)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	mux.HandleFunc("POST /api/windows/{index}/retry", s.handleRetry)
	mux.HandleFunc("POST /api/windows/{index}/simulate-failure", s.handleSimulateFailure)
	mux.HandleFunc("GET /api/export", s.handleExport)
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	fmt.Printf("audio fingerprint UI: http://%s\n", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) InitialAnnotation(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runResumeLocked(ctx)
}

type waveformView struct {
	BucketSamples int       `json:"bucket_samples"`
	Min           []float64 `json:"min"`
	Max           []float64 `json:"max"`
}

type stateResponse struct {
	MaterialID   string                    `json:"material_id"`
	SampleRate   int                       `json:"sample_rate"`
	SampleCount  int                       `json:"sample_count"`
	ContentHash  string                    `json:"content_hash"`
	Waveform     waveformView              `json:"waveform"`
	Manifest     audiofp.Manifest          `json:"manifest"`
	Params       audiofp.Params            `json:"params"`
	Limits       audiofp.ResourceLimits    `json:"limits"`
	Committed    int                       `json:"committed_windows"`
	TotalWindows int                       `json:"total_windows"`
	Windows      []audiofp.Window          `json:"windows"`
	Result       *audiofp.AnnotationResult `json:"result"`
	Compare      *audiofp.DiffReport       `json:"compare"`
	CompareError *audiofp.Error            `json:"compare_error"`
	Notice       string                    `json:"notice,omitempty"`
	DBPath       string                    `json:"db_path"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeState(w, http.StatusOK)
}

func (s *Server) writeState(w http.ResponseWriter, status int) {
	committed, total := s.machine.Progress()
	resp := stateResponse{
		MaterialID: s.mat.PCM.ID, SampleRate: s.mat.PCM.SampleRate,
		SampleCount: s.mat.PCM.SampleCount, ContentHash: s.mat.ContentHash(),
		Waveform: buildWaveform(s.mat.PCM.Samples, 1200),
		Manifest: s.mat.Manifest, Params: s.params, Limits: s.limits,
		Committed: committed, TotalWindows: total, Windows: s.machine.Snapshot(),
		Result: s.result, Compare: s.compare, CompareError: s.compareError,
		Notice: s.notice, DBPath: s.dbPath,
	}
	writeJSON(w, status, resp)
}

func buildWaveform(samples []float64, bucketCount int) waveformView {
	bucket := int(math.Ceil(float64(len(samples)) / float64(bucketCount)))
	if bucket < 1 {
		bucket = 1
	}
	count := (len(samples) + bucket - 1) / bucket
	view := waveformView{BucketSamples: bucket, Min: make([]float64, count), Max: make([]float64, count)}
	for b := 0; b < count; b++ {
		minV := math.Inf(1)
		maxV := math.Inf(-1)
		start := b * bucket
		end := start + bucket
		if end > len(samples) {
			end = len(samples)
		}
		for i := start; i < end; i++ {
			minV = math.Min(minV, samples[i])
			maxV = math.Max(maxV, samples[i])
		}
		view.Min[b] = minV
		view.Max[b] = maxV
	}
	return view
}

type paramsRequest struct {
	audiofp.Params
	MaxWorkers             *int `json:"max_workers"`
	PerWorkerBufferSamples *int `json:"per_worker_buffer_samples"`
	MaxQueuedJobs          *int `json:"max_queued_jobs"`
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	var req paramsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, audiofp.NewHTTPError(audiofp.CodeSchema, "", "invalid JSON: %v", err))
		return
	}
	params := req.Params.Normalize()
	if err := params.Validate(); err != nil {
		s.writeErr(w, err)
		return
	}
	limits := s.limits
	if req.MaxWorkers != nil {
		limits.MaxWorkers = *req.MaxWorkers
	}
	if req.PerWorkerBufferSamples != nil {
		limits.PerWorkerBufferSamples = *req.PerWorkerBufferSamples
	}
	if req.MaxQueuedJobs != nil {
		limits.MaxQueuedJobs = *req.MaxQueuedJobs
	}
	limits = limits.Normalize()
	if err := limits.Validate(params); err != nil {
		s.writeErr(w, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	geometryChanged := geometrySignature(s.params) != geometrySignature(params)
	s.params = params
	s.limits = limits
	if s.machine != nil {
		s.machine.UpdateUncommittedParameters(params)
	}
	s.notice = ""
	if geometryChanged {
		s.notice = "帧长度/帧移/频带数已变更：未提交窗口使用新参数；点击“增量标注”会建立新参数计划，点击“全量重算”可比较全新结果。"
	} else if s.result != nil && geometryMatchesFrames(s.result.Frames, params) {
		result, err := audiofp.BuildAnnotationResult("interactive", s.mat, params, limits, s.result.Frames, s.machine)
		s.result = result
		if err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
			s.writeErr(w, err)
			return
		}
	}
	s.writeState(w, http.StatusOK)
}

func geometrySignature(p audiofp.Params) string {
	return fmt.Sprintf("%d/%d/%d", p.FrameLength, p.FrameShift, p.BandCount)
}

func geometryMatchesFrames(frames []audiofp.Fingerprint, p audiofp.Params) bool {
	if len(frames) == 0 {
		return false
	}
	f := frames[0]
	return f.FrameLength == p.FrameLength && f.FrameShift == p.FrameShift && f.BandCount == p.BandCount
}

type annotateRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	var req annotateRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Mode == "" {
		req.Mode = "incremental"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	switch req.Mode {
	case "full":
		err = s.runFullLocked(r.Context())
	case "incremental":
		err = s.runIncrementalLocked(r.Context())
	default:
		err = audiofp.NewHTTPError(audiofp.CodePara, "mode", "mode must be full or incremental")
	}
	if err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
		s.writeErr(w, err)
		return
	}
	s.writeState(w, http.StatusOK)
}

func (s *Server) runFullLocked(ctx context.Context) error {
	result, err := audiofp.AnnotateFull(ctx, s.mat, s.params, s.limits)
	if result != nil {
		s.result = result
		machine := audiofp.NewFrameWindowMachine(s.mat.PCM.SampleCount, s.params)
		for _, fp := range result.Frames {
			if err := machine.Start(fp.FrameIndex); err != nil {
				return err
			}
			if err := machine.Commit(fp.FrameIndex, fp, s.params); err != nil {
				return err
			}
		}
		s.machine = machine
		s.notice = fmt.Sprintf("全量重算完成：%d 个窗口已提交", len(result.Frames))
	}
	return err
}

func (s *Server) runIncrementalLocked(ctx context.Context) error {
	if err := s.limits.Validate(s.params); err != nil {
		return err
	}
	if s.machine != nil {
		committed, total := s.machine.Progress()
		featureChanged := false
		for _, w := range s.machine.Snapshot() {
			if w.Fingerprint != nil && w.Fingerprint.FeatureVersion != s.params.FeatureVersion() {
				featureChanged = true
				break
			}
		}
		if featureChanged && committed == total {
			s.notice = "特征参数版本已变化且旧窗口均已提交冻结；已按新参数版本执行一次全量重算。"
			return s.runFullLocked(ctx)
		}
	}
	expectedTotal := audiofp.FrameCount(s.mat.PCM.SampleCount, s.params)
	if s.machine == nil || s.machine.Total() != expectedTotal {
		s.machine = audiofp.NewFrameWindowMachine(s.mat.PCM.SampleCount, s.params)
	}
	buffer := make([]float64, s.params.FrameLength, s.limits.PerWorkerBufferSamples)
	for i := 0; i < s.machine.Total(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		w, ok := s.machine.Window(i)
		if !ok || w.State == audiofp.StateCommitted {
			continue
		}
		params := s.params
		if w.ParamSnapshot != nil {
			params = *w.ParamSnapshot
		}
		start := w.StartSample
		if start < 0 || start+params.FrameLength > len(s.mat.PCM.Samples) {
			err := audiofp.NewHTTPError(audiofp.CodeRange, fmt.Sprintf("windows[%d].start_sample", i),
				"window frame [%d,%d) is outside material", start, start+params.FrameLength)
			_ = s.machine.Fail(i, err)
			return err
		}
		if err := s.machine.Start(i); err != nil {
			return err
		}
		copyBuffer := buffer[:params.FrameLength]
		copy(copyBuffer, s.mat.PCM.Samples[start:start+params.FrameLength])
		fp := audiofp.ExtractFrameBuffer(copyBuffer, i, start, s.mat.PCM.SampleRate, params)
		if err := s.machine.Commit(i, fp, params); err != nil {
			_ = s.machine.Fail(i, err)
			return err
		}
	}
	frames, err := s.machine.OrderedCommittedFingerprints()
	if err != nil {
		return err
	}
	result, qerr := audiofp.BuildAnnotationResult("incremental", s.mat, s.params, s.limits, frames, s.machine)
	s.result = result
	s.notice = fmt.Sprintf("增量标注完成：%d 个已提交窗口", len(frames))
	return qerr
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.writeErr(w, audiofp.NewHTTPError(audiofp.CodePara, "index", "invalid window index"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.retryWindowLocked(r.Context(), index); err != nil {
		if !audiofp.HasCode(err, audiofp.CodeQual) {
			s.writeErr(w, err)
			return
		}
	}
	s.writeState(w, http.StatusOK)
}

func (s *Server) retryWindowLocked(ctx context.Context, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, ok := s.machine.Window(index)
	if !ok {
		return audiofp.NewHTTPError(audiofp.CodeRange, "index", "window %d not found", index)
	}
	params := s.params
	if w.ParamSnapshot != nil {
		params = *w.ParamSnapshot
	}
	if err := s.limits.Validate(params); err != nil {
		return err
	}
	if err := s.machine.Start(index); err != nil {
		return err
	}
	buffer := make([]float64, params.FrameLength, s.limits.PerWorkerBufferSamples)
	start := w.StartSample
	if start+params.FrameLength > len(s.mat.PCM.Samples) {
		err := audiofp.NewHTTPError(audiofp.CodeRange, "start_sample", "window %d outside material", index)
		_ = s.machine.Fail(index, err)
		return err
	}
	copy(buffer, s.mat.PCM.Samples[start:start+params.FrameLength])
	fp := audiofp.ExtractFrameBuffer(buffer[:params.FrameLength], index, start, s.mat.PCM.SampleRate, params)
	if err := s.machine.Commit(index, fp, params); err != nil {
		return err
	}
	frames, err := s.machine.OrderedCommittedFingerprints()
	if err != nil {
		if de, ok := audiofp.AsError(err); ok && de.Code == audiofp.CodeState {
			s.notice = "窗口已重试并提交；仍存在前序未提交窗口，先运行增量标注即可完成区间聚合。"
			return nil
		}
		return err
	}
	result, qerr := audiofp.BuildAnnotationResult("window-retry", s.mat, s.params, s.limits, frames, s.machine)
	s.result = result
	return qerr
}

func (s *Server) handleSimulateFailure(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.writeErr(w, audiofp.NewHTTPError(audiofp.CodePara, "index", "invalid window index"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.machine.Window(index)
	if !ok {
		s.writeErr(w, audiofp.NewHTTPError(audiofp.CodeRange, "index", "window %d not found", index))
		return
	}
	if window.State == audiofp.StatePending {
		if err := s.machine.Start(index); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	if window.State != audiofp.StateFailed {
		if err := s.machine.Fail(index, errors.New("simulated extraction failure for retry demo")); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	s.notice = fmt.Sprintf("窗口 %d 已模拟为失败，可点击重试", index)
	s.writeState(w, http.StatusOK)
}

type resetRequest struct {
	CommittedPrefix int `json:"committed_prefix"`
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.machine = audiofp.NewFrameWindowMachine(s.mat.PCM.SampleCount, s.params)
	s.result = nil
	s.compare = nil
	s.compareError = nil
	s.notice = "窗口状态已重置"
	if req.CommittedPrefix > 0 {
		buffer := make([]float64, s.params.FrameLength)
		for i := 0; i < req.CommittedPrefix && i < s.machine.Total(); i++ {
			start := audiofp.FrameStart(i, s.params)
			if err := s.machine.Start(i); err != nil {
				s.writeErr(w, err)
				return
			}
			copy(buffer, s.mat.PCM.Samples[start:start+s.params.FrameLength])
			fp := audiofp.ExtractFrameBuffer(buffer, i, start, s.mat.PCM.SampleRate, s.params)
			if err := s.machine.Commit(i, fp, s.params); err != nil {
				s.writeErr(w, err)
				return
			}
		}
		frames, _ := s.machine.OrderedCommittedFingerprints()
		result, qerr := audiofp.BuildAnnotationResult("partial-reset", s.mat, s.params, s.limits, frames, s.machine)
		s.result = result
		if qerr != nil && !audiofp.HasCode(qerr, audiofp.CodeQual) {
			s.writeErr(w, qerr)
			return
		}
		s.notice = fmt.Sprintf("已构造前 %d 个已提交窗口，其余为待提取", req.CommittedPrefix)
	}
	s.writeState(w, http.StatusOK)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.runResumeLocked(r.Context()); err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
		s.writeErr(w, err)
		return
	}
	s.writeState(w, http.StatusOK)
}

func (s *Server) runResumeLocked(ctx context.Context) error {
	result, err := audiofp.AnnotateResume(ctx, s.mat, s.params, s.limits, s.dbPath)
	if result != nil {
		s.result = result
		s.machine = audiofp.NewFrameWindowMachine(s.mat.PCM.SampleCount, s.params)
		for _, fp := range result.Frames {
			_ = s.machine.Start(fp.FrameIndex)
			_ = s.machine.Commit(fp.FrameIndex, fp, s.params)
		}
		s.notice = fmt.Sprintf("断点恢复完成：恢复 %d 窗，快照作废=%v", result.RecoveredWindows, result.SnapshotReset)
	}
	return err
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	full, errFull := audiofp.AnnotateFull(ctx, s.mat, s.params, s.limits)
	realtime, errRT := audiofp.AnnotateRealtime(ctx, s.mat, s.params, s.limits)
	resume, errResume := audiofp.AnnotateResume(ctx, s.mat, s.params, s.limits, s.dbPath)
	for _, err := range []error{errFull, errRT, errResume} {
		if err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
			s.writeErr(w, err)
			return
		}
	}
	report, diffErr := audiofp.EnsureIdentical(full, realtime, resume)
	s.compare = report
	if de, ok := audiofp.AsError(diffErr); ok {
		s.compareError = de
	} else {
		s.compareError = nil
	}
	if resume != nil {
		s.notice = fmt.Sprintf("三路径比对完成：consistent=%v recovered=%d reset=%v",
			report.Consistent, resume.RecoveredWindows, resume.SnapshotReset)
	}
	s.writeState(w, http.StatusOK)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result == nil {
		result, err := audiofp.AnnotateFull(r.Context(), s.mat, s.params, s.limits)
		if err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
			s.writeErr(w, err)
			return
		}
		s.result = result
	}
	format := r.URL.Query().Get("format")
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="annotations.csv"`)
		writer := csv.NewWriter(w)
		_ = writer.Write([]string{"id", "category", "start_sample", "end_sample", "confidence", "mean_similarity"})
		for _, in := range s.result.Intervals {
			_ = writer.Write([]string{
				in.ID, in.Category, strconv.Itoa(in.StartSample), strconv.Itoa(in.EndSample),
				strconv.FormatFloat(in.Confidence, 'f', 6, 64),
				strconv.FormatFloat(in.MeanSimilarity, 'f', 6, 64),
			})
		}
		writer.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="annotation.json"`)
	_ = json.NewEncoder(w).Encode(s.result)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	de, ok := audiofp.AsError(err)
	if !ok {
		de = &audiofp.Error{Code: "E_INTERNAL", Message: err.Error()}
	}
	status := http.StatusInternalServerError
	switch de.Code {
	case audiofp.CodePara, audiofp.CodeSchema:
		status = http.StatusBadRequest
	case audiofp.CodeRange, audiofp.CodeQual:
		status = http.StatusUnprocessableEntity
	case audiofp.CodeState, audiofp.CodeDiff:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"error": de})
}
