package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"time"
)

type DiagnosticApp struct {
	Sample     *GeneratedSample
	Loaded     map[string]*Session
	Paths      *ThreePathReport
	TruthCheck error
	Checkpoint *CheckpointState
	Rejected   []RejectedCheckpoint
}

func (a *DiagnosticApp) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

type diagnosticView struct {
	SessionID      string               `json:"sessionId"`
	Seed           int64                `json:"seed"`
	Algorithm      string               `json:"algorithm"`
	Matched        bool                 `json:"matched"`
	SourceVersions []string             `json:"sourceVersions"`
	DomainRoots    []domainView         `json:"domainRoots"`
	Windows        []windowView         `json:"windows"`
	ErrorReports   []WindowErrorReport  `json:"errorReports"`
	Arbitrations   []ArbitrationRecord  `json:"arbitrations"`
	Invalidations  []WindowInvalidation `json:"invalidations"`
	Checkpoint     CheckpointSummary    `json:"checkpoint"`
	Rejected       []RejectedCheckpoint `json:"rejected"`
	TruthOK        bool                 `json:"truthOk"`
	TruthError     string               `json:"truthError,omitempty"`
	TotalPackets   int                  `json:"totalPackets"`
	TotalBytes     int64                `json:"totalBytes"`
}

type domainView struct {
	Domain
	Screens  []Screen     `json:"screens"`
	Children []domainView `json:"children"`
}

type windowView struct {
	Sequence    int          `json:"sequence"`
	WindowIndex int          `json:"windowIndex"`
	WindowID    string       `json:"windowId"`
	Status      WindowState  `json:"status"`
	StartTick   int64        `json:"startTick"`
	EndTick     int64        `json:"endTick"`
	Screens     []screenView `json:"screens"`
	ContentHash string       `json:"contentHash"`
}

type screenView struct {
	ScreenID      string    `json:"screenId"`
	Failed        bool      `json:"failed"`
	Failure       string    `json:"failure,omitempty"`
	Output        Region    `json:"output"`
	MappingParams string    `json:"mappingParams"`
	PacketCount   int       `json:"packetCount"`
	ByteCount     int64     `json:"byteCount"`
	Packets       []Packet  `json:"packets"`
	Errors        MapErrors `json:"errors"`
}

func buildDiagnosticView(a *DiagnosticApp) diagnosticView {
	if a == nil || a.Sample == nil || a.Sample.Session == nil {
		return diagnosticView{}
	}
	s := a.Sample.Session
	view := diagnosticView{
		SessionID: s.ID, Seed: a.Sample.Seed, Algorithm: s.AlgorithmVersion,
		Matched: a.Paths != nil && a.Paths.Matched,
		TruthOK: a.TruthCheck == nil,
	}
	if a.TruthCheck != nil {
		view.TruthError = a.TruthCheck.Error()
	}
	if a.Paths != nil && a.Paths.Realtime != nil {
		view.ErrorReports = a.Paths.Realtime.ErrorReports
		view.Arbitrations = a.Paths.Realtime.Arbitrations
		view.Invalidations = a.Paths.Realtime.Invalidations
		view.Checkpoint = SummaryOf(a.Paths.Realtime.Checkpoint)
		view.Rejected = append([]RejectedCheckpoint(nil), a.Paths.Realtime.Rejected...)
		for _, w := range a.Paths.Realtime.Windows {
			wv := windowView{
				Sequence: w.Sequence, WindowIndex: w.WindowIndex, WindowID: w.WindowID,
				Status: w.Status, StartTick: w.StartTick, EndTick: w.EndTick,
				ContentHash: w.ContentHash,
			}
			for _, sc := range w.Screens {
				sv := screenView{
					ScreenID: sc.ScreenID, Failed: sc.Failed, Failure: sc.Failure,
					Output: sc.Mapping.Output, MappingParams: sc.MappingParams,
					PacketCount: len(sc.Packets), Packets: sc.Packets,
					Errors: sc.Mapping.Errors,
				}
				for _, p := range sc.Packets {
					sv.ByteCount += p.Bytes
					view.TotalBytes += p.Bytes
				}
				view.TotalPackets += len(sc.Packets)
				wv.Screens = append(wv.Screens, sv)
			}
			view.Windows = append(view.Windows, wv)
		}
	}
	if a.Checkpoint != nil {
		view.Checkpoint = SummaryOf(a.Checkpoint)
	}
	if len(a.Rejected) > 0 {
		view.Rejected = a.Rejected
	}
	if a.Paths != nil && a.Paths.Resume != nil && len(a.Paths.Resume.Rejected) > 0 {
		view.Rejected = a.Paths.Resume.Rejected
	}
	versions := make([]string, 0, len(a.Loaded))
	for version := range a.Loaded {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	view.SourceVersions = versions
	view.DomainRoots = buildDomainViews(s)
	return view
}

func buildDomainViews(s *Session) []domainView {
	byID := map[string]*domainView{}
	for _, d := range s.Domains {
		byID[d.ID] = &domainView{Domain: d, Screens: []Screen{}}
	}
	for _, sc := range s.Screens {
		if dv := byID[sc.Domain]; dv != nil {
			dv.Screens = append(dv.Screens, sc)
		}
	}
	roots := []domainView{}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dv := byID[id]
		if dv.Parent == "" {
			roots = append(roots, *dv)
			continue
		}
		parent := byID[dv.Parent]
		if parent != nil {
			parent.Children = append(parent.Children, *dv)
		}
	}
	return roots
}

