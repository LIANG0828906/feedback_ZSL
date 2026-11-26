package build

import (
	"sort"
	"strconv"

	"layerforge/internal/apperr"
	"layerforge/internal/model"
)

type StepDerivation struct {
	Step        int      `json:"step"`
	Name        string   `json:"name"`
	LayerID     string   `json:"layerId"`
	SizeBytes   int64    `json:"sizeBytes"`
	CacheKey    string   `json:"cacheKey"`
	LayerDigest string   `json:"layerDigest"`
	DependsOn   []string `json:"dependsOn"`
}

type LayerTruth struct {
	ID           string `json:"id"`
	SizeBytes    int64  `json:"sizeBytes"`
	Digest       string `json:"digest"`
	ProducerStep int    `json:"producerStep"`
	CacheKey     string `json:"cacheKey,omitempty"`
}

type Derivation struct {
	ProjectHash string                 `json:"projectHash"`
	Steps       map[int]StepDerivation `json:"steps"`
	Layers      map[string]LayerTruth  `json:"layers"`
}

func Derive(g *Graph) (*Derivation, error) {
	result := &Derivation{
		ProjectHash: model.SemanticHash(g.Project),
		Steps:       make(map[int]StepDerivation, len(g.Steps)),
		Layers:      make(map[string]LayerTruth, len(g.Layers)),
	}
	for _, base := range g.Project.Bases {
		result.Layers[base.ID] = LayerTruth{
			ID:           base.ID,
			SizeBytes:    base.SizeBytes,
			Digest:       base.Digest,
			ProducerStep: 0,
		}
	}
	for _, number := range g.Topo {
		step := g.Steps[number]
		inputs := append([]model.FileInput(nil), step.Inputs...)
		sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
		dependencies := append([]string(nil), step.DependsOn...)
		sort.Strings(dependencies)
		fields := []string{step.Name, step.CommandFingerprint}
		for _, input := range inputs {
			fields = append(fields, "input", input.Path, input.Digest)
		}
		for _, dependency := range dependencies {
			truth, ok := result.Layers[dependency]
			if !ok {
				return nil, apperr.WithLayers(
					apperr.WithSteps(apperr.New(apperr.GraphMissingRef, "derive-cache-key", "steps.dependsOn", "dependency has no derived layer digest"), step.Number),
					dependency,
				)
			}
			fields = append(fields, "dependency", dependency, truth.Digest)
		}
		cacheKey := model.HashFields(model.CacheKeyAlgorithm, fields...)
		digest := model.HashFields(
			"layerforge-layer/v1",
			step.Output.ID,
			strconv.FormatInt(step.Output.SizeBytes, 10),
			cacheKey,
		)
		derivation := StepDerivation{
			Step:        step.Number,
			Name:        step.Name,
			LayerID:     step.Output.ID,
			SizeBytes:   step.Output.SizeBytes,
			CacheKey:    cacheKey,
			LayerDigest: digest,
			DependsOn:   dependencies,
		}
		result.Steps[number] = derivation
		result.Layers[step.Output.ID] = LayerTruth{
			ID:           step.Output.ID,
			SizeBytes:    step.Output.SizeBytes,
			Digest:       digest,
			ProducerStep: step.Number,
			CacheKey:     cacheKey,
		}
	}
	return result, nil
}

func DeriveProject(project model.Project) (*Graph, *Derivation, error) {
	graph, err := BuildGraph(project)
	if err != nil {
		return nil, nil, err
	}
	derivation, err := Derive(graph)
	if err != nil {
		return nil, nil, err
	}
	return graph, derivation, nil
}

func CompareTruth(actual *Derivation, expected *Derivation) error {
	if actual.ProjectHash != expected.ProjectHash {
		return apperr.New(apperr.CheckpointIntegrity, "compare-truth", "projectHash", "semantic project hash differs")
	}
	if len(actual.Steps) != len(expected.Steps) {
		return apperr.New(apperr.CheckpointIntegrity, "compare-truth", "steps", "step count differs from generator truth")
	}
	for number, want := range expected.Steps {
		got, ok := actual.Steps[number]
		if !ok {
			return apperr.WithSteps(apperr.New(apperr.CheckpointIntegrity, "compare-truth", "steps", "step missing from parsed project"), number)
		}
		if got.CacheKey != want.CacheKey {
			return apperr.WithLayers(apperr.WithSteps(apperr.New(apperr.CheckpointIntegrity, "compare-truth", "cacheKey", "cache key differs from truth"), number), want.LayerID)
		}
		if got.LayerDigest != want.LayerDigest {
			return apperr.WithLayers(apperr.WithSteps(apperr.New(apperr.CheckpointIntegrity, "compare-truth", "layerDigest", "layer digest differs from truth"), number), want.LayerID)
		}
	}
	for layerID, want := range expected.Layers {
		got, ok := actual.Layers[layerID]
		if !ok || got.Digest != want.Digest || got.SizeBytes != want.SizeBytes {
			return apperr.WithLayers(apperr.New(apperr.CheckpointIntegrity, "compare-truth", "layers", "layer metadata differs from truth"), layerID)
		}
	}
	return nil
}
