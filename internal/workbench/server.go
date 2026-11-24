package workbench

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/incremental"
	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/pool"
	"github.com/gameaudio/workbench/internal/storage"
)

// Server 持有可变工程与编排器，所有编辑由互斥锁串行化。
type Server struct {
	mu       sync.Mutex
	project  *model.Project
	cfg      model.GeoConfig
	poolCfg  pool.Config
	dbPath   string
	store    *storage.Store
	eng      *engine.Engine
	orch     *incremental.Orchestrator
	results  []model.Result
	profiles []model.Profile
	invs     []model.Invalidation
}

// NewServer 构造工作台服务并完成首次全量计算。
func NewServer(p *model.Project, cfg model.GeoConfig, poolCfg pool.Config, dbPath string,
) (*Server, error) {
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Server{project: p, cfg: cfg, poolCfg: poolCfg, dbPath: dbPath, store: store}
	if err := s.rebuildEngine(); err != nil {
		return nil, err
	}
	if err := s.orch.SaveSnapshotMeta(); err != nil {
		return nil, err
	}
	if err := s.recompute(false); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) rebuildEngine() error {
	eng, err := engine.New(s.project, s.cfg)
	if err != nil {
		return err
	}
	s.eng = eng
	s.orch = incremental.New(s.project, eng, s.store, s.cfg, s.poolCfg)
	return nil
}

func (s *Server) recompute(full bool) error {
	var results []model.Result
	var profiles []model.Profile
	var err error
	if full {
		results, profiles, err = s.orch.FullRecompute()
	} else {
		results, profiles, err = s.orch.Incremental()
	}
	if err != nil {
		return err
	}
	s.results = results
	s.profiles = profiles
	return nil
}

// Routes 注册 HTTP 路由。
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/project", s.handleProject)
	mux.HandleFunc("/api/results", s.handleResults)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/invalidations", s.handleInvalidations)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/edit/region", s.handleEditRegion)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/full", s.handleFull)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.project)
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.results)
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.profiles)
}

func (s *Server) handleInvalidations(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.invs)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hits, misses, bp := s.orch.CacheStats()
	writeJSON(w, map[string]any{
		"cacheHits": hits, "cacheMisses": misses,
		"backpressure": bp, "storage": s.store.Describe(),
	})
}

// editRegionRequest 是区域编辑请求。
type editRegionRequest struct {
	RegionID   string        `json:"regionId"`
	ReverbTime *float64      `json:"reverbTime,omitempty"`
	ERDensity  *float64      `json:"erDensity,omitempty"`
	Gain       *float64      `json:"gain,omitempty"`
	Vertices   []model.Point `json:"vertices,omitempty"`
}

func (s *Server) handleEditRegion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req editRegionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i := range s.project.Regions {
		if s.project.Regions[i].ID == req.RegionID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, map[string]string{"error": "E_REF", "detail": "区域不存在: " + req.RegionID})
		return
	}

	prevCache, prevHash := s.orch.ExportCache()
	geometryChanged := len(req.Vertices) > 0
	if req.ReverbTime != nil {
		s.project.Regions[idx].ReverbTime = *req.ReverbTime
	}
	if req.ERDensity != nil {
		s.project.Regions[idx].ERDensity = *req.ERDensity
	}
	if req.Gain != nil {
		s.project.Regions[idx].Gain = *req.Gain
	}
	if geometryChanged {
		s.project.Regions[idx].Vertices = req.Vertices
		s.project.Regions[idx].GeomVersion++
	}
	if req.ReverbTime != nil || req.ERDensity != nil || req.Gain != nil {
		s.project.Regions[idx].ParamVersion++
	}

	reason := "param edited"
	if geometryChanged {
		reason = "boundary dragged"
	}
	invalidations := s.orch.InvalidateRegion(req.RegionID, reason)
	s.invs = invalidations
	if err := s.rebuildEngine(); err != nil {
		writeJSON(w, map[string]string{"error": "E_DEGEN", "detail": err.Error()})
		return
	}
	s.orch.SeedCache(prevCache, prevHash)
	if err := s.recompute(false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"invalidations": invalidations,
		"results":       s.results,
		"profiles":      s.profiles,
	})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SourceID   string `json:"sourceId"`
		ListenerID string `json:"listenerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 单对重试，不触碰其他缓存。
	rr := s.orch.RetryPair(req.SourceID, req.ListenerID)
	for i := range s.results {
		if s.results[i].SourceID == req.SourceID && s.results[i].ListenerID == req.ListenerID {
			s.results[i] = rr
		}
	}
	writeJSON(w, rr)
}

func (s *Server) handleFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recompute(true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"results": s.results, "profiles": s.profiles})
}

// Close 释放底层存储。
func (s *Server) Close() error { return s.store.Close() }
