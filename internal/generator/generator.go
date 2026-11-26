package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"layerforge/internal/build"
	"layerforge/internal/model"
)

const (
	DefaultSeed      int64 = 42
	DefaultProjectID       = "fixture-atlas-service"
)

type Fixture struct {
	Seed       int64
	Project    model.Project
	Graph      *build.Graph
	Derivation *build.Derivation
	Versions   map[string][]byte
	Truth      []byte
}

type v1Manifest struct {
	Schema               string      `json:"schema"`
	ProjectID            string      `json:"projectId"`
	ProjectName          string      `json:"projectName"`
	Seed                 int64       `json:"seed"`
	Units                units       `json:"units"`
	BaseLayers           []v1Base    `json:"baseLayers"`
	Steps                []v1Step    `json:"steps"`
	Layers               []v1Layer   `json:"layers"`
	Shares               []v1Share   `json:"shares"`
	LatencyBudgetSeconds json.Number `json:"latencyBudgetSeconds"`
	MaxRounds            int         `json:"maxRounds"`
	ThreadLimit          int         `json:"threadLimit"`
	MemoryPerThreadBytes int64       `json:"memoryPerThreadBytes"`
	BufferPerThread      int         `json:"bufferPerThread"`
}

type v1Base struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"sizeBytes"`
	Digest    string `json:"digest"`
}

type v1Step struct {
	Number             int           `json:"number"`
	Name               string        `json:"name"`
	Inputs             []v1Input     `json:"inputFiles"`
	CommandFingerprint string        `json:"commandFingerprint"`
	DependsOn          []string      `json:"dependsOn"`
	Output             v1OutputLayer `json:"outputLayer"`
}

type v1Input struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type v1OutputLayer struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"sizeBytes"`
}

type v1Layer struct {
	ID           string `json:"id"`
	SizeBytes    int64  `json:"sizeBytes"`
	ProducerStep int    `json:"producerStep"`
}

type v1Share struct {
	LayerID   string `json:"layer"`
	Consumers []int  `json:"consumers"`
}

type units struct {
	Size string `json:"size"`
	Time string `json:"time"`
}

type v2Manifest struct {
	SchemaVersion       string      `json:"schemaVersion"`
	Project             v2Project   `json:"project"`
	Units               units       `json:"units"`
	BaseRefs            []v2Base    `json:"baseRefs"`
	Tasks               []v2Task    `json:"tasks"`
	LayerTree           v2LayerTree `json:"layerTree"`
	SharedLayers        []v2Share   `json:"sharedLayers"`
	InteractionBudgetMS int64       `json:"interactionBudgetMs"`
	MaxRounds           int         `json:"maxRounds"`
	ThreadLimit         int         `json:"threadLimit"`
	MemoryPerThreadKiB  int64       `json:"memoryPerThreadKiB"`
	BufferPerThread     int         `json:"bufferPerThread"`
}

type v2Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Seed int64  `json:"seed"`
}

type v2Base struct {
	ID      string `json:"id"`
	SizeKiB int64  `json:"sizeKiB"`
	Digest  string `json:"digest"`
}

type v2Task struct {
	Step               int           `json:"step"`
	Name               string        `json:"name"`
	Inputs             []v1Input     `json:"inputFiles"`
	CommandFingerprint string        `json:"commandFingerprint"`
	DependsOn          []string      `json:"dependsOn"`
	Output             v2OutputLayer `json:"output"`
}

type v2OutputLayer struct {
	Layer   string `json:"layer"`
	SizeKiB int64  `json:"sizeKiB"`
}

type v2LayerTree struct {
	Name     string    `json:"name"`
	Children []v2Group `json:"children"`
}

type v2Group struct {
	Name         string    `json:"name"`
	Layer        string    `json:"layer,omitempty"`
	SizeKiB      int64     `json:"sizeKiB,omitempty"`
	ProducerStep *int      `json:"producerStep,omitempty"`
	Children     []v2Group `json:"children,omitempty"`
}

type v2Share struct {
	LayerID   string `json:"layer"`
	Consumers []int  `json:"consumers"`
}

