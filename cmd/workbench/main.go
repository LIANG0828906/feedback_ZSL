package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"iconbench/internal/model"
	"iconbench/internal/pipeline"
	"iconbench/internal/sample"
	"iconbench/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "generate":
			dataDir := flagString(os.Args[2:], "-data", "data")
			m, err := sample.Generate(dataDir)
			must(err)
			fmt.Printf("generated %d images in %s\n", len(m.Images), dataDir)
			return
		case "recompute":
			dataDir := flagString(os.Args[2:], "-data", "data")
			m, err := sample.LoadManifest(filepath.Join(dataDir, "manifest.json"))
			if err != nil {
				m, err = sample.Generate(dataDir)
				must(err)
			}
			app, err := web.NewApp(dataDir, false)
			must(err)
			defer app.Close()
			p := model.DefaultParams()
			for _, rec := range m.Images {
				res, err := app.Pipe.RunImage(context.Background(), rec.ID, p, pipeline.RunOptions{})
				must(err)
				fmt.Printf("%s regions=%d iou=%.4f mismatch=%d centroid=%.3f area=%.3f commits=%d\n", rec.ID, len(res.Stats), res.Metrics.IoU, res.Metrics.MismatchPixels, res.Metrics.MaxCentroidErrorPx, res.Metrics.MaxAreaRelativeError, len(res.Sequence))
				if res.Report != nil && res.Report.StateError != nil {
					must(res.Report.StateError)
				}
				if !res.Report.WithinTolerance {
					must(fmt.Errorf("%s exceeds declared truth tolerances", rec.ID))
				}
			}
			return
		}
	}

	addr := flag.String("addr", "127.0.0.1:8080", "listen address; use 127.0.0.1 for local-only access")
	dataDir := flag.String("data", "data", "deterministic sample and SQLite data directory")
	skipPrep := flag.Bool("skip-prep", false, "skip generation/full recomputation before serving")
	flag.Parse()

	app, err := web.NewApp(*dataDir, !*skipPrep)
	must(err)
	defer app.Close()

	server := &http.Server{Addr: *addr, Handler: app.Handler()}
	log.Printf("IconBench workbench listening on http://%s", *addr)
	log.Printf("data directory: %s", *dataDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func flagString(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if len(a) > len(name)+1 && a[:len(name)+1] == name+"=" {
			return a[len(name)+1:]
		}
	}
	return def
}
