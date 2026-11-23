package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sync/atomic"
	"time"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

type Performance struct {
	Mode              Mode    `json:"mode"`
	ElapsedMS         float64 `json:"elapsed_ms"`
	PeakHeapBytes     uint64  `json:"peak_heap_bytes"`
	TotalAllocBytes   uint64  `json:"total_alloc_bytes"`
	MaxParallel       int     `json:"max_parallel"`
	WorkerBufferBytes int64   `json:"worker_buffer_bytes"`
}

type RunResult struct {
	Mesh          mesh.Mesh     `json:"-"`
	Decision      Decision      `json:"decision"`
	Stats         mesh.Stats    `json:"stats"`
	StageOrder    []StageName   `json:"stage_order"`
	StageHashes   []string      `json:"stage_hashes"`
	StageStats    []mesh.Stats  `json:"stage_stats"`
	FillArea      float64       `json:"fill_area"`
	SnapshotStats []StageRecord `json:"-"`
	Performance   Performance   `json:"performance"`
}

type QualityItem struct {
	Item      string  `json:"item"`
	Fine      float64 `json:"fine"`
	Spatial   float64 `json:"spatial"`
	Delta     float64 `json:"delta"`
	Tolerance float64 `json:"tolerance"`
	Passed    bool    `json:"passed"`
}

type ScaleSnapshot struct {
	Vertices int
	Faces    int
	Euler    int
	FillArea float64
	AreaTol  float64
}

type ConsistencyReport struct {
	InputHash          string        `json:"input_hash"`
	QualityFingerprint string        `json:"quality_fingerprint"`
	Passed             bool          `json:"passed"`
	Items              []QualityItem `json:"items"`
	Fine               Performance   `json:"fine"`
	Spatial            Performance   `json:"spatial"`
	FineDecision       Decision      `json:"fine_decision"`
	SpatialDecision    Decision      `json:"spatial_decision"`
}

type DeterminismReport struct {
	InputHash      string      `json:"input_hash"`
	Equal          bool        `json:"equal"`
	SerialParallel bool        `json:"serial_parallel_equal"`
	SerialHash     string      `json:"serial_hash"`
	ParallelHash   string      `json:"parallel_hash"`
	SerialStages   []string    `json:"serial_stage_hashes"`
	ParallelStages []string    `json:"parallel_stage_hashes"`
	CommitSequence []StageName `json:"commit_sequence"`
	Serial         Performance `json:"serial"`
	Parallel       Performance `json:"parallel"`
}

func RunAll(sample generator.Sample, cfg Config) (*RunResult, *errs.CodedError) {
	cfg = cfg.Normalized()
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	decision, de := Decide(sample.Mesh, cfg)
	if de != nil {
		return nil, de
	}
	var result RunResult
	var runErr *errs.CodedError
	perf := measureRun(func() {
		back := sample.Mesh
		for i, stage := range StageOrder {
			out, err := computeStage(i, back, sample, cfg, decision)
			if err != nil {
				ce, ok := errs.As(err)
				if !ok {
					ce = errs.New(errs.CodeGeom, "%v", err)
				}
				ce.Stage = string(stage)
				runErr = ce
				return
			}
			back = out.Mesh
			result.StageOrder = append(result.StageOrder, stage)
			result.StageHashes = append(result.StageHashes, out.Mesh.ContentHash())
			result.StageStats = append(result.StageStats, out.Stats)
			result.SnapshotStats = append(result.SnapshotStats, StageRecord{
				Index: i, Name: stage, Status: StatusCommitted,
				Stats: &out.Stats, Details: out.Details,
			})
			if stage == StageFill {
				if rep, ok := out.Details["fill"].(FillReport); ok {
					result.FillArea = rep.AddedArea
				}
			}
			if stage == StageStats {
				result.Mesh = back
				result.Stats = out.Stats
			}
		}
	})
	if runErr != nil {
		return nil, runErr
	}
	perf.Mode = decision.Selected
	perf.MaxParallel = cfg.MaxParallel
	perf.WorkerBufferBytes = int64(cfg.MaxParallel) * cfg.PerWorkerBufferMB * 1024 * 1024
	result.Decision = decision
	result.Performance = perf
	return &result, nil
}

