package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"audiobench/internal/dsp"
	"audiobench/internal/model"
	"audiobench/internal/pipeline"
)

// Router 注册全部本地 HTTP 路由。
func (m *Manager) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", m.health)
	mux.HandleFunc("/api/materials", m.listMaterials)
	mux.HandleFunc("/api/sessions", m.openSession)
	mux.HandleFunc("/api/sessions/", m.sessionRoutes)
	mux.HandleFunc("/", m.serveUI)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	if appErr, ok := err.(*model.AppError); ok {
		status := http.StatusBadRequest
		if appErr.Code == model.EIO {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, appErr)
		return
	}
	writeJSON(w, http.StatusInternalServerError,
		model.NewErr(model.EIO, "内部错误: %v", err))
}

func (m *Manager) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "offline": "true"})
}

func (m *Manager) listMaterials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, model.NewErr(model.ESchema, "仅支持 GET"))
		return
	}
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		writeErr(w, model.NewErr(model.EIO, "读取数据目录失败: %v", err))
		return
	}
	var descs []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && e.Name() != "dataset.json" && e.Name() != "categories.json" {
			descs = append(descs, filepath.Join(m.dataDir, e.Name()))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"descriptors": descs, "open_sessions": m.List()})
}

func (m *Manager) openSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, model.NewErr(model.ESchema, "仅支持 POST"))
		return
	}
	var req struct {
		Descriptor string `json:"descriptor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, model.NewErr(model.ESchema, "请求体不是合法 JSON: %v", err))
		return
	}
	s, err := m.OpenSession(req.Descriptor)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (m *Manager) sessionRoutes(w http.ResponseWriter, r *http.Request) {
	// /api/sessions/{id}/{action?}
	path := r.URL.Path[len("/api/sessions/"):]
	var id, action string
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			id, action = path[:i], path[i+1:]
			break
		}
	}
	if id == "" {
		id = path
	}
	s, ok := m.Get(id)
	if !ok {
		writeErr(w, model.NewErr(model.ESchema, "会话 %q 不存在", id))
		return
	}
	switch action {
	case "":
		m.getSession(w, r, s)
	case "params":
		m.updateParams(w, r, s)
	case "run":
		m.runRealtime(w, r, s)
	case "resume":
		m.runResume(w, r, s)
	case "full":
		m.runFull(w, r, s)
	case "pause":
		s.mu.Lock()
		if s.Engine != nil {
			s.Engine.Pause()
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
	case "cancel":
		s.mu.Lock()
		if s.CancelCtx != nil {
			s.CancelCtx()
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	case "retry":
		m.retryWindow(w, r, s)
	case "compare":
		m.comparePaths(w, r, s)
	case "export":
		m.export(w, r, s)
	default:
		writeErr(w, model.NewErr(model.ESchema, "未知动作 %q", action))
	}
}

func (m *Manager) getSession(w http.ResponseWriter, _ *http.Request, s *Session) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (m *Manager) updateParams(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeErr(w, model.NewErr(model.ESchema, "仅支持 PUT/POST"))
		return
	}
	var p model.Params
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, model.NewErr(model.ESchema, "参数 JSON 非法: %v", err))
		return
	}
	if err := dsp.ValidateParams(p); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	// 已提交窗口冻结：存在已提交窗口时拒绝静默改写，要求作废重算。
	committed := 0
	if s.Plan != nil {
		committed, _ = s.Plan.Progress()
	}
	if committed > 0 {
		s.mu.Unlock()
		writeErr(w, model.NewErr(model.EState,
			"已有 %d 个已提交冻结窗口，参数调整不得静默改写；请先作废快照再全量重算",
			committed).With("committed_windows", committed))
		return
	}
	s.Params = p
	s.Plan = nil
	s.Engine = nil
	s.LastAnn = nil
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (m *Manager) runRealtime(w http.ResponseWriter, r *http.Request, s *Session) {
	cfg := s.runCfg()
	ctx, cancel := context.WithCancel(r.Context())
	s.mu.Lock()
	s.CancelCtx = cancel
	s.Running = true
	s.mu.Unlock()
	ann, err := pipeline.RunRealtime(ctx, cfg)
	s.mu.Lock()
	s.Running = false
	if ann != nil {
		s.Plan = ann.Plan
		s.Engine = pipeline.NewEngine(ann.Plan, s.Samples, s.Params, s.Material.Categories,
			s.Material.SampleRate, nil, s.Limits)
		s.LastAnn = ann
	}
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, annView(ann))
}

func (m *Manager) runResume(w http.ResponseWriter, r *http.Request, s *Session) {
	cfg := s.runCfg()
	outcome, err := pipeline.RunResume(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	ann := outcome.Annotation
	s.mu.Lock()
	s.Plan = ann.Plan
	s.LastAnn = ann
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"annotation":  annView(ann),
		"invalidated": outcome.Invalidated,
		"reason":      outcome.Reason,
	})
}

func (m *Manager) runFull(w http.ResponseWriter, r *http.Request, s *Session) {
	cfg := s.runCfg()
	ann, err := pipeline.RunFull(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.LastAnn = ann
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, annView(ann))
}

func (m *Manager) retryWindow(w http.ResponseWriter, r *http.Request, s *Session) {
	var req struct {
		WindowIndex int `json:"window_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, model.NewErr(model.ESchema, "请求 JSON 非法: %v", err))
		return
	}
	s.mu.Lock()
	eng := s.Engine
	s.mu.Unlock()
	if eng == nil {
		writeErr(w, model.NewErr(model.EState, "尚无窗口计划，请先运行标注"))
		return
	}
	if err := eng.RetryWindow(req.WindowIndex); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (m *Manager) comparePaths(w http.ResponseWriter, r *http.Request, s *Session) {
	cfg := s.runCfg()
	// 全量重算作为基准（独立、无检查点依赖）。
	full, err := pipeline.RunFull(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	rt, err := pipeline.RunRealtime(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	outcome, err := pipeline.RunResume(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	errs := pipeline.CompareThree(rt, outcome.Annotation, full)
	resp := map[string]any{
		"consistent":         len(errs) == 0,
		"divergences":        errs,
		"resume_invalidated": outcome.Invalidated,
		"resume_reason":      outcome.Reason,
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusConflict, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (m *Manager) export(w http.ResponseWriter, r *http.Request, s *Session) {
	s.mu.Lock()
	ann := s.LastAnn
	s.mu.Unlock()
	if ann == nil {
		writeErr(w, model.NewErr(model.EState, "尚无标注结果可导出"))
		return
	}
	path := filepath.Join(m.varDir, s.Material.ID+"_export.json")
	data, _ := json.MarshalIndent(annView(ann), "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeErr(w, model.NewErr(model.EIO, "写导出文件失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"export_path": path})
}

// snapshot 生成会话的只读视图（含波形降采样、窗口矩阵、状态统计）。
func (s *Session) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := map[string]any{
		"material_id":   s.Material.ID,
		"sample_rate":   s.Material.SampleRate,
		"total_samples": s.Material.TotalSamples,
		"params":        s.Params,
		"window_frames": s.WindowFrames,
		"limits":        s.Limits,
		"categories":    s.Material.Categories,
		"declared":      s.Material.Events,
		"waveform":      downsample(s.Samples, 400),
		"frame_count":   dsp.FrameCount(len(s.Samples), s.Params.FrameLength, s.Params.FrameShift),
	}
	if s.Plan != nil {
		committed, total := s.Plan.Progress()
		matrix := make([]map[string]any, len(s.Plan.Windows))
		for i, win := range s.Plan.Windows {
			matrix[i] = map[string]any{
				"index": win.Index, "start_frame": win.StartFrame,
				"end_frame": win.EndFrame, "state": win.State,
				"fail_reason": win.FailReason,
			}
		}
		view["windows"] = matrix
		view["progress"] = map[string]int{"committed": committed, "total": total}
	}
	if s.LastAnn != nil {
		view["annotation"] = annView(s.LastAnn)
		view["running"] = s.Running
	}
	return view
}

func annView(a *pipeline.Annotation) map[string]any {
	view := map[string]any{
		"path":              a.Path,
		"events":            a.Events,
		"quality":           a.Quality,
		"voice_ratio":       a.VoiceRatio,
		"block_events":      a.BlockEvents,
		"invalidated":       a.Invalidated,
		"fingerprint_count": len(a.Fingerprints),
		// 仅回传前若干帧指纹供界面逐窗查看，避免超大响应。
		"fingerprints_head": headFingerprints(a.Fingerprints, 64),
	}
	if a.QualityErr != nil {
		view["quality_error"] = a.QualityErr
	}
	return view
}

func headFingerprints(fps []model.Fingerprint, n int) []model.Fingerprint {
	if len(fps) <= n {
		return fps
	}
	return fps[:n]
}

func downsample(samples []float64, buckets int) []float64 {
	if len(samples) <= buckets {
		return samples
	}
	out := make([]float64, buckets)
	step := len(samples) / buckets
	for b := 0; b < buckets; b++ {
		peak := 0.0
		start := b * step
		end := start + step
		if b == buckets-1 {
			end = len(samples)
		}
		for i := start; i < end; i++ {
			v := samples[i]
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		out[b] = peak
	}
	return out
}