type v3Manifest struct {
	SchemaVersion        string        `json:"schemaVersion"`
	Manifest             v3Project     `json:"manifest"`
	Units                units         `json:"units"`
	CacheKeyDerivation   string        `json:"cacheKeyDerivation"`
	Bases                []v1Base      `json:"bases"`
	Operations           []v3Operation `json:"operations"`
	Layers               []v1Layer     `json:"layers"`
	Reuse                []v1Share     `json:"reuse"`
	LatencyBudgetMS      int64         `json:"latencyBudgetMs"`
	MaxRounds            int           `json:"maxRounds"`
	ThreadLimit          int           `json:"threadLimit"`
	MemoryPerThreadBytes int64         `json:"memoryPerThreadBytes"`
	BufferPerThread      int           `json:"bufferPerThread"`
}

type v3Project struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Seed      int64  `json:"seed"`
}

type v3Operation struct {
	Step               int           `json:"step"`
	Name               string        `json:"name"`
	Inputs             []v1Input     `json:"inputs"`
	CommandFingerprint string        `json:"commandFingerprint"`
	Requires           []string      `json:"requires"`
	Output             v1OutputLayer `json:"output"`
	CacheKeyDerivation string        `json:"cacheKeyDerivation"`
}

type truthFile struct {
	Seed        int64                  `json:"seed"`
	ProjectID   string                 `json:"projectId"`
	ProjectHash string                 `json:"projectHash"`
	Schemas     []string               `json:"schemas"`
	Steps       []build.StepDerivation `json:"steps"`
	Layers      []build.LayerTruth     `json:"layers"`
}

func Generate(seed int64) (*Fixture, error) {
	project := canonicalProject(seed)
	graph, derivation, err := build.DeriveProject(project)
	if err != nil {
		return nil, err
	}
	versions := map[string][]byte{
		"project.v1.json": mustJSON(toV1(project)),
		"project.v2.json": mustJSON(toV2(project)),
		"project.v3.json": mustJSON(toV3(project)),
	}
	truth := truthFrom(seed, project, derivation)
	return &Fixture{
		Seed:       seed,
		Project:    project,
		Graph:      graph,
		Derivation: derivation,
		Versions:   versions,
		Truth:      mustJSON(truth),
	}, nil
}

func (f *Fixture) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(f.Versions))
	for name := range f.Versions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeFile(filepath.Join(dir, name), f.Versions[name]); err != nil {
			return err
		}
	}
	return writeFile(filepath.Join(dir, "truth.json"), f.Truth)
}

func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func canonicalProject(seed int64) model.Project {
	base := func(id string, kib int64) model.BaseLayer {
		size := kib * 1024
		return model.BaseLayer{
			ID:        id,
			SizeBytes: size,
			Digest:    model.HashFields("generated-base/v1", strconv.FormatInt(seed, 10), id, strconv.FormatInt(size, 10)),
		}
	}
	inputs := func(paths ...string) []model.FileInput {
		out := make([]model.FileInput, 0, len(paths))
		for _, path := range paths {
			out = append(out, model.FileInput{
				Path:   path,
				Digest: model.HashFields("generated-input/v1", strconv.FormatInt(seed, 10), path),
			})
		}
		return out
	}
	step := func(number int, name string, fingerprint string, deps []string, output string, kib int64, paths ...string) model.Step {
		return model.Step{
			Number:             number,
			Name:               name,
			Inputs:             inputs(paths...),
			CommandFingerprint: model.HashFields("generated-command/v1", strconv.FormatInt(seed, 10), fingerprint),
			DependsOn:          append([]string(nil), deps...),
			Output:             model.Layer{ID: output, SizeBytes: kib * 1024},
		}
	}

	return model.Project{
		ID:   DefaultProjectID,
		Name: "atlas-service",
		Seed: seed,
		Bases: []model.BaseLayer{
			base("base:toolchain", 48),
			base("base:runtime", 24),
			base("base:vendor", 16),
		},
		Steps: []model.Step{
			step(1, "compile", "go build -trimpath ./cmd/atlas", []string{"base:toolchain", "base:runtime"}, "step:compile", 24,
				"src/main.go", "src/graph.go"),
			step(2, "test", "go test -race ./...", []string{"step:compile", "base:runtime"}, "step:test", 10,
				"tests/main_test.go", "tests/graph_test.go"),
			step(3, "lint", "staticcheck ./...", []string{"step:compile"}, "step:lint", 6,
				".staticcheck.conf", "src/graph.go"),
			step(4, "package", "go run ./tools/package -mode=tar", []string{"step:compile", "base:vendor"}, "step:package", 18,
				"deploy/package.json", "deploy/files.list"),
			step(5, "scan", "go run ./tools/scan -policy=strict", []string{"step:test", "step:package"}, "step:scan", 8,
				"security/policy.yaml", "security/allowlist.txt"),
			step(6, "image", "go run ./tools/image -reproducible", []string{"step:package", "step:scan", "base:runtime"}, "step:image", 16,
				"deploy/image.json", "deploy/labels.json"),
			step(7, "docs", "go run ./tools/docs -check", []string{"base:runtime"}, "step:docs", 5,
				"docs/index.md", "docs/api.md"),
			step(8, "release", "go run ./tools/release -verify", []string{"step:image", "step:docs", "step:test"}, "step:release", 12,
				"release/manifest.yaml", "release/notes.md"),
		},
		Shares: []model.Share{
			{LayerID: "base:runtime", Consumers: []int{1, 2, 6, 7}},
			{LayerID: "step:compile", Consumers: []int{2, 3, 4}},
			{LayerID: "step:test", Consumers: []int{5, 8}},
			{LayerID: "step:package", Consumers: []int{5, 6}},
		},
		Budget: model.Budget{
			Latency:         17 * 1000 * 1000, // 17ms
			MaxRounds:       6,
			ThreadLimit:     2,
			MemoryPerThread: 64 * 1024,
			BufferPerThread: 2,
		},
	}
}

