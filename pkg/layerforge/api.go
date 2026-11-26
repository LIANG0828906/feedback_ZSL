// Package layerforge exposes the embeddable domain facade. The implementation
// remains under internal/; external modules import only this package.
package layerforge

import (
	"context"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/checkpoint"
	"layerforge/internal/engine"
	"layerforge/internal/generator"
	"layerforge/internal/loader"
	"layerforge/internal/model"
)

type (
	Project        = model.Project
	Budget         = model.Budget
	Step           = model.Step
	Layer          = model.Layer
	FileInput      = model.FileInput
	BaseLayer      = model.BaseLayer
	Share          = model.Share
	BuildOptions   = engine.BuildOptions
	BudgetOverride = engine.BudgetOverride
	BuildResult    = engine.BuildResult
	CleanupResult  = engine.CleanupResult
	Snapshot       = checkpoint.Snapshot
	LayerRecord    = checkpoint.LayerRecord
	IndexRecord    = checkpoint.IndexRecord
	RoundRecord    = checkpoint.RoundRecord
	Graph          = build.Graph
	Derivation     = build.Derivation
	StepDerivation = build.StepDerivation
	LayerTruth     = build.LayerTruth
	BusinessError  = apperr.Error
	BusinessCode   = apperr.Code
)

const (
	CacheKeyAlgorithm = model.CacheKeyAlgorithm
	CheckpointVersion = model.CheckpointVersion
)

type Definition struct {
	Project Project
	Schema  string
	Graph   *Graph
}

func LoadManifest(data []byte) (*Definition, error) {
	loaded, err := loader.Load(data)
	if err != nil {
		return nil, err
	}
	return &Definition{Project: loaded.Project, Schema: loaded.Schema, Graph: loaded.Graph}, nil
}

func LoadManifestFile(path string) (*Definition, error) {
	loaded, err := loader.LoadFile(path)
	if err != nil {
		return nil, err
	}
	return &Definition{Project: loaded.Project, Schema: loaded.Schema, Graph: loaded.Graph}, nil
}

func Derive(project Project) (*Graph, *Derivation, error) {
	return build.DeriveProject(project)
}

func GenerateSample(seed int64) (Project, *Derivation, map[string][]byte, []byte, error) {
	fixture, err := generator.Generate(seed)
	if err != nil {
		return Project{}, nil, nil, nil, err
	}
	return fixture.Project, fixture.Derivation, fixture.Versions, fixture.Truth, nil
}

type Builder struct {
	inner *engine.Engine
}

func NewBuilder(project Project, checkpointPath string) (*Builder, error) {
	inner, err := engine.New(project, checkpoint.Open(checkpointPath))
	if err != nil {
		return nil, err
	}
	return &Builder{inner: inner}, nil
}

func (b *Builder) Build(ctx context.Context, options BuildOptions) (BuildResult, error) {
	return b.inner.Build(ctx, options)
}

func (b *Builder) Cleanup(candidates []string) (CleanupResult, error) {
	return b.inner.Cleanup(candidates)
}

func (b *Builder) LoadCheckpoint() (*Snapshot, error) {
	return b.inner.Store.Load()
}

func (b *Builder) Project() Project {
	return b.inner.Project.Clone()
}

func EditStep(project Project, stepNumber int, mutate func(*Step)) (Project, error) {
	return engine.EditStep(project, stepNumber, mutate)
}

func SemanticHash(project Project) string {
	return model.SemanticHash(project)
}

func ErrorCode(err error) BusinessCode {
	return apperr.CodeOf(err)
}

func IsCode(err error, code BusinessCode) bool {
	return apperr.IsCode(err, code)
}
