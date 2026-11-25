package acousticfield

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"sync"
)

type ComparisonView struct {
	Label   string `json:"label"`
	Same    bool   `json:"same"`
	Message string `json:"message,omitempty"`
}

type DiagnosticServer struct {
	engine     *Engine
	mu         sync.RWMutex
	comparison ComparisonView
	handler    http.Handler
}

func NewDiagnosticServer(engine *Engine) *DiagnosticServer {
	d := &DiagnosticServer{engine: engine}
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handlePage)
	mux.HandleFunc("/api/report", d.handleReport)
	mux.HandleFunc("/api/state", d.handleState)
	d.handler = mux
	return d
}

func (d *DiagnosticServer) SetComparison(label string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.comparison = ComparisonView{Label: label, Same: err == nil}
	if err != nil {
		d.comparison.Message = err.Error()
	}
}

func (d *DiagnosticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.handler.ServeHTTP(w, r)
}

func (d *DiagnosticServer) handleReport(w http.ResponseWriter, _ *http.Request) {
	report := d.engine.Report()
	d.mu.RLock()
	cmp := d.comparison
	d.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"report": report, "comparison": cmp, "state": d.engine.StateSnapshot(),
	})
}

func (d *DiagnosticServer) handleState(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d.engine.StateSnapshot())
}

type heatCell struct {
	Band   string `json:"band"`
	Energy int64  `json:"energy"`
	Style  template.CSS
}

type heatRow struct {
	ChunkID int        `json:"chunk_id"`
	Total   int64      `json:"total"`
	Cells   []heatCell `json:"cells"`
}

type invalidationRow struct {
	ChunkID  int
	Distance int
	Reasons  string
}

type queueRow struct {
	ChunkID int
	Reason  string
	Wait    string
}

type workerErrorRow struct {
	ChunkID  int
	WorkerID int
	Error    string
}

type pageData struct {
	RoundID         int64
	Options         string
	HeatRows        []heatRow
	Bands           []string
	Transfers       []Transfer
	TransferLimit   int
	TotalTransfers  int
	Invalidations   []invalidationRow
	Conservation    ConservationReport
	CacheHits       int
	Recomputed      int
	QueueEvents     []queueRow
	WorkerErrors    []workerErrorRow
	Comparison      ComparisonView
	CommittedChunks int
	CommittedRounds int
	TruthDiff       string
}