func toV1(project model.Project) v1Manifest {
	manifest := v1Manifest{
		Schema:               "layerforge/v1",
		ProjectID:            project.ID,
		ProjectName:          project.Name,
		Seed:                 project.Seed,
		Units:                units{Size: "bytes", Time: "seconds"},
		LatencyBudgetSeconds: json.Number(formatSeconds(int64(project.Budget.Latency))),
		MaxRounds:            project.Budget.MaxRounds,
		ThreadLimit:          project.Budget.ThreadLimit,
		MemoryPerThreadBytes: project.Budget.MemoryPerThread,
		BufferPerThread:      project.Budget.BufferPerThread,
	}
	for _, base := range project.Bases {
		manifest.BaseLayers = append(manifest.BaseLayers, v1Base{ID: base.ID, SizeBytes: base.SizeBytes, Digest: base.Digest})
	}
	for _, step := range project.Steps {
		entry := v1Step{
			Number:             step.Number,
			Name:               step.Name,
			CommandFingerprint: step.CommandFingerprint,
			DependsOn:          append([]string(nil), step.DependsOn...),
			Output:             v1OutputLayer{ID: step.Output.ID, SizeBytes: step.Output.SizeBytes},
		}
		for _, input := range step.Inputs {
			entry.Inputs = append(entry.Inputs, v1Input{Path: input.Path, Digest: input.Digest})
		}
		manifest.Steps = append(manifest.Steps, entry)
	}
	manifest.Layers = flatLayers(project)
	for _, share := range project.Shares {
		manifest.Shares = append(manifest.Shares, v1Share{LayerID: share.LayerID, Consumers: append([]int(nil), share.Consumers...)})
	}
	return manifest
}

func toV2(project model.Project) v2Manifest {
	manifest := v2Manifest{
		SchemaVersion:       "v2",
		Project:             v2Project{ID: project.ID, Name: project.Name, Seed: project.Seed},
		Units:               units{Size: "kib", Time: "milliseconds"},
		InteractionBudgetMS: project.Budget.Latency.Milliseconds(),
		MaxRounds:           project.Budget.MaxRounds,
		ThreadLimit:         project.Budget.ThreadLimit,
		MemoryPerThreadKiB:  project.Budget.MemoryPerThread / 1024,
		BufferPerThread:     project.Budget.BufferPerThread,
	}
	for _, base := range project.Bases {
		manifest.BaseRefs = append(manifest.BaseRefs, v2Base{ID: base.ID, SizeKiB: base.SizeBytes / 1024, Digest: base.Digest})
	}
	for _, step := range project.Steps {
		task := v2Task{
			Step:               step.Number,
			Name:               step.Name,
			CommandFingerprint: step.CommandFingerprint,
			DependsOn:          append([]string(nil), step.DependsOn...),
			Output:             v2OutputLayer{Layer: step.Output.ID, SizeKiB: step.Output.SizeBytes / 1024},
		}
		for _, input := range step.Inputs {
			task.Inputs = append(task.Inputs, v1Input{Path: input.Path, Digest: input.Digest})
		}
		manifest.Tasks = append(manifest.Tasks, task)
	}
	manifest.LayerTree = v2LayerTree{
		Name: "artifact-root",
		Children: []v2Group{
			{Name: "base", Children: layerTreeLeaves(project, true)},
			{Name: "steps", Children: layerTreeLeaves(project, false)},
		},
	}
	for _, share := range project.Shares {
		manifest.SharedLayers = append(manifest.SharedLayers, v2Share{LayerID: share.LayerID, Consumers: append([]int(nil), share.Consumers...)})
	}
	return manifest
}

