// Package diag provides the local deterministic diagnostics HTTP surface.
package diag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	ballisticgrid "ballisticgrid"
)

type Dashboard struct {
	Level  *ballisticgrid.Level
	Camera ballisticgrid.Camera
	Scene  *ballisticgrid.SceneTiles
	Plan   *ballisticgrid.InvalidationPlan
	Report *ballisticgrid.VerificationReport
}

func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", d.index)
	mux.HandleFunc("/diagram.svg", d.svg)
	mux.HandleFunc("/api/summary", d.summary)
	mux.HandleFunc("/api/invalidation", d.invalidation)
	mux.HandleFunc("/api/inject", d.inject)
	return mux
}

func (d *Dashboard) summary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if d.Report != nil {
		_ = json.NewEncoder(w).Encode(d.Report)
		return
	}
	_ = json.NewEncoder(w).Encode(d.Scene)
}

func (d *Dashboard) invalidation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(d.Plan)
}

func (d *Dashboard) inject(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	level := ballisticgrid.Generate(ballisticgrid.DefaultSeed)
	cam := ballisticgrid.DefaultCamera()
	var payload any
	switch kind {
	case "decode":
		_, payload = ballisticgrid.InjectDecodeFault(level)
	case "duplicate":
		payload = ballisticgrid.InjectDuplicateFault(level)
	case "order":
		payload = ballisticgrid.InjectOrderFault(level, cam)
	case "norm":
		_, payload = ballisticgrid.InjectNormFault(level, cam)
	case "block":
		payload = ballisticgrid.InjectBlockFault(cam)
	case "bounce":
		_, payload = ballisticgrid.InjectBounceFault(level)
	case "cover":
		scene, err := ballisticgrid.ProjectScene(level, cam)
		if err == nil {
			err = scene.ValidateCoverage(1)
		}
		ge, _ := ballisticgrid.AsError(err, ballisticgrid.CodeCover)
		payload = ge
	default:
		payload = map[string]string{"error": "unknown injection kind", "supported": "decode,duplicate,order,norm,block,bounce,cover"}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	status := http.StatusOK
	if ge, ok := payload.(*ballisticgrid.Error); ok && ge != nil {
		switch ge.Code {
		case ballisticgrid.CodeDecode, ballisticgrid.CodeOrder, ballisticgrid.CodeNorm,
			ballisticgrid.CodeBlock, ballisticgrid.CodeBounce, ballisticgrid.CodeCover:
			status = http.StatusUnprocessableEntity
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (d *Dashboard) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<title>弹道与反弹网格诊断</title><style>
body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:24px;color:#0f172a;background:#f8fafc}
h1{font-size:22px} h2{font-size:17px;margin-top:28px}
.card{background:#fff;border:1px solid #e2e8f0;border-radius:10px;padding:16px;margin:12px 0;box-shadow:0 1px 2px #0f172a0d}
table{border-collapse:collapse;width:100%;font-size:12px} th,td{border-bottom:1px solid #e2e8f0;padding:6px 8px;text-align:left}
code{background:#f1f5f9;padding:1px 4px;border-radius:4px} .ok{color:#15803d;font-weight:700}.bad{color:#b91c1c;font-weight:700}
button{margin:2px;padding:6px 10px;border-radius:8px;border:1px solid #cbd5e1;background:#fff;cursor:pointer}
button:hover{background:#eff6ff} pre{background:#0f172a;color:#dbeafe;padding:12px;border-radius:8px;overflow:auto}
svg{width:100%;height:auto;background:#fff;border:1px solid #cbd5e1;border-radius:8px}
</style></head><body>`)
	b.WriteString("<h1>弹道预测与障碍反弹网格校验内核</h1>")
	b.WriteString(`<p>固定种子 <code>20261127</code>；标准库确定性生成、定点物理、分块哈希、SQLite 快照与失效传播。数据接口：<a href="/api/summary">/api/summary</a>，图：<a href="/diagram.svg">/diagram.svg</a>。</p>`)
	b.WriteString(`<div class="card"><button onclick="inject('decode')">注入 E_DECODE</button>`)
	b.WriteString(`<button onclick="inject('order')">注入 E_ORDER</button>`)
	b.WriteString(`<button onclick="inject('norm')">注入 E_NORM</button>`)
	b.WriteString(`<button onclick="inject('block')">注入 E_BLOCK</button>`)
	b.WriteString(`<button onclick="inject('bounce')">注入 E_BOUNCE</button>`)
	b.WriteString(`<button onclick="inject('cover')">注入 E_COVER</button>`)
	b.WriteString(`<pre id="inject">点击按钮查看稳定错误结构。</pre></div>`)
	b.WriteString(`<div class="card">`)
	b.WriteString(d.svgMarkup())
	b.WriteString(`</div>`)
	b.WriteString(`<div class="card"><h2>分块覆盖</h2>`)
	b.WriteString(d.tileTable())
	b.WriteString(`</div>`)
	b.WriteString(`<div class="card"><h2>反弹点</h2>`)
	b.WriteString(d.bounceTable())
	b.WriteString(`</div>`)
	if d.Plan != nil {
		b.WriteString(`<div class="card"><h2>失效传播清单</h2>`)
		b.WriteString(invalidationTable(d.Plan))
		b.WriteString(`</div>`)
	}
	b.WriteString(`<script>
async function inject(k){const r=await fetch('/api/inject?kind='+k);const j=await r.json();document.getElementById('inject').textContent=JSON.stringify(j,null,2)}
</script>`)
	b.WriteString(`</body></html>`)
	_, _ = w.Write([]byte(b.String()))
}

func (d *Dashboard) tileTable() string {
	if d.Scene == nil {
		return "<p>无分块数据。</p>"
	}
	var b strings.Builder
	b.WriteString("<table><thead><tr><th>tile</th><th>polyCoverage</th><th>coverage‰</th><th>contentHash</th><th>obstacles</th><th>requests</th></tr></thead><tbody>")
	for _, t := range d.Scene.Tiles {
		b.WriteString(fmt.Sprintf("<tr><td>(%d,%d)</td><td>%.6f</td><td>%d</td><td><code>%s</code></td><td>%s</td><td>%s</td></tr>",
			t.ID.Col, t.ID.Row, t.PolyCoverageRatio, t.CoveragePermille, t.ContentHash[:16],
			strings.Join(t.ObstacleIDs, ","), strings.Join(t.RequestIDs, ",")))
	}
	b.WriteString("</tbody></table>")
	return b.String()
}

func (d *Dashboard) bounceTable() string {
	if d.Scene == nil {
		return "<p>无弹道数据。</p>"
	}
	var b strings.Builder
	b.WriteString("<table><thead><tr><th>request</th><th>first</th><th>#</th><th>frame</th><th>obstacle</th><th>point(mm)</th></tr></thead><tbody>")
	for _, r := range d.Scene.Results {
		if len(r.Bounces) == 0 {
			b.WriteString(fmt.Sprintf("<tr><td>%s</td><td colspan=4>无命中：%s</td><td>escape=%v</td></tr>", r.RequestID, r.TerminalReason, r.Escaped))
			continue
		}
		for _, hit := range r.Bounces {
			b.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>(%d,%d)</td></tr>",
				r.RequestID, r.FirstObstacleID, hit.Seq, hit.Frame, hit.ObstacleID, hit.Point.X/1000, hit.Point.Y/1000))
		}
	}
	b.WriteString("</tbody></table>")
	return b.String()
}

func invalidationTable(plan *ballisticgrid.InvalidationPlan) string {
	var b strings.Builder
	b.WriteString("<table><thead><tr><th>tile</th><th>downstream</th><th>old endpoint</th><th>old normal</th><th>old restitution</th><th>basis</th></tr></thead><tbody>")
	for _, item := range plan.Items {
		b.WriteString(fmt.Sprintf("<tr><td>(%d,%d)</td><td>%v</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td></tr>",
			item.Tile.Col, item.Tile.Row, item.Downstream, item.OldVersions.Endpoint, item.OldVersions.Normal,
			item.OldVersions.Restitution, strings.Join(item.Reasons, "; ")))
	}
	b.WriteString("</tbody></table>")
	return b.String()
}

func (d *Dashboard) svg(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	_, _ = w.Write([]byte(d.svgMarkup()))
}

func (d *Dashboard) svgMarkup() string {
	const sx = 640.0 / 32000.0 // camera shows 32000 mm horizontally
	const sy = 320.0 / 16000.0
	xf := func(v int64) float64 { return float64(v/ballisticgrid.PositionScale) * sx }
	yf := func(v int64) float64 { return float64(v/ballisticgrid.PositionScale) * sy }
	var b strings.Builder
	b.WriteString(`<svg viewBox="0 0 640 320" role="img" aria-label="tile coverage and bounce propagation">`)
	b.WriteString(`<rect x="0" y="0" width="640" height="320" fill="#f8fafc"/>`)
	// Tile grid: default camera tile is 8000 mm and 160 user units.
	for row := 0; row < d.Camera.Rows(); row++ {
		for col := 0; col < d.Camera.Cols(); col++ {
			fill := "#ffffff"
			opacity := "1"
			if d.Scene != nil {
				for _, t := range d.Scene.Tiles {
					if t.ID.Col == col && t.ID.Row == row {
						alpha := 0.08 + float64(t.CoveragePermille)/1000.0*2.0
						if alpha > 0.55 {
							alpha = 0.55
						}
						fill = fmt.Sprintf("rgba(37,99,235,%.3f)", alpha)
						opacity = "1"
					}
				}
			}
			b.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="160" height="160" fill="%s" stroke="#cbd5e1"/>`, col*160, row*160, fill))
			_ = opacity
			b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" font-size="10" fill="#64748b">%d,%d</text>`, col*160+6, row*160+14, col, row))
		}
	}
	// World bounds.
	b.WriteString(`<rect x="0" y="0" width="480" height="320" fill="none" stroke="#0f172a" stroke-width="2"/>`)
	if d.Level != nil {
		for _, o := range d.Level.Obstacles {
			b.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#1d4ed8" stroke-width="4" stroke-linecap="round"/>`, xf(o.A.X), yf(o.A.Y), xf(o.B.X), yf(o.B.Y)))
			b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" font-size="10" fill="#1e3a8a">%s</text>`, xf(o.A.X)+3, yf(o.A.Y)-4, o.ID))
		}
	}
	if d.Scene != nil {
		for _, res := range d.Scene.Results {
			if len(res.Path) >= 2 {
				var pts strings.Builder
				for _, p := range res.Path {
					pts.WriteString(fmt.Sprintf("%.1f,%.1f ", xf(p.X), yf(p.Y)))
				}
				b.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#16a34a" stroke-width="1.5" opacity="0.9"/>`, pts.String()))
			}
			for _, hit := range res.Bounces {
				b.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="#dc2626" stroke="#fff" stroke-width="1"/><text x="%.1f" y="%.1f" font-size="10" fill="#991b1b">%s#%d</text>`,
					xf(hit.Point.X), yf(hit.Point.Y), xf(hit.Point.X)+5, yf(hit.Point.Y)-5, hit.ObstacleID, hit.Seq))
			}
		}
	}
	if d.Plan != nil {
		for _, item := range d.Plan.Items {
			x := item.Tile.Col * 160
			y := item.Tile.Row * 160
			b.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="160" height="160" fill="none" stroke="#dc2626" stroke-width="3" stroke-dasharray="8 5"/>`, x, y))
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}
