// Package panel 提供不依赖桌面 WebView 的本地可视化面板：
// net/http 提供与 Wails 应用同构的 JSON API 与波形/误差曲线页面，
// 支持暂停、重试与导出；参数调整只作用于未提交帧。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"

	"spatialir/internal/checkpoint"
	"spatialir/internal/generator"
	"spatialir/internal/loader"
	"spatialir/internal/model"
	"spatialir/internal/pipeline"
)

type session struct {
	mu       sync.Mutex
	dir      string
	engines  map[string]*pipeline.Engine
	stores   map[string]*checkpoint.Store
	datasets map[string]*loader.Dataset
	params   map[string]model.FrameParams
}

// Serve 启动面板 HTTP 服务。
func Serve(dir, addr string) error {
	s := &session{
		dir:      dir,
		engines:  map[string]*pipeline.Engine{},
		stores:   map[string]*checkpoint.Store{},
		datasets: map[string]*loader.Dataset{},
		params:   map[string]model.FrameParams{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/", handleIndex)
	fmt.Printf("panel listening on http://%s (dir=%s)\n", addr, dir)
	return http.ListenAndServe(addr, mux)
}

func defaultParams(length int) model.FrameParams {
	return model.FrameParams{
		FrameLength: min(1024, length), FrameHop: min(512, length/2), WindowLen: min(768, length),
		ConvWindow: 64, RenderBlock: 512, InterpOrder: 1, BoundScale: 1,
		MaxWorkers: 4, BufferCap: 4096,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *session) ensure(sample string) (*loader.Dataset, *pipeline.Engine, *checkpoint.Store, model.FrameParams, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds, ok := s.datasets[sample]; ok {
		return ds, s.engines[sample], s.stores[sample], s.params[sample], nil
	}
	ds, err := loader.Load(filepath.Join(s.dir, sample), sample+"_manifest.json")
	if err != nil {
		return nil, nil, nil, model.FrameParams{}, err
	}
	params := defaultParams(ds.Manifest.LengthSamples)
	if err := loader.ValidateParams(params, ds.Manifest.LengthSamples); err != nil {
		return nil, nil, nil, params, err
	}
	store, err := checkpoint.Open(filepath.Join(s.dir, sample, sample+"_panel.db"))
	if err != nil {
		return nil, nil, nil, params, err
	}
	eng := pipeline.NewEngine(ds, params)
	s.datasets[sample] = ds
	s.engines[sample] = eng
	s.stores[sample] = store
	s.params[sample] = params
	return ds, eng, store, params, nil
}

func (s *session) handleSamples(w http.ResponseWriter, r *http.Request) {
	entries, err := listSamples(s.dir)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSONResp(w, entries)
}

type runRequest struct {
	Sample string             `json:"sample"`
	Mode   pipeline.RunMode   `json:"mode"`
	Pause  int                `json:"pause"`
	Params *model.FrameParams `json:"params,omitempty"`
}

func (s *session) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	ds, eng, store, params, err := s.ensure(req.Sample)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.Params != nil {
		// 参数调整只允许作用于未提交帧：已提交帧冻结。
		// 帧划分变化会改变总帧数，此时重建引擎；检查点中的已提交帧仍受
		// 快照元数据保护，不会被静默改写。
		if err := loader.ValidateParams(*req.Params, ds.Manifest.LengthSamples); err != nil {
			writeErr(w, err)
			return
		}
		params = *req.Params
		s.mu.Lock()
		s.params[req.Sample] = params
		if len(eng.Frames()) != len(pipeline.NewEngine(ds, params).Frames()) {
			s.engines[req.Sample] = pipeline.NewEngine(ds, params)
			eng = s.engines[req.Sample]
		}
		s.mu.Unlock()
	}
	res, err := eng.Run(r.Context(), pipeline.Options{
		Dataset: ds, Params: params, Store: store, Mode: req.Mode, PauseAt: req.Pause,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSONResp(w, res)
}

func (s *session) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sample string `json:"sample"`
		Frames []int  `json:"frames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	_, eng, _, _, err := s.ensure(req.Sample)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := eng.RetryFailed(req.Frames...); err != nil {
		writeErr(w, err)
		return
	}
	writeJSONResp(w, map[string]string{"status": "retried"})
}

func (s *session) handleExport(w http.ResponseWriter, r *http.Request) {
	sample := r.URL.Query().Get("sample")
	_, _, _, _, err := s.ensure(sample)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+sample+"_report.json")
	w.Header().Set("Content-Type", "application/json")
	store := s.stores[sample]
	snaps, err := store.LoadFrames()
	if err != nil {
		writeErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(snaps)
}

func listSamples(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*_manifest.json"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		manifest, err := generator.ReadManifest(m)
		if err != nil {
			continue
		}
		_ = manifest
		out = append(out, filepath.Base(filepath.Dir(m)))
	}
	return out, nil
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	if ae, ok := err.(*model.AppError); ok {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ae)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

var _ = context.Background
