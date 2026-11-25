package demo

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"skeletonretarget/internal/codec"
	"skeletonretarget/internal/generator"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/pipeline"
	"skeletonretarget/internal/retarget"
)

type Options struct {
	Seed    int64
	WorkDir string
}

type Result struct {
	Data    *Data
	Cleanup func()
}

type FormatInfo struct {
	Name          string `json:"name"`
	Format        string `json:"format"`
	Version       string `json:"version"`
	SourceVersion string `json:"sourceVersion"`
	ActionHash    string `json:"actionHash"`
	TopologyHash  string `json:"topologyHash"`
	Frames        int    `json:"frames"`
	SourceJoints  int    `json:"sourceJoints"`
	TargetJoints  int    `json:"targetJoints"`
	Conversions   int    `json:"conversions"`
}

type JointInfo struct {
	ID       string `json:"id"`
	Parent   string `json:"parent"`
	Rest     string `json:"rest"`
	MinAngle string `json:"minAngle"`
	MaxAngle string `json:"maxAngle"`
}

type TargetJointInfo struct {
	ID         string `json:"id"`
	Parent     string `json:"parent"`
	Rest       string `json:"rest"`
	Proportion string `json:"proportion"`
}

type MappingInfo struct {
	ID               string `json:"id"`
	Source           string `json:"source"`
	Target           string `json:"target"`
	RotationScale    string `json:"rotationScale"`
	TranslationScale string `json:"translationScale"`
}

type CurvePoint struct {
	Frame            int    `json:"frame"`
	NormResidualPPM  int64  `json:"normResidualPpm"`
	MulTruncationPPM int64  `json:"mulTruncationPpm"`
	ScaleRoundingPPM int64  `json:"scaleRoundingPpm"`
	RemapPPM         int64  `json:"remapPpm"`
	BoundPPM         int64  `json:"boundPpm"`
	ActualPPM        *int64 `json:"actualPpm,omitempty"`
	PositionPPM      *int64 `json:"positionPpm,omitempty"`
}

type PathInfo struct {
	Name               string `json:"name"`
	Digest             string `json:"digest"`
	Complete           bool   `json:"complete"`
	Frames             int    `json:"frames"`
	RecoveredTo        int    `json:"recoveredTo"`
	CacheHits          int    `json:"cacheHits"`
	Recomputed         int    `json:"recomputed"`
	InvalidationCount  int    `json:"invalidationCount"`
	CommittedIntervals int    `json:"committedIntervals"`
	QueueWaitNanos     int64  `json:"queueWaitNanos"`
	QueueFullWaits     int64  `json:"queueFullWaits"`
}

type EditInfo struct {
	Description   string                  `json:"description"`
	Invalidations []pipeline.Invalidation `json:"invalidations"`
	Hits          int                     `json:"hits"`
	Recomputed    int                     `json:"recomputed"`
	Comparison    pipeline.Comparison     `json:"comparison"`
}

type Data struct {
	Seed              int64                  `json:"seed"`
	Formats           []FormatInfo           `json:"formats"`
	SourceSkeleton    []JointInfo            `json:"sourceSkeleton"`
	TargetSkeleton    []TargetJointInfo      `json:"targetSkeleton"`
	Mappings          []MappingInfo          `json:"mappings"`
	Frames            []retarget.FrameResult `json:"frames"`
	Curve             []CurvePoint           `json:"curve"`
	Paths             []PathInfo             `json:"paths"`
	Comparisons       []pipeline.Comparison  `json:"comparisons"`
	Edit              EditInfo               `json:"edit"`
	AlgorithmVersion  string                 `json:"algorithmVersion"`
	GeneratedInterval int                    `json:"intervalSize"`
}

