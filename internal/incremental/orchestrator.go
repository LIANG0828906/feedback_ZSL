package incremental

import (
	"encoding/json"
	"sort"

	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/hash"
	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/pool"
	"github.com/gameaudio/workbench/internal/profile"
	"github.com/gameaudio/workbench/internal/storage"
)

// WindowWriter 抽象串行 SQLite 写入，便于测试替换。
type WindowWriter interface {
	CommitWindow(rec storage.WindowRecord, results []model.Result) error
}

// Orchestrator 管理缓存、依赖失效与三条计算路径。
type Orchestrator struct {
	project *model.Project
	eng     *engine.Engine
	store   *storage.Store
	cfg     model.GeoConfig
	poolCfg pool.Config

	// 内存缓存：pairKey -> 结果与输入哈希。
	cache     map[string]model.Result
	inputHash map[string]string

	// 依赖关系：区域 -> 声源/听者 -> 声源听者对；墙 -> 对。
	pairRegion map[string]string

	lastInvalidations []model.Invalidation
	cacheHits         int
	cacheMiss         int
	backpressure      []pool.BackpressureEvent
}

// New 构造编排器。
func New(p *model.Project, eng *engine.Engine, store *storage.Store,
	cfg model.GeoConfig, poolCfg pool.Config) *Orchestrator {
	o := &Orchestrator{
		project:    p,
		eng:        eng,
		store:      store,
		cfg:        cfg,
		poolCfg:    poolCfg,
		cache:      map[string]model.Result{},
		inputHash:  map[string]string{},
		pairRegion: map[string]string{},
	}
	o.buildDependencies()
	return o
}

// buildDependencies 显式记录区域→声源/听者→对、墙→对的依赖图。
func (o *Orchestrator) buildDependencies() {
	// 对的区域归属取声源所在区域（与声学合成口径一致）。
	for _, s := range o.project.Sources {
		for _, l := range o.project.Listeners {
			key := engine.PairKey(s.ID, l.ID)
			o.pairRegion[key] = s.RegionID
		}
	}
}

// pairInputHash 计算单对当前输入的完整哈希。
func (o *Orchestrator) pairInputHash(sourceID, listenerID string,
	s model.Source, l model.Listener, region model.Region) string {
	return hash.PairHash(hash.PairInput{
		Source:      s,
		Listener:    l,
		Region:      region,
		Walls:       o.project.Walls,
		ScaleBits:   o.cfg.ScaleBits,
		AlgoVersion: engine.AlgorithmVersion,
	})
}

// FullRecompute 从空缓存全量重算全部对，按窗口提交检查点。
func (o *Orchestrator) FullRecompute() ([]model.Result, []model.Profile, error) {
	o.cache = map[string]model.Result{}
	o.inputHash = map[string]string{}
	o.cacheHits, o.cacheMiss = 0, 0
	return o.runAll(true)
}

// Incremental 复用未失效缓存，仅重算受影响对。
func (o *Orchestrator) Incremental() ([]model.Result, []model.Profile, error) {
	return o.runAll(false)
}

// runAll 统一驱动两条路径；full=true 时忽略缓存强制重算。
type pairTask struct {
	key      string
	sourceID string
	listenID string
	region   model.Region
	inHash   string
	useCache bool
}

