// Command gensample renders deterministic three-schema project samples and
// their parsed snapshots into the workspace. It never calls any cloud API.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"coldkernel/internal/gen"
	"coldkernel/internal/sample"
)

func main() {
	outDir := flag.String("out", "testdata/samples", "output directory")
	seed := flag.Uint64("seed", 20261126, "deterministic generator seed")
	flag.Parse()

	if err := run(*outDir, *seed); err != nil {
		fmt.Fprintf(os.Stderr, "gensample: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string, seed uint64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	triple, err := sample.Build(seed)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	spec := gen.Generate(seed)
	files := map[string][]byte{
		"project_v1.json": triple.RawV1,
		"project_v2.json": triple.RawV2,
		"project_v3.json": triple.RawV3,
	}
	specRaw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	files["canonical_spec.json"] = specRaw
	for name, raw := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(raw))
	}
	fmt.Printf("functions=%d requests=%d horizon_ticks=%d\n",
		len(triple.ProjectV1.Functions), len(triple.ProjectV1.Requests),
		triple.ProjectV1.HorizonTicks)
	return nil
}
