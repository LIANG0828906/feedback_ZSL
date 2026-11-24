package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"volumelab/pkg/samplegen"
)

func main() {
	var (
		outDir = flag.String("out", "samples", "output directory for deterministic volume files")
		seed   = flag.Int64("seed", 42, "deterministic sample seed")
		name   = flag.String("manifest", samplegen.DefaultManifestName, "manifest file name")
	)
	flag.Parse()
	result, err := samplegen.Generate(samplegen.Options{OutputDir: *outDir, Seed: *seed, ManifestName: *name})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample generation failed: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "cannot write result: %v\n", err)
		os.Exit(1)
	}
}
