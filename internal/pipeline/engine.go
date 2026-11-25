package pipeline

import (
	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/sample"
)

type Engine struct {
	SampleID string
	Manifest *sample.Manifest
	Params   Params
}

func NewEngine(sampleID string, manifest *sample.Manifest, params Params) *Engine {
	params = params.WithDefaults()
	return &Engine{SampleID: sampleID, Manifest: manifest, Params: params}
}

func (e *Engine) RunAll(input *mesh.Mesh) (*PathResult, error) {
	result := &PathResult{Path: "one_shot", SampleID: e.SampleID, ParamVersion: e.Params.Version()}
	current := input.Clone()
	var seam []int

	for _, stage := range Stages {
		snap, next, nextSeam, err := e.RunStage(stage, current, seam)
		if snap != nil {
			result.Stages = append(result.Stages, *snap)
		}
		if err != nil {
			return result, err
		}
		seam = nextSeam
		current = next
		result.CommittedSequence = append(result.CommittedSequence, stage)
	}

	last := result.Stages[len(result.Stages)-1]
	result.FinalHash = last.MeshHash
	result.FinalStats = last.Stats
	return result, nil
}

func (e *Engine) RunStage(stage string, current *mesh.Mesh, seam []int) (*StageSnapshot, *mesh.Mesh, []int, error) {
	if current == nil {
		return nil, nil, seam, errs.New(errs.ESTATE, "cannot run %s on nil mesh", stage)
	}
	idx := StageIndex(stage)
	snap := &StageSnapshot{
		SampleID: e.SampleID, Stage: stage, StageIndex: idx, State: StateCommitted,
		ParamVersion: e.Params.Version(), AlgoVersion: AlgoVersion, Params: e.Params,
		Mesh: current.Clone(),
	}
	next := current.Clone()
	nextSeam := seam

	switch stage {
	case StageDiagnostic:
		if err := current.ValidateIndices(); err != nil {
			return e.fail(snap, err), nil, seam, err
		}
		stats := mesh.Analyze(current)
		snap.Stats = stats
		defects := []sample.Defect{}
		if e.Manifest != nil {
			defects = e.Manifest.Defects
		}
		snap.Diagnostic = &DiagnosticReport{ValidationOK: stats.DegenerateFaces == 0, Stats: stats, Defects: defects}

	case StageWeld:
		if err := e.Params.ValidateWeld(); err != nil {
			return e.fail(snap, err), nil, seam, err
		}
		welded, report, err := Weld(current, e.Params.WeldTolerance)
		snap.Weld = &report
		if err != nil {
			return e.fail(snap, err), nil, seam, err
		}
		next = welded

	case StageFill:
		filled, report, err := FillHoles(current)
		snap.Fill = &report
		if err != nil {
			return e.fail(snap, err), nil, seam, err
		}
		next = filled
		nextSeam = report.SeamVertices

	case StageOrient:
		oriented, report := OrientConsistent(current)
		snap.Orient = &report
		next = oriented

	case StageSmooth:
		if err := e.Params.ValidateSmooth(); err != nil {
			return e.fail(snap, err), nil, seam, err
		}
		before := mesh.Analyze(current)
		smoothed, report := Smooth(current, e.Params.LaplacianWeight, e.Params.SmoothIterations, seam)
		snap.Smooth = &report
		after := mesh.Analyze(smoothed)
		if err := CheckTopologyUnchanged(before, after); err != nil {
			err = errs.WithDetails(err, map[string]any{
				"before_vertices": before.Vertices, "after_vertices": after.Vertices,
				"before_faces": before.Faces, "after_faces": after.Faces,
				"before_euler": before.Euler, "after_euler": after.Euler,
			})
			return e.fail(snap, err), nil, seam, err
		}
		next = smoothed

	case StageStats:
		next = current.Clone()

	default:
		err := errs.New(errs.ESTATE, "unknown stage %q", stage)
		return e.fail(snap, err), nil, seam, err
	}

	snap.Stats = mesh.Analyze(next)
	snap.Stats.SelfIntersections = len(mesh.SelfIntersectingPairs(next))
	snap.Mesh = next.Clone()
	snap.MeshHash = snap.Mesh.Hash()

	if e.Manifest != nil {
		truthReport, err := CheckTruth(next, e.Manifest)
		snap.Truth = truthReport
		if err != nil {
			return e.fail(snap, err), next, nextSeam, err
		}
	}
	return snap, next, nextSeam, nil
}

func (e *Engine) fail(snap *StageSnapshot, err error) *StageSnapshot {
	snap.State = StateFailed
	if ce, ok := err.(*errs.Error); ok {
		snap.ErrorCode = ce.Code
		snap.ErrorMessage = ce.Message
	} else {
		snap.ErrorCode = "E_INTERNAL"
		snap.ErrorMessage = err.Error()
	}
	snap.MeshHash = snap.Mesh.Hash()
	snap.Stats = mesh.Analyze(snap.Mesh)
	return snap
}
