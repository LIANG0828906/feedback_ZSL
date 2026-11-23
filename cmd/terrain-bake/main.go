package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"terrain-bake/bake"
	"terrain-bake/project"
	"terrain-bake/server"
)

type commonFlags struct {
	data        string
	cache       string
	out         string
	workers     int
	vertexLimit int
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	common := addCommon(fs)
	var err error
	switch name {
	case "bake":
		force := fs.Bool("force", false, "discard all cached artifacts first")
		validate := fs.Bool("validate", false, "validate analytic truth samples")
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runBake(*common, *force, *validate)
		}
	case "serve":
		addr := fs.String("addr", "127.0.0.1:8090", "local listen address")
		noBake := fs.Bool("no-bake", false, "do not bake missing blocks before serving")
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runServe(*common, *addr, *noBake)
		}
	case "invalidate":
		block := fs.String("block", "", "block id")
		kind := fs.String("kind", "height", "height or material")
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runInvalidate(*common, *block, *kind)
		}
	case "edit":
		block := fs.String("block", "", "block id")
		kind := fs.String("kind", "height", "height or material")
		col := fs.Int("col", 0, "sample column")
		row := fs.Int("row", 0, "sample row")
		layer := fs.Int("layer", 0, "material layer")
		value := fs.String("value", "0", "height value or material quantized weight")
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runEdit(*common, *block, *kind, *col, *row, *layer, *value)
		}
	case "retry":
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runRetry(*common)
		}
	case "compare":
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runCompare(*common)
		}
	case "status":
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = runStatus(*common)
		}
	case "version":
		fmt.Println("terrain-bake (Go standard library, schema v1)")
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", name)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if err == flag.ErrHelp {
			return
		}
		printError(err)
		os.Exit(1)
	}
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.data, "data", "data", "terrain project data directory")
	fs.StringVar(&c.cache, "cache", "cache", "persistent cache directory")
	fs.StringVar(&c.out, "out", "build", "render batch output directory")
	fs.IntVar(&c.workers, "workers", 0, "override max bake workers")
	fs.IntVar(&c.vertexLimit, "vertex-limit", 0, "override per-thread vertex limit")
	return c
}

func load(c commonFlags) (*project.Project, *bake.Graph, *bake.Manager, *project.Error) {
	p, e := project.Load(c.data)
	if e != nil {
		return nil, nil, nil, e
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		return nil, nil, nil, e
	}
	params := bake.ParamsFromManifest(p.Manifest)
	if c.workers > 0 {
		params.MaxWorkers = c.workers
	}
	if c.vertexLimit > 0 {
		params.MaxVerticesPerThread = c.vertexLimit
	}
	m, e := bake.NewManager(p, g, bake.ManagerOptions{
		CacheDir:  c.cache,
		OutputDir: c.out,
		Params:    params,
	})
	if e != nil {
		return nil, nil, nil, e
	}
	return p, g, m, nil
}

func runBake(c commonFlags, force, validate bool) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	ctx, stop := signalContext()
	defer stop()
	report, e := m.BakeAll(ctx, force)
	if report != nil {
		printJSON(report)
	}
	if e != nil {
		return e
	}
	if validate {
		p := m.Project()
		if p.TruthStale {
			return project.NewError(project.CodeTruth, "", "truth", "analytic truth is stale because edits.json is present")
		}
		artifacts := make(map[string]*bake.Artifact)
		for _, id := range p.BlockOrder {
			artifacts[id] = m.Artifact(id)
		}
		truth := bake.ValidateTruth(p, artifacts)
		printJSON(truth)
		if !truth.OK {
			return project.NewError(project.CodeTruth, "", "truth", "%d analytic truth checks failed", len(truth.Mismatches))
		}
	}
	return nil
}

func runServe(c commonFlags, addr string, noBake bool) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	ctx, stop := signalContext()
	defer stop()
	if !noBake {
		report, e := m.BakeAll(ctx, false)
		if report != nil {
			printJSON(report)
		}
		if e != nil {
			fmt.Fprintf(os.Stderr, "startup bake warning: %v\n", e)
		}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(m).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Printf("terrain-bake diagnostics: http://%s\n", addr)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func runInvalidate(c commonFlags, block, kind string) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	report, e := m.InvalidateQuery(block, kind)
	if e != nil {
		return e
	}
	printJSON(report)
	return nil
}

func runEdit(c commonFlags, block, kind string, col, row, layer int, rawValue string) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	ctx, stop := signalContext()
	defer stop()
	if _, e := m.BakeAll(ctx, false); e != nil {
		return e
	}
	var report *bake.BakeReport
	switch kind {
	case "height":
		v, err := strconv.ParseFloat(rawValue, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return project.NewError(project.CodeSchema, block, "value", "height value must be a finite number")
		}
		report, e = m.EditHeight(ctx, block, col, row, v)
	case "material":
		v, err := strconv.ParseFloat(rawValue, 64)
		if err != nil || v != math.Trunc(v) {
			return project.NewError(project.CodeWeight, block, "value", "material value must be an integer weight")
		}
		report, e = m.EditMaterial(ctx, block, col, row, layer, int(v))
	default:
		return project.NewError(project.CodeSchema, block, "kind", "kind must be height or material")
	}
	if report != nil {
		printJSON(report)
	}
	if e != nil {
		return e
	}
	return nil
}

func runRetry(c commonFlags) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	ctx, stop := signalContext()
	defer stop()
	report, e := m.RetryFailed(ctx)
	if report != nil {
		printJSON(report)
	}
	if e != nil {
		return e
	}
	return nil
}

func runCompare(c commonFlags) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	ctx, stop := signalContext()
	defer stop()
	diff, e := m.CompareToFull(ctx)
	if diff != nil {
		printJSON(diff)
	}
	if e != nil {
		return e
	}
	return nil
}

func runStatus(c commonFlags) error {
	_, _, m, e := load(c)
	if e != nil {
		return e
	}
	printJSON(m.Status())
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(data))
}

func printError(err error) {
	if e, ok := err.(*project.Error); ok {
		data, _ := json.MarshalIndent(map[string]any{"ok": false, "error": e}, "", "  ")
		fmt.Println(string(data))
		return
	}
	fmt.Printf("{\n  \"ok\": false,\n  \"error\": {\"code\": \"E_BAKE\", \"message\": %q}\n}\n", err.Error())
}

func usage() {
	fmt.Fprintf(os.Stderr, `terrain-bake - deterministic terrain bake workbench

Usage:
  terrain-bake bake     [-data DIR] [-cache DIR] [-out DIR] [-force] [-validate] [-workers N]
  terrain-bake serve    [-data DIR] [-cache DIR] [-out DIR] [-addr 127.0.0.1:8090]
  terrain-bake invalidate -block ID [-kind height|material]
  terrain-bake edit     -block ID -kind height|material -col N -row N [-layer N] -value V
  terrain-bake retry
  terrain-bake compare
  terrain-bake status

All commands use only the Go standard library and local files.
`)
}
