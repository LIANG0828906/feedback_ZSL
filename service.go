package maskqa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type RunOptions struct {
	Mode              string `json:"mode"`
	StopAfterCommits  int    `json:"stop_after_commits,omitempty"`
	InjectFailureNode string `json:"inject_failure_node,omitempty"`
	IncludePixels     bool   `json:"include_pixels,omitempty"`
}

type CommitRef struct {
	Seq          int64  `json:"seq"`
	SampleID     string `json:"sample_id"`
	BlockX       int    `json:"block_x"`
	BlockY       int    `json:"block_y"`
	BlockID      string `json:"block_id"`
	Stage        Stage  `json:"stage"`
	CacheKey     string `json:"cache_key"`
	ContentHash  string `json:"content_hash"`
	ParamVersion string `json:"param_version"`
}

type SampleRunReport struct {
	SampleID          string       `json:"sample_id"`
	BlockCount        int          `json:"block_count"`
	Final             *Mask        `json:"final"`
	Components        []Component  `json:"components"`
	InitialComponents []Component  `json:"initial_components"`
	ErrorReport       *ErrorReport `json:"error_report"`
	Reused            []NodeRef    `json:"reused"`
	Recomputed        []NodeRef    `json:"recomputed"`
	Failed            []NodeRef    `json:"failed"`
}

type RunReport struct {
	Mode             string             `json:"mode"`
	Params           Params             `json:"params"`
	Incomplete       bool               `json:"incomplete"`
	StopAfterCommits int                `json:"stop_after_commits,omitempty"`
	CommitSequence   []CommitRef        `json:"commit_sequence"`
	SampleReports    []*SampleRunReport `json:"sample_reports"`
	WaitRecords      []WaitRecord       `json:"wait_records"`
	SchedulerMetrics SchedulerMetrics   `json:"scheduler_metrics"`
	Invalidations    []InvalidationItem `json:"invalidations,omitempty"`
}

type Service struct {
	Dataset   *Dataset
	Store     *Store
	Params    Params
	Scheduler *BoundedScheduler
}

func NewService(d *Dataset, dbPath string) (*Service, error) {
	if err := d.Params.Validate(); err != nil {
		return nil, err
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := store.RecoverIntegrity(context.Background(), StageVersionMap(d.Params)); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Service{Dataset: d, Store: store, Params: d.Params, Scheduler: NewBoundedScheduler(d.Params)}, nil
}

func (s *Service) Close() error { return s.Store.Close() }

func StageParamVersion(p Params, stage Stage) string {
	var relevant any
	switch stage {
	case StageGradient:
		relevant = struct {
			Operator string `json:"operator"`
			Size     int    `json:"size"`
			Scale    int64  `json:"scale"`
			WR       int    `json:"wr"`
			WG       int    `json:"wg"`
			WB       int    `json:"wb"`
		}{p.Operator, p.OperatorSize, p.FixedScale, p.LumaWeightR, p.LumaWeightG, p.LumaWeightB}
	case StageEdge:
		relevant = struct {
			Quantile int   `json:"quantile"`
			Band     int   `json:"band"`
			Scale    int64 `json:"scale"`
		}{p.QuantilePermille, p.EdgeBandWidth, p.FixedScale}
	case StageRadius:
		relevant = struct {
			Bins   int   `json:"bins"`
			Window int   `json:"window"`
			Min    int   `json:"min"`
			Max    int   `json:"max"`
			Scale  int64 `json:"scale"`
		}{p.CurvatureBins, p.CurvatureWindow, p.RadiusMin, p.RadiusMax, p.FixedScale}
	case StageClose:
		relevant = struct {
			Min   int   `json:"min"`
			Max   int   `json:"max"`
			Scale int64 `json:"scale"`
		}{p.RadiusMin, p.RadiusMax, p.FixedScale}
	case StageOpen:
		relevant = struct {
			Iterations int   `json:"iterations"`
			Threshold  int   `json:"threshold"`
			Scale      int64 `json:"scale"`
		}{p.MaxIterations, p.ChangeThreshold, p.FixedScale}
	case StageCompose:
		relevant = struct {
			Threshold int `json:"threshold"`
			OpenW     int `json:"open_w"`
			AuxW      int `json:"aux_w"`
		}{p.ComposeThresholdPermille, p.ComposeOpenWeight, p.ComposeAuxWeight}
	}
	return "pv-" + HashCanonical(struct {
		Algo     string `json:"algo"`
		Stage    Stage  `json:"stage"`
		Relevant any    `json:"relevant"`
	}{AlgoVersion, stage, relevant})[:16]
}

func StageVersionMap(p Params) map[Stage]string {
	out := map[Stage]string{}
	for _, st := range Stages {
		out[st] = StageParamVersion(p, st)
	}
	return out
}

func BlockInputHash(s *Sample, b BlockCoord) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d,%d,%d,%d", s.InputHash, b.OriginX, b.OriginY, b.Width, b.Height)
	for y := b.OriginY; y < b.OriginY+b.Height; y++ {
		start := (y*s.Width + b.OriginX) * 4
		end := start + b.Width*4
		_, _ = h.Write(s.Raster.RGBA[start:end])
		mStart := y*s.Width + b.OriginX
		mEnd := mStart + b.Width
		_, _ = h.Write(s.Rough.Data[mStart:mEnd])
	}
	return "bih-" + hex.EncodeToString(h.Sum(nil))
}

