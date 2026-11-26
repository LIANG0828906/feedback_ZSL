package vpf

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
)

type ChunkView struct {
	Index     int    `json:"index"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Status    string `json:"status"`
	CacheKey  string `json:"cache_key"`
	Digest    string `json:"digest"`
	CommitSeq int64  `json:"commit_seq"`
	Source    string `json:"source"`
	Invalid   bool   `json:"invalid"`
}

type PropagationExample struct {
	Name   string            `json:"name"`
	Change Change            `json:"change"`
	Plan   *InvalidationPlan `json:"plan"`
}

type DiagnosticsView struct {
	ProjectID       string               `json:"project_id"`
	SourceVersion   int                  `json:"source_version"`
	Width           int                  `json:"width"`
	Height          int                  `json:"height"`
	TileWidth       int                  `json:"tile_width"`
	TileHeight      int                  `json:"tile_height"`
	ChunkCount      int                  `json:"chunk_count"`
	Outline         string               `json:"outline"`
	Layers          []Layer              `json:"layers"`
	Chunks          []ChunkView          `json:"chunks"`
	Examples        []PropagationExample `json:"examples"`
	ThreePath       *ThreePathReport     `json:"-"`
	Incremental     *IncrementalReport   `json:"incremental"`
	TruthFrameCount int                  `json:"truth_frame_count"`
	TruthSamples    int                  `json:"truth_samples"`
	Verification    map[string]any       `json:"verification"`
}

func BuildDiagnosticsView(ctx context.Context) (*DiagnosticsView, error) {
	bundle, err := GenerateSample(DefaultSeed)
	if err != nil {
		return nil, err
	}
	p := bundle.Loaded[SchemaV3]
	store := NewMemoryStore()
	incremental, err := VerifyIncrementalEdit(ctx, p, 0, store, func(edited *Project) (Change, error) {
		return SetLayerOpacity(edited, "slate_badge", 4200)
	})
	if err != nil {
		return nil, err
	}
	records, err := store.ListCheckpoints(ctx, 0)
	if err != nil {
		return nil, err
	}
	recordByChunk := map[int]CheckpointRecord{}
	for _, record := range records {
		recordByChunk[record.ChunkIndex] = record
	}

	chunks := make([]ChunkView, 0, p.ChunkCount())
	invalidSet := map[int]bool{}
	renderedSet := map[int]bool{}
	for _, index := range incremental.Plan.InvalidChunks {
		invalidSet[index] = true
	}
	for _, index := range incremental.RenderedChunks {
		renderedSet[index] = true
	}
	for i := 0; i < p.ChunkCount(); i++ {
		bounds := p.TileRect(i)
		record := recordByChunk[i]
		status := "reused"
		source := "persisted_reuse"
		if renderedSet[i] {
			status = "invalidated + recommitted"
			source = "rendered_committed"
		}
		chunks = append(chunks, ChunkView{
			Index: i, X: bounds.X, Y: bounds.Y, W: bounds.W, H: bounds.H,
			Status: status, Source: source, CacheKey: ShortHash(record.CacheKey),
			Digest: ShortHash(record.PixelDigest), CommitSeq: record.CommitSeq,
			Invalid: invalidSet[i],
		})
	}

	threePath, err := VerifyThreePaths(ctx, bundle.Loaded[SchemaV1], 0, func() CheckpointStore { return NewMemoryStore() })
	if err != nil {
		return nil, err
	}

	examples, err := buildPropagationExamples()
	if err != nil {
		return nil, err
	}
	sampleCount := 0
	for _, frame := range bundle.Truth.Frames {
		sampleCount += len(frame.SampleWeights)
	}
	view := &DiagnosticsView{
		ProjectID: p.ProjectID, SourceVersion: p.SourceVersion,
		Width: p.Width, Height: p.Height, TileWidth: p.TileWidth, TileHeight: p.TileHeight,
		ChunkCount: p.ChunkCount(), Outline: TreeOutline(p), Layers: p.Layers, Chunks: chunks,
		Examples: examples, ThreePath: threePath, Incremental: incremental,
		TruthFrameCount: len(bundle.Truth.Frames), TruthSamples: sampleCount,
		Verification: map[string]any{
			"pixel_identical":    threePath.PixelIdentical,
			"sequence_identical": threePath.SequenceIdentical,
			"frame_digest":       threePath.RealtimeDigest,
			"invalid_chunks":     incremental.Plan.InvalidChunks,
			"reused_chunks":      incremental.Plan.ReusableChunks,
		},
	}
	return view, nil
}

func buildPropagationExamples() ([]PropagationExample, error) {
	ctx := context.Background()
	_ = ctx
	makePlan := func(version int, edit func(*Project) (Change, error), name string) (PropagationExample, error) {
		p, err := GenerateSample(DefaultSeed)
		if err != nil {
			return PropagationExample{}, err
		}
		project := p.Loaded[version]
		change, err := edit(project)
		if err != nil {
			return PropagationExample{}, err
		}
		graph, err := BuildDependencyGraph(project, 0)
		if err != nil {
			return PropagationExample{}, err
		}
		return PropagationExample{Name: name, Change: change, Plan: graph.Invalidate(change.DependencyID)}, nil
	}
	opacity, err := makePlan(SchemaV3, func(p *Project) (Change, error) {
		return SetLayerOpacity(p, "slate_badge", 4200)
	}, "编辑徽章透明度")
	if err != nil {
		return nil, err
	}
	group, err := makePlan(SchemaV2, func(p *Project) (Change, error) {
		return SetGroupVisibility(p, "overlay_group", false)
	}, "切换覆盖组可见性")
	if err != nil {
		return nil, err
	}
	ripple, err := makePlan(SchemaV1, func(p *Project) (Change, error) {
		return EditRippleSource(p, "r_left", nil, 0, 0, 4321)
	}, "修改左侧涟漪源")
	if err != nil {
		return nil, err
	}
	return []PropagationExample{opacity, group, ripple}, nil
}

func DiagnosticsHandler(view *DiagnosticsView) http.Handler {
	tmpl := template.Must(template.New("diagnostics").Funcs(template.FuncMap{
		"toJSON": func(v any) template.JS {
			data, _ := json.Marshal(v)
			return template.JS(data)
		},
	}).Parse(diagnosticsHTML))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, view); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	})
	return mux
}

const diagnosticsHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>VPF 虚拟制片前景栈诊断</title>
<style>
:root{color-scheme:dark;font-family:Inter,ui-sans-serif,system-ui,"Noto Sans CJK SC",sans-serif;background:#0b1020;color:#e5eefc}
body{margin:0;line-height:1.45}header{padding:28px 36px;background:linear-gradient(135deg,#12233f,#111827);border-bottom:1px solid #26364f}
h1{margin:0 0 6px;font-size:26px}h2{margin:0 0 14px;font-size:18px;color:#9cc8ff}main{padding:24px 36px;display:grid;grid-template-columns:360px 1fr;gap:20px}
.card{background:#111a2e;border:1px solid #24334d;border-radius:14px;padding:18px;box-shadow:0 10px 30px #0003}
.full{grid-column:1/-1}.mono,code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.outline{white-space:pre;background:#0a1222;border:1px solid #22314b;border-radius:10px;padding:12px;overflow:auto}
.grid{display:grid;grid-template-columns:repeat(3,112px);gap:10px}.chunk{height:82px;border-radius:12px;padding:10px;box-sizing:border-box;border:1px solid #314461;background:#122039;position:relative}
.chunk.reused{border-color:#2f9e6e;background:linear-gradient(135deg,#123528,#122039)}.chunk.invalid{border-color:#f59e0b;background:linear-gradient(135deg,#4a2f08,#172033)}
.chunk b{font-size:18px}.chunk small{display:block;color:#a9bdd8;margin-top:4px}.badge{display:inline-block;border-radius:999px;padding:2px 9px;font-size:12px;margin:2px;background:#1d3557;color:#cfe6ff;border:1px solid #31527f}
.ok{color:#6ee7b7}.warn{color:#fbbf24}.layers{width:100%;border-collapse:collapse;font-size:13px}.layers td,.layers th{border-bottom:1px solid #24334d;padding:7px 8px;text-align:left}.examples{display:grid;grid-template-columns:repeat(3,1fr);gap:14px}.example{background:#0c1526;border:1px solid #24334d;border-radius:12px;padding:14px}.chain{font-size:12px;color:#b7c9e6;max-height:150px;overflow:auto}.arrow{color:#60a5fa}.kpi{font-size:28px;font-weight:750}.sub{color:#91a4c1;font-size:13px}
</style>
</head>
<body>
<header><h1>虚拟制片前景层栈合成迭代核</h1><div>项目 <code>{{.ProjectID}}</code> · 源 schema v{{.SourceVersion}} · {{.Width}}×{{.Height}} · {{.ChunkCount}} 个 {{.TileWidth}}×{{.TileHeight}} 分块 · 真值 {{.TruthFrameCount}} 帧/{{.TruthSamples}} 采样点</div></header>
<main>
<section class="card"><h2>层栈 / 组树</h2><div class="outline">{{.Outline}}</div></section>
<section class="card"><h2>分块检查点状态</h2><div class="grid">
{{range .Chunks}}<div class="chunk {{if .Invalid}}invalid{{else}}reused{{end}}"><b>#{{.Index}}</b><small>{{.Status}}</small><small class="mono">{{.CacheKey}}</small></div>{{end}}
</div><p class="sub">绿色：未受影响，直接复用持久化检查点；琥珀：依赖图判定失效并重算提交。</p>
<p><span class="badge ok">像素一致：{{.Verification.pixel_identical}}</span><span class="badge ok">序列一致：{{.Verification.sequence_identical}}</span><span class="badge">帧摘要 <code>{{.Verification.frame_digest}}</code></span></p>
</section>
<section class="card full"><h2>图层参数</h2><table class="layers"><thead><tr><th>ID</th><th>类型</th><th>可见</th><th>透明度 BP</th><th>混合</th><th>内容摘要</th><th>来源路径</th></tr></thead><tbody>
{{range .Layers}}<tr><td><code>{{.ID}}</code></td><td>{{.Kind}}</td><td>{{.Visible}}</td><td>{{.OpacityBP}}</td><td>{{.Blend}}</td><td>{{.ContentSummary}}</td><td class="mono">{{.SourcePath}}</td></tr>{{end}}
</tbody></table></section>
<section class="card full"><h2>失效传播示意</h2><div class="examples">
{{range .Examples}}<div class="example"><h3>{{.Name}}</h3><p class="sub"><code>{{.Change.DependencyID}}</code></p><p><span class="badge warn">失效块 {{.Plan.InvalidChunks}}</span><span class="badge ok">复用块 {{.Plan.ReusableChunks}}</span></p><div class="chain">传播节点：{{range .Plan.VisitedNodes}}<div>→ <code>{{.}}</code></div>{{end}}</div></div>{{end}}
</div></section>
<section class="card full"><h2>机器状态</h2><p><a href="/api/state">/api/state JSON</a></p></section>
</main></body></html>`
