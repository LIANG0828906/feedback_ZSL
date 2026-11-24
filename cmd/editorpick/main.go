package main

import (
	"flag"
	"fmt"
	"os"

	"editorpick/internal/samplegen"
	"editorpick/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		fs := flag.NewFlagSet("generate", flag.ExitOnError)
		dir := fs.String("dir", "samples/default", "sample output directory")
		seed := fs.Int64("seed", samplegen.DefaultSeed, "deterministic seed")
		_ = fs.Parse(os.Args[2:])
		if err := samplegen.Generate(*dir, *seed); err != nil {
			fatal(err)
		}
		fmt.Printf("generated deterministic sample in %s (seed=%d)\n", *dir, *seed)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		dir := fs.String("dir", "samples/default", "sample directory")
		out := fs.String("report", "samples/default/verification-report.json", "JSON report output path")
		_ = fs.Parse(os.Args[2:])
		report, err := verify.Run(*dir, *out)
		if err != nil {
			fatal(err)
		}
		if !report.OK {
			fmt.Fprintf(os.Stderr, "verification failed: %s\n", report.FirstFailure)
			os.Exit(1)
		}
		fmt.Printf("verification passed: %s\n", *out)
	case "demo":
		fs := flag.NewFlagSet("demo", flag.ExitOnError)
		dir := fs.String("dir", "samples/default", "sample directory")
		_ = fs.Parse(os.Args[2:])
		if err := verify.Demo(*dir); err != nil {
			fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: editorpick <generate|verify|demo> [flags]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