func (d *DiagnosticServer) handlePage(w http.ResponseWriter, _ *http.Request) {
	report := d.engine.Report()
	level := d.engine.Level()
	d.mu.RLock()
	comparison := d.comparison
	d.mu.RUnlock()

	bands := make([]string, 0, len(level.Bands))
	for _, b := range level.Bands {
		bands = append(bands, b.ID)
	}
	sort.Strings(bands)
	maxByBand := map[string]int64{}
	for _, res := range report.Results {
		for _, band := range bands {
			if v := bandValue(res.ArrivalEnergy, band); v > maxByBand[band] {
				maxByBand[band] = v
			}
		}
	}
	var heat []heatRow
	for _, res := range report.Results {
		row := heatRow{ChunkID: res.ChunkID, Total: sumBandEnergy(res.ArrivalEnergy)}
		for _, band := range bands {
			e := bandValue(res.ArrivalEnergy, band)
			ratio := 0.0
			if maxByBand[band] > 0 {
				ratio = float64(e) / float64(maxByBand[band])
			}
			alpha := 0.05 + 0.75*math.Sqrt(math.Max(0, math.Min(1, ratio)))
			row.Cells = append(row.Cells, heatCell{
				Band: band, Energy: e,
				Style: template.CSS(fmt.Sprintf("background-color: rgba(205,65,45,%.3f)", alpha)),
			})
		}
		heat = append(heat, row)
	}
	var inv []invalidationRow
	for _, item := range report.Invalidations {
		inv = append(inv, invalidationRow{
			ChunkID: item.ChunkID, Distance: item.Distance, Reasons: joinReasons(item.Reasons),
		})
	}
	var queue []queueRow
	for _, e := range report.QueueEvents {
		queue = append(queue, queueRow{ChunkID: e.ChunkID, Reason: e.Reason, Wait: e.WaitText})
	}
	var workerErrors []workerErrorRow
	for _, e := range report.WorkerErrors {
		msg := e.Message
		if msg == "" && e.Err != nil {
			msg = e.Err.Error()
		}
		workerErrors = append(workerErrors, workerErrorRow{ChunkID: e.ChunkID, WorkerID: e.WorkerID, Error: msg})
	}
	transferLimit := len(report.Results)
	if transferLimit > 0 {
		transferLimit = 0
		for _, res := range report.Results {
			transferLimit += len(res.Outgoing)
		}
	}
	show := transferLimit
	if show > 80 {
		show = 80
	}
	var transfers []Transfer
	for _, res := range report.Results {
		for _, t := range res.Outgoing {
			if len(transfers) >= show {
				break
			}
			transfers = append(transfers, t)
		}
	}
	data := pageData{
		RoundID: report.RoundID, Options: d.engine.options.String(), HeatRows: heat, Bands: bands,
		Transfers: transfers, TransferLimit: show, TotalTransfers: transferLimit,
		Invalidations: inv, Conservation: report.Conservation, CacheHits: report.CacheHits,
		Recomputed: report.Recomputed, QueueEvents: queue, Comparison: comparison,
		WorkerErrors:    workerErrors,
		CommittedChunks: report.CommittedChunks, CommittedRounds: report.CommittedRounds,
	}
	if report.TruthDiff != nil {
		data.TruthDiff = report.TruthDiff.String()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func joinReasons(v []string) string {
	if len(v) == 0 {
		return ""
	}
	s := v[0]
	for _, item := range v[1:] {
		s += ", " + item
	}
	return s
}

var pageTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Acoustic Field Reconstruction Diagnostics</title>
<style>
body{font-family:ui-sans-serif,system-ui,sans-serif;margin:24px;color:#1e2329;background:#f7f5f1}
h1,h2{font-weight:650} h2{margin-top:28px;border-bottom:1px solid #d8d2c8;padding-bottom:6px}
.meta{display:flex;gap:18px;flex-wrap:wrap;color:#5a626b}
.card{background:#fff;border:1px solid #ded8cd;border-radius:8px;padding:12px}
table{border-collapse:collapse;width:100%;font-size:13px;background:#fff}
th,td{border:1px solid #ddd5c9;padding:5px 7px;text-align:right}
th:first-child,td:first-child{text-align:left}
td.heat{min-width:110px;font-variant-numeric:tabular-nums}
.ok{color:#176b3a;font-weight:700}.bad{color:#a32319;font-weight:700}
pre{white-space:pre-wrap;background:#fff;border:1px solid #ded8cd;border-radius:8px;padding:12px;font-size:12px;max-height:320px;overflow:auto}
.pill{display:inline-block;border:1px solid #ccc3b5;border-radius:999px;padding:2px 8px;margin:2px}
</style>
</head>
<body>
<h1>Acoustic Field Reconstruction Diagnostics</h1>
<div class="meta">
<span>round <b>{{.RoundID}}</b></span>
<span>committed chunks <b>{{.CommittedChunks}}</b></span>
<span>committed rounds <b>{{.CommittedRounds}}</b></span>
<span>cache hits <b>{{.CacheHits}}</b></span>
<span>recomputed <b>{{.Recomputed}}</b></span>
<span>{{.Options}}</span>
</div>
{{if .TruthDiff}}<p class="bad">Truth diff: {{.TruthDiff}}</p>{{end}}

<h2>Per-band arrival energy heatmap</h2>
<table>
<thead><tr><th>Chunk</th>{{range .Bands}}<th>{{.}}</th>{{end}}<th>Total</th></tr></thead>
<tbody>
{{range .HeatRows}}<tr><td>c{{.ChunkID}}</td>{{range .Cells}}<td class="heat" style="{{.Style}}">{{.Energy}}</td>{{end}}<td>{{.Total}}</td></tr>{{end}}
</tbody></table>

<h2>Boundary transfer flow</h2>
<p>Showing {{len .Transfers}} of {{.TotalTransfers}} outgoing records.</p>
<table>
<thead><tr><th>Source</th><th>From</th><th>To</th><th>Wave</th><th>Boundary</th><th>Energy</th></tr></thead>
<tbody>
{{range .Transfers}}<tr><td>{{.SourceID}}</td><td>c{{.FromChunk}}</td><td>c{{.ToChunk}}</td><td>{{.Wave}}</td><td>{{.Boundary.Side}} @ ({{.Boundary.Position.X}},{{.Boundary.Position.Y}})</td><td>{{range .Energy}}{{.BandID}}={{.Energy}} {{end}}</td></tr>{{end}}
</tbody></table>

<h2>Invalidation propagation</h2>
{{if .Invalidations}}
<table><thead><tr><th>Chunk</th><th>Distance</th><th>Reasons</th></tr></thead><tbody>
{{range .Invalidations}}<tr><td>c{{.ChunkID}}</td><td>{{.Distance}}</td><td>{{.Reasons}}</td></tr>{{end}}
</tbody></table>
{{else}}<p>No invalidated chunks recorded for the latest edit.</p>{{end}}

<h2>Conservation</h2>
{{if .Conservation.OK}}<p class="ok">PASS</p>{{else}}<p class="bad">FAIL: {{.Conservation.Error}}</p>{{end}}
<p>Maximum absolute residual: <b>{{.Conservation.MaxResidual}}</b> energy units.</p>
<table><thead><tr><th>Source</th><th>Band</th><th>Emission</th><th>Chunk energy</th><th>Boundary net</th><th>Escape</th><th>Residual</th></tr></thead><tbody>
{{range .Conservation.Bands}}<tr><td>{{.SourceID}}</td><td>{{.BandID}}</td><td>{{.Emission}}</td><td>{{.ChunkEnergy}}</td><td>{{.BoundaryNet}}</td><td>{{.Escape}}</td><td>{{.Residual}}</td></tr>{{end}}
</tbody></table>

<h2>Queue and cache detail</h2>
{{if .QueueEvents}}<table><thead><tr><th>Chunk</th><th>Reason</th><th>Wait</th></tr></thead><tbody>
{{range .QueueEvents}}<tr><td>c{{.ChunkID}}</td><td>{{.Reason}}</td><td>{{.Wait}}</td></tr>{{end}}
</tbody></table>{{else}}<p>No worker queue waits were recorded.</p>{{end}}

<h2>Failure isolation</h2>
{{if .WorkerErrors}}<table><thead><tr><th>Chunk</th><th>Worker</th><th>Error</th></tr></thead><tbody>
{{range .WorkerErrors}}<tr><td>c{{.ChunkID}}</td><td>{{.WorkerID}}</td><td>{{.Error}}</td></tr>{{end}}
</tbody></table>{{else}}<p>No worker failures recorded.</p>{{end}}

<h2>Incremental vs full comparison</h2>
{{if .Comparison.Label}}<p>Run: <b>{{.Comparison.Label}}</b> {{if .Comparison.Same}}<span class="ok">MATCH</span>{{else}}<span class="bad">DIFF</span><br><code>{{.Comparison.Message}}</code>{{end}}</p>{{else}}<p>No comparison has been attached.</p>{{end}}

<p><a href="/api/report">JSON report</a> · <a href="/api/state">State machine</a></p>
</body></html>`))