func (a *DiagnosticApp) handleIndex(w http.ResponseWriter, _ *http.Request) {
	view := buildDiagnosticView(a)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := diagnosticTemplate.Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *DiagnosticApp) handleState(w http.ResponseWriter, _ *http.Request) {
	view := buildDiagnosticView(a)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(view)
}

var diagnosticTemplate = template.Must(template.New("diagnostics").Funcs(template.FuncMap{
	"region": func(r Region) string {
		return template.HTMLEscapeString(regionString(r))
	},
	"join": func(values []string) string {
		out := ""
		for i, v := range values {
			if i > 0 {
				out += ", "
			}
			out += v
		}
		return out
	},
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MirrorLab 会话诊断</title>
<style>
:root{color-scheme:light dark;--bg:#0f1720;--card:#172433;--line:#304256;--fg:#e7eef7;--muted:#9fb0c4;--ok:#3ddc97;--bad:#ff7a90;--accent:#79b8ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{padding:24px 28px;border-bottom:1px solid var(--line);background:linear-gradient(110deg,#132235,#111923)}
h1{margin:0 0 6px;font-size:22px}.muted{color:var(--muted)}main{padding:20px 28px 60px;max-width:1500px;margin:auto}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:12px;margin:16px 0 26px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px}.card b{display:block;font-size:19px;color:var(--accent)}
section{margin:26px 0}h2{font-size:17px;border-left:4px solid var(--accent);padding-left:9px}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line)}
th,td{padding:7px 9px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}th{color:var(--muted);font-weight:600}
details{background:var(--card);border:1px solid var(--line);border-radius:9px;margin:10px 0;padding:10px 14px}
summary{cursor:pointer;font-weight:700}code{color:#ffd580}.ok{color:var(--ok)}.bad{color:var(--bad)}
ul.tree{list-style:none;padding-left:18px;border-left:1px dashed var(--line)}ul.tree.root{padding-left:0;border-left:0}
.screen{padding:5px 0}.pill{display:inline-block;border:1px solid var(--line);border-radius:999px;padding:0 7px;margin-right:5px;color:var(--muted)}
.warning{border-color:#7b4a22;background:#291d13}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:12px}
</style>
</head>
<body>
<header>
<h1>观战投屏会话诊断</h1>
<div class="muted">session <code>{{.SessionID}}</code> · seed <code>{{.Seed}}</code> · algorithm <code>{{.Algorithm}}</code></div>
</header>
<main>
<div class="cards">
<div class="card">三路径一致<b class="{{if .Matched}}ok{{else}}bad{{end}}">{{if .Matched}}YES{{else}}NO{{end}}</b></div>
<div class="card">真值核对<b class="{{if .TruthOK}}ok{{else}}bad{{end}}">{{if .TruthOK}}PASS{{else}}FAIL{{end}}</b></div>
<div class="card">窗口<b>{{len .Windows}}</b></div>
<div class="card">镜像包<b>{{.TotalPackets}}</b></div>
<div class="card">包字节<b>{{.TotalBytes}}</b></div>
<div class="card">已装载 schema<b>{{join .SourceVersions}}</b></div>
</div>
{{if .TruthError}}<section class="card warning"><h2>真值错误</h2><code>{{.TruthError}}</code></section>{{end}}
<section>
<h2>拓扑</h2>
<div class="grid"><div>{{template "domain" .DomainRoots}}</div>
<div class="card"><b>检查点</b>
<div>available: <code>{{.Checkpoint.Available}}</code></div>
<div>sequence: <code>{{.Checkpoint.WindowSeq}}</code></div>
<div>window: <code>{{.Checkpoint.LastWindowID}}</code></div>
<div>events consumed: <code>{{.Checkpoint.EventsConsumed}}</code></div>
<div>content: <code>{{.Checkpoint.ContentHash}}</code></div></div></div>
</section>
<section>
<h2>逐窗口与镜像包</h2>
{{range .Windows}}
<details {{if eq .Status "committed"}}open{{end}}>
<summary>{{.WindowID}} · seq {{.Sequence}} · tick {{.StartTick}}–{{.EndTick}} · <span class="{{if eq .Status "committed"}}ok{{else}}bad{{end}}">{{.Status}}</span></summary>
<div class="muted">content <code>{{.ContentHash}}</code></div>
{{range .Screens}}
<h3>{{.ScreenID}} {{if .Failed}}<span class="bad">failed</span>{{else}}<span class="ok">committed</span>{{end}}</h3>
<div class="muted">{{.MappingParams}} · output <code>{{region .Output}}</code> · packets <code>{{.PacketCount}}</code> · bytes <code>{{.ByteCount}}</code></div>
{{if .Failure}}<div class="bad">{{.Failure}}</div>{{end}}
<div class="muted">scale residue <code>{{.Errors.ScaleRoundingX}}/{{.Errors.ScaleRoundingY}}</code> · crop <code>{{.Errors.CropTruncationX}}/{{.Errors.CropTruncationY}}</code></div>
{{if .Packets}}
<table><thead><tr><th>seq</th><th>region</th><th>ticks</th><th>bytes</th><th>hash</th></tr></thead><tbody>
{{range .Packets}}<tr><td>{{.Seq}}</td><td>{{region .Region}}</td><td>{{.StartTick}}–{{.EndTick}}</td><td>{{.Bytes}}</td><td><code>{{.ContentHash}}</code></td></tr>{{end}}
</tbody></table>
{{end}}
{{end}}
</details>
{{end}}
</section>
<section>
<h2>误差报告与累计上界</h2>
<table><thead><tr><th>window</th><th>screen</th><th>coord</th><th>scale(1/100px)</th><th>crop(px edges)</th><th>time(ms)</th><th>scale denom</th></tr></thead><tbody>
{{range .ErrorReports}}<tr><td>{{.WindowID}}</td><td>{{.ScreenID}}</td><td>{{.Cumulative.CoordinateResidual}}</td><td>{{.Cumulative.ScaleRounding}}</td><td>{{.Cumulative.CropTruncation}}</td><td>{{.Cumulative.TimebaseResidualMs}}</td><td>{{.Cumulative.ScaleDenominator}}</td></tr>{{end}}
</tbody></table>
</section>
<section>
<h2>裁决与失效</h2>
<table><thead><tr><th>tick</th><th>event</th><th>kind</th><th>screen</th><th>winner</th><th>suppressed</th><th>basis</th></tr></thead><tbody>
{{range .Arbitrations}}<tr><td>{{.Tick}}</td><td>{{.EventID}}</td><td>{{.EventKind}}</td><td>{{.ScreenID}}</td><td>{{.WinnerID}}</td><td>{{join .SuppressedIDs}}</td><td>{{.Basis}}</td></tr>{{end}}
</tbody></table>
{{if .Invalidations}}<h2>窗口失效</h2><table><thead><tr><th>screen</th><th>window</th><th>tick</th><th>reason</th></tr></thead><tbody>
{{range .Invalidations}}<tr><td>{{.ScreenID}}</td><td>{{.WindowID}}</td><td>{{.Tick}}</td><td>{{.Reason}}</td></tr>{{end}}
</tbody></table>{{end}}
{{if .Rejected}}<h2>被拒绝检查点</h2><table><thead><tr><th>seq</th><th>window</th><th>reason</th></tr></thead><tbody>
{{range .Rejected}}<tr><td>{{.WindowSeq}}</td><td>{{.WindowID}}</td><td>{{.Reason}}</td></tr>{{end}}
</tbody></table>{{end}}
</section>
</main>
</body>
</html>
{{define "domain"}}
<ul class="tree root">
{{range .}}<li><div class="screen"><b>{{.ID}}</b> <span class="pill">origin {{.OriginX}},{{.OriginY}}</span></div>
{{if .Screens}}<ul class="tree">{{range .Screens}}<li class="screen"><strong>{{.ID}}</strong> <span class="pill">{{.Width}}×{{.Height}}</span><span class="pill">origin {{.OriginX}},{{.OriginY}}</span><span class="pill">packet ≤ {{.MaxPacketBytes}}</span><br><span class="muted">scale {{range .ScaleCapabilities}}{{.}} {{end}}· chroma {{range .ChromaCapabilities}}{{.}} {{end}}</span></li>{{end}}</ul>{{end}}
{{if .Children}}{{template "domain" .Children}}{{end}}
</li>{{end}}
</ul>
{{end}}`))

func regionString(r Region) string {
	return "[" + itoa64(r.X) + "," + itoa64(r.Y) + "," + itoa64(r.W) + "x" + itoa64(r.H) + "]"
}

func ServeDiagnostics(ctx context.Context, addr string, app *DiagnosticApp) error {
	server := &http.Server{Addr: addr, Handler: app.Handler(),
		ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
