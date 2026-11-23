package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"terrain-bake/bake"
	"terrain-bake/project"
)

type Server struct {
	manager *bake.Manager

	mu               sync.Mutex
	lastReport       *bake.BakeReport
	lastInvalidation *bake.InvalidationReport
}

func New(manager *bake.Manager) *Server {
	return &Server{manager: manager}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/preview/", s.handlePreview)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/invalidate", s.handleInvalidate)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/compare", s.handleCompare)
	return mux
}

type pageBlock struct {
	State      bake.BlockState
	Def        project.Block
	PreviewURL string
	Color      string
	Affected   bool
	Source     bool
}

type svgNode struct {
	X, Y, W, H int
	BlockID    string
	Status     string
	Class      string
}

type svgEdge struct {
	X1, Y1, X2, Y2 int
	SeamID         string
	Active         bool
}

type pageData struct {
	Status       bake.ManagerStatus
	Blocks       []pageBlock
	SVGNodes     []svgNode
	SVGEdges     []svgEdge
	SVGWidth     int
	SVGHeight    int
	Invalidation *bake.InvalidationReport
	Report       *bake.BakeReport
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "GET required"))
		return
	}
	status := s.manager.Status()
	p := s.manager.Project()
	g := s.manager.Graph()
	stateMap := make(map[string]bake.BlockState, len(status.States))
	for _, st := range status.States {
		stateMap[st.BlockID] = st
	}
	s.mu.Lock()
	lastReport := s.lastReport
	lastInvalidation := s.lastInvalidation
	s.mu.Unlock()
	affected := map[string]bool{}
	source := ""
	if lastInvalidation != nil {
		source = lastInvalidation.Source
		for _, e := range lastInvalidation.Affected {
			affected[e.BlockID] = true
		}
	}
	data := pageData{
		Status:       status,
		Invalidation: lastInvalidation,
		Report:       lastReport,
		SVGWidth:     580,
		SVGHeight:    320,
	}
	byID := make(map[string]project.Block, len(p.Manifest.Blocks))
	for _, def := range p.Manifest.Blocks {
		byID[def.ID] = def
	}
	for _, def := range p.Manifest.Blocks {
		st := stateMap[def.ID]
		data.Blocks = append(data.Blocks, pageBlock{
			State:      st,
			Def:        def,
			PreviewURL: "/preview/" + url.PathEscape(def.ID) + ".png",
			Color:      materialColor(st.MaterialRatios, p.Manifest.Materials),
			Affected:   affected[def.ID],
			Source:     source == def.ID,
		})
		data.SVGNodes = append(data.SVGNodes, svgNode{
			X: 24 + def.Col*180, Y: 24 + def.Row*140, W: 140, H: 86,
			BlockID: def.ID, Status: st.Status, Class: svgClass(st.Status, affected[def.ID], source == def.ID),
		})
		if x := 24 + def.Col*180 + 140 + 24; x > data.SVGWidth {
			data.SVGWidth = x
		}
		if y := 24 + def.Row*140 + 86 + 24; y > data.SVGHeight {
			data.SVGHeight = y
		}
	}
	for _, seam := range g.AllSeams() {
		a, aok := byID[seam.A]
		b, bok := byID[seam.B]
		if !aok || !bok {
			continue
		}
		x1 := 24 + a.Col*180 + 70
		y1 := 24 + a.Row*140 + 43
		x2 := 24 + b.Col*180 + 70
		y2 := 24 + b.Row*140 + 43
		data.SVGEdges = append(data.SVGEdges, svgEdge{
			X1: x1, Y1: y1, X2: x2, Y2: y2, SeamID: seam.ID,
			Active: (affected[seam.A] || source == seam.A) && (affected[seam.B] || source == seam.B),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "GET required"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.manager.Status()})
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "GET required"))
		return
	}
	blockID := r.URL.Query().Get("block")
	kind := r.URL.Query().Get("kind")
	report, e := s.manager.InvalidateQuery(blockID, kind)
	if e != nil {
		writeError(w, http.StatusBadRequest, e)
		return
	}
	s.mu.Lock()
	s.lastInvalidation = report
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "invalidation": report, "hitBlocks": report.HitBlocks})
}

