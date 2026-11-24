package diag

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"levelacoustic/internal/editing"
	"levelacoustic/internal/fixed"
	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
	"levelacoustic/internal/store"
)

type Server struct {
	mu         sync.Mutex
	engine     *solver.Engine
	projectOut string
	msg        string
	err        string
}

func New(engine *solver.Engine, projectOut string) *Server {
	return &Server{engine: engine, projectOut: projectOut}
}

func (s *Server) ListenAndServe(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("diagnostic server must bind to a loopback address, got %q", addr)
	}
	httpServer := &http.Server{Addr: addr, Handler: s.Handler()}
	return httpServer.ListenAndServe()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/solve", s.handleSolve)
	mux.HandleFunc("/api/full", s.handleFull)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/retry", s.handleRetry)
	return recoverHandler(mux)
}

func recoverHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				http.Error(w, fmt.Sprintf("internal error: %v", recovered), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.pageData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, project, restore, err := s.engine.Snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": result, "project": project, "restore": restore,
		"store_stats": s.storeStats(), "invalidation": s.engine.LastInvalidation(),
	})
}

func (s *Server) handleSolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.engine.Solve(r.Context())
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	s.msg = fmt.Sprintf("incremental solve complete: %s", result.Progress)
	s.redirect(w, r)
}

