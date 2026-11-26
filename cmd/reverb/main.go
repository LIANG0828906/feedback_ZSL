// Command reverb runs the deterministic acoustic-field demo and diagnostic page.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"acousticfield/internal/checkpoint"
	"acousticfield/internal/demo"
	"acousticfield/internal/generator"
	"acousticfield/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		demoCmd(os.Args[2:])
	case "generate":
		generateCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`reverb <demo|generate|serve>
  demo -out out/demo             run full/realtime/recovery and verification
  generate -out out/sample       write fixed-seed deterministic sample
  serve -report out/demo/report.json -addr 127.0.0.1:8088`)
}

func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	out := fs.String("out", "out/demo", "output directory")
	_ = fs.Parse(args)
	result, err := demo.Run(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("demo output: %s\n", result.Root)
	fmt.Printf("compare ok=%v\n", result.Comparison.OK)
	if result.Comparison.First != nil {
		fmt.Printf("first mismatch: %s\n", result.Comparison.First.String())
		os.Exit(1)
	}
	for name, auditResult := range result.Audits {
		fmt.Printf("audit %-8s ok=%v chunks=%d findings=%d\n", name, auditResult.OK, len(auditResult.ChunkOrder), len(auditResult.Findings))
		if auditResult.First != nil {
			fmt.Printf("  first: %s\n", auditResult.First.String())
		}
	}
	if !result.Comparison.OK {
		os.Exit(1)
	}
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "out/sample", "output JSON file directory")
	seed := fs.Int64("seed", generator.DefaultSeed, "fixed seed")
	_ = fs.Parse(args)
	level, truth := generator.Generate(*seed)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	if err := checkpoint.WriteJSONAtomic(*out+"/level.json", level); err != nil {
		fatal(err)
	}
	if err := checkpoint.WriteJSONAtomic(*out+"/truth.json", truth); err != nil {
		fatal(err)
	}
	fmt.Printf("generated seed=%d chunks=%d bands=%d at %s\n", level.Seed, len(level.Chunks), len(level.BandIDs), *out)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	report := fs.String("report", "out/demo/report.json", "report.json path")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	_ = fs.Parse(args)
	fmt.Printf("diagnostic page: http://%s/ (report=%s)\n", *addr, *report)
	if err := http.ListenAndServe(*addr, web.NewFileHandler(*report)); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
