package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"volumefog/fog"
)

//go:embed template.html
var pageTemplate string

//go:embed injection.html
var injectionTemplate string

type cell struct {
	Class    string
	Radiance float64
	X        int
	Y        int
}

type dashboard struct {
	Report       *fog.DiagnosticsReport
	PreviewCells []cell
	ExactCells   []cell
	QueueEvents  []fog.QueueEvent
	MaxRadiance  float64
}

type injectionView struct {
	Report *fog.InjectionReport
	Tiles  []string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "local listen address")
	seed := flag.Int64("seed", 42, "deterministic scene seed")
	dataDir := flag.String("data", "", "diagnostic data directory")
	width := flag.Int("width", 16, "viewport width")
	height := flag.Int("height", 12, "viewport height")
	once := flag.Bool("once", false, "run diagnostics, print a summary and exit")
	jsonOut := flag.String("json-out", "", "optional diagnostics JSON path (with -once)")
	flag.Parse()

	if *dataDir == "" {
		dir, err := os.MkdirTemp("", "vfog-workbench-")
		if err != nil {
			log.Fatal(err)
		}
		*dataDir = dir
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	opts := fog.DefaultGenerateOptions(*seed)
	opts.Width, opts.Height = *width, *height
	baseScene := fog.Generate(opts)
	if f := baseScene.Validate(); f != nil {
		log.Fatalf("generated scene invalid: %v", f)
	}
	log.Printf("running deterministic diagnostics...")
	report, err := fog.RunDiagnostics(baseScene, *dataDir)
	if err != nil {
		log.Fatalf("diagnostics failed: %v", err)
	}
	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_ = f.Close()
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
		log.Printf("diagnostics JSON written to %s", *jsonOut)
	}
	log.Printf("diagnostics ready: preview %.2fms, cache %d/%d, checkpoint entries %d",
		report.Preview.Decision.ElapsedMS, report.PostEditHits,
		len(report.Exact.Tiles), report.Checkpoint.Entries)
	if *once {
		printSummary(report)
		return
	}

	tpl := template.Must(template.New("dashboard").Funcs(templateFuncs()).Parse(pageTemplate))
	injTpl := template.Must(template.New("injection").Funcs(templateFuncs()).Parse(injectionTemplate))

	mux := newMux(baseScene, report, tpl, injTpl)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("workbench: http://%s/  (data: %s)", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newMux(baseScene *fog.Scene, report *fog.DiagnosticsReport, tpl, injTpl *template.Template) *http.ServeMux {
	view := buildDashboard(report)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tpl.Execute(w, view); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	})
	mux.HandleFunc("/api/render", func(w http.ResponseWriter, r *http.Request) {
		mode := fog.RenderMode(r.URL.Query().Get("mode"))
		if mode != fog.RenderPreview && mode != fog.RenderExact {
			mode = fog.RenderExact
		}
		frame, err := fog.NewEngine(baseScene.Clone(), nil).Render(r.Context(), fog.RenderOptions{Mode: mode})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err != nil && frame == nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = enc.Encode(map[string]any{"error": err})
			return
		}
		_ = enc.Encode(frame)
	})
	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		kind := fog.InjectionKind(strings.TrimSpace(r.URL.Query().Get("kind")))
		switch kind {
		case fog.InjectDecode, fog.InjectOrder, fog.InjectDensity, fog.InjectBlock:
		default:
			http.Error(w, "kind must be decode, order, density or block", http.StatusBadRequest)
			return
		}
		failTile := r.URL.Query().Get("tile")
		if failTile == "" {
			failTile = "tile-r00-c00"
		}
		failMedia := r.URL.Query().Get("media")
		if failMedia == "" && len(baseScene.Media) > 0 {
			failMedia = baseScene.Media[0].ID
		}
		rep := fog.RunInjection(baseScene.Clone(), kind, failTile, failMedia)
		iv := injectionView{Report: rep}
		if rep.Frame != nil {
			for _, t := range rep.Frame.Tiles {
				iv.Tiles = append(iv.Tiles, t.ID)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := injTpl.Execute(w, iv); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func printSummary(rep *fog.DiagnosticsReport) {
	fmt.Printf("scene: seed=%d viewport=%dx%d media=%d snapshot=%s\n",
		rep.Scene.Seed, rep.Scene.Width, rep.Scene.Height, len(rep.Scene.Media), rep.Scene.SnapshotVersion)
	fmt.Printf("preview: steps=%d shadow=%d elapsed=%.3fms limit=%dms met=%t\n",
		rep.Preview.Decision.StepsActual, rep.Preview.Decision.ShadowActual,
		rep.Preview.Decision.ElapsedMS, rep.Preview.Decision.LatencyLimitMS, rep.Preview.Decision.LatencyMet)
	fmt.Printf("exact: steps=%d shadow=%d tiles=%d elapsed=%.3fms\n",
		rep.Exact.Decision.StepsActual, rep.Exact.Decision.ShadowActual,
		len(rep.Exact.Tiles), rep.Exact.Decision.ElapsedMS)
	fmt.Printf("preview/exact: coverage=%t max_diff=%.9g tolerance=%.4g diverged=%t\n",
		rep.Comparison.SameCoverage, rep.Comparison.MaxDifference,
		rep.Comparison.Tolerance, rep.Comparison.Diverged)
	recomputed := 0
	if rep.PostEdit != nil {
		recomputed = rep.PostEdit.Stats.RenderedTiles
	}
	fmt.Printf("cache: no_edit=%d/%d post_edit=%d recomputed=%d\n",
		rep.CacheNoEditHits, len(rep.Exact.Tiles), rep.PostEditHits, recomputed)
	if rep.EditReport != nil && len(rep.EditReport.Sources) > 0 {
		fmt.Printf("invalidation: source=%s tile_count=%d unchanged=%d\n",
			rep.EditReport.Sources[0], len(rep.EditReport.Tiles), len(rep.EditReport.Unchanged))
	} else {
		fmt.Println("invalidation: no impacted source")
	}
	fmt.Printf("three-path: pixels=%t summaries=%t commit=%t cache_hits=%v\n",
		rep.ThreePath.PixelsEqual, rep.ThreePath.SummariesEqual, rep.ThreePath.CommitEqual, rep.ThreePath.CacheHits)
	fmt.Print("injections:")
	for _, inj := range rep.Injections {
		fmt.Printf(" %s=%s", inj.Kind, inj.Code)
	}
	fmt.Println()
	fmt.Printf("checkpoint: entries=%d tiles=%d corrupt_lines=%d truncated_tail=%t\n",
		rep.Checkpoint.Entries, rep.Checkpoint.Tiles, rep.Checkpoint.CorruptLines, rep.Checkpoint.TruncatedTail)
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtf": func(v float64, digits int) string {
			return fmt.Sprintf("%.*f", digits, v)
		},
		"yes": func(v bool) string {
			if v {
				return "是"
			}
			return "否"
		},
		"add": func(a, b int) int { return a + b },
	}
}