func Build(ctx context.Context, options Options) (*Result, error) {
	if options.Seed == 0 {
		options.Seed = 42
	}
	cleanup := func() {}
	if options.WorkDir == "" {
		dir, err := os.MkdirTemp("", "skeletonretarget-demo-*")
		if err != nil {
			return nil, err
		}
		options.WorkDir = dir
		cleanup = func() { _ = os.RemoveAll(dir) }
	}
	if err := os.MkdirAll(options.WorkDir, 0o755); err != nil {
		cleanup()
		return nil, err
	}
	sample, err := generator.Generate(options.Seed)
	if err != nil {
		cleanup()
		return nil, err
	}
	named := []struct {
		name string
		data []byte
	}{
		{"studio", sample.Studio},
		{"engine", sample.Engine},
		{"legacy", sample.Legacy},
	}
	docs := make(map[string]*model.Document, len(named))
	data := &Data{Seed: options.Seed, AlgorithmVersion: retarget.AlgorithmVersion, GeneratedInterval: 4}
	for _, item := range named {
		doc, loadErr := codec.Load(item.data)
		if loadErr != nil {
			cleanup()
			return nil, loadErr
		}
		docs[item.name] = doc
		data.Formats = append(data.Formats, FormatInfo{
			Name: item.name, Format: doc.Format, Version: doc.Version, SourceVersion: doc.SourceVersion,
			ActionHash: doc.ContentHash(), TopologyHash: doc.TopologyHash(),
			Frames: len(doc.Clip.Frames), SourceJoints: len(doc.Skeleton.Joints),
			TargetJoints: len(doc.Target.Joints), Conversions: len(doc.Conversions),
		})
	}
	primary := docs["studio"]
	data.SourceSkeleton = make([]JointInfo, 0, len(primary.Skeleton.Joints))
	for _, joint := range primary.Skeleton.Joints {
		data.SourceSkeleton = append(data.SourceSkeleton, JointInfo{
			ID: joint.ID, Parent: joint.Parent,
			Rest:     fmt.Sprintf("%d/%d/%d @%d", joint.Rest.X, joint.Rest.Y, joint.Rest.Z, joint.Rest.Scale),
			MinAngle: fmt.Sprintf("%.3f deg", radToDeg(joint.Constraint.MinRad, joint.Constraint.Scale)),
			MaxAngle: fmt.Sprintf("%.3f deg", radToDeg(joint.Constraint.MaxRad, joint.Constraint.Scale)),
		})
	}
	data.TargetSkeleton = make([]TargetJointInfo, 0, len(primary.Target.Joints))
	for _, joint := range primary.Target.Joints {
		data.TargetSkeleton = append(data.TargetSkeleton, TargetJointInfo{
			ID: joint.ID, Parent: joint.Parent,
			Rest:       fmt.Sprintf("%d/%d/%d @%d", joint.Rest.X, joint.Rest.Y, joint.Rest.Z, joint.Rest.Scale),
			Proportion: joint.Proportion.String(),
		})
	}
	data.Mappings = make([]MappingInfo, 0, len(primary.Retarget.Mappings))
	for _, mapping := range primary.Retarget.Mappings {
		data.Mappings = append(data.Mappings, MappingInfo{
			ID: mapping.ID, Source: mapping.Source, Target: mapping.Target,
			RotationScale: mapping.RotationScale.String(), TranslationScale: mapping.TranslationScale.String(),
		})
	}

	pathDB := filepath.Join(options.WorkDir, "paths.db")
	pathOptions := pipeline.DefaultOptions(pathDB)
	loadReport, err := pipeline.Run(ctx, primary, pathOptions, pipeline.Load, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	incrementalReport, err := pipeline.Run(ctx, primary, pathOptions, pipeline.Incremental, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	recoveryReport, err := pipeline.Run(ctx, primary, pathOptions, pipeline.Recovery, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	data.Frames = loadReport.Frames
	data.Curve = curve(loadReport.Frames)
	data.Paths = []PathInfo{
		pathInfo("load", loadReport),
		pathInfo("incremental", incrementalReport),
		pathInfo("recovery", recoveryReport),
	}
	data.Comparisons = append(data.Comparisons,
		namedComparison("load", "incremental", loadReport.Frames, incrementalReport.Frames),
		namedComparison("load", "recovery", loadReport.Frames, recoveryReport.Frames),
	)
	for _, name := range []string{"engine", "legacy"} {
		report, runErr := pipeline.Run(ctx, docs[name], pipeline.DefaultOptions(filepath.Join(options.WorkDir, "format-"+name+".db")), pipeline.Load, nil)
		if runErr != nil {
			cleanup()
			return nil, runErr
		}
		data.Comparisons = append(data.Comparisons,
			namedComparison("studio", name, loadReport.Frames, report.Frames))
	}

	edited := pipeline.CloneDocument(primary)
	for i := range edited.Skeleton.Joints {
		if edited.Skeleton.Joints[i].ID == "arm_l" {
			edited.Skeleton.Joints[i].Constraint.MaxRad = int64(math.Round(18.5 * math.Pi / 180 * float64(model.AngleScale)))
		}
	}
	editOptions := pipeline.DefaultOptions(pathDB)
	editOptions.Retarget.DisableTruthCheck = true
	editReport, err := pipeline.Run(ctx, edited, editOptions, pipeline.Incremental, []pipeline.Edit{{
		Kind: pipeline.EditConstraint, Joint: "arm_l", Reason: "arm_l max swing tightened to 18.5 deg",
	}})
	if err != nil {
		cleanup()
		return nil, err
	}
	fullOptions := pipeline.DefaultOptions(filepath.Join(options.WorkDir, "edited-full.db"))
	fullOptions.Retarget.DisableTruthCheck = true
	editedFull, err := pipeline.Run(ctx, edited, fullOptions, pipeline.Load, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	hits, recomputed := intervalCounts(editReport.Intervals)
	data.Edit = EditInfo{
		Description:   "arm_l constraint max tightened from 80 deg to 18.5 deg",
		Invalidations: editReport.Invalidations, Hits: hits, Recomputed: recomputed,
		Comparison: namedComparison("edited-full", "edited-incremental", editedFull.Frames, editReport.Frames),
	}
	return &Result{Data: data, Cleanup: cleanup}, nil
}

func curve(frames []retarget.FrameResult) []CurvePoint {
	out := make([]CurvePoint, 0, len(frames))
	for _, frame := range frames {
		point := CurvePoint{Frame: frame.Frame}
		var actual, position int64
		hasActual := false
		for _, joint := range frame.Joints {
			point.NormResidualPPM = max64(point.NormResidualPPM, joint.Report.NormResidualPPM)
			point.MulTruncationPPM = max64(point.MulTruncationPPM, joint.Report.MulTruncationPPM)
			point.ScaleRoundingPPM = max64(point.ScaleRoundingPPM, joint.Report.ScaleRoundingPPM)
			point.RemapPPM = max64(point.RemapPPM, joint.Report.RemapOrientationPPM)
			point.BoundPPM = max64(point.BoundPPM, joint.Report.BoundPPM)
			if joint.Report.ActualDeviationPPM != nil {
				hasActual = true
				actual = max64(actual, *joint.Report.ActualDeviationPPM)
				position = max64(position, *joint.Report.PositionDeviationPPM)
			}
		}
		if hasActual {
			point.ActualPPM = &actual
			point.PositionPPM = &position
		}
		out = append(out, point)
	}
	return out
}

func pathInfo(name string, report *pipeline.Report) PathInfo {
	hits, recomputed := intervalCounts(report.Intervals)
	return PathInfo{
		Name: name, Digest: pipeline.FramesDigest(report.Frames), Complete: report.Complete,
		Frames: len(report.Frames), RecoveredTo: report.RecoveredTo, CacheHits: hits, Recomputed: recomputed,
		InvalidationCount: len(report.Invalidations), CommittedIntervals: len(report.Intervals),
		QueueWaitNanos: report.Queue.WaitNanos, QueueFullWaits: report.Queue.FullWaits,
	}
}

func intervalCounts(intervals []pipeline.IntervalRun) (hits, recomputed int) {
	for _, interval := range intervals {
		if interval.CacheHit {
			hits++
		} else {
			recomputed++
		}
	}
	return hits, recomputed
}

func namedComparison(leftName, rightName string, left, right []retarget.FrameResult) pipeline.Comparison {
	comparison := pipeline.CompareFrames(left, right)
	comparison.Left, comparison.Right = leftName, rightName
	return comparison
}

func radToDeg(value, scale int64) float64 {
	if scale == 0 {
		return 0
	}
	return float64(value) / float64(scale) * 180 / math.Pi
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