func toV3(project model.Project) v3Manifest {
	manifest := v3Manifest{
		SchemaVersion:        "v3",
		Manifest:             v3Project{ProjectID: project.ID, Name: project.Name, Seed: project.Seed},
		Units:                units{Size: "bytes", Time: "milliseconds"},
		CacheKeyDerivation:   model.CacheKeyAlgorithm,
		LatencyBudgetMS:      project.Budget.Latency.Milliseconds(),
		MaxRounds:            project.Budget.MaxRounds,
		ThreadLimit:          project.Budget.ThreadLimit,
		MemoryPerThreadBytes: project.Budget.MemoryPerThread,
		BufferPerThread:      project.Budget.BufferPerThread,
	}
	for _, base := range project.Bases {
		manifest.Bases = append(manifest.Bases, v1Base{ID: base.ID, SizeBytes: base.SizeBytes, Digest: base.Digest})
	}
	for _, step := range project.Steps {
		operation := v3Operation{
			Step:               step.Number,
			Name:               step.Name,
			CommandFingerprint: step.CommandFingerprint,
			Requires:           append([]string(nil), step.DependsOn...),
			Output:             v1OutputLayer{ID: step.Output.ID, SizeBytes: step.Output.SizeBytes},
			CacheKeyDerivation: model.CacheKeyAlgorithm,
		}
		for _, input := range step.Inputs {
			operation.Inputs = append(operation.Inputs, v1Input{Path: input.Path, Digest: input.Digest})
		}
		manifest.Operations = append(manifest.Operations, operation)
	}
	manifest.Layers = flatLayers(project)
	for _, share := range project.Shares {
		manifest.Reuse = append(manifest.Reuse, v1Share{LayerID: share.LayerID, Consumers: append([]int(nil), share.Consumers...)})
	}
	return manifest
}

func flatLayers(project model.Project) []v1Layer {
	layers := make([]v1Layer, 0, len(project.Bases)+len(project.Steps))
	for _, base := range project.Bases {
		layers = append(layers, v1Layer{ID: base.ID, SizeBytes: base.SizeBytes, ProducerStep: 0})
	}
	for _, step := range project.Steps {
		layers = append(layers, v1Layer{ID: step.Output.ID, SizeBytes: step.Output.SizeBytes, ProducerStep: step.Number})
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].ID < layers[j].ID })
	return layers
}

func layerTreeLeaves(project model.Project, bases bool) []v2Group {
	var leaves []v2Group
	if bases {
		for _, base := range project.Bases {
			producer := 0
			leaves = append(leaves, v2Group{Name: base.ID, Layer: base.ID, SizeKiB: base.SizeBytes / 1024, ProducerStep: &producer})
		}
	} else {
		for _, step := range project.Steps {
			producer := step.Number
			leaves = append(leaves, v2Group{Name: step.Output.ID, Layer: step.Output.ID, SizeKiB: step.Output.SizeBytes / 1024, ProducerStep: &producer})
		}
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].Layer < leaves[j].Layer })
	return leaves
}

func truthFrom(seed int64, project model.Project, derivation *build.Derivation) truthFile {
	truth := truthFile{
		Seed:        seed,
		ProjectID:   project.ID,
		ProjectHash: derivation.ProjectHash,
		Schemas:     []string{"layerforge/v1", "v2", "v3"},
	}
	for _, step := range derivation.Steps {
		truth.Steps = append(truth.Steps, step)
	}
	sort.Slice(truth.Steps, func(i, j int) bool { return truth.Steps[i].Step < truth.Steps[j].Step })
	for _, layer := range derivation.Layers {
		truth.Layers = append(truth.Layers, layer)
	}
	sort.Slice(truth.Layers, func(i, j int) bool { return truth.Layers[i].ID < truth.Layers[j].ID })
	return truth
}

func formatSeconds(duration int64) string {
	seconds := duration / 1e9
	nanos := duration % 1e9
	if nanos == 0 {
		return strconv.FormatInt(seconds, 10)
	}
	return fmt.Sprintf("%d.%09d", seconds, nanos)
}

func mustJSON(value any) []byte {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}
