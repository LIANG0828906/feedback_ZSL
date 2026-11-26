package main

import (
	"os"
	"path/filepath"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func commandGen(args []string) int {
	flags := newFlagSet("gen")
	output := flags.String("out", "samples", "output directory")
	seed := flags.Int64("seed", samplegen.DefaultSeed, "deterministic seed")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	bundle, err := samplegen.WriteBundle(*output, *seed)
	if err != nil {
		return fail(err, exitBusiness)
	}
	type fileSummary struct {
		Schema string `json:"schema"`
		Path   string `json:"path"`
		Hash   string `json:"hash"`
		Bytes  int    `json:"bytes"`
	}
	files := make([]fileSummary, 0, len(bundle.Files))
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		document, err := registry.LoadDocument(bundle.Files[schema])
		if err != nil {
			return fail(err, exitBusiness)
		}
		hash, err := document.Project.ModelHash()
		if err != nil {
			return fail(err, exitBusiness)
		}
		path := filepath.Join(*output, string(schema)+".json")
		if _, err := os.Stat(path); err != nil {
			return fail(err, exitBusiness)
		}
		files = append(files, fileSummary{Schema: string(schema), Path: path, Hash: hash, Bytes: len(bundle.Files[schema])})
	}
	return emit(map[string]any{
		"ok":              true,
		"command":         "gen",
		"seed":            *seed,
		"project_version": bundle.Project.ProjectVersion,
		"steps":           len(bundle.Truth.Steps),
		"files":           files,
	})
}
