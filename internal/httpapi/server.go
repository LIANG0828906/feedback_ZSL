// Package httpapi 提供基于 net/http 的本地诊断工作台页面与 JSON API。
package httpapi

import (
	"encoding/json"
	"net/http"

	"occlbench/internal/core"
	"occlbench/internal/streaming"
	"occlbench/internal/workbench"
)

// Server 诊断 HTTP 服务。
type Server struct {
	app *workbench.App
	mux *http.ServeMux
}

// New 构造路由齐全的诊断服务。
func New(app *workbench.App) *Server {
	s := &Server{app: app, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("/api/chunks", s.handleChunks)
	s.mux.HandleFunc("/api/preview", s.handlePreview)
	s.mux.HandleFunc("/api/precise", s.handlePrecise)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
	s.mux.HandleFunc("/api/budget", s.handleBudget)
	s.mux.HandleFunc("/api/quant", s.handleQuant)
	s.mux.HandleFunc("/api/retry", s.handleRetry)
	s.mux.HandleFunc("/api/export", s.handleExport)
}

// Handler 返回根处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	code := core.CodeOf(err)
	status := http.StatusBadRequest
	if code == core.EIO {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"error": code, "detail": err.Error()})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.Snapshot())
}

// chunkView 分块着色与状态展示数据。
type chunkView struct {
	ID        int     `json:"id"`
	Partition int     `json:"partition"`
	Color     string  `json:"color"`
	MinX      float64 `json:"minX"`
	MinZ      float64 `json:"minZ"`
	MaxX      float64 `json:"maxX"`
	MaxZ      float64 `json:"maxZ"`
	LoadCost  float64 `json:"loadCost"`
	Memory    float64 `json:"memory"`
}

func (s *Server) handleChunks(w http.ResponseWriter, r *http.Request) {
	world := s.app.World()
	views := make([]chunkView, 0)
	for _, ch := range world.AllChunks() {
		views = append(views, chunkView{
			ID: ch.ID, Partition: ch.Partition, Color: ch.Color,
			MinX: ch.Box.Min.X, MinZ: ch.Box.Min.Z,
			MaxX: ch.Box.Max.X, MaxZ: ch.Box.Max.Z,
			LoadCost: ch.LoadCost, Memory: ch.MemoryCost,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chunks":     views,
		"occluders":  world.Occluders,
		"trajectory": world.Trajectory.Samples,
	})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	res, meta, err := s.app.RunPreview()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "meta": meta})
}

func (s *Server) handlePrecise(w http.ResponseWriter, r *http.Request) {
	res, err := s.app.RunPrecise(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	paths, diverge, _, err := s.app.Verify(r.Context())
	resp := map[string]any{"ok": err == nil, "paths": pathNames(paths)}
	if diverge != nil {
		resp["divergence"] = diverge
	}
	if err != nil {
		resp["error"] = core.CodeOf(err)
		resp["detail"] = err.Error()
		writeJSON(w, http.StatusConflict, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func pathNames(paths map[workbench.PathName][]*streaming.Window) []workbench.PathName {
	names := make([]workbench.PathName, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	return names
}

func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slots  int     `json:"slots"`
		Memory float64 `json:"memory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, core.NewError(core.ESchema, "bad json body", map[string]any{"err": err.Error()}))
		return
	}
	if err := s.app.UpdateBudget(req.Slots, req.Memory); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.app.Snapshot())
}

func (s *Server) handleQuant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GridResolution int `json:"gridResolution"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, core.NewError(core.ESchema, "bad json body", map[string]any{"err": err.Error()}))
		return
	}
	if err := s.app.UpdateQuantResolution(req.GridResolution); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.app.Snapshot())
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Window int `json:"window"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, core.NewError(core.ESchema, "bad json body", map[string]any{"err": err.Error()}))
		return
	}
	if err := s.app.RetryWindow(req.Window); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retried": req.Window})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	snap := s.app.Snapshot()
	w.Header().Set("Content-Disposition", `attachment; filename="diagnostic-export.json"`)
	writeJSON(w, http.StatusOK, snap)
}
