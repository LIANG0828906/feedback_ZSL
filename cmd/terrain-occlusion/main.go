package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"terrain-occlusion/internal/diag"
	"terrain-occlusion/internal/diagnostic"
	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/projectio"
	"terrain-occlusion/internal/samplegen"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`terrain-occlusion commands:
  generate [-out samples/project.json]
  verify   [-in samples/project.json] [-artifacts artifacts]
  serve    [-in samples/project.json] [-artifacts artifacts] [-addr 127.0.0.1:8080]`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "samples/project.json", "deterministic sample JSON path")
	_ = fs.Parse(args)
	p, err := samplegen.Generate()
	must(err)
	must(projectio.SaveFile(*out, p))
	fmt.Printf("generated seed=%d chunks=%d camera_samples=%d -> %s\n", p.Seed, len(p.Chunks), len(p.CameraTrack), *out)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	in := fs.String("in", "samples/project.json", "project JSON")
	artifactsDir := fs.String("artifacts", "artifacts", "SQLite and report output directory")
	_ = fs.Parse(args)

	p, err := loadOrDefault(*in)
	must(err)
	report := diagnostic.RunThreePaths(p, diagnostic.ArtifactOptions(*artifactsDir))
	reportPath := filepath.Join(*artifactsDir, "diagnostic-report.json")
	must(os.MkdirAll(*artifactsDir, 0o755))
	must(os.WriteFile(reportPath, mustJSON(report), 0o644))
	printSummary(report)
	if !report.OK {
		for _, e := range report.VerificationErrors {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}
	fmt.Printf("report=%s\n", reportPath)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	in := fs.String("in", "samples/project.json", "project JSON")
	artifactsDir := fs.String("artifacts", "artifacts", "SQLite output directory")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	_ = fs.Parse(args)
	p, err := loadOrDefault(*in)
	must(err)
	report := diagnostic.RunThreePaths(p, diagnostic.ArtifactOptions(*artifactsDir))
	must(os.MkdirAll(*artifactsDir, 0o755))
	must(os.WriteFile(filepath.Join(*artifactsDir, "diagnostic-report.json"), mustJSON(report), 0o644))
	printSummary(report)

	server := diag.NewServer(report)
	fmt.Printf("diagnostic page: http://%s/\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, server.Handler()))
}

func loadOrDefault(path string) (*model.Project, error) {
	if p, err := projectio.LoadFile(path); err == nil {
		return p, nil
	} else if !os.IsNotExist(err) {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil, err
		}
	}
	return samplegen.Generate()
}

func printSummary(r *model.DiagnosticReport) {
	counts := map[model.Verdict]int{}
	revisits := 0
	for _, s := range r.Realtime.Results {
		for _, v := range s.Verdicts {
			counts[v.Final]++
		}
		if s.RevisitHit {
			revisits++
		}
	}
	fmt.Printf("verdicts visible=%d occluded=%d uncertain=%d revisit_hits=%d\n", counts[model.VerdictVisible], counts[model.VerdictOccluded], counts[model.VerdictUncertain], revisits)
	fmt.Printf("three-path identical=%v first_sample=%d field=%s\n", r.Comparison.Identical, r.Comparison.FirstSample, r.Comparison.Field)
}

func must(err error) {
	if err != nil {
		if te, ok := err.(*model.Error); ok {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", te.Code, te.Field, te.Message)
			if te.Details != nil {
				b, _ := json.Marshal(te.Details)
				fmt.Fprintln(os.Stderr, string(b))
			}
			os.Exit(1)
		}
		log.Fatal(err)
	}
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	must(err)
	return b
}