type editRequest struct {
	Block string  `json:"block"`
	Kind  string  `json:"kind"`
	Col   int     `json:"col"`
	Row   int     `json:"row"`
	Layer int     `json:"layer"`
	Value float64 `json:"value"`
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "POST required"))
		return
	}
	var req editRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, project.NewError(project.CodeDecode, "", "request", "decode edit request: %v", err))
		return
	}
	var report *bake.BakeReport
	var e *project.Error
	switch req.Kind {
	case "height":
		report, e = s.manager.EditHeight(r.Context(), req.Block, req.Col, req.Row, req.Value)
	case "material":
		weight := int(req.Value)
		if req.Value != math.Trunc(req.Value) {
			e = project.NewError(project.CodeWeight, req.Block, "value", "material weight must be an integer")
		} else {
			report, e = s.manager.EditMaterial(r.Context(), req.Block, req.Col, req.Row, req.Layer, weight)
		}
	default:
		e = project.NewError(project.CodeSchema, req.Block, "kind", "kind must be height or material")
	}
	s.mu.Lock()
	if report != nil {
		s.lastReport = report
		if report.Invalidation != nil {
			s.lastInvalidation = report.Invalidation
		}
	}
	s.mu.Unlock()
	writeOperation(w, report, e)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "POST required"))
		return
	}
	report, e := s.manager.RetryFailed(r.Context())
	s.mu.Lock()
	s.lastReport = report
	s.mu.Unlock()
	writeOperation(w, report, e)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "POST required"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "canceled": s.manager.CancelActive()})
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "GET or POST required"))
		return
	}
	diff, e := s.manager.CompareToFull(r.Context())
	if e != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": e, "diff": diff})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "diff": diff})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, project.NewError(project.CodeSchema, "", "method", "GET required"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/preview/")
	name = strings.TrimSuffix(name, ".png")
	blockID, err := url.PathUnescape(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, project.NewError(project.CodeDecode, "", "preview", "invalid preview path"))
		return
	}
	p := s.manager.Project()
	if _, ok := p.Blocks[blockID]; !ok {
		writeError(w, http.StatusNotFound, project.NewError(project.CodeSchema, blockID, "preview", "unknown block"))
		return
	}
	img := renderPreview(s.manager, blockID)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if err := png.Encode(w, img); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderPreview(m *bake.Manager, blockID string) image.Image {
	const width, height = 240, 160
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	a := m.Artifact(blockID)
	p := m.Project()
	bd := p.Blocks[blockID]
	if a == nil {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				c := color.NRGBA{R: 52, G: 56, B: 62, A: 255}
				if (x/12+y/12)%2 == 0 {
					c = color.NRGBA{R: 62, G: 67, B: 74, A: 255}
				}
				img.Set(x, y, c)
			}
		}
		return img
	}
	def := bd.Def
	gfx := (def.Grid.Cols-1)*m.Params().Subdivisions + 1
	gfz := (def.Grid.Rows-1)*m.Params().Subdivisions + 1
	layers := def.Material.Layers
	materials := p.Manifest.Materials
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := int(math.Round(float64(x) / float64(width-1) * float64(gfx-1)))
			r := int(math.Round(float64(y) / float64(height-1) * float64(gfz-1)))
			idx := r*gfx + c
			if idx >= len(a.Positions) {
				continue
			}
			var rv, gv, bv float64
			if idx*layers+layers <= len(a.MaterialWeights) {
				for layer := 0; layer < layers && layer < len(materials); layer++ {
					w := float64(a.MaterialWeights[idx*layers+layer]) / float64(m.Params().QuantMax)
					rv += w * materials[layer].Color[0]
					gv += w * materials[layer].Color[1]
					bv += w * materials[layer].Color[2]
				}
			}
			shade := 0.45 + 0.55*math.Max(0, a.Normals[idx].Y)
			rv = math.Min(1, rv*shade+0.06)
			gv = math.Min(1, gv*shade+0.06)
			bv = math.Min(1, bv*shade+0.06)
			img.Set(x, y, color.NRGBA{R: uint8(rv * 255), G: uint8(gv * 255), B: uint8(bv * 255), A: 255})
		}
	}
	return img
}

func materialColor(ratios []float64, materials []project.Material) string {
	var r, g, b float64
	for i, v := range ratios {
		if i >= len(materials) {
			break
		}
		r += v * materials[i].Color[0]
		g += v * materials[i].Color[1]
		b += v * materials[i].Color[2]
	}
	return fmt.Sprintf("#%02x%02x%02x", clampByte(r), clampByte(g), clampByte(b))
}

