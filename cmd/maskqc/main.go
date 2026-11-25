package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"maskqc"
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
	fmt.Println(`maskqc - deterministic stroke/mask topology workbench

Usage:
  maskqc generate [-out samples] [-seed 424242]
  maskqc verify   [-samples samples] [-db artifacts/maskqc.db] [-json artifacts/verification-report.json]
  maskqc serve    [-addr 127.0.0.1:18099] [-samples samples] [-db artifacts/maskqc-web.db]`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", maskqc.FixedSeed, "deterministic seed")
	_ = fs.Parse(args)
	set, err := maskqc.GenerateSampleSet(*seed)
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	if err := set.WriteTo(*out); err != nil {
		log.Fatalf("write samples: %v", err)
	}
	fmt.Printf("generated %d projects with seed %d in %s\n", len(set.Projects), *seed, *out)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	samples := fs.String("samples", "samples", "sample root")
	db := fs.String("db", filepath.Join("artifacts", "maskqc.db"), "SQLite checkpoint path")
	jsonOut := fs.String("json", filepath.Join("artifacts", "verification-report.json"), "JSON report path")
	_ = fs.Parse(args)
	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		log.Fatal(err)
	}
	report, err := maskqc.VerifyRoot(*samples, *db)
	if err != nil {
		log.Fatalf("verify: %v", err)
	}
	if *jsonOut != "" {
		if err := os.MkdirAll(filepath.Dir(*jsonOut), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*jsonOut, report.MustJSON(), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	for _, p := range report.Projects {
		fmt.Printf("%s: ok=%v versions=%d threeMaskEqual=%v sequenceEqual=%v conclusionEqual=%v scheduledEqual=%v\n",
			p.ID, p.OK, len(p.Versions), p.ThreeMaskEqual, p.SequenceEqual, p.ConclusionEqual, p.ScheduledEqual)
		for _, v := range p.Versions {
			fmt.Printf("  %s loaded=%v alphaExact=%v topo=%v dir=%v\n", v.Version, v.Loaded, v.AlphaExact, v.TopologyOK, v.DirectionOK)
		}
	}
	if *jsonOut != "" {
		fmt.Printf("report written to %s\n", *jsonOut)
	}
	if !report.OK {
		os.Exit(1)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:18099", "listen address")
	samples := fs.String("samples", "samples", "sample root")
	db := fs.String("db", filepath.Join("artifacts", "maskqc-web.db"), "SQLite checkpoint path")
	_ = fs.Parse(args)
	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		log.Fatal(err)
	}
	server, err := maskqc.NewServer(*samples, *db)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}
	defer server.Close()
	fmt.Printf("MaskQC workbench: http://%s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, server.Handler()))
}
