package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/model"
)

type Result struct {
	Project    model.Project
	Graph      *build.Graph
	Schema     string
	SourcePath string
}

func LoadFile(path string) (*Result, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CheckpointIO, "load-manifest", path, err)
	}
	result, err := Load(data)
	if err != nil {
		return nil, err
	}
	result.SourcePath = path
	return result, nil
}

func Load(data []byte) (result *Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if businessError, ok := recovered.(*apperr.Error); ok {
				result = nil
				err = businessError
				return
			}
			panic(recovered)
		}
	}()
	doc, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	schema, err := detectSchema(doc)
	if err != nil {
		return nil, err
	}
	var project model.Project
	switch schema {
	case "layerforge/v1":
		project, err = parseV1(doc)
	case "v2":
		project, err = parseV2(doc)
	case "v3":
		project, err = parseV3(doc)
	default:
		return nil, apperr.New(apperr.SchemaVersion, "detect-schema", "schema", "unsupported schema version")
	}
	if err != nil {
		return nil, err
	}
	graph, err := build.BuildGraph(project)
	if err != nil {
		return nil, err
	}
	return &Result{Project: project, Graph: graph, Schema: schema}, nil
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, apperr.New(apperr.SchemaType, "decode-json", "$", "invalid JSON: "+err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, apperr.New(apperr.SchemaType, "decode-json", "$", "multiple top-level JSON values")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, apperr.New(apperr.SchemaType, "decode-json", "$", "top-level value must be an object")
	}
	return obj, nil
}

func detectSchema(doc map[string]any) (string, error) {
	raw, hasSchema := doc["schema"]
	rawVersion, hasVersion := doc["schemaVersion"]
	if hasSchema && hasVersion {
		return "", apperr.New(apperr.SchemaVersion, "detect-schema", "$", "schema and schemaVersion cannot coexist")
	}
	if !hasSchema && !hasVersion {
		return "", apperr.New(apperr.SchemaVersion, "detect-schema", "$", "missing schema discriminator")
	}
	if hasSchema {
		value, ok := raw.(string)
		if !ok || value != "layerforge/v1" {
			return "", apperr.New(apperr.SchemaVersion, "detect-schema", "schema", "expected layerforge/v1")
		}
		return value, nil
	}
	value, ok := rawVersion.(string)
	if !ok || (value != "v2" && value != "v3") {
		return "", apperr.New(apperr.SchemaVersion, "detect-schema", "schemaVersion", "expected v2 or v3")
	}
	return value, nil
}

