// Command gen-samples writes deterministic schema documents, queries, and truth.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"dnslab/generator"
	"dnslab/spec"
)

func main() {
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	out := flag.String("out", "samples", "output directory")
	flag.Parse()

	bundle, err := generator.Generate(*seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	projectDir := filepath.Join(*out, "projects")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	files := map[string]string{
		spec.SchemaV1: "v1-flat-literal-seconds.json",
		spec.SchemaV2: "v2-flat-list-millis.json",
		spec.SchemaV3: "v3-tree-absolute-negative.json",
	}
	for schema, name := range files {
		path := filepath.Join(projectDir, name)
		if err := os.WriteFile(path, bundle.Docs[schema], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
	}
	writeJSON(filepath.Join(*out, "queries.json"), bundle.Queries)
	writeJSON(filepath.Join(*out, "truth.json"), bundle.Truth)
	writeJSON(filepath.Join(*out, "canonical-project.json"), bundle.Project)
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote", path)
}