func measureRun(fn func()) Performance {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapAlloc
	done := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				for {
					old := atomic.LoadUint64(&peak)
					if ms.HeapAlloc <= old || atomic.CompareAndSwapUint64(&peak, old, ms.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	close(done)
	<-samplerDone
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > atomic.LoadUint64(&peak) {
		atomic.StoreUint64(&peak, after.HeapAlloc)
	}
	return Performance{
		ElapsedMS:       float64(elapsed.Microseconds()) / 1000,
		PeakHeapBytes:   peak,
		TotalAllocBytes: after.TotalAlloc - before.TotalAlloc,
	}
}

func RunConsistency(sample generator.Sample, cfg Config) (*ConsistencyReport, *errs.CodedError) {
	fineCfg := cfg.Normalized()
	fineCfg.Mode = ModeExact
	fine, ferr := RunAll(sample, fineCfg)
	if ferr != nil {
		return nil, ferr
	}
	spatialCfg := cfg.Normalized()
	spatialCfg.Mode = ModeSpatial
	spatial, serr := RunAll(sample, spatialCfg)
	if serr != nil {
		return nil, serr
	}
	items := CompareScaleQuality(
		ScaleSnapshot{
			Vertices: fine.Stats.Vertices, Faces: fine.Stats.Faces,
			Euler: fine.Stats.EulerCharacteristic, FillArea: fine.FillArea,
		},
		ScaleSnapshot{
			Vertices: spatial.Stats.Vertices, Faces: spatial.Stats.Faces,
			Euler: spatial.Stats.EulerCharacteristic, FillArea: spatial.FillArea,
		},
	)
	report := &ConsistencyReport{
		InputHash:          sample.Mesh.ContentHash(),
		Items:              items,
		Fine:               fine.Performance,
		Spatial:            spatial.Performance,
		FineDecision:       fine.Decision,
		SpatialDecision:    spatial.Decision,
		Passed:             true,
		QualityFingerprint: qualityFingerprint(items),
	}
	_, scaleErr := failedScaleItems(items)
	report.Passed = scaleErr == nil
	if !report.Passed {
		return report, scaleErr
	}
	return report, nil
}

func CompareScaleQuality(fine, spatial ScaleSnapshot) []QualityItem {
	areaTol := fine.AreaTol
	if areaTol <= 0 {
		areaTol = math.Max(1e-9, math.Abs(fine.FillArea)*1e-6)
	}
	return []QualityItem{
		compareQuality("vertices", float64(fine.Vertices), float64(spatial.Vertices), 0),
		compareQuality("faces", float64(fine.Faces), float64(spatial.Faces), 0),
		compareQuality("euler_characteristic", float64(fine.Euler), float64(spatial.Euler), 0),
		compareQuality("hole_fill_area", fine.FillArea, spatial.FillArea, areaTol),
	}
}

func ValidateScaleQuality(fine, spatial ScaleSnapshot) ([]QualityItem, *errs.CodedError) {
	items := CompareScaleQuality(fine, spatial)
	failed, e := failedScaleItems(items)
	if e != nil {
		e.Details = failed
	}
	return items, e
}

func failedScaleItems(items []QualityItem) ([]QualityItem, *errs.CodedError) {
	var failed []QualityItem
	for _, it := range items {
		if !it.Passed {
			failed = append(failed, it)
		}
	}
	if len(failed) == 0 {
		return nil, nil
	}
	first := failed[0]
	e := errs.New(errs.CodeScale,
		"scale quality mismatch in %s: fine=%g spatial=%g tolerance=%g",
		first.Item, first.Fine, first.Spatial, first.Tolerance)
	e.Item = first.Item
	e.Left = first.Fine
	e.Right = first.Spatial
	e.Tolerance = first.Tolerance
	e.Details = failed
	return failed, e
}

func compareQuality(item string, fine, spatial, tol float64) QualityItem {
	return QualityItem{
		Item: item, Fine: fine, Spatial: spatial,
		Delta: math.Abs(fine - spatial), Tolerance: tol,
		Passed: math.Abs(fine-spatial) <= tol,
	}
}

func qualityFingerprint(items []QualityItem) string {
	type canonical struct {
		Item    string  `json:"item"`
		Fine    float64 `json:"fine"`
		Spatial float64 `json:"spatial"`
	}
	c := make([]canonical, len(items))
	for i, it := range items {
		c[i] = canonical{Item: it.Item, Fine: it.Fine, Spatial: it.Spatial}
	}
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func RunDeterminism(sample generator.Sample, cfg Config) (*DeterminismReport, *errs.CodedError) {
	serialCfg := cfg.Normalized()
	serialCfg.Mode = ModeSpatial
	serialCfg.MaxParallel = 1
	serial, e := RunAll(sample, serialCfg)
	if e != nil {
		return nil, e
	}
	parallelCfg := cfg.Normalized()
	parallelCfg.Mode = ModeSpatial
	if parallelCfg.MaxParallel < 2 {
		parallelCfg.MaxParallel = 2
	}
	parallel, e := RunAll(sample, parallelCfg)
	if e != nil {
		return nil, e
	}
	report := &DeterminismReport{
		InputHash:      sample.Mesh.ContentHash(),
		SerialHash:     serial.Mesh.ContentHash(),
		ParallelHash:   parallel.Mesh.ContentHash(),
		SerialStages:   serial.StageHashes,
		ParallelStages: parallel.StageHashes,
		CommitSequence: append([]StageName(nil), StageOrder...),
		Serial:         serial.Performance,
		Parallel:       parallel.Performance,
	}
	report.Equal = report.SerialHash == report.ParallelHash &&
		stringSlicesEqual(report.SerialStages, report.ParallelStages) &&
		serial.Stats.TopologyEqual(parallel.Stats)
	report.SerialParallel = report.Equal
	if !report.Equal {
		return report, errs.New(errs.CodeState,
			"serial and parallel runs produced different topology or stage hashes")
	}
	return report, nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r ConsistencyReport) Summary() string {
	return fmt.Sprintf("consistent=%t fingerprint=%s fine=%.1fms/%.1fMiB spatial=%.1fms/%.1fMiB",
		r.Passed, r.QualityFingerprint, r.Fine.ElapsedMS,
		float64(r.Fine.PeakHeapBytes)/1024/1024, r.Spatial.ElapsedMS,
		float64(r.Spatial.PeakHeapBytes)/1024/1024)
}
