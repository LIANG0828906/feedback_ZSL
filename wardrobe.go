// Package wardrobe is the public integration facade for the deterministic
// character part batching and assembly conflict kernel.
//
// Advanced persistence and scheduling packages remain under internal/, while
// the aliases and functions here cover the stable first acceptance version.
package wardrobe

import (
	"context"

	"wardrobekernel/internal/assembly"
	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/cache"
	"wardrobekernel/internal/checkpoint"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/pipeline"
	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/scheduler"
	"wardrobekernel/internal/schema"
)

const (
	FixedSeed         = model.FixedSeed
	AlgorithmVersion  = model.AlgorithmVersion
	ConstraintVersion = model.ConstraintVersion

	CodeSchema = "E_SCHEMA"
	CodeArb    = "E_ARB"
	CodeDiff   = "E_DIFF"
	CodeVer    = "E_VER"
)

type (
	Project           = model.Project
	Truth             = model.Truth
	Plan              = model.Plan
	Report            = model.Report
	Batch             = model.Batch
	Part              = model.Part
	Conflict          = model.Conflict
	Decision          = model.Decision
	AssemblyResult    = model.AssemblyResult
	BatchConstraints  = model.BatchConstraints
	ComparisonReport  = model.ComparisonReport
	WaitRecord        = model.WaitRecord
	PartFailure       = model.PartFailure
	CheckpointStore   = checkpoint.Store
	PersistentCache   = cache.PersistentCache
	SchedulerOptions  = scheduler.Options
	RealtimeOptions   = pipeline.RealtimeOptions
	RealtimeResult    = pipeline.RealtimeResult
	ResumeResult      = pipeline.ResumeResult
	IncrementalResult = pipeline.IncrementalResult
)

// GenerateSample builds the fixed-seed deterministic acceptance project.
func GenerateSample(seed int64) (*Project, error) { return samplegen.Generate(seed) }

// WriteSampleDir writes project, truth and diagnostic fixtures.
func WriteSampleDir(dir string, seed int64) error { return samplegen.WriteSampleDir(dir, seed) }

// LoadProject reads and validates a project JSON file.
func LoadProject(path string) (*Project, error) { return schema.LoadProject(path) }

// LoadTruth reads a truth JSON file.
func LoadTruth(path string) (*Truth, error) { return schema.LoadTruth(path) }

// EvaluateAssembly runs the order-independent legality kernel.
func EvaluateAssembly(project *Project, candidateIDs []string) (*AssemblyResult, error) {
	return assembly.Evaluate(project, candidateIDs)
}

// PlanBatches runs deterministic conflict evaluation and batch partitioning.
func PlanBatches(project *Project) (*Plan, error) { return batching.Plan(project) }

// FullPlan runs batching and enriches dependency/cache keys.
func FullPlan(project *Project) (*Plan, error) { return pipeline.FullPlan(project) }

// VerifyTruth compares a fresh plan to loaded deterministic truth.
func VerifyTruth(project *Project, truth *Truth) (*Plan, error) {
	return pipeline.VerifyTruth(project, truth)
}

// RunRealtime runs bounded parallel preparation and optional checkpoints.
func RunRealtime(ctx context.Context, project *Project, opts RealtimeOptions) (*RealtimeResult, error) {
	return pipeline.RunRealtime(ctx, project, opts)
}

// ResumeCheckpoint opens a versioned SQLite checkpoint.
func ResumeCheckpoint(path, algorithmVersion, constraintVersion string) (*CheckpointStore, error) {
	return checkpoint.Open(path, algorithmVersion, constraintVersion)
}

// LoadPersistentCache opens or initializes an atomic JSON batch cache.
func LoadPersistentCache(path, algorithmVersion, constraintVersion string) (*PersistentCache, error) {
	return cache.Load(path, algorithmVersion, constraintVersion)
}

// ResumeAndContinue recovers durable batches and recomputes the tail.
func ResumeAndContinue(ctx context.Context, project *Project, store *CheckpointStore, opts SchedulerOptions) (*ResumeResult, error) {
	return pipeline.ResumeAndContinue(ctx, project, store, opts)
}

// IncrementalReplan reuses cache entries and compares against full recompute.
func IncrementalReplan(oldProject *Project, oldPlan *Plan, cache *PersistentCache, newProject *Project) (*IncrementalResult, error) {
	return pipeline.IncrementalReplan(oldProject, oldPlan, cache, newProject)
}

// CloneProject deep-copies a project for edit simulations.
func CloneProject(project *Project) (*Project, error) { return pipeline.CloneProject(project) }

// EditMaterialVersion changes a material slot version.
func EditMaterialVersion(project *Project, partID, slotID, version string) error {
	return pipeline.EditMaterialVersion(project, partID, slotID, version)
}

// EditGeometryScale changes a part geometry vertex footprint.
func EditGeometryScale(project *Project, partID string, vertexDelta int) error {
	return pipeline.EditGeometryScale(project, partID, vertexDelta)
}
