package ortho

import (
	"context"
	"image"
	"strings"
)

// RootKey 由源图内容哈希构造链根缓存键。
func RootKey(srcHash string) string {
	return HashBytes([]byte("ROOT"), []byte(srcHash))
}

// StageKey 由阶段标识、参数版本、参数值与上游缓存键共同决定。
// 源图内容哈希经由链根键逐段传递，等价于输入内容哈希参与键构造。
func StageKey(st Stage, upstreamKey string) string {
	return HashBytes(
		[]byte(st.ID()),
		[]byte(st.Version()),
		st.CanonicalParams(),
		[]byte(upstreamKey),
	)
}

// StageReport 描述一次运行中某阶段的失效原因与复用状态。
type StageReport struct {
	Stage       string
	Key         string
	Invalidated bool
	Reason      string // cache_miss / params_or_input_changed / upstream_invalidated
	Reused      bool
	Status      string // reused / recomputed / failed
}

// Result 为一次管线运行的产出。
type Result struct {
	Final       image.Image
	Reports     []StageReport
	Outputs     map[string]image.Image
	CommitOrder []string
}

// Pipeline 将多阶段变换组织为显式依赖链。
type Pipeline struct {
	Stages  []Stage
	Rcfg    ResampleConfig
	Cache   *Cache
	WaitLog *WaitLog
}

func NewPipeline(stages []Stage, cfg ResampleConfig, cache *Cache) *Pipeline {
	if cache == nil {
		cache = NewCache()
	}
	return &Pipeline{Stages: stages, Rcfg: cfg, Cache: cache, WaitLog: &WaitLog{}}
}

// Version 返回管线整体参数版本（各阶段版本拼接）。
func (p *Pipeline) Version() string {
	parts := make([]string, 0, len(p.Stages))
	for _, st := range p.Stages {
		parts = append(parts, st.Version())
	}
	return strings.Join(parts, "|")
}

// Plan 计算失效清单：仅将参数或输入发生变化的阶段及其全部下游标记失效。
func (p *Pipeline) Plan(srcHash string) []StageReport {
	reports := make([]StageReport, len(p.Stages))
	upstream := RootKey(srcHash)
	invalidated := false
	for i, st := range p.Stages {
		key := StageKey(st, upstream)
		rep := StageReport{Stage: st.ID(), Key: key}
		reason := ""
		switch {
		case invalidated:
			reason = "upstream_invalidated"
		default:
			entry, ok := p.Cache.Get(st.ID())
			if !ok {
				reason = "cache_miss"
			} else if entry.Key != key {
				reason = "params_or_input_changed"
			}
		}
		if reason != "" {
			rep.Invalidated = true
			rep.Reason = reason
			p.Cache.MarkInvalid(st.ID())
			invalidated = true
		} else {
			rep.Reused = true
		}
		reports[i] = rep
		upstream = key
	}
	return reports
}

// Run 按依赖拓扑序执行：失效阶段重算并原子替换缓存，未失效阶段直接复用。
// 单阶段异常仅标记该阶段失败，其他阶段缓存不受污染，可修正后重试。
func (p *Pipeline) Run(ctx context.Context, src image.Image, srcHash string) (*Result, *Error) {
	reports := p.Plan(srcHash)
	res := &Result{Reports: reports, Outputs: make(map[string]image.Image)}
	cur := src
	for i, st := range p.Stages {
		if reports[i].Reused {
			entry, _ := p.Cache.Get(st.ID())
			cur = entry.Img
			reports[i].Status = "reused"
			res.Outputs[st.ID()] = cur
			continue
		}
		out, err := Resample(ctx, cur, st, p.Rcfg, p.WaitLog)
		if err != nil {
			reports[i].Status = "failed"
			return res, err
		}
		entry := &CacheEntry{Key: reports[i].Key, Img: out.Img, ContentHash: ContentHash(out.Img)}
		if cerr := p.Cache.Commit(st.ID(), entry); cerr != nil {
			reports[i].Status = "failed"
			return res, cerr
		}
		reports[i].Status = "recomputed"
		res.Outputs[st.ID()] = out.Img
		cur = out.Img
	}
	res.Final = cur
	res.CommitOrder = p.Cache.CommitOrder()
	return res, nil
}

// CompareImages 逐像素比对两幅图像，返回首个不一致像素坐标。
func CompareImages(a, b image.Image) (x, y int, equal bool) {
	if Channels(a) != Channels(b) {
		return 0, 0, false
	}
	ba, bb := a.Bounds(), b.Bounds()
	if ba.Dx() != bb.Dx() || ba.Dy() != bb.Dy() {
		return 0, 0, false
	}
	for yy := 0; yy < ba.Dy(); yy++ {
		for xx := 0; xx < ba.Dx(); xx++ {
			for c := 0; c < Channels(a); c++ {
				if getChannel(a, xx, yy, c) != getChannel(b, xx, yy, c) {
					return xx, yy, false
				}
			}
		}
	}
	return 0, 0, true
}

// CompareRuns 按阶段序比对两次运行的各阶段输出，
// 不一致时返回 E_DIFF 并给出首个不一致像素坐标与所属阶段。
func CompareRuns(stageOrder []string, a, b map[string]image.Image) *Error {
	for _, id := range stageOrder {
		ia, oka := a[id]
		ib, okb := b[id]
		if !oka || !okb {
			return &Error{Code: EDiff, Stage: id, Msg: "stage output missing in one run"}
		}
		if x, y, ok := CompareImages(ia, ib); !ok {
			return &Error{Code: EDiff, Stage: id, X: x, Y: y, Msg: "incremental and full recompute differ"}
		}
	}
	return nil
}
