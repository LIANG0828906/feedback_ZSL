package main

import (
	"bytes"
	"encoding/json"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

type versionSummary struct {
	Schema         string `json:"schema"`
	ModelHash      string `json:"model_hash"`
	Steps          int    `json:"steps"`
	TruthMatches   bool   `json:"truth_matches"`
	HealthChanges  int    `json:"health_transitions"`
	Queries        int    `json:"queries"`
	DeferredProbes int    `json:"deferred_probes"`
	DroppedProbes  int    `json:"dropped_probes"`
}

func commandCompare(args []string) int {
	flags := newFlagSet("compare")
	samplesDir := flags.String("samples", "samples", "sample directory (generated in memory if absent)")
	seed := flags.Int64("seed", samplegen.DefaultSeed, "sample seed")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	bundle, err := loadBundle(*samplesDir, *seed)
	if err != nil {
		return fail(err, exitBusiness)
	}
	baselineHash := ""
	var baselineTruth []byte
	summaries := make([]versionSummary, 0, 3)
	allMatch := true
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		document, err := registry.LoadDocument(bundle.Files[schema])
		if err != nil {
			return fail(err, exitBusiness)
		}
		hash, err := document.Project.ModelHash()
		if err != nil {
			return fail(err, exitBusiness)
		}
		if baselineHash == "" {
			baselineHash = hash
		}
		run, err := registry.RunProject(document.Project, registry.RunOptions{})
		if err != nil {
			return fail(err, exitBusiness)
		}
		actual := run.Truth()
		actualJSON, _ := json.Marshal(actual)
		if baselineTruth == nil {
			baselineTruth = actualJSON
		}
		truthJSON, _ := json.Marshal(document.Truth)
		matchesTruth := bytes.Equal(actualJSON, truthJSON)
		matchesBaseline := bytes.Equal(actualJSON, baselineTruth)
		summary := versionSummary{
			Schema:       string(schema),
			ModelHash:    hash,
			Steps:        len(actual.Steps),
			TruthMatches: matchesTruth && matchesBaseline,
		}
		for _, step := range actual.Steps {
			summary.HealthChanges += len(step.Transitions)
			summary.Queries += len(step.Queries)
			summary.DeferredProbes += len(step.DeferredProbes)
			summary.DroppedProbes += len(step.DroppedProbes)
		}
		if !summary.TruthMatches || hash != baselineHash {
			allMatch = false
		}
		summaries = append(summaries, summary)
	}
	report := map[string]any{
		"ok":              allMatch,
		"command":         "compare",
		"project_version": bundle.Project.ProjectVersion,
		"model_hash":      baselineHash,
		"versions":        summaries,
	}
	if !allMatch {
		report["code"] = registry.CodeVerificationFailed
	}
	if err := jsonEncodeStdout(report); err != nil {
		return fail(err, exitInternal)
	}
	if !allMatch {
		return exitBusiness
	}
	return exitOK
}
