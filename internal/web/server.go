// Package web 用标准库 net/http 提供本地诊断工作台，按逻辑 tick 逐场景演示。
// 不引入任何前端依赖或外部数据库；页面在服务端渲染。
package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"sort"
	"sync"

	"skillsched/internal/checkpoint"
	"skillsched/internal/depgraph"
	"skillsched/internal/engine"
	"skillsched/internal/gen"
	"skillsched/internal/model"
)

// Server 持有确定性场景与诊断结果。
type Server struct {
	mu         sync.Mutex
	scenario   *gen.Scenario
	wires      *gen.WireSet
	full       []model.TickTruth
	tree       []model.TickTruth
	concurrent *engine.ConcurrentResult
	graph      *depgraph.Graph
	store      *checkpoint.Store
	tmpl       *template.Template
}

// New 构造工作台：生成固定种子场景并预计算各路径结果。
func New(dbDir string) (*Server, error) {
	sc := gen.Generate("workbench", gen.DefaultConfig())
	wires, err := sc.ExportWires()
	if err != nil {
		return nil, err
	}
	full, err := engine.RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		return nil, err
	}
	tree, err := engine.RunFull(sc.Rules, sc.Intents, true)
	if err != nil {
		return nil, err
	}
	conc, err := engine.RunConcurrent(sc.Rules, sc.Intents, false, 2, 1)
	if err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(filepath.Join(dbDir, "workbench.db"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		scenario: sc, wires: wires, full: full, tree: tree,
		concurrent: conc, graph: depgraph.Build(sc.Rules), store: store,
	}
	s.tmpl = template.Must(template.New("page").Parse(pageHTML))
	return s, nil
}

// Close 释放检查点连接。
func (s *Server) Close() error { return s.store.Close() }

// Handler 注册路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleOverview)
	mux.HandleFunc("/ticks", s.handleTicks)
	mux.HandleFunc("/compare", s.handleCompare)
	mux.HandleFunc("/invalidate", s.handleInvalidate)
	mux.HandleFunc("/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("/api/truth", s.handleAPITruth)
	return mux
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := map[string]any{
		"Title":    "场景总览",
		"Scenario": s.scenario,
		"Pools":    s.scenario.Rules.Pools,
		"Skills":   s.scenario.Rules.Skills,
		"Groups":   s.scenario.Rules.Groups,
		"Intents":  len(s.scenario.Intents),
	}
	s.render(w, "overview", data)
}

func (s *Server) handleTicks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tick := 0
	fmt.Sscanf(r.URL.Query().Get("tick"), "%d", &tick)
	if tick < 0 {
		tick = 0
	}
	if tick >= len(s.full) {
		tick = len(s.full) - 1
	}
	data := map[string]any{
		"Title": "逐 Tick 诊断",
		"Tick":  s.full[tick],
		"Index": tick,
		"Total": len(s.full),
		"Prev":  tick - 1,
		"Next":  tick + 1,
	}
	if tick == 0 {
		data["Prev"] = 0
	}
	if tick == len(s.full)-1 {
		data["Next"] = tick
	}
	s.render(w, "ticks", data)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type diff struct {
		Name  string
		Tick  int
		Skill string
		Field string
		OK    bool
	}
	diffs := []diff{
		{Name: "全量(扁平) vs 全量(表达式树)"},
		{Name: "全量 vs 并发受限预算"},
	}
	if t, sk, f, ok := engine.Compare(s.full, s.tree); ok {
		diffs[0].Tick, diffs[0].Skill, diffs[0].Field, diffs[0].OK = t, sk, f, false
	} else {
		diffs[0].OK = true
	}
	if t, sk, f, ok := engine.Compare(s.full, s.concurrent.Ticks); ok {
		diffs[1].Tick, diffs[1].Skill, diffs[1].Field, diffs[1].OK = t, sk, f, false
	} else {
		diffs[1].OK = true
	}
	data := map[string]any{"Title": "三路径比对", "Diffs": diffs, "Queue": s.concurrent.Results}
	s.render(w, "compare", data)
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid := s.scenario.Rules.Skills[0].ID
	for tick := 0; tick < s.scenario.Rules.Params.Horizon; tick++ {
		for _, sk := range s.scenario.Rules.Skills {
			s.graph.Put(tick, sk.ID, model.SkillTick{SkillID: sk.ID})
		}
	}
	before := s.graph.CachedTicks()
	items := s.graph.Invalidate(depgraph.Edit{
		Kind: depgraph.EditCooldown, NodeID: "cdr:" + sid,
		TickFrom: 10, TickTo: 19, Reason: "workbench demo edit cooldown",
		NewContent: map[string]int64{"cooldown": 9},
	}, s.scenario.Rules.Params.Horizon)
	after := s.graph.CachedTicks()
	data := map[string]any{
		"Title": "失效传播", "Node": "cdr:" + sid,
		"Before": before, "After": after, "Items": items,
	}
	s.render(w, "invalidate", data)
}

func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 重新提交全部窗口，展示原子串行写入与窗口顺序。
	_ = s.store.Reset()
	hash := s.scenario.Rules.ContentHash()
	size := s.scenario.Rules.Params.WindowSize
	eng := engine.New(s.scenario.Rules, engine.InitialRuntime(s.scenario.Rules), false)
	type winInfo struct {
		Index, From, To int
		State           string
	}
	var wins []winInfo
	idx := 0
	for start := 0; start < s.scenario.Rules.Params.Horizon; start += size {
		chunk, err := eng.RunWindow(start, size, s.scenario.Intents)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rec := &checkpoint.WindowRecord{
			WindowIndex: idx, TickFrom: start, TickTo: start + len(chunk) - 1,
			WindowState: engine.WinCommitted, ContentHash: hash,
			SourceVersion: string(s.scenario.Rules.SourceVersion),
			AlgorithmVer:  checkpoint.AlgorithmVersion,
			ParamVersion:  s.scenario.Rules.Params.ParamVersion,
			Runtime:       eng.Runtime(), Truth: chunk,
		}
		if err := s.store.CommitWindow(rec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		wins = append(wins, winInfo{Index: idx, From: start, To: rec.TickTo, State: rec.WindowState})
		idx++
	}
	recs, err := s.store.LoadAll(hash, s.scenario.Rules.SourceVersion, s.scenario.Rules.Params.ParamVersion)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title": "检查点", "Windows": wins, "Loaded": len(recs),
		"Algo": checkpoint.AlgorithmVersion, "Hash": hash,
	}
	s.render(w, "checkpoint", data)
}

func (s *Server) handleAPITruth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.scenario.Truth)
}

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	data["Page"] = page
	// 保证 map 型池余量在模板中稳定呈现。
	if tk, ok := data["Tick"].(model.TickTruth); ok {
		keys := make([]string, 0, len(tk.Pools))
		for k := range tk.Pools {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		data["PoolKeys"] = keys
	}
	if err := s.tmpl.ExecuteTemplate(w, page, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