func buildDashboard(rep *fog.DiagnosticsReport) dashboard {
	v := dashboard{Report: rep}
	if rep.Preview != nil {
		v.PreviewCells = imageCells(rep.Preview)
	}
	if rep.Exact != nil {
		v.ExactCells = imageCells(rep.Exact)
		v.QueueEvents = append(v.QueueEvents, rep.Exact.Stats.QueueEvents...)
	}
	if rep.PostEdit != nil {
		v.QueueEvents = append(v.QueueEvents, rep.PostEdit.Stats.QueueEvents...)
	}
	for _, c := range append(append([]cell(nil), v.PreviewCells...), v.ExactCells...) {
		if math.Abs(c.Radiance) > v.MaxRadiance {
			v.MaxRadiance = math.Abs(c.Radiance)
		}
	}
	return v
}

func imageCells(f *fog.Frame) []cell {
	maxR := 1e-12
	for _, p := range f.Pixels {
		if !math.IsNaN(p.Radiance) && math.Abs(p.Radiance) > maxR {
			maxR = math.Abs(p.Radiance)
		}
	}
	out := make([]cell, 0, len(f.Pixels))
	for _, p := range f.Pixels {
		level := 0
		if !math.IsNaN(p.Radiance) {
			v := math.Abs(p.Radiance) / maxR
			if v > 0 {
				level = 1 + int(math.Log1p(v*20)/math.Log(21)*9)
			}
		}
		if level > 9 {
			level = 9
		}
		out = append(out, cell{Class: fmt.Sprintf("v%d", level), Radiance: p.Radiance, X: p.X, Y: p.Y})
	}
	return out
}