func parseV1(doc map[string]any) (model.Project, error) {
	allowed := []string{
		"schema", "projectId", "projectName", "seed", "units", "baseLayers", "steps", "layers", "shares",
		"latencyBudgetSeconds", "maxRounds", "threadLimit", "memoryPerThreadBytes", "bufferPerThread",
	}
	if err := checkFields(doc, "$", allowed); err != nil {
		return model.Project{}, err
	}
	if err := checkUnits(doc, "seconds", "bytes"); err != nil {
		return model.Project{}, err
	}
	project := model.Project{
		ID:   mustString(doc, "projectId", "$"),
		Name: mustString(doc, "projectName", "$"),
	}
	var err error
	if project.Seed, err = mustInt(doc, "seed", "$"); err != nil {
		return model.Project{}, err
	}
	baseArray, err := mustArray(doc, "baseLayers", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range baseArray {
		path := fmt.Sprintf("$.baseLayers[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		if err := checkFields(obj, path, []string{"id", "sizeBytes", "digest"}); err != nil {
			return model.Project{}, err
		}
		project.Bases = append(project.Bases, model.BaseLayer{
			ID:        mustString(obj, "id", path),
			SizeBytes: mustIntDefault(obj, "sizeBytes", path),
			Digest:    mustString(obj, "digest", path),
		})
	}
	stepArray, err := mustArray(doc, "steps", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range stepArray {
		path := fmt.Sprintf("$.steps[%d]", i)
		step, err := parseV1Step(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		project.Steps = append(project.Steps, step)
	}
	layers, err := parseFlatLayers(doc, "layers", "$", "sizeBytes")
	if err != nil {
		return model.Project{}, err
	}
	if err := validateLayerMetadata(project, layers, "$.layers"); err != nil {
		return model.Project{}, err
	}
	if project.Shares, err = parseShares(doc, "shares", "$"); err != nil {
		return model.Project{}, err
	}
	seconds, err := mustJSONNumber(doc, "latencyBudgetSeconds", "$")
	if err != nil {
		return model.Project{}, err
	}
	millis, err := decimalSecondsToMillis(seconds.String(), "$.latencyBudgetSeconds")
	if err != nil {
		return model.Project{}, err
	}
	if project.Budget, err = parseBudget(doc, "$", millis, 1, "memoryPerThreadBytes"); err != nil {
		return model.Project{}, err
	}
	return project, nil
}

func parseV1Step(raw any, path string) (model.Step, error) {
	obj, err := asObject(raw, path)
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(obj, path, []string{"number", "name", "inputFiles", "commandFingerprint", "dependsOn", "outputLayer"}); err != nil {
		return model.Step{}, err
	}
	number, err := mustInt(obj, "number", path)
	if err != nil {
		return model.Step{}, err
	}
	inputs, err := parseInputs(obj, "inputFiles", path)
	if err != nil {
		return model.Step{}, err
	}
	deps, err := parseStringArray(obj, "dependsOn", path)
	if err != nil {
		return model.Step{}, err
	}
	outputRaw, err := required(obj, "outputLayer", path)
	if err != nil {
		return model.Step{}, err
	}
	outputObj, err := asObject(outputRaw, path+".outputLayer")
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(outputObj, path+".outputLayer", []string{"id", "sizeBytes"}); err != nil {
		return model.Step{}, err
	}
	return model.Step{
		Number:             int(number),
		Name:               mustString(obj, "name", path),
		Inputs:             inputs,
		CommandFingerprint: mustString(obj, "commandFingerprint", path),
		DependsOn:          deps,
		Output: model.Layer{
			ID:        mustString(outputObj, "id", path+".outputLayer"),
			SizeBytes: mustIntDefault(outputObj, "sizeBytes", path+".outputLayer"),
		},
	}, nil
}

func parseV2(doc map[string]any) (model.Project, error) {
	allowed := []string{
		"schemaVersion", "project", "units", "baseRefs", "tasks", "layerTree", "sharedLayers",
		"interactionBudgetMs", "maxRounds", "threadLimit", "memoryPerThreadKiB", "bufferPerThread",
	}
	if err := checkFields(doc, "$", allowed); err != nil {
		return model.Project{}, err
	}
	if err := checkUnits(doc, "milliseconds", "kib"); err != nil {
		return model.Project{}, err
	}
	projectRaw, err := required(doc, "project", "$")
	if err != nil {
		return model.Project{}, err
	}
	projectObj, err := asObject(projectRaw, "$.project")
	if err != nil {
		return model.Project{}, err
	}
	if err := checkFields(projectObj, "$.project", []string{"id", "name", "seed"}); err != nil {
		return model.Project{}, err
	}
	project := model.Project{
		ID:   mustString(projectObj, "id", "$.project"),
		Name: mustString(projectObj, "name", "$.project"),
	}
	if project.Seed, err = mustInt(projectObj, "seed", "$.project"); err != nil {
		return model.Project{}, err
	}
	baseArray, err := mustArray(doc, "baseRefs", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range baseArray {
		path := fmt.Sprintf("$.baseRefs[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		if err := checkFields(obj, path, []string{"id", "sizeKiB", "digest"}); err != nil {
			return model.Project{}, err
		}
		kib, err := mustInt(obj, "sizeKiB", path)
		if err != nil {
			return model.Project{}, err
		}
		if kib <= 0 {
			return model.Project{}, apperr.New(apperr.SchemaUnit, "parse-v2", path+".sizeKiB", "size must be positive")
		}
		sizeBytes, err := multiplyUnit(kib, 1024, path+".sizeKiB")
		if err != nil {
			return model.Project{}, err
		}
		project.Bases = append(project.Bases, model.BaseLayer{
			ID:        mustString(obj, "id", path),
			SizeBytes: sizeBytes,
			Digest:    mustString(obj, "digest", path),
		})
	}
	taskArray, err := mustArray(doc, "tasks", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range taskArray {
		path := fmt.Sprintf("$.tasks[%d]", i)
		step, err := parseV2Task(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		project.Steps = append(project.Steps, step)
	}
	treeRaw, err := required(doc, "layerTree", "$")
	if err != nil {
		return model.Project{}, err
	}
	tree, err := asObject(treeRaw, "$.layerTree")
	if err != nil {
		return model.Project{}, err
	}
	if err := checkFields(tree, "$.layerTree", []string{"name", "children"}); err != nil {
		return model.Project{}, err
	}
	var layers []layerMetadata
	children, err := mustArray(tree, "children", "$.layerTree")
	if err != nil {
		return model.Project{}, err
	}
	for i, child := range children {
		flattened, err := flattenV2Group(child, fmt.Sprintf("$.layerTree.children[%d]", i))
		if err != nil {
			return model.Project{}, err
		}
		layers = append(layers, flattened...)
	}
	if err := validateLayerMetadata(project, layers, "$.layerTree"); err != nil {
		return model.Project{}, err
	}
	if project.Shares, err = parseShares(doc, "sharedLayers", "$"); err != nil {
		return model.Project{}, err
	}
	millis, err := mustInt(doc, "interactionBudgetMs", "$")
	if err != nil {
		return model.Project{}, err
	}
	if project.Budget, err = parseBudget(doc, "$", millis, 1024, "memoryPerThreadKiB"); err != nil {
		return model.Project{}, err
	}
	return project, nil
}

func parseV2Task(raw any, path string) (model.Step, error) {
	obj, err := asObject(raw, path)
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(obj, path, []string{"step", "name", "inputFiles", "commandFingerprint", "dependsOn", "output"}); err != nil {
		return model.Step{}, err
	}
	number, err := mustInt(obj, "step", path)
	if err != nil {
		return model.Step{}, err
	}
	inputs, err := parseInputs(obj, "inputFiles", path)
	if err != nil {
		return model.Step{}, err
	}
	deps, err := parseStringArray(obj, "dependsOn", path)
	if err != nil {
		return model.Step{}, err
	}
	outputRaw, err := required(obj, "output", path)
	if err != nil {
		return model.Step{}, err
	}
	outputObj, err := asObject(outputRaw, path+".output")
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(outputObj, path+".output", []string{"layer", "sizeKiB"}); err != nil {
		return model.Step{}, err
	}
	kib, err := mustInt(outputObj, "sizeKiB", path+".output")
	if err != nil {
		return model.Step{}, err
	}
	if kib <= 0 {
		return model.Step{}, apperr.New(apperr.SchemaUnit, "parse-v2", path+".output.sizeKiB", "size must be positive")
	}
	sizeBytes, err := multiplyUnit(kib, 1024, path+".output.sizeKiB")
	if err != nil {
		return model.Step{}, err
	}
	return model.Step{
		Number:             int(number),
		Name:               mustString(obj, "name", path),
		Inputs:             inputs,
		CommandFingerprint: mustString(obj, "commandFingerprint", path),
		DependsOn:          deps,
		Output: model.Layer{
			ID:        mustString(outputObj, "layer", path+".output"),
			SizeBytes: sizeBytes,
		},
	}, nil
}

func flattenV2Group(raw any, path string) ([]layerMetadata, error) {
	obj, err := asObject(raw, path)
	if err != nil {
		return nil, err
	}
	_, hasLayer := obj["layer"]
	_, hasChildren := obj["children"]
	switch {
	case hasLayer && hasChildren:
		return nil, apperr.New(apperr.SchemaField, "parse-v2-tree", path, "node cannot have both layer and children")
	case !hasLayer && !hasChildren:
		return nil, apperr.New(apperr.SchemaRequired, "parse-v2-tree", path, "node requires layer or children")
	case hasLayer:
		if err := checkFields(obj, path, []string{"name", "layer", "sizeKiB", "producerStep"}); err != nil {
			return nil, err
		}
		kib, err := mustInt(obj, "sizeKiB", path)
		if err != nil {
			return nil, err
		}
		producer, err := mustInt(obj, "producerStep", path)
		if err != nil {
			return nil, err
		}
		return []layerMetadata{{
			ID:           mustString(obj, "layer", path),
			SizeBytes:    kib * 1024,
			ProducerStep: int(producer),
			Path:         path,
		}}, nil
	default:
		if err := checkFields(obj, path, []string{"name", "children"}); err != nil {
			return nil, err
		}
		children, err := mustArray(obj, "children", path)
		if err != nil {
			return nil, err
		}
		var out []layerMetadata
		for i, child := range children {
			nested, err := flattenV2Group(child, fmt.Sprintf("%s.children[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		}
		return out, nil
	}
}

func parseV3(doc map[string]any) (model.Project, error) {
	allowed := []string{
		"schemaVersion", "manifest", "units", "cacheKeyDerivation", "bases", "operations", "layers", "reuse",
		"latencyBudgetMs", "maxRounds", "threadLimit", "memoryPerThreadBytes", "bufferPerThread",
	}
	if err := checkFields(doc, "$", allowed); err != nil {
		return model.Project{}, err
	}
	if err := checkUnits(doc, "milliseconds", "bytes"); err != nil {
		return model.Project{}, err
	}
	algorithm, err := mustStringValue(doc, "cacheKeyDerivation", "$")
	if err != nil {
		return model.Project{}, err
	}
	if algorithm != model.CacheKeyAlgorithm {
		return model.Project{}, apperr.New(apperr.SchemaCacheKey, "parse-v3", "$.cacheKeyDerivation", "unsupported cache key derivation")
	}
	manifestRaw, err := required(doc, "manifest", "$")
	if err != nil {
		return model.Project{}, err
	}
	manifestObj, err := asObject(manifestRaw, "$.manifest")
	if err != nil {
		return model.Project{}, err
	}
	if err := checkFields(manifestObj, "$.manifest", []string{"projectId", "name", "seed"}); err != nil {
		return model.Project{}, err
	}
	project := model.Project{
		ID:   mustString(manifestObj, "projectId", "$.manifest"),
		Name: mustString(manifestObj, "name", "$.manifest"),
	}
	if project.Seed, err = mustInt(manifestObj, "seed", "$.manifest"); err != nil {
		return model.Project{}, err
	}
	bases, err := mustArray(doc, "bases", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range bases {
		path := fmt.Sprintf("$.bases[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		if err := checkFields(obj, path, []string{"id", "sizeBytes", "digest"}); err != nil {
			return model.Project{}, err
		}
		project.Bases = append(project.Bases, model.BaseLayer{
			ID:        mustString(obj, "id", path),
			SizeBytes: mustIntDefault(obj, "sizeBytes", path),
			Digest:    mustString(obj, "digest", path),
		})
	}
	operations, err := mustArray(doc, "operations", "$")
	if err != nil {
		return model.Project{}, err
	}
	for i, raw := range operations {
		path := fmt.Sprintf("$.operations[%d]", i)
		step, err := parseV3Operation(raw, path)
		if err != nil {
			return model.Project{}, err
		}
		project.Steps = append(project.Steps, step)
	}
	layers, err := parseFlatLayers(doc, "layers", "$", "sizeBytes")
	if err != nil {
		return model.Project{}, err
	}
	if err := validateLayerMetadata(project, layers, "$.layers"); err != nil {
		return model.Project{}, err
	}
	if project.Shares, err = parseShares(doc, "reuse", "$"); err != nil {
		return model.Project{}, err
	}
	millis, err := mustInt(doc, "latencyBudgetMs", "$")
	if err != nil {
		return model.Project{}, err
	}
	if project.Budget, err = parseBudget(doc, "$", millis, 1, "memoryPerThreadBytes"); err != nil {
		return model.Project{}, err
	}
	return project, nil
}

func parseV3Operation(raw any, path string) (model.Step, error) {
	obj, err := asObject(raw, path)
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(obj, path, []string{"step", "name", "inputs", "commandFingerprint", "requires", "output", "cacheKeyDerivation"}); err != nil {
		return model.Step{}, err
	}
	algorithm, err := mustStringValue(obj, "cacheKeyDerivation", path)
	if err != nil {
		return model.Step{}, err
	}
	if algorithm != model.CacheKeyAlgorithm {
		return model.Step{}, apperr.New(apperr.SchemaCacheKey, "parse-v3", path+".cacheKeyDerivation", "unsupported cache key derivation")
	}
	number, err := mustInt(obj, "step", path)
	if err != nil {
		return model.Step{}, err
	}
	inputs, err := parseInputs(obj, "inputs", path)
	if err != nil {
		return model.Step{}, err
	}
	deps, err := parseStringArray(obj, "requires", path)
	if err != nil {
		return model.Step{}, err
	}
	outputRaw, err := required(obj, "output", path)
	if err != nil {
		return model.Step{}, err
	}
	outputObj, err := asObject(outputRaw, path+".output")
	if err != nil {
		return model.Step{}, err
	}
	if err := checkFields(outputObj, path+".output", []string{"id", "sizeBytes"}); err != nil {
		return model.Step{}, err
	}
	return model.Step{
		Number:             int(number),
		Name:               mustString(obj, "name", path),
		Inputs:             inputs,
		CommandFingerprint: mustString(obj, "commandFingerprint", path),
		DependsOn:          deps,
		Output: model.Layer{
			ID:        mustString(outputObj, "id", path+".output"),
			SizeBytes: mustIntDefault(outputObj, "sizeBytes", path+".output"),
		},
	}, nil
}

type layerMetadata struct {
	ID           string
	SizeBytes    int64
	ProducerStep int
	Path         string
}

func parseFlatLayers(doc map[string]any, field, basePath, sizeField string) ([]layerMetadata, error) {
	array, err := mustArray(doc, field, basePath)
	if err != nil {
		return nil, err
	}
	out := make([]layerMetadata, 0, len(array))
	for i, raw := range array {
		path := fmt.Sprintf("%s.%s[%d]", basePath, field, i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		if err := checkFields(obj, path, []string{"id", sizeField, "producerStep"}); err != nil {
			return nil, err
		}
		size, err := mustInt(obj, sizeField, path)
		if err != nil {
			return nil, err
		}
		producer, err := mustInt(obj, "producerStep", path)
		if err != nil {
			return nil, err
		}
		out = append(out, layerMetadata{
			ID:           mustString(obj, "id", path),
			SizeBytes:    size,
			ProducerStep: int(producer),
			Path:         path,
		})
	}
	return out, nil
}

func validateLayerMetadata(project model.Project, metadata []layerMetadata, path string) error {
	expected := make(map[string]layerMetadata, len(project.Bases)+len(project.Steps))
	for _, base := range project.Bases {
		expected[base.ID] = layerMetadata{ID: base.ID, SizeBytes: base.SizeBytes, ProducerStep: 0}
	}
	for _, step := range project.Steps {
		expected[step.Output.ID] = layerMetadata{ID: step.Output.ID, SizeBytes: step.Output.SizeBytes, ProducerStep: step.Number}
	}
	seen := make(map[string]bool, len(metadata))
	for _, item := range metadata {
		if seen[item.ID] {
			return apperr.WithLayers(apperr.New(apperr.LayerMetadata, "validate-layer-metadata", item.Path, "duplicate layer metadata"), item.ID)
		}
		seen[item.ID] = true
		want, ok := expected[item.ID]
		if !ok {
			return apperr.WithLayers(apperr.New(apperr.LayerMetadata, "validate-layer-metadata", item.Path, "layer metadata has no producer or base"), item.ID)
		}
		if item.SizeBytes != want.SizeBytes || item.ProducerStep != want.ProducerStep {
			err := apperr.WithLayers(apperr.New(apperr.LayerMetadata, "validate-layer-metadata", item.Path, "layer size or producer differs from step declaration"), item.ID)
			if want.ProducerStep > 0 {
				err = apperr.WithSteps(err, want.ProducerStep)
			}
			return err
		}
	}
	var missing []string
	for id, want := range expected {
		if !seen[id] {
			missing = append(missing, id)
			if want.ProducerStep > 0 {
				_ = want
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return apperr.WithLayers(apperr.New(apperr.LayerMetadata, "validate-layer-metadata", path, "layer metadata is incomplete"), missing...)
	}
	return nil
}

func parseShares(doc map[string]any, field, basePath string) ([]model.Share, error) {
	array, err := mustArray(doc, field, basePath)
	if err != nil {
		return nil, err
	}
	out := make([]model.Share, 0, len(array))
	for i, raw := range array {
		path := fmt.Sprintf("%s.%s[%d]", basePath, field, i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		if err := checkFields(obj, path, []string{"layer", "consumers"}); err != nil {
			return nil, err
		}
		consumers, err := parseIntArray(obj, "consumers", path)
		if err != nil {
			return nil, err
		}
		out = append(out, model.Share{LayerID: mustString(obj, "layer", path), Consumers: consumers})
	}
	return out, nil
}

func parseBudget(obj map[string]any, path string, latencyMillis, memoryMultiplier int64, memoryField string) (model.Budget, error) {
	maxRounds, err := mustInt(obj, "maxRounds", path)
	if err != nil {
		return model.Budget{}, err
	}
	threads, err := mustInt(obj, "threadLimit", path)
	if err != nil {
		return model.Budget{}, err
	}
	if latencyMillis <= 0 {
		return model.Budget{}, apperr.New(apperr.BudgetLatency, "parse-budget", path, "interaction latency budget must be positive")
	}
	if threads <= 0 {
		return model.Budget{}, apperr.New(apperr.BudgetThreads, "parse-budget", path+".threadLimit", "thread limit must be positive")
	}
	memory, err := mustInt(obj, memoryField, path)
	if err != nil {
		return model.Budget{}, err
	}
	if memory <= 0 {
		return model.Budget{}, apperr.New(apperr.BudgetMemory, "parse-budget", path+"."+memoryField, "memory budget must be positive")
	}
	memoryBytes, err := multiplyUnit(memory, memoryMultiplier, path+"."+memoryField)
	if err != nil {
		return model.Budget{}, err
	}
	buffer, err := mustInt(obj, "bufferPerThread", path)
	if err != nil {
		return model.Budget{}, err
	}
	if buffer <= 0 {
		return model.Budget{}, apperr.New(apperr.BackpressureQueue, "parse-budget", path+".bufferPerThread", "buffer capacity must be positive")
	}
	if maxRounds <= 0 {
		return model.Budget{}, apperr.New(apperr.BudgetLatency, "parse-budget", path+".maxRounds", "max rounds must be positive")
	}
	return model.Budget{
		Latency:         time.Duration(latencyMillis) * time.Millisecond,
		MaxRounds:       int(maxRounds),
		ThreadLimit:     int(threads),
		MemoryPerThread: memoryBytes,
		BufferPerThread: int(buffer),
	}, nil
}

func multiplyUnit(value, multiplier int64, path string) (int64, error) {
	if value < 0 || (multiplier > 0 && value > math.MaxInt64/multiplier) {
		return 0, apperr.New(apperr.SchemaUnit, "convert-unit", path, "value is outside int64 byte range")
	}
	return value * multiplier, nil
}

func parseInputs(obj map[string]any, field, path string) ([]model.FileInput, error) {
	array, err := mustArray(obj, field, path)
	if err != nil {
		return nil, err
	}
	out := make([]model.FileInput, 0, len(array))
	for i, raw := range array {
		itemPath := fmt.Sprintf("%s.%s[%d]", path, field, i)
		item, err := asObject(raw, itemPath)
		if err != nil {
			return nil, err
		}
		if err := checkFields(item, itemPath, []string{"path", "digest"}); err != nil {
			return nil, err
		}
		out = append(out, model.FileInput{
			Path:   mustString(item, "path", itemPath),
			Digest: mustString(item, "digest", itemPath),
		})
	}
	return out, nil
}

func parseStringArray(obj map[string]any, field, path string) ([]string, error) {
	array, err := mustArray(obj, field, path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(array))
	for i, raw := range array {
		value, ok := raw.(string)
		if !ok {
			return nil, apperr.New(apperr.SchemaType, "parse-manifest", fmt.Sprintf("%s.%s[%d]", path, field, i), "expected string")
		}
		out = append(out, value)
	}
	return out, nil
}

func parseIntArray(obj map[string]any, field, path string) ([]int, error) {
	array, err := mustArray(obj, field, path)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(array))
	for i, raw := range array {
		value, err := asInt(raw, fmt.Sprintf("%s.%s[%d]", path, field, i))
		if err != nil {
			return nil, err
		}
		out = append(out, int(value))
	}
	return out, nil
}

func checkUnits(doc map[string]any, timeUnit, sizeUnit string) error {
	raw, err := required(doc, "units", "$")
	if err != nil {
		return err
	}
	obj, err := asObject(raw, "$.units")
	if err != nil {
		return err
	}
	if err := checkFields(obj, "$.units", []string{"size", "time"}); err != nil {
		return err
	}
	gotTime, err := mustStringValue(obj, "time", "$.units")
	if err != nil {
		return err
	}
	gotSize, err := mustStringValue(obj, "size", "$.units")
	if err != nil {
		return err
	}
	if !unitEqual(gotTime, timeUnit) {
		return apperr.New(apperr.SchemaUnit, "validate-units", "$.units.time", "field naming requires "+timeUnit)
	}
	if !unitEqual(gotSize, sizeUnit) {
		return apperr.New(apperr.SchemaUnit, "validate-units", "$.units.size", "field naming requires "+sizeUnit)
	}
	return nil
}

func unitEqual(got, want string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "ms":
			return "milliseconds"
		case "sec", "s":
			return "seconds"
		case "kibibytes":
			return "kib"
		}
		return value
	}
	return normalize(got) == normalize(want)
}

func checkFields(obj map[string]any, path string, allowed []string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = true
	}
	var unknown []string
	for field := range obj {
		if !allowedSet[field] {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return apperr.New(apperr.SchemaField, "validate-fields", path, "unknown field(s): "+strings.Join(unknown, ","))
	}
	return nil
}

func required(obj map[string]any, field, path string) (any, error) {
	value, ok := obj[field]
	if !ok {
		return nil, apperr.New(apperr.SchemaRequired, "parse-manifest", path+"."+field, "required field is missing")
	}
	return value, nil
}

func asObject(value any, path string) (map[string]any, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, apperr.New(apperr.SchemaType, "parse-manifest", path, "expected object")
	}
	return obj, nil
}

func mustArray(obj map[string]any, field, path string) ([]any, error) {
	value, err := required(obj, field, path)
	if err != nil {
		return nil, err
	}
	array, ok := value.([]any)
	if !ok {
		return nil, apperr.New(apperr.SchemaType, "parse-manifest", path+"."+field, "expected array")
	}
	return array, nil
}

func mustString(obj map[string]any, field, path string) string {
	value, err := mustStringValue(obj, field, path)
	if err != nil {
		panic(err)
	}
	return value
}

func mustStringValue(obj map[string]any, field, path string) (string, error) {
	value, err := required(obj, field, path)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", apperr.New(apperr.SchemaType, "parse-manifest", path+"."+field, "expected non-empty string")
	}
	return text, nil
}

func mustInt(obj map[string]any, field, path string) (int64, error) {
	value, err := required(obj, field, path)
	if err != nil {
		return 0, err
	}
	return asInt(value, path+"."+field)
}

func mustIntDefault(obj map[string]any, field, path string) int64 {
	value, err := mustInt(obj, field, path)
	if err != nil {
		panic(err)
	}
	return value
}

func asInt(value any, path string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, apperr.New(apperr.SchemaType, "parse-manifest", path, "expected integer")
	}
	integer, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, apperr.New(apperr.SchemaType, "parse-manifest", path, "expected integer")
	}
	return integer, nil
}

func mustJSONNumber(obj map[string]any, field, path string) (json.Number, error) {
	value, err := required(obj, field, path)
	if err != nil {
		return "", err
	}
	number, ok := value.(json.Number)
	if !ok {
		return "", apperr.New(apperr.SchemaType, "parse-manifest", path+"."+field, "expected number")
	}
	return number, nil
}

func decimalSecondsToMillis(value, path string) (int64, error) {
	rat, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0, apperr.New(apperr.SchemaUnit, "parse-seconds", path, "invalid seconds value")
	}
	rat.Mul(rat, big.NewRat(1000, 1))
	if !rat.IsInt() {
		return 0, apperr.New(apperr.SchemaUnit, "parse-seconds", path, "seconds value is not an exact millisecond")
	}
	if !rat.Num().IsInt64() {
		return 0, apperr.New(apperr.SchemaUnit, "parse-seconds", path, "seconds value is out of range")
	}
	return rat.Num().Int64(), nil
}
