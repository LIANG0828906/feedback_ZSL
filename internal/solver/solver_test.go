package solver_test

import (
	"context"
	"testing"

	"retargetlab/internal/generator"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
)

func TestSolverMatchesAnalyticTruthForAllGeneratedTargets(t *testing.T) {
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range ws.Targets {
		t.Run(target.Name, func(t *testing.T) {
			truth, ok := ws.TruthByTarget(target.Name)
			if !ok {
				t.Fatalf("missing truth for %s", target.Name)
			}
			s, err := solver.New(ws.Source, ws.Clip, target.Skeleton, ws.Mapping, solver.DefaultParams())
			if err != nil {
				t.Fatal(err)
			}
			results := make([]solver.FrameResult, 0, len(ws.Clip.Frames))
			for frame := range ws.Clip.Frames {
				r, err := s.SolveFrame(context.Background(), frame)
				if err != nil {
					t.Fatalf("frame %d: %v", frame, err)
				}
				results = append(results, r)
			}
			if err := solver.CompareTruthWithSource(ws.Source, target.Skeleton, truth, results); err != nil {
				t.Fatal(err)
			}
			for _, r := range results {
				if r.Constraint.EffectorViolation > truth.Tolerance {
					t.Fatalf("frame %d effector violation %.9g exceeds tolerance %.9g", r.Frame, r.Constraint.EffectorViolation, truth.Tolerance)
				}
			}
		})
	}
}

func TestConstraintProjectionAndAngleLimit(t *testing.T) {
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	// Shorten a forearm so the source-space hand goal is unreachable without
	// projection, and tighten the pelvis limit so clamping is observable.
	idx := target.Skeleton.JointIndex("forearm_l")
	target.Skeleton.Joints[idx].LocalOffset = target.Skeleton.Joints[idx].LocalOffset.Mul(0.45)
	pelvis := target.Skeleton.JointIndex("pelvis")
	target.Skeleton.Joints[pelvis].RotationLimitDeg = 1
	s, err := solver.New(ws.Source, ws.Clip, target.Skeleton, ws.Mapping, solver.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.SolveFrame(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Constraint.AngleViolations == 0 || result.Constraint.AngleViolationDeg <= 0 {
		t.Fatalf("expected an angle-limit violation, got %+v", result.Constraint)
	}
	if result.Constraint.Iterations == 0 {
		t.Fatalf("expected iterative projection to run, got %+v", result.Constraint)
	}
	if result.Constraint.EffectorViolation <= 0 {
		t.Fatalf("expected an unreachable-effector residual, got %+v", result.Constraint)
	}
}

func TestCompareFrameResultsReportsFirstDiffDetails(t *testing.T) {
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	s, err := solver.New(ws.Source, ws.Clip, target.Skeleton, ws.Mapping, solver.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	left, err := s.SolveFrame(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	right := left
	right.World = append([]solver.JointWorld(nil), left.World...)
	right.World[5].Position.X += 0.25
	err = solver.CompareFrameResults(left, right)
	if !model.IsCode(err, model.E_DIFF) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	ce, ok := err.(*model.CodedError)
	if !ok || ce.Frame != 3 || ce.Joint == "" || ce.Left == nil || ce.Right == nil {
		t.Fatalf("E_DIFF lacks frame/joint/measured values: %+v", ce)
	}
}