func (o *Orchestrator) runAll(full bool) ([]model.Result, []model.Profile, error) {
	sourceByID := map[string]model.Source{}
	listenerByID := map[string]model.Listener{}
	regionByID := map[string]model.Region{}
	for _, s := range o.project.Sources {
		sourceByID[s.ID] = s
	}
	for _, l := range o.project.Listeners {
		listenerByID[l.ID] = l
	}
	for _, r := range o.project.Regions {
		regionByID[r.ID] = r
	}

	// 稳定顺序：先声源 ID 后听者 ID，保证窗口顺序可复现。
	var tasks []pairTask
	for _, s := range o.project.Sources {
		for _, l := range o.project.Listeners {
			key := engine.PairKey(s.ID, l.ID)
			region := regionByID[s.RegionID]
			inHash := o.pairInputHash(s.ID, l.ID, s, l, region)
			useCache := false
			if !full {
				if cached, ok := o.cache[key]; ok && o.inputHash[key] == inHash && !cached.Failed {
					useCache = true
				} else if o.store != nil {
					if persisted, hit, _ := o.store.LoadPair(key, inHash); hit && !persisted.Failed {
						o.cache[key] = persisted
						o.inputHash[key] = inHash
						useCache = true
					}
				}
			}
			tasks = append(tasks, pairTask{
				key: key, sourceID: s.ID, listenID: l.ID,
				region: region, inHash: inHash, useCache: useCache,
			})
		}
	}

	// 分区：缓存命中 vs 需计算。
	var results []model.Result
	var computeJobs []pool.Job
	computeTask := map[string]pairTask{}
	for _, t := range tasks {
		if t.useCache {
			cached := o.cache[t.key]
			cached.CacheHit = true
			results = append(results, cached)
			o.cacheHits++
			continue
		}
		o.cacheMiss++
		computeJobs = append(computeJobs, pool.Job{SourceID: t.sourceID, ListenerID: t.listenID})
		computeTask[t.key] = t
	}

	// 有界线程池并行计算未命中对。
	if len(computeJobs) > 0 {
		pl := pool.New(o.poolCfg, o.eng.ComputePair)
		done := make(chan struct{})
		go func() {
			for _, j := range computeJobs {
				pl.Submit(j)
			}
			pl.CloseAndWait()
			close(done)
		}()
		var fresh []model.Result
		for r := range pl.Result() {
			r.CacheHit = false
			fresh = append(fresh, r)
		}
		<-done
		o.backpressure = pl.Backpressure()

		// 写内存缓存并按窗口提交检查点。
		for _, r := range fresh {
			key := engine.PairKey(r.SourceID, r.ListenerID)
			t := computeTask[key]
			o.cache[key] = r
			o.inputHash[key] = t.inHash
			results = append(results, r)
		}
		if err := o.commitWindows(fresh, computeTask); err != nil {
			return nil, nil, err
		}
	}

	// 稳定排序结果，保证三条路径输出顺序一致。
	sort.Slice(results, func(i, j int) bool {
		if results[i].SourceID != results[j].SourceID {
			return results[i].SourceID < results[j].SourceID
		}
		return results[i].ListenerID < results[j].ListenerID
	})
	profiles := profile.Build(o.project, results)
	return results, profiles, nil
}

// commitWindows 按听者窗口聚合新结果并串行原子写入。
func (o *Orchestrator) commitWindows(fresh []model.Result, tasks map[string]pairTask) error {
	if o.store == nil {
		return nil
	}
	windowByListener := map[string]int{}
	for _, l := range o.project.Listeners {
		windowByListener[l.ID] = l.WindowID
	}
	groups := map[int][]model.Result{}
	hashes := map[int]map[string]bool{}
	for _, r := range fresh {
		w := windowByListener[r.ListenerID]
		groups[w] = append(groups[w], r)
		if hashes[w] == nil {
			hashes[w] = map[string]bool{}
		}
		hashes[w][tasks[engine.PairKey(r.SourceID, r.ListenerID)].inHash] = true
	}
	windowIDs := make([]int, 0, len(groups))
	for w := range groups {
		windowIDs = append(windowIDs, w)
	}
	sort.Ints(windowIDs)
	for _, w := range windowIDs {
		rs := groups[w]
		sort.Slice(rs, func(i, j int) bool {
			return engine.PairKey(rs[i].SourceID, rs[i].ListenerID) <
				engine.PairKey(rs[j].SourceID, rs[j].ListenerID)
		})
		pairKeys := make([]string, 0, len(rs))
		hashSet := make([]string, 0, len(hashes[w]))
		for k := range hashes[w] {
			hashSet = append(hashSet, k)
		}
		sort.Strings(hashSet)
		for _, r := range rs {
			pairKeys = append(pairKeys, engine.PairKey(r.SourceID, r.ListenerID))
		}
		summary, _ := json.Marshal(summarize(rs))
		profStat, _ := json.Marshal(profile.Build(o.project, rs))
		rec := storage.WindowRecord{
			WindowID:    w,
			PairKeys:    pairKeys,
			InputHash:   hash.Content(hashSet),
			Summary:     string(summary),
			ProfileStat: string(profStat),
		}
		if err := o.store.CommitWindow(rec, rs); err != nil {
			return err
		}
	}
	return nil
}