func clampByte(v float64) int {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return int(math.Round(v * 255))
}

func svgClass(status string, affected, source bool) string {
	switch {
	case source:
		return "node source"
	case affected:
		return "node affected"
	case status == "failed":
		return "node failed"
	case status == "dirty" || status == "baking":
		return "node dirty"
	default:
		return "node ok"
	}
}

func writeOperation(w http.ResponseWriter, report *bake.BakeReport, e *project.Error) {
	if report == nil && e != nil {
		writeError(w, http.StatusBadRequest, e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": e == nil, "report": report, "error": e})
}

func writeError(w http.ResponseWriter, code int, e *project.Error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": e})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

var pageTemplate = template.Must(template.New("index").Funcs(template.FuncMap{
	"pct": func(v float64) string { return fmt.Sprintf("%.2f%%", v*100) },
	"ms":  func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"join": func(v []string) string {
		if len(v) == 0 {
			return "-"
		}
		return strings.Join(v, " → ")
	},
}).Parse(indexHTML))

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Terrain Bake Workbench</title>
<style>
:root{--bg:#0d1117;--panel:#161b22;--line:#30363d;--text:#e6edf3;--muted:#8b949e;--good:#3fb950;--warn:#d29922;--bad:#f85149;--accent:#58a6ff}
*{box-sizing:border-box}
body{margin:0;font:14px/1.45 ui-monospace,SFMono-Regular,Consolas,monospace;background:var(--bg);color:var(--text)}
header{padding:22px 28px 14px;border-bottom:1px solid var(--line);background:linear-gradient(120deg,#101820,#1b2637)}
h1{margin:0 0 6px;font-size:24px;letter-spacing:.3px}
.muted{color:var(--muted)}
.wrap{padding:20px 28px;display:grid;gap:18px}
.grid{display:grid;grid-template-columns:minmax(0,1.7fr) minmax(320px,.8fr);gap:18px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px}
.panel h2{margin:0 0 12px;font-size:15px;text-transform:uppercase;letter-spacing:.12em;color:#c9d1d9}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(210px,1fr));gap:10px}
.block{border:1px solid var(--line);border-radius:8px;overflow:hidden;background:#0f141b}
.block.affected{border-color:var(--bad);box-shadow:0 0 0 1px rgba(248,81,73,.35)}
.block.source{border-color:var(--warn);box-shadow:0 0 0 1px rgba(210,153,34,.35)}
.block img{width:100%;height:96px;display:block;image-rendering:pixelated;background:#222831}
.block .body{padding:9px 10px}
.row{display:flex;justify-content:space-between;gap:8px;margin:2px 0}
.bad{color:var(--bad)} .good{color:var(--good)} .warn{color:var(--warn)}
.pill{display:inline-block;padding:1px 7px;border-radius:999px;border:1px solid var(--line);font-size:12px;text-transform:uppercase}
svg{width:100%;height:auto;min-height:180px;background:#0b1017;border:1px solid var(--line);border-radius:8px}
.node{z-index:2}.node rect{stroke-width:1.4}
.node text{fill:#e6edf3;font-size:12px;font-family:inherit}
.node .sub{fill:#8b949e;font-size:10px}
.node.ok rect{fill:#13291b;stroke:#3fb950}
.node.dirty rect{fill:#2b2413;stroke:#d29922}
.node.failed rect{fill:#301719;stroke:#f85149}
.node.affected rect{fill:#421b1d;stroke:#f85149;filter:drop-shadow(0 0 5px rgba(248,81,73,.45))}
.node.source rect{fill:#332711;stroke:#d29922;filter:drop-shadow(0 0 6px rgba(210,153,34,.55))}
path.edge{stroke:#3b4655;stroke-width:1.2;fill:none}
path.edge.active{stroke:#f85149;stroke-width:2.4}
table{border-collapse:collapse;width:100%}
th,td{padding:6px 8px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:normal;text-transform:uppercase;font-size:11px;letter-spacing:.08em}
code{color:#79c0ff;word-break:break-all}
input,select{background:#0d1117;color:var(--text);border:1px solid var(--line);border-radius:6px;padding:7px 8px;font:inherit;width:100%}
button{background:#1f6feb;border:1px solid #388bfd;color:white;border-radius:6px;padding:7px 11px;font:inherit;cursor:pointer}
button.secondary{background:#21262d;border-color:var(--line);color:var(--text)}
.form{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:9px}
.form .wide{grid-column:1/-1}
pre{white-space:pre-wrap;word-break:break-word;background:#0b1017;border:1px solid var(--line);border-radius:8px;padding:10px;max-height:330px;overflow:auto}
.err{color:var(--bad);white-space:pre-wrap}
@media(max-width:980px){.grid{grid-template-columns:1fr}}
</style>
</head>
<body>
<header>
<h1>Terrain Bake Workbench</h1>
<div class="muted">deterministic open-world terrain batches · cache version <span class="good">{{.Status.ParamsVersion}}</span> · algorithm {{.Status.AlgorithmVersion}} · generation {{.Status.Generation}}</div>
<div class="muted">content <code>{{.Status.ContentHash}}</code></div>
</header>
<div class="wrap">
<div class="grid">
  <section class="panel">
    <h2>Block Preview</h2>
    <div class="cards">
      {{range .Blocks}}
      <div class="block {{if .Affected}}affected{{end}} {{if .Source}}source{{end}}">
        <img src="{{.PreviewURL}}" alt="preview {{.Def.ID}}">
        <div class="body">
          <div class="row"><strong>{{.Def.ID}}</strong><span class="pill">{{.State.Status}}</span></div>
          <div class="row muted"><span>grid {{.Def.Col}},{{.Def.Row}}</span><span>{{.State.VertexCount}} v / {{.State.InstanceCount}} i</span></div>
          <div class="row"><span>seam violations</span><span class="{{if gt .State.Seams.ConstraintViolations 0}}bad{{else}}good{{end}}">{{.State.Seams.ConstraintViolations}}</span></div>
          <div class="row"><span>max normal angle</span><span>{{printf "%.6g" .State.Seams.MaxNormalAngleDeg}}°</span></div>
          <div class="row"><span>last bake</span><span>{{ms .State.LastBakeMS}} ms</span></div>
          <div class="row"><span>cache hit</span><span class="{{if .State.CacheHit}}good{{else}}warn{{end}}">{{if .State.CacheHit}}yes{{else}}no{{end}}</span></div>
          <div style="height:4px;background:{{.Color}};margin-top:7px;border-radius:4px"></div>
          {{if .State.LastError}}<div class="err">{{.State.LastError.Code}} {{.State.LastError.Field}} {{.State.LastError.Message}}</div>{{end}}
        </div>
      </div>
      {{end}}
    </div>
  </section>
  <section class="panel">
    <h2>Invalidation Graph</h2>
    <svg viewBox="0 0 {{.SVGWidth}} {{.SVGHeight}}" role="img" aria-label="seam dependency graph">
      {{range .SVGEdges}}<path class="edge {{if .Active}}active{{end}}" d="M {{.X1}} {{.Y1}} L {{.X2}} {{.Y2}}"></path>{{end}}
      {{range .SVGNodes}}<g class="{{.Class}}" transform="translate({{.X}},{{.Y}})">
        <rect width="{{.W}}" height="{{.H}}" rx="8"></rect>
        <text x="12" y="27">{{.BlockID}}</text>
        <text class="sub" x="12" y="47">{{.Status}}</text>
      </g>{{end}}
    </svg>
    {{if .Invalidation}}
      <div style="margin-top:10px">
        <div><span class="warn">source</span> <code>{{.Invalidation.Source}}</code> ({{.Invalidation.ChangeKind}})</div>
        <table>
          <tr><th>affected</th><th>reason</th><th>dependency path</th></tr>
          {{range .Invalidation.Affected}}<tr><td><code>{{.BlockID}}</code></td><td>{{.Reason}}</td><td><code>{{join .Path}}</code></td></tr>{{end}}
        </table>
        <div class="muted" style="margin-top:8px">cache hits: {{join .Invalidation.HitBlocks}}</div>
      </div>
    {{else}}<p class="muted">Submit an invalidation query or edit to color the propagation path.</p>{{end}}
  </section>
</div>

<section class="panel">
  <h2>Edit &amp; Incremental Rebake</h2>
  <form id="editForm" class="form">
    <label>Block<select name="block">{{range .Blocks}}<option value="{{.Def.ID}}">{{.Def.ID}} ({{.Def.Col}},{{.Def.Row}})</option>{{end}}</select></label>
    <label>Kind<select name="kind"><option value="height">height</option><option value="material">material</option></select></label>
    <label>Column<input name="col" type="number" min="0" value="3"></label>
    <label>Row<input name="row" type="number" min="0" value="4"></label>
    <label>Layer (material only)<input name="layer" type="number" min="0" value="1"></label>
    <label>Value<input name="value" type="number" step="any" value="0.75"></label>
    <div class="wide" style="display:flex;gap:8px;flex-wrap:wrap">
      <button type="submit">Apply edit</button>
      <button type="button" class="secondary" data-action="retry">Retry failed</button>
      <button type="button" class="secondary" data-action="cancel">Cancel active bake</button>
      <button type="button" class="secondary" data-action="compare">Compare incremental vs full</button>
    </div>
  </form>
  <p class="muted">Edits are persisted atomically to <code>data/edits.json</code> before rebaking. A failed block is not committed; retry never rewrites healthy neighbor artifacts.</p>
  <pre id="result">ready</pre>
</section>

<section class="panel">
  <h2>Cache &amp; Timing</h2>
  <table>
    <tr><th>block</th><th>status</th><th>cache</th><th>bake ms</th><th>artifact sha256</th><th>dependencies</th></tr>
    {{range .Status.States}}<tr>
      <td><code>{{.BlockID}}</code></td><td>{{.Status}}</td><td>{{if .CacheHit}}HIT{{else}}MISS{{end}}</td>
      <td>{{ms .LastBakeMS}}</td><td><code>{{.ArtifactHash}}</code></td><td>{{join .Dependencies}}</td>
    </tr>{{end}}
  </table>
  <div class="muted" style="margin-top:10px">snapshot <code>{{.Status.CacheSnapshotPath}}</code> · valid={{.Status.CacheValid}} · {{.Status.CacheReason}}</div>
  <div class="muted">output <code>{{.Status.OutputDir}}</code> · artifacts {{.Status.ArtifactCount}}/{{len .Status.States}}{{if .Status.TruthStale}} · <span class="warn">analytic truth stale after edit</span>{{end}}</div>
</section>

{{if .Report}}
<section class="panel">
  <h2>Last Bake Report</h2>
  <div class="row"><span>affected</span><span>{{join .Report.Affected}}</span></div>
  <div class="row"><span>cache hits</span><span>{{join .Report.CacheHits}}</span></div>
  <div class="row"><span>commit order</span><span>{{join .Report.CommitOrder}}</span></div>
  <div class="row"><span>duration</span><span>{{ms .Report.DurationMS}} ms</span></div>
  {{if .Report.Failures}}<div class="err">{{range .Report.Failures}}{{.Code}} {{.BlockID}}: {{.Message}}
{{end}}</div>{{end}}
  <pre>{{.Report}}</pre>
</section>
{{end}}
</div>
<script>
const result = document.getElementById('result');
async function call(url, opts){
  try{
    const r = await fetch(url, opts || {method:'POST'});
    const text = await r.text();
    let data; try{data=JSON.parse(text)}catch(_){data={raw:text}}
    result.textContent = JSON.stringify(data,null,2);
    if(data && data.ok) setTimeout(()=>location.reload(), 250);
  }catch(e){ result.textContent = String(e); }
}
document.getElementById('editForm').addEventListener('submit', (ev)=>{
  ev.preventDefault();
  const f = new FormData(ev.currentTarget);
  const payload = {
    block:f.get('block'), kind:f.get('kind'),
    col:Number(f.get('col')), row:Number(f.get('row')),
    layer:Number(f.get('layer')), value:Number(f.get('value'))
  };
  call('/api/edit',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
});
document.querySelectorAll('[data-action]').forEach(btn=>btn.addEventListener('click',()=>{
  const a=btn.dataset.action;
  if(a==='retry') call('/api/retry',{method:'POST'});
  if(a==='cancel') call('/api/cancel',{method:'POST'});
  if(a==='compare') call('/api/compare',{method:'POST'});
}));
</script>
</body>
</html>`