func (s *Server) handleFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.engine.Reset(r.Context()); err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	result, err := s.engine.Solve(r.Context())
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	s.msg = fmt.Sprintf("full recompute complete: %s", result.Progress)
	s.redirect(w, r)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	roomID := r.FormValue("room")
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.engine.RetryRoom(r.Context(), roomID)
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	s.msg = fmt.Sprintf("room %s retried: %s", roomID, result.Progress)
	s.redirect(w, r)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	op, err := editFromForm(r)
	if err != nil {
		s.mu.Lock()
		s.err = err.Error()
		s.redirect(w, r)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	project := s.engine.Project()
	if project == nil {
		s.err = "project snapshot unavailable"
		s.redirect(w, r)
		return
	}
	next, err := editing.ApplyOne(project, op)
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	report, err := s.engine.ApplyProject(r.Context(), next, op.Op)
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	if err := writeJSONAtomic(s.projectOut, next); err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	result, err := s.engine.Solve(r.Context())
	if err != nil {
		s.err = err.Error()
		s.redirect(w, r)
		return
	}
	s.msg = fmt.Sprintf("%s: invalidated=%v reused=%v progress=%s",
		op.Op, report.InvalidatedRooms, report.ReusedRooms, result.Progress)
	s.redirect(w, r)
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	target := "/"
	values := url.Values{}
	if s.msg != "" {
		values.Set("msg", s.msg)
	}
	if s.err != "" {
		values.Set("err", s.err)
	}
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	s.msg = ""
	s.err = ""
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func editFromForm(r *http.Request) (model.EditOp, error) {
	op := model.EditOp{Op: r.FormValue("op")}
	parseInt := func(name string) (int, error) {
		value := strings.TrimSpace(r.FormValue(name))
		if value == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return n, nil
	}
	parseInt64 := func(name string) (int64, error) {
		value := strings.TrimSpace(r.FormValue(name))
		if value == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return n, nil
	}
	switch op.Op {
	case "set_room_param":
		op.RoomID = r.FormValue("room_id")
		rt, err := parseInt64("rt60_ms")
		if err != nil {
			return op, err
		}
		density, err := parseInt64("density_milli")
		if err != nil {
			return op, err
		}
		gain, err := parseInt64("gain_milli")
		if err != nil {
			return op, err
		}
		op.RT60Milliseconds, op.EarlyDensityMilli, op.ReverbGainMilli = &rt, &density, &gain
	case "add_door":
		start, err := parseInt("start")
		if err != nil {
			return op, err
		}
		width, err := parseInt("width")
		if err != nil {
			return op, err
		}
		op.Door = &model.Door{ID: r.FormValue("door_id"), WallID: r.FormValue("wall_id"), Start: start, Width: width}
	case "remove_door":
		op.DoorID = r.FormValue("door_id")
	case "drag_boundary":
		op.Axis = r.FormValue("axis")
		var err error
		op.OldCoord, err = parseInt("old_coord")
		if err != nil {
			return op, err
		}
		op.NewCoord, err = parseInt("new_coord")
		if err != nil {
			return op, err
		}
		op.SpanStart, err = parseInt("span_start")
		if err != nil {
			return op, err
		}
		op.SpanEnd, err = parseInt("span_end")
		if err != nil {
			return op, err
		}
	default:
		return op, fmt.Errorf("unknown edit op %q", op.Op)
	}
	return op, nil
}

func (s *Server) storeStats() store.StoreStats {
	if st, ok := s.engine.StoreStats(); ok {
		return st
	}
	return store.StoreStats{}
}

type pageData struct {
	Project       *model.Project
	Result        *model.SolveResult
	Restore       solver.RestoreReport
	Invalidation  *model.InvalidationReport
	StoreStats    store.StoreStats
	Logs          []map[string]any
	SVG           template.HTML
	Charts        []Chart
	Msg           string
	Err           string
	GoVersion     string
	ShortRevision string
}

type Chart struct {
	ListenerID string
	RoomID     string
	SVG        template.HTML
}

func (s *Server) pageData(r *http.Request) (pageData, error) {
	result, project, restore, err := s.engine.Snapshot(r.Context())
	if err != nil {
		return pageData{}, err
	}
	data := pageData{
		Project:      project,
		Result:       result,
		Restore:      restore,
		Invalidation: s.engine.LastInvalidation(),
		StoreStats:   s.storeStats(),
		SVG:          template.HTML(renderLevelSVG(project, result)),
		Charts:       buildCharts(result),
		Msg:          r.URL.Query().Get("msg"),
		Err:          r.URL.Query().Get("err"),
		GoVersion:    runtime.Version(),
	}
	if len(result.Revision) >= 12 {
		data.ShortRevision = result.Revision[:12]
	} else {
		data.ShortRevision = result.Revision
	}
	if st := s.engine.Store(); st != nil {
		data.Logs, _ = st.CheckpointLog(r.Context(), 30)
	}
	return data, nil
}

func buildCharts(result *model.SolveResult) []Chart {
	byListener := make(map[string][]model.ListenerResult)
	for _, result := range result.ListenerResults {
		byListener[result.ListenerID] = append(byListener[result.ListenerID], result)
	}
	ids := make([]string, 0, len(byListener))
	for id := range byListener {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Chart, 0, len(ids))
	colors := []string{"#d9485f", "#2d7dd2", "#3a9b77", "#d08b26", "#7656c9", "#3b8899"}
	for _, id := range ids {
		results := byListener[id]
		sort.Slice(results, func(i, j int) bool { return results[i].SourceID < results[j].SourceID })
		minDB, maxDB := int64(1<<62-1), int64(-1<<62)
		hasCurve := false
		for _, res := range results {
			if !res.Reachable || len(res.CurveDBMicro) == 0 {
				continue
			}
			hasCurve = true
			for _, v := range res.CurveDBMicro {
				if v < minDB {
					minDB = v
				}
				if v > maxDB {
					maxDB = v
				}
			}
		}
		var b strings.Builder
		b.WriteString(`<svg class="curve" viewBox="0 0 520 180" role="img">`)
		b.WriteString(`<rect x="0" y="0" width="520" height="180" fill="#fbfdff"/>`)
		if hasCurve {
			if maxDB-minDB < 1_000_000 {
				maxDB += 1_000_000
				minDB -= 1_000_000
			}
			for i := 0; i <= 4; i++ {
				y := 12 + float64(i)*36
				b.WriteString(fmt.Sprintf(`<line x1="52" y1="%.1f" x2="508" y2="%.1f" stroke="#e6edf5"/>`, y, y))
				db := fixed.ToFloat(maxDB) - float64(i)*(fixed.ToFloat(maxDB-minDB)/4)
				b.WriteString(fmt.Sprintf(`<text x="4" y="%.1f" class="axis">%.1f</text>`, y+4, db))
			}
			for ci, res := range results {
				if !res.Reachable || len(res.CurveDBMicro) == 0 {
					continue
				}
				points := make([]string, 0, len(res.CurveDBMicro))
				for i, v := range res.CurveDBMicro {
					x := 52 + 456*float64(i)/float64(maxInt(1, len(res.CurveDBMicro)-1))
					y := 168 - 156*(fixed.ToFloat(v-minDB)/fixed.ToFloat(maxDB-minDB))
					points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
				}
				b.WriteString(fmt.Sprintf(`<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`,
					colors[ci%len(colors)], strings.Join(points, " ")))
				b.WriteString(fmt.Sprintf(`<text x="%d" y="174" class="legend" fill="%s">%s</text>`,
					58+ci*58, colors[ci%len(colors)], html.EscapeString(res.SourceID)))
			}
		} else {
			b.WriteString(`<text x="160" y="94" class="unreachable">all source paths unreachable</text>`)
		}
		b.WriteString(`</svg>`)
		out = append(out, Chart{ListenerID: id, RoomID: results[0].ListenerRoomID, SVG: template.HTML(b.String())})
	}
	return out
}

func renderLevelSVG(project *model.Project, result *model.SolveResult) string {
	if project == nil {
		return ""
	}
	colors := []string{"#eaf2fb", "#f6efe4", "#e9f6ee", "#f4e9ef", "#eef0fa", "#e8f5f5"}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<svg class="level" viewBox="0 0 %d %d" role="img">`, project.Bounds.W, project.Bounds.H))
	b.WriteString(fmt.Sprintf(`<rect x="0" y="0" width="%d" height="%d" fill="#f7f9fc"/>`, project.Bounds.W, project.Bounds.H))
	roomByID := make(map[string]model.Room, len(project.Rooms))
	for i, room := range project.Rooms {
		roomByID[room.ID] = room
		b.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" fill="%s" stroke="#23364a" stroke-width="2"/>`,
			room.Rect.X, room.Rect.Y, room.Rect.W, room.Rect.H, colors[i%len(colors)]))
		cx := room.Rect.X + room.Rect.W/2
		cy := room.Rect.Y + room.Rect.H/2
		b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="room-label" text-anchor="middle">%s</text>`,
			cx, cy-18, html.EscapeString(room.ID)))
		b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="room-sub" text-anchor="middle">%.0fms  %.2f gain</text>`,
			cx, cy+12, float64(room.RT60Milliseconds), float64(room.ReverbGainMilli)/1000))
	}
	wallByID := make(map[string]model.Wall, len(project.Walls))
	for _, wall := range project.Walls {
		wallByID[wall.ID] = wall
		b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#23364a" stroke-width="5"/>`,
			wall.X1, wall.Y1, wall.X2, wall.Y2))
	}
	for _, door := range project.Doors {
		wall, ok := wallByID[door.WallID]
		if !ok {
			continue
		}
		if wall.Horizontal() {
			b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#26a269" stroke-width="9"/>`,
				door.Start, wall.Y1, door.Start+door.Width, wall.Y1))
			b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="door-label">%s</text>`,
				door.Start+door.Width/2, wall.Y1-10, html.EscapeString(door.ID)))
		} else {
			b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#26a269" stroke-width="9"/>`,
				wall.X1, door.Start, wall.X1, door.Start+door.Width))
			b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="door-label">%s</text>`,
				wall.X1+12, door.Start+door.Width/2, html.EscapeString(door.ID)))
		}
		if a, okA := roomByID[wall.RoomA]; okA {
			if c, okB := roomByID[wall.RoomB]; okB {
				b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#8a5bd6" stroke-width="1.5" stroke-dasharray="7 7" opacity=".55"/>`,
					a.Rect.X+a.Rect.W/2, a.Rect.Y+a.Rect.H/2, c.Rect.X+c.Rect.W/2, c.Rect.Y+c.Rect.H/2))
			}
		}
	}
	for _, source := range project.Sources {
		b.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="10" fill="#2468c7" stroke="#fff" stroke-width="2"/>`, source.X, source.Y))
		b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="point-label" fill="#164a91">%s</text>`,
			source.X+13, source.Y+5, html.EscapeString(source.ID)))
	}
	for _, listener := range project.Listeners {
		b.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="8" fill="#e47b25" stroke="#fff" stroke-width="2"/>`, listener.X, listener.Y))
		b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="point-label" fill="#9b4c08">%s</text>`,
			listener.X+11, listener.Y+4, html.EscapeString(listener.ID)))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	return os.Rename(name, path)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Room Acoustics Incremental Solver</title>