// InvalidateRegion 实现区域重划或参数编辑的精确失效传播。
// reason 描述编辑类型；只失效引用该区域的声源听者对及其下游剖面。
func (o *Orchestrator) InvalidateRegion(regionID, reason string) []model.Invalidation {
	inv := []model.Invalidation{}
	for key, region := range o.pairRegion {
		if region == regionID {
			inv = append(inv, model.Invalidation{
				ObjectID: key, Kind: "pair",
				Reason: "region " + regionID + " " + reason,
			})
			delete(o.cache, key)
			delete(o.inputHash, key)
		}
	}
	// 下游听者剖面失效（剖面由对派生，随对一起标记）。
	listenerSeen := map[string]bool{}
	for _, in := range inv {
		lid := listenerOf(in.ObjectID)
		if !listenerSeen[lid] {
			listenerSeen[lid] = true
			inv = append(inv, model.Invalidation{
				ObjectID: lid, Kind: "profile",
				Reason: "downstream of region " + regionID + " " + reason,
			})
		}
	}
	sort.Slice(inv, func(i, j int) bool {
		if inv[i].ObjectID != inv[j].ObjectID {
			return inv[i].ObjectID < inv[j].ObjectID
		}
		return inv[i].Kind < inv[j].Kind
	})
	o.lastInvalidations = inv
	return inv
}

// InvalidateWalls 在墙体集合变化时失效全部对（几何输入全局共享），
// 但仍仅作用于缓存项，不重建工程结构。
func (o *Orchestrator) InvalidateWalls(reason string) []model.Invalidation {
	inv := []model.Invalidation{}
	for key := range o.cache {
		inv = append(inv, model.Invalidation{
			ObjectID: key, Kind: "pair",
			Reason: "wall-set changed: " + reason,
		})
	}
	o.cache = map[string]model.Result{}
	o.inputHash = map[string]string{}
	sort.Slice(inv, func(i, j int) bool { return inv[i].ObjectID < inv[j].ObjectID })
	o.lastInvalidations = inv
	return inv
}

// RetryPair 清空单对失败缓存并立即重算，不影响其他对。
func (o *Orchestrator) RetryPair(sourceID, listenerID string) model.Result {
	key := engine.PairKey(sourceID, listenerID)
	delete(o.cache, key)
	delete(o.inputHash, key)
	r := o.eng.ComputePair(sourceID, listenerID)
	o.cache[key] = r
	return r
}

// CacheStats 返回命中/未命中与背压事件统计。
func (o *Orchestrator) CacheStats() (hits, misses int, bp []pool.BackpressureEvent) {
	return o.cacheHits, o.cacheMiss, o.backpressure
}

// LastInvalidations 返回最近一次失效清单。
func (o *Orchestrator) LastInvalidations() []model.Invalidation {
	return o.lastInvalidations
}

// ExportCache 导出内存缓存（编辑后切换编排器时迁移命中项）。
func (o *Orchestrator) ExportCache() (map[string]model.Result, map[string]string) {
	results := make(map[string]model.Result, len(o.cache))
	hashes := make(map[string]string, len(o.inputHash))
	for k, v := range o.cache {
		results[k] = v
	}
	for k, v := range o.inputHash {
		hashes[k] = v
	}
	return results, hashes
}

// SeedCache 导入既有缓存。
func (o *Orchestrator) SeedCache(results map[string]model.Result, hashes map[string]string) {
	for k, v := range results {
		o.cache[k] = v
	}
	for k, v := range hashes {
		o.inputHash[k] = v
	}
}

func listenerOf(pairKey string) string {
	for i := len(pairKey) - 1; i >= 0; i-- {
		if pairKey[i] == '>' {
			return pairKey[i+1:]
		}
	}
	return pairKey
}

// summarize 计算窗口内曲线采样摘要（每采样点均值）。
func summarize(rs []model.Result) map[string]any {
	if len(rs) == 0 {
		return map[string]any{"pairs": 0}
	}
	n := len(rs[0].Curve)
	mean := make([]float64, n)
	for _, r := range rs {
		for k := 0; k < n; k++ {
			mean[k] += r.Curve[k]
		}
	}
	for k := range mean {
		mean[k] /= float64(len(rs))
	}
	return map[string]any{"pairs": len(rs), "curveMean": mean}
}