type cacheKeyPayload struct {
	InputHash        string   `json:"input_hash"`
	SampleID         string   `json:"sample_id"`
	BlockID          string   `json:"block_id"`
	Stage            Stage    `json:"stage"`
	RadiusValuesHash string   `json:"radius_values_hash,omitempty"`
	ParamVersion     string   `json:"param_version"`
	UpstreamKeys     []string `json:"upstream_keys,omitempty"`
}

func MakeCacheKey(sampleID string, b BlockCoord, stage Stage, inputHash, radiusHash, paramVersion string, upstream []string) string {
	return "ck-" + HashCanonical(cacheKeyPayload{inputHash, sampleID, b.ID(), stage, radiusHash, paramVersion, append([]string(nil), upstream...)})[:24]
}

func ContentHash(output any) (string, string, error) {
	b, err := json.Marshal(output)
	if err != nil {
		return "", "", err
	}
	return string(b), HashBytes("maskqa-content-v1", b), nil
}

func DecodeCheckpoint[T any](cp *Checkpoint) (*T, error) {
	var out T
	if err := json.Unmarshal([]byte(cp.OutputJSON), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func ChangedParamFields(old, next Params) []string {
	ov := reflect.ValueOf(old)
	nv := reflect.ValueOf(next)
	t := reflect.TypeOf(old)
	var fields []string
	for i := 0; i < t.NumField(); i++ {
		if !reflect.DeepEqual(ov.Field(i).Interface(), nv.Field(i).Interface()) {
			tag := t.Field(i).Tag.Get("json")
			if name := strings.Split(tag, ",")[0]; name != "" {
				fields = append(fields, name)
			}
		}
	}
	sort.Strings(fields)
	return fields
}

func stageAffectedByField(field string) Stage {
	switch {
	case strings.HasPrefix(field, "gradient") || field == "operator" || field == "operator_size" || strings.Contains(field, "luma"):
		return StageGradient
	case strings.Contains(field, "quantile") || strings.Contains(field, "edge_band"):
		return StageEdge
	case strings.Contains(field, "curvature") || strings.Contains(field, "radius"):
		return StageRadius
	case strings.Contains(field, "iteration") || strings.Contains(field, "change_threshold"):
		return StageOpen
	case strings.Contains(field, "compose"):
		return StageCompose
	case field == "block_size":
		return StageGradient
	default:
		return StageGradient
	}
}

func (s *Service) PlanEdit(oldParams, newParams Params) []InvalidationItem {
	changed := ChangedParamFields(oldParams, newParams)
	seedStages := map[Stage]string{}
	for _, f := range changed {
		st := stageAffectedByField(f)
		if existing, ok := seedStages[st]; ok {
			seedStages[st] = existing + "," + f
		} else {
			seedStages[st] = f
		}
	}
	stageIndex := map[Stage]int{}
	for i, st := range Stages {
		stageIndex[st] = i
	}
	items := map[string]InvalidationItem{}
	for _, sample := range s.Dataset.Samples {
		for _, block := range sample.Blocks {
			for seed, field := range seedStages {
				seedIdx := stageIndex[seed]
				for _, st := range Stages[seedIdx:] {
					ref := NodeRef{SampleID: sample.ID, Block: block, Stage: st}
					item := InvalidationItem{SampleID: sample.ID, Block: block, Stage: st}
					if st == seed {
						item.Reason = "parameter_version_changed"
						item.ParamField = field
						item.Upstream = []string{"param:" + field}
					} else {
						prev := Stages[stageIndex[st]-1]
						item.Reason = "upstream_dependency_invalidated"
						item.Upstream = []string{
							fmt.Sprintf("node:%s/%s/%s", sample.ID, seed, block.ID()),
							fmt.Sprintf("node:%s/%s/%s", sample.ID, prev, block.ID()),
							fmt.Sprintf("global:%s-assembly", sample.ID),
						}
					}
					items[ref.Key()] = item
				}
			}
		}
	}
	out := make([]InvalidationItem, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SampleID != out[j].SampleID {
			return out[i].SampleID < out[j].SampleID
		}
		if stageIndex[out[i].Stage] != stageIndex[out[j].Stage] {
			return stageIndex[out[i].Stage] < stageIndex[out[j].Stage]
		}
		if out[i].Block.IndexY != out[j].Block.IndexY {
			return out[i].Block.IndexY < out[j].Block.IndexY
		}
		return out[i].Block.IndexX < out[j].Block.IndexX
	})
	return out
}

func (s *Service) ApplyEdit(ctx context.Context, p Params) ([]InvalidationItem, error) {
	items := s.PlanEdit(s.Params, p)
	if err := s.Store.AddInvalidations(ctx, items); err != nil {
		return nil, err
	}
	s.Params = p
	s.Scheduler = NewBoundedScheduler(p)
	return items, nil
}

func AssembleMask(width, height int, blocks []BlockCoord, cores []*Mask) *Mask {
	full := NewMask(width, height)
	for i, b := range blocks {
		if cores[i] == nil {
			continue
		}
		for ly := 0; ly < b.Height; ly++ {
			for lx := 0; lx < b.Width; lx++ {
				full.Set(b.OriginX+lx, b.OriginY+ly, cores[i].At(lx, ly))
			}
		}
	}
	return full
}

func ExtractCore(full *Mask, b BlockCoord) *Mask {
	core := NewMask(b.Width, b.Height)
	for ly := 0; ly < b.Height; ly++ {
		for lx := 0; lx < b.Width; lx++ {
			core.Set(lx, ly, full.At(b.OriginX+lx, b.OriginY+ly))
		}
	}
	return core
}
