package terrainpipeline

import (
	"terrainpipeline/internal/artifact"
	"terrainpipeline/internal/budget"
	"terrainpipeline/internal/checkpoint"
	"terrainpipeline/internal/depgraph"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/pipeline"
	"terrainpipeline/internal/projectio"
	"terrainpipeline/internal/samplegen"
	"terrainpipeline/internal/verify"
)

const FixedSeed = model.FixedSeed

type (
	Project       = model.Project
	Block         = model.Block
	BlockResult   = model.BlockResult
	SeamReport    = model.SeamReport
	Error         = model.Error
	ErrorCode     = model.ErrorCode
	Store         = checkpoint.Store
	Service       = pipeline.Service
	Outcome       = pipeline.Outcome
	RunOptions    = pipeline.RunOptions
	Edit          = depgraph.Edit
	BudgetOptions = budget.Options
	Report        = verify.Report
)

const (
	CodeSchema = model.CodeSchema
	CodeSize   = model.CodeSize
	CodeRef    = model.CodeRef
	CodeSeam   = model.CodeSeam
	CodeDiff   = model.CodeDiff
	CodeBudget = model.CodeBudget
	CodeIO     = model.CodeIO
)

func GenerateSample(seed int64) *Project {
	return samplegen.Generate(seed)
}

func WriteSample(dir string, project *Project) error {
	return projectio.WriteSample(dir, project)
}

func LoadProject(dir string) (*Project, error) {
	return projectio.LoadProject(dir)
}

func OpenStore(path string) (*Store, error) {
	return checkpoint.Open(path)
}

func NewService(root string, store *Store) *Service {
	return pipeline.NewService(root, store)
}

func RunVerification(work string) (*Report, error) {
	return verify.RunAll(work)
}

func ExportBlockArtifacts(root string, result *BlockResult) error {
	return artifact.ExportBlockArtifacts(root, result)
}

func ExportSeams(root string, seams []*SeamReport) error {
	return artifact.ExportSeams(root, seams)
}

func NewError(code ErrorCode, blockID, field, format string, args ...any) *Error {
	return model.NewError(code, blockID, field, format, args...)
}
