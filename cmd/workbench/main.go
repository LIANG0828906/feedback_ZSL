// Command workbench runs the deterministic StrokeForge demo, the browser
// workbench and the local acceptance suite.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
	"strokeforge/internal/model"
	"strokeforge/internal/service"
)

type checkResult struct {
	Name       string  `json:"name"`
	OK         bool    `json:"ok"`
	Expected   string  `json:"expected"`
	Actual     string  `json:"actual"`
	DurationMS float64 `json:"duration_ms"`
}

type report struct {
	OK           bool          `json:"ok"`
	Seed         int64         `json:"seed"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Algorithm    string        `json:"algorithm"`
	GoVersion    string        `json:"go_version"`
	GlyphCount   int           `json:"glyph_count"`
	SegmentCount int           `json:"segment_count"`
	Checks       []checkResult `json:"checks"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		runDemo(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "generate":
		runGenerate(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: workbench [demo|serve|generate] [flags]")
}

func runGenerate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	out := fs.String("out", "artifacts", "output directory")
	_ = fs.Parse(args)
	svc, err := service.New(*seed, engine.DefaultPoolConfig(), filepath.Join(*out, "checkpoint"))
	must(err)
	path, err := svc.WriteSample(*out)
	must(err)
	fmt.Println("wrote", path)
}

func runDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	out := fs.String("out", "artifacts", "output directory")
	_ = fs.Parse(args)
	must(os.MkdirAll(*out, 0o755))

	rep := &report{
		Seed:      *seed,
		StartedAt: time.Now().UTC(),
		Algorithm: model.AlgorithmVersion,
		GoVersion: goVersion(),
	}
	suite := newSuite(*seed, *out)
	rep.Checks = suite.runAll()
	rep.FinishedAt = time.Now().UTC()
	rep.GlyphCount = len(suite.svc.Doc().Glyphs)
	rep.SegmentCount = suite.segmentCount()
	rep.OK = true
	for _, c := range rep.Checks {
		if !c.OK {
			rep.OK = false
		}
	}
	// Export sample and masks.
	path, err := suite.svc.WriteSample(*out)
	must(err)
	for _, g := range suite.svc.Doc().Glyphs {
		png, err := suite.svc.MaskPNG(g.ID)
		must(err)
		must(os.WriteFile(filepath.Join(*out, "mask-"+g.ID+".png"), png, 0o644))
	}
	raw, _ := json.MarshalIndent(rep, "", "  ")
	must(os.WriteFile(filepath.Join(*out, "acceptance_report.json"), raw, 0o644))
	fmt.Printf("PASS seed=%d glyphs=%d segments=%d\n", *seed, rep.GlyphCount, rep.SegmentCount)
	if !rep.OK {
		for _, c := range rep.Checks {
			if !c.OK {
				fmt.Println("FAIL", c.Name, c.Actual)
			}
		}
		os.Exit(1)
	}
	fmt.Println("sample:", path)
	fmt.Println("report:", filepath.Join(*out, "acceptance_report.json"))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func goVersion() string { return engineGoVersion }
