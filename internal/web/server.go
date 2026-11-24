package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	"mcapture/internal/pipeline"
	"mcapture/internal/session"
	"mcapture/internal/store"
	"os"
)

// Server 提供回环本地诊断页面与 JSON API。
type Server struct {
	dir, work, addr string
	mu              sync.Mutex
	sess            *session.Session
	cfg             pipeline.AlgoConfig
	last            *pipeline.Result
	lastEngine      *pipeline.Engine
	openStore       *store.Store
	lastErr         error
	httpSrv         *http.Server
}

// NewServer 装载会话并准备诊断服务；addr 必须是回环地址。
func NewServer(dir, work, addr string) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("bad addr: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || (!ip.IsLoopback()) {
		return nil, fmt.Errorf("diagnostic server may only bind loopback, got %q", host)
	}
	sess, err := session.Load(dir)
	if err != nil {
		return nil, err
	}
	s := &Server{dir: dir, work: work, addr: addr, sess: sess, cfg: pipeline.DefaultConfig(sess.Manifest)}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/inject", s.handleInject)
	s.httpSrv = &http.Server{Addr: addr, Handler: mux}
	return s, nil
}

// ListenAndServe 启动回环 HTTP 服务。
func (s *Server) ListenAndServe() error { return s.httpSrv.ListenAndServe() }

// Close 释放底层检查点连接。
func (s *Server) Close() error {
	if s.openStore != nil {
		return s.openStore.Close()
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	resp := map[string]string{"error": err.Error(), "code": session.CodeOf(err)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := map[string]any{
		"session":  s.sess.Manifest,
		"config":   s.cfg,
		"progress": s.progressLocked(),
	}
	if s.last != nil {
		state["events"] = s.last.Events
		state["commit_seq"] = s.last.CommitSeq
		state["drift"] = s.last.Drift
		state["residuals"] = s.last.Residuals
		state["segments"] = s.segmentViewLocked()
		state["waveforms"] = s.waveformViewLocked()
	}
	if s.lastErr != nil {
		state["last_error"] = map[string]string{
			"code": session.CodeOf(s.lastErr), "message": s.lastErr.Error(),
		}
	}
	writeJSON(w, state)
}

type runRequest struct {
	Mode    string  `json:"mode"`
	Workers int     `json:"workers"`
	Seg     int64   `json:"seg"`
	HalfWin int     `json:"halfwin"`
	Order   float64 `json:"order"`
	Tol     float64 `json:"tol"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if req.Seg > 0 {
		cfg.SegmentSamples = req.Seg
	}
	if req.HalfWin > 0 {
		cfg.HalfWindow = req.HalfWin
	}
	if req.Order > 0 {
		cfg.SincOrder = req.Order
	}
	if req.Tol > 0 {
		cfg.Tolerance = req.Tol
	}
	workers := req.Workers
	if workers < 1 {
		workers = cfg.MaxWorkers
	}
	mode := pipeline.Mode(req.Mode)
	if mode == "" {
		mode = pipeline.ModeFull
	}
	db := filepath.Join(s.work, "web-"+string(mode)+".db")
	if mode == pipeline.ModeFull || mode == pipeline.ModeRealtime {
		_ = os.Remove(db)
		_ = os.Remove(db + "-wal")
		_ = os.Remove(db + "-shm")
	}
	if s.openStore != nil {
		_ = s.openStore.Close()
		s.openStore = nil
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	st, err := store.Open(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_ = st.PutMeta("session_fingerprint", s.sess.Manifest.Fingerprint())
	_ = st.PutMeta("param_version", cfg.ParamVersion)
	_ = st.PutMeta("algo_version", cfg.AlgoVersion)
	eng, err := pipeline.NewRunnableEngine(s.sess, cfg, st)
	if err != nil {
		_ = st.Close()
		writeErr(w, http.StatusConflict, err)
		return
	}
	res, err := pipeline.RunEngine(r.Context(), eng, mode, workers)
	if err != nil {
		s.lastErr = err
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.openStore = st
	s.cfg = cfg
	s.lastEngine = eng
	s.last = res
	s.lastErr = nil
	writeJSON(w, map[string]any{
		"ok": true, "mode": mode, "progress": res.Progress,
		"commit_seq": res.CommitSeq, "events": len(res.Events),
	})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		Segment int    `json:"segment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastEngine == nil {
		writeErr(w, http.StatusConflict,
			session.NewError(session.EState, "no active engine; trigger a run first"))
		return
	}
	if err := s.lastEngine.RetrySegment(req.Channel, req.Segment); err != nil {
		s.lastErr = err
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.last = s.lastEngine.Collect(pipeline.ModeResume)
	s.lastErr = nil
	writeJSON(w, map[string]any{"ok": true, "channel": req.Channel, "segment": req.Segment,
		"progress": s.last.Progress})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Out string `json:"out"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Out == "" {
		req.Out = filepath.Join(s.work, "export")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		writeErr(w, http.StatusConflict, session.NewError(session.EState, "no result to export"))
		return
	}
	if err := pipeline.ExportMultiChannel(req.Out, s.sess, s.last); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "out": req.Out})
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fault  string  `json:"fault"`
		Marker int     `json:"marker"`
		Gain   float64 `json:"gain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dir := filepath.Join(s.work, "injected-session")
	gen := session.DefaultGenConfig()
	switch req.Fault {
	case session.EChan, "chan":
		gen.DropChannel = "ch1"
	case session.EMark, "mark":
		gen.DropMarker = req.Marker
	case session.EClip, "clip":
		gen.ClipGain = req.Gain
	default:
		writeErr(w, http.StatusBadRequest, session.NewError(session.EPara, "unknown fault"))
		return
	}
	m, faults, err := session.Generate(dir, gen)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 立即尝试装载/预检，演示异常链路定位。
	sess, loadErr := session.Load(dir)
	if loadErr == nil {
		eng, engErr := pipeline.NewEngine(sess, pipeline.DefaultConfig(m), nil)
		if engErr == nil {
			loadErr = eng.Run(context.Background(), pipeline.ModeFull, 1)
		}
		if engErr != nil || loadErr != nil {
			loadErr = first(engErr, loadErr)
		}
	}
	resp := map[string]any{"ok": true, "faults": faults, "dir": dir}
	if loadErr != nil {
		resp["detected"] = map[string]string{
			"code": session.CodeOf(loadErr), "message": loadErr.Error(),
		}
	}
	writeJSON(w, resp)
}

func first(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (s *Server) progressLocked() [2]int {
	if s.last == nil {
		return [2]int{0, 0}
	}
	return s.last.Progress
}