<style>
:root{color-scheme:light;--ink:#1f2e3c;--muted:#617286;--line:#dbe4ee;--card:#fff;--accent:#2468c7;--good:#1d7f50;--bad:#b53a45}
*{box-sizing:border-box}body{margin:0;background:#eef3f8;color:var(--ink);font:14px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
header{padding:18px 26px;background:#162b3f;color:#fff}header h1{margin:0 0 4px;font-size:22px}header p{margin:0;color:#c5d4e2}
main{padding:20px;max-width:1480px;margin:auto}.grid{display:grid;grid-template-columns:minmax(0,1.25fr) minmax(360px,.75fr);gap:18px}.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px;margin-bottom:18px;box-shadow:0 5px 18px #1d345012}
h2{font-size:16px;margin:0 0 12px}.metrics{display:flex;gap:10px;flex-wrap:wrap}.metric{background:#f4f8fc;border:1px solid #e0e9f3;border-radius:9px;padding:8px 11px}.metric b{display:block;font-size:17px}
.pill{display:inline-block;padding:2px 8px;border-radius:99px;font-size:12px;background:#edf2f8;margin:2px}.committed{background:#e1f3e9;color:#1d7f50}.failed{background:#fae4e6;color:#a92f3a}.pending{background:#fff3d7;color:#8b6400}.analyzing_connectivity,.solving_parameters{background:#e5edfb;color:#285aab}
table{width:100%;border-collapse:collapse;font-size:13px}th,td{text-align:left;padding:7px 8px;border-bottom:1px solid #edf1f6;vertical-align:top}th{color:var(--muted);font-weight:600}
.table-wrap{overflow:auto;max-height:440px}.level{width:100%;height:auto;border-radius:8px}.room-label{font-size:24px;font-weight:700;fill:#21384d}.room-sub{font-size:13px;fill:#607184}.point-label,.door-label{font-size:12px;font-weight:600}.door-label{fill:#1d7f50}.axis{font-size:10px;fill:#718096}.legend{font-size:10px}
.curve{width:100%;height:auto;border:1px solid #e4ebf3;border-radius:8px;margin-bottom:10px}.unreachable{font-size:13px;fill:#8b5d00}
form{display:flex;gap:7px;flex-wrap:wrap;align-items:end;margin:8px 0;padding-top:8px;border-top:1px dashed #e4ebf3}label{font-size:12px;color:var(--muted);display:flex;flex-direction:column;gap:3px}input,select{padding:6px 8px;border:1px solid #cdd9e5;border-radius:6px;background:#fff;min-width:70px}button{padding:7px 11px;border:0;border-radius:7px;background:var(--accent);color:#fff;font-weight:600;cursor:pointer}button.secondary{background:#425b73}button.danger{background:#a92f3a}button.small{padding:4px 8px;font-size:12px}
.notice{padding:9px 12px;border-radius:8px;margin-bottom:12px}.ok{background:#e4f5ec;color:#176740}.error{background:#fae4e6;color:#9d2732}.mono{font-family:ui-monospace,SFMono-Regular,Consolas,monospace}.muted{color:var(--muted)}code{background:#f0f4f8;padding:1px 5px;border-radius:4px}
@media(max-width:980px){.grid{grid-template-columns:1fr}}
</style>
</head>
<body>
<header><h1>Room Acoustics Incremental Solver</h1><p>Deterministic geometry · door-chain connectivity · fixed-point decay · SQLite checkpoints · {{.GoVersion}}</p></header>
<main>
{{if .Msg}}<div class="notice ok">{{.Msg}}</div>{{end}}{{if .Err}}<div class="notice error">{{.Err}}</div>{{end}}
<div class="card"><div class="metrics">
<div class="metric">Progress<b>{{.Result.Progress}}</b></div>
<div class="metric">Committed<b>{{.Result.CommittedRooms}} / {{.Result.TotalRooms}}</b></div>
<div class="metric">Revision<b class="mono">{{.ShortRevision}}</b></div>
<div class="metric">Restore<b>{{if .Restore.Discarded}}discarded{{else}}{{len .Restore.ReusedRooms}} reused / {{len .Restore.StaleRooms}} stale{{end}}</b></div>
<div class="metric">Checkpoint writes<b>{{.StoreStats.Writes}}</b></div>
<div class="metric">Worker jobs<b>{{.Result.Resources.Jobs}} · queued {{.Result.Resources.QueuedJobs}}</b></div>
<div class="metric">Buffers<b>{{.Result.Resources.BufferCount}} × {{.Result.Resources.MaxBufferSamples}}</b></div>
<div class="metric">Max wait<b>{{.Result.Resources.MaxWaitMillis}} ms</b></div>
</div>
<form method="post" action="/api/solve"><button>Incremental solve</button></form>
<form method="post" action="/api/full"><button class="danger">Full reset + solve</button></form>
</div>
<div class="grid">
<div>
<div class="card"><h2>Level Plan / Doors / Sources / Listeners</h2>{{.SVG}}</div>
<div class="card"><h2>Room State Matrix</h2><div class="table-wrap"><table><thead><tr><th>Room</th><th>State</th><th>RT60 / density / gain</th><th>Doors</th><th>Listener pairs</th><th>Actions</th></tr></thead><tbody>
{{range .Result.RoomStates}}{{$room := .RoomID}}<tr><td><b>{{.RoomID}}</b><br><span class="muted">gen {{.Changes}}</span></td><td><span class="pill {{.State}}">{{.State}}</span><br><span class="muted">{{.LastReason}}</span></td><td>{{range $.Project.Rooms}}{{if eq .ID $room}}{{.RT60Milliseconds}} ms / {{.EarlyDensityMilli}} / {{.ReverbGainMilli}}{{end}}{{end}}</td><td>{{range $.Result.RoomCheckpoints}}{{if eq .RoomID $room}}{{range .Doors}}<span class="pill">{{.ID}}</span>{{end}}{{end}}{{end}}</td><td>{{range $.Result.ListenerResults}}{{if eq .ListenerRoomID $room}}<span class="pill">{{.SourceID}}→{{.ListenerID}}</span>{{end}}{{end}}</td><td>{{if eq .State "failed"}}<form method="post" action="/api/retry"><input type="hidden" name="room" value="{{.RoomID}}"><button class="small">Retry</button></form>{{end}}</td></tr>{{end}}
</tbody></table></div></div>
<div class="card"><h2>Per-Listener Decay Curves</h2>{{range .Charts}}<h3 style="font-size:13px;margin:10px 0 5px">{{.ListenerID}} · room {{.RoomID}}</h3>{{.SVG}}{{end}}</div>
</div>
<div>
<div class="card"><h2>Edit and Recompute</h2>
<form method="post" action="/api/edit"><input type="hidden" name="op" value="set_room_param"><label>Room<input name="room_id" value="R6" size="5"></label><label>RT60 ms<input name="rt60_ms" value="680"></label><label>Density milli<input name="density_milli" value="700"></label><label>Gain milli<input name="gain_milli" value="1050"></label><button>Apply room parameters</button></form>
<form method="post" action="/api/edit"><input type="hidden" name="op" value="add_door"><label>Door id<input name="door_id" value="D7" size="5"></label><label>Wall<input name="wall_id" value="W56" size="5"></label><label>Start<input name="start" value="2080"></label><label>Width<input name="width" value="80"></label><button>Add door</button></form>
<form method="post" action="/api/edit"><input type="hidden" name="op" value="remove_door"><label>Door id<input name="door_id" value="D6" size="5"></label><button class="danger">Remove door</button></form>
<form method="post" action="/api/edit"><input type="hidden" name="op" value="drag_boundary"><label>Axis<select name="axis"><option>x</option><option>y</option></select></label><label>Old<input name="old_coord" value="840"></label><label>New<input name="new_coord" value="880"></label><label>Span start<input name="span_start" value="1600"></label><label>Span end<input name="span_end" value="2200"></label><button>Drag boundary</button></form>
</div>
<div class="card"><h2>Invalidation Propagation</h2>{{if .Invalidation}}<p class="muted">operation={{.Invalidation.Operation}} · invalidated={{.Invalidation.InvalidatedRooms}} · reused={{.Invalidation.ReusedRooms}}</p><div class="table-wrap"><table><thead><tr><th>Room</th><th>Reason</th><th>Reused</th><th>Affected pairs</th></tr></thead><tbody>{{range .Invalidation.Entries}}<tr><td>{{.RoomID}}</td><td>{{.Reason}}</td><td>{{.Reused}}</td><td class="mono">{{range .AffectedPairs}}{{.}} {{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No edit has run in this process.</p>{{end}}</div>
<div class="card"><h2>Worker / Buffer Queue</h2><p>max parallel={{.Result.Resources.MaxParallel}}, buffers={{.Result.Resources.BufferCount}} × {{.Result.Resources.MaxBufferSamples}} samples, total wait={{.Result.Resources.TotalWaitMicros}} µs</p><div class="table-wrap"><table><thead><tr><th>Job</th><th>Reason</th><th>Wait</th></tr></thead><tbody>{{range .Result.Resources.Events}}<tr><td>{{.JobID}}</td><td>{{.Reason}}</td><td>{{.WaitMicros}} µs</td></tr>{{end}}</tbody></table></div></div>
<div class="card"><h2>Checkpoint Write Log</h2><div class="table-wrap"><table><thead><tr><th>Seq</th><th>Room</th><th>Hash</th></tr></thead><tbody>{{range .Logs}}<tr><td>{{.seq}}</td><td>{{.room_id}}</td><td class="mono">{{.content_hash}}</td></tr>{{end}}</tbody></table></div></div>
</div>
</div>
</main>
</body>
</html>`))
