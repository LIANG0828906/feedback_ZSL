package retarget

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/topology"
)

const AlgorithmVersion = "retarget-fixed-v1"

type Options struct {
	WorkingScale      int64
	Parallelism       int
	QueueBudget       int
	JointBufferLimit  int
	FailFrames        map[int]string
	DisableTruthCheck bool
}

func DefaultOptions() Options {
	return Options{
		WorkingScale:     1_000_000,
		Parallelism:      4,
		QueueBudget:      8,
		JointBufferLimit: 2,
	}
}

func (o Options) normalized() (Options, *model.Error) {
	if o.WorkingScale == 0 {
		o.WorkingScale = 1_000_000
	}
	if o.Parallelism == 0 {
		o.Parallelism = 1
	}
	if o.QueueBudget == 0 {
		o.QueueBudget = 4
	}
	if o.JointBufferLimit == 0 {
		o.JointBufferLimit = 1
	}
	if o.WorkingScale < 1_000 || o.WorkingScale > 1_000_000_000 {
		return o, &model.Error{Code: model.CodeComp, Frame: -1, Message: "working scale must be between 1e3 and 1e9"}
	}
	if o.Parallelism < 0 || o.QueueBudget < 0 || o.JointBufferLimit < 1 {
		return o, &model.Error{Code: model.CodeFrame, Frame: -1, Message: "invalid worker budget"}
	}
	return o, nil
}

type JointReport struct {
	Joint                string `json:"joint"`
	NormResidualPPM      int64  `json:"normResidualPpm"`
	MulTruncationPPM     int64  `json:"mulTruncationPpm"`
	ScaleRoundingPPM     int64  `json:"scaleRoundingPpm"`
	RemapOrientationPPM  int64  `json:"remapOrientationPpm"`
	BoundPPM             int64  `json:"boundPpm"`
	ActualDeviationPPM   *int64 `json:"actualDeviationPpm,omitempty"`
	PositionDeviationPPM *int64 `json:"positionDeviationPpm,omitempty"`
	WithinBound          bool   `json:"withinBound"`
	TruthBacked          bool   `json:"truthBacked"`
}

type JointPose struct {
	Joint         string          `json:"joint"`
	WorldRotation model.Quat      `json:"worldRotation"`
	LocalRotation model.Quat      `json:"localRotation"`
	Position      model.FixedVec3 `json:"position"`
	Report        JointReport     `json:"report"`
}

type FrameResult struct {
	Frame          int                   `json:"frame"`
	TimeMS         int64                 `json:"timeMs"`
	Joints         []JointPose           `json:"joints"`
	Normalizations []NormalizationRecord `json:"normalizations"`
}

type NormTruncation struct {
	Stage     string `json:"stage"`
	Component string `json:"component"`
	Remainder int64  `json:"remainder"`
	Divisor   int64  `json:"divisor"`
	Mode      string `json:"mode"`
}

type NormalizationRecord struct {
	Joint       string           `json:"joint"`
	ResidualPPM int64            `json:"residualPpm"`
	Clamped     bool             `json:"clamped"`
	Truncations []NormTruncation `json:"truncations"`
}

type FrameFailure struct {
	Frame   int    `json:"frame"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Result struct {
	AlgorithmVersion string         `json:"algorithmVersion"`
	Scale            int64          `json:"scale"`
	Frames           []FrameResult  `json:"frames"`
	Failures         []FrameFailure `json:"failures,omitempty"`
	Queue            QueueStats     `json:"queue"`
}

type preparedFrame struct {
	local          []model.Quat
	world          []model.Quat
	normPPM        []int64
	mulPPM         []int64
	root           model.FixedVec3
	normalizations []NormalizationRecord
}

type prepared struct {
	doc         *model.Document
	options     Options
	sourceIndex map[string]int
	targetIndex map[string]int
	mapByTarget map[string]model.Mapping
	sourceDepth []int
	targetDepth []int
	levels      [][]int
	frames      map[int]*preparedFrame
	targetRest  []model.FixedVec3
}

func prepare(doc *model.Document, options Options) (*prepared, *model.Error) {
	if doc == nil {
		return nil, &model.Error{Code: model.CodeSchema, Frame: -1, Message: "document is nil"}
	}
	workingDoc, topoErr := canonicalDocument(doc)
	if topoErr != nil {
		return nil, topoErr
	}
	doc = workingDoc
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	p := &prepared{
		doc: doc, options: options,
		sourceIndex: doc.Skeleton.Index(), targetIndex: doc.Target.Index(),
		mapByTarget: make(map[string]model.Mapping, len(doc.Retarget.Mappings)),
		frames:      make(map[int]*preparedFrame, len(doc.Clip.Frames)),
	}
	for _, mapping := range doc.Retarget.Mappings {
		if _, ok := p.sourceIndex[mapping.Source]; !ok {
			return nil, &model.Error{Code: model.CodeMap, Version: doc.SourceVersion, Joint: mapping.Source, Frame: -1, Message: "mapping source is missing"}
		}
		if _, ok := p.targetIndex[mapping.Target]; !ok {
			return nil, &model.Error{Code: model.CodeMap, Version: doc.SourceVersion, Joint: mapping.Target, Frame: -1, Message: "mapping target is missing"}
		}
		p.mapByTarget[mapping.Target] = mapping
	}
	for _, joint := range doc.Target.Joints {
		if _, ok := p.mapByTarget[joint.ID]; !ok {
			return nil, &model.Error{Code: model.CodeMap, Version: doc.SourceVersion, Joint: joint.ID, Frame: -1, Message: "target joint has no mapping"}
		}
	}
	p.sourceDepth = make([]int, len(doc.Skeleton.Joints))
	for i, joint := range doc.Skeleton.Joints {
		if joint.Parent == "" {
			p.sourceDepth[i] = 0
		} else {
			p.sourceDepth[i] = p.sourceDepth[p.sourceIndex[joint.Parent]] + 1
		}
	}
	p.targetDepth = make([]int, len(doc.Target.Joints))
	levelMap := make(map[int]bool)
	for i, joint := range doc.Target.Joints {
		depth := 0
		if joint.Parent != "" {
			depth = p.targetDepth[p.targetIndex[joint.Parent]] + 1
		}
		p.targetDepth[i] = depth
		levelMap[depth] = true
	}
	var levels []int
	for level := range levelMap {
		levels = append(levels, level)
	}
	sort.Ints(levels)
	p.levels = make([][]int, len(levels))
	for i, level := range levels {
		for jointIndex := range doc.Target.Joints {
			if p.targetDepth[jointIndex] == level {
				p.levels[i] = append(p.levels[i], jointIndex)
			}
		}
	}
	p.targetRest = make([]model.FixedVec3, len(doc.Target.Joints))
	for i, joint := range doc.Target.Joints {
		rescaled, _ := fixed.RescaleVec(joint.Rest, options.WorkingScale, model.RoundHalfUp)
		p.targetRest[i] = rescaled
	}
	for _, frame := range doc.Clip.Frames {
		preparedFrame, perr := prepareFrame(p, frame)
		if perr != nil {
			return nil, perr
		}
		p.frames[frame.Index] = preparedFrame
	}
	return p, nil
}

func canonicalDocument(doc *model.Document) (*model.Document, *model.Error) {
	sortedJoints, err := topology.SortSkeleton(doc.Skeleton.Joints)
	if err != nil {
		err.Version = doc.SourceVersion
		return nil, err
	}
	sortedTarget, err := topology.SortTarget(doc.Target.Joints)
	if err != nil {
		err.Version = doc.SourceVersion
		return nil, err
	}
	out := *doc
	out.Skeleton.Joints = sortedJoints
	out.Target.Joints = sortedTarget
	out.Clip.Frames = make([]model.Frame, len(doc.Clip.Frames))
	oldIndex := make(map[string]int, len(doc.Skeleton.Joints))
	for i, joint := range doc.Skeleton.Joints {
		oldIndex[joint.ID] = i
	}
	for i, frame := range doc.Clip.Frames {
		reordered := frame
		reordered.Rotations = make([]model.Quat, len(sortedJoints))
		for j, joint := range sortedJoints {
			sourceIndex, ok := oldIndex[joint.ID]
			if !ok || sourceIndex >= len(frame.Rotations) {
				return nil, &model.Error{Code: model.CodeTopo, Version: doc.SourceVersion, Joint: joint.ID, Frame: frame.Index, Message: "joint rotation is missing after topology normalization"}
			}
			reordered.Rotations[j] = frame.Rotations[sourceIndex]
		}
		out.Clip.Frames[i] = reordered
	}
	return &out, nil
}

func prepareFrame(p *prepared, frame model.Frame) (*preparedFrame, *model.Error) {
	count := len(p.doc.Skeleton.Joints)
	pf := &preparedFrame{
		local: make([]model.Quat, count), world: make([]model.Quat, count),
		normPPM: make([]int64, count), mulPPM: make([]int64, count),
		normalizations: make([]NormalizationRecord, count),
	}
	root, _ := fixed.RescaleVec(frame.Root, p.options.WorkingScale, model.RoundHalfUp)
	pf.root = root
	for i, joint := range p.doc.Skeleton.Joints {
		raw, rescaleTruncs := fixed.RescaleQuat(frame.Rotations[i], p.options.WorkingScale, model.RoundHalfUp)
		normalized, normErr := fixed.Normalize(raw, model.RoundHalfUp)
		if normErr != nil {
			normErr.Version = p.doc.SourceVersion
			normErr.Joint = joint.ID
			normErr.Frame = frame.Index
			return nil, normErr
		}
		if normalized.NormResidualPPM > p.doc.Tolerance.NormPPM {
			return nil, &model.Error{
				Code: model.CodeNorm, Version: p.doc.SourceVersion, Joint: joint.ID, Frame: frame.Index,
				Message: fmt.Sprintf("normalization residual %d ppm exceeds tolerance %d ppm", normalized.NormResidualPPM, p.doc.Tolerance.NormPPM),
			}
		}
		q := normalized.Q
		clamped := false
		if clampedQ, changed, clampErr := fixed.ClampConstraint(q, joint.Constraint); clampErr != nil {
			clampErr.Version = p.doc.SourceVersion
			clampErr.Joint = joint.ID
			clampErr.Frame = frame.Index
			return nil, clampErr
		} else if changed {
			q = clampedQ
			clamped = true
		}
		pf.local[i] = q
		pf.normPPM[i] = normalized.NormResidualPPM
		allTruncs := append(append([]fixed.Truncation(nil), rescaleTruncs...), normalized.Truncations...)
		truncs := make([]NormTruncation, 0, len(allTruncs))
		for _, trunc := range allTruncs {
			truncs = append(truncs, NormTruncation{
				Stage: trunc.Stage, Component: trunc.Component, Remainder: trunc.Remainder,
				Divisor: trunc.Divisor, Mode: string(trunc.Mode),
			})
		}
		pf.normalizations[i] = NormalizationRecord{
			Joint: joint.ID, ResidualPPM: normalized.NormResidualPPM,
			Clamped: clamped, Truncations: truncs,
		}
	}
	for i, joint := range p.doc.Skeleton.Joints {
		if joint.Parent == "" {
			pf.world[i] = pf.local[i]
			continue
		}
		parent := p.sourceIndex[joint.Parent]
		world, truncs, mulErr := fixed.Mul(pf.world[parent], pf.local[i], model.RoundHalfUp)
		if mulErr != nil {
			mulErr.Version = p.doc.SourceVersion
			mulErr.Joint = joint.ID
			mulErr.Frame = frame.Index
			return nil, mulErr
		}
		pf.world[i] = world
		pf.mulPPM[i] = truncationPPM(truncs, p.options.WorkingScale)
	}
	return pf, nil
}

func truncationPPM(truncs []fixed.Truncation, scale int64) int64 {
	total := int64(0)
	for _, trunc := range truncs {
		if trunc.Divisor <= 0 {
			continue
		}
		total += abs64(trunc.Remainder) * 1_000_000 / (trunc.Divisor * scale)
		if trunc.Stage == "rescale" {
			total += 1_000_000 / scale
		}
	}
	return total
}

type frameWork struct {
	mu     sync.Mutex
	frame  FrameResult
	failed *model.Error
	joints []JointPose
}

func ComputeFrames(doc *model.Document, options Options, frameIndices []int) (*Result, error) {
	return ComputeFramesContext(context.Background(), doc, options, frameIndices)
}

func ComputeFramesContext(ctx context.Context, doc *model.Document, options Options, frameIndices []int) (*Result, error) {
	p, prepErr := prepare(doc, options)
	if prepErr != nil {
		return nil, prepErr
	}
	if len(frameIndices) == 0 {
		for _, frame := range doc.Clip.Frames {
			frameIndices = append(frameIndices, frame.Index)
		}
	}
	sort.Ints(frameIndices)
	result := &Result{AlgorithmVersion: AlgorithmVersion, Scale: p.options.WorkingScale}
	workByFrame := make(map[int]*frameWork, len(frameIndices))
	validFrames := make([]int, 0, len(frameIndices))
	for _, frameIndex := range frameIndices {
		frame, ok := doc.FrameIndexOf(frameIndex), p.frames[frameIndex]
		if frame < 0 || ok == nil {
			return nil, &model.Error{Code: model.CodeOrder, Version: doc.SourceVersion, Frame: frameIndex, Message: "requested frame does not exist"}
		}
		work := &frameWork{
			frame: FrameResult{
				Frame: frameIndex, TimeMS: doc.Clip.Frames[frame].TimeMS,
				Joints:         make([]JointPose, len(doc.Target.Joints)),
				Normalizations: append([]NormalizationRecord(nil), p.frames[frameIndex].normalizations...),
			},
			joints: make([]JointPose, len(doc.Target.Joints)),
		}
		for i, joint := range doc.Target.Joints {
			work.joints[i].Joint = joint.ID
		}
		workByFrame[frameIndex] = work
		validFrames = append(validFrames, frameIndex)
	}

	pool := newWorkerPool(p.options.Parallelism, p.options.QueueBudget, p.options.JointBufferLimit)
	defer pool.close()
	for _, level := range p.levels {
		var levelWG sync.WaitGroup
		for _, frameIndex := range validFrames {
			work := workByFrame[frameIndex]
			work.mu.Lock()
			failed := work.failed != nil
			work.mu.Unlock()
			if failed {
				continue
			}
			if message, injected := p.options.FailFrames[frameIndex]; injected {
				work.mu.Lock()
				if work.failed == nil {
					work.failed = &model.Error{Code: model.CodeFrame, Version: doc.SourceVersion, Frame: frameIndex, Message: message}
				}
				work.mu.Unlock()
				continue
			}
			for _, targetIdx := range level {
				targetIdx := targetIdx
				frameIndex := frameIndex
				work := work
				levelWG.Add(1)
				err := pool.submit(ctx, job{bufferSize: 1, label: fmt.Sprintf("frame:%d/joint:%s", frameIndex, doc.Target.Joints[targetIdx].ID), run: func() error {
					defer levelWG.Done()
					if jobErr := computeTargetJoint(p, work, frameIndex, targetIdx); jobErr != nil {
						work.mu.Lock()
						if work.failed == nil {
							work.failed = jobErr
						}
						work.mu.Unlock()
						return jobErr
					}
					return nil
				}})
				if err != nil {
					levelWG.Done()
					pool.close()
					return nil, err
				}
			}
		}
		levelWG.Wait()
	}
	stats := pool.stats()
	stats.WorkerCount = p.options.Parallelism
	result.Queue = stats
	var boundErr *model.Error
	for _, frameIndex := range validFrames {
		work := workByFrame[frameIndex]
		work.mu.Lock()
		if work.failed != nil {
			result.Failures = append(result.Failures, FrameFailure{
				Frame: frameIndex, Code: work.failed.Code, Message: work.failed.Message,
			})
			work.mu.Unlock()
			continue
		}
		work.frame.Joints = work.joints
		if err := attachTruth(p, &work.frame); err != nil && boundErr == nil {
			boundErr = err
		}
		result.Frames = append(result.Frames, work.frame)
		work.mu.Unlock()
	}
	if boundErr != nil {
		return result, boundErr
	}
	return result, nil
}

func Compute(doc *model.Document, options Options) (*Result, error) {
	return ComputeFrames(doc, options, nil)
}

func computeTargetJoint(p *prepared, work *frameWork, frameIndex, targetIdx int) *model.Error {
	doc := p.doc
	targetJoint := doc.Target.Joints[targetIdx]
	mapping := p.mapByTarget[targetJoint.ID]
	sourceIdx := p.sourceIndex[mapping.Source]
	frame := p.frames[frameIndex]
	sourceWorld := frame.world[sourceIdx]
	world, scaleTruncs, adjustErr := fixed.AdjustRotation(sourceWorld, mapping.RotationScale, model.RoundHalfUp)
	if adjustErr != nil {
		adjustErr.Version = doc.SourceVersion
		adjustErr.Joint = targetJoint.ID
		adjustErr.Frame = frameIndex
		return adjustErr
	}
	local := world
	mulTrunc := int64(0)
	if targetJoint.Parent != "" {
		parentIdx := p.targetIndex[targetJoint.Parent]
		var truncs []fixed.Truncation
		var mulErr *model.Error
		local, truncs, mulErr = fixed.Mul(fixed.Inverse(work.joints[parentIdx].WorldRotation), world, model.RoundHalfUp)
		if mulErr != nil {
			mulErr.Version = doc.SourceVersion
			mulErr.Joint = targetJoint.ID
			mulErr.Frame = frameIndex
			return mulErr
		}
		mulTrunc = truncationPPM(truncs, p.options.WorkingScale)
	}
	rest := p.targetRest[targetIdx]
	var position model.FixedVec3
	if targetJoint.Parent == "" {
		position, _, adjustErr = fixed.ScaleVec(frame.root, mapping.TranslationScale, targetJoint.Proportion)
		if adjustErr != nil {
			adjustErr.Version = doc.SourceVersion
			adjustErr.Joint = targetJoint.ID
			adjustErr.Frame = frameIndex
			return adjustErr
		}
	} else {
		parentIdx := p.targetIndex[targetJoint.Parent]
		offset, rotateErr := fixed.RotateVec(work.joints[parentIdx].WorldRotation, rest)
		if rotateErr != nil {
			rotateErr.Version = doc.SourceVersion
			rotateErr.Joint = targetJoint.ID
			rotateErr.Frame = frameIndex
			return rotateErr
		}
		parentPos := work.joints[parentIdx].Position
		position = model.FixedVec3{X: parentPos.X + offset.X, Y: parentPos.Y + offset.Y, Z: parentPos.Z + offset.Z, Scale: parentPos.Scale}
	}
	normPPM := int64(0)
	sourceMulTrunc := int64(0)
	for idx := sourceIdx; ; {
		if frame.normPPM[idx] > normPPM {
			normPPM = frame.normPPM[idx]
		}
		sourceMulTrunc += frame.mulPPM[idx]
		parent := doc.Skeleton.Joints[idx].Parent
		if parent == "" {
			break
		}
		idx = p.sourceIndex[parent]
	}
	mulTrunc += sourceMulTrunc
	chainDepth := p.sourceDepth[sourceIdx] + p.targetDepth[targetIdx] + 2
	mulTerm := int64(chainDepth) * ceilDiv(8_000_000, p.options.WorkingScale)
	scaleTerm := truncationPPM(scaleTruncs, p.options.WorkingScale) + ceilDiv(4_000_000, p.options.WorkingScale)
	mapTerm := int64(chainDepth) * ceilDiv(4_000_000, p.options.WorkingScale)
	bound := normPPM + mulTerm + mulTrunc + scaleTerm + mapTerm + 32
	work.mu.Lock()
	work.joints[targetIdx] = JointPose{
		Joint: targetJoint.ID, WorldRotation: world, LocalRotation: local, Position: position,
		Report: JointReport{
			Joint: targetJoint.ID, NormResidualPPM: normPPM, MulTruncationPPM: mulTerm + mulTrunc,
			ScaleRoundingPPM: scaleTerm, RemapOrientationPPM: mapTerm, BoundPPM: bound, WithinBound: true,
		},
	}
	work.mu.Unlock()
	return nil
}

func attachTruth(p *prepared, frame *FrameResult) *model.Error {
	for i := range frame.Joints {
		joint := &frame.Joints[i]
		sample, ok := p.doc.TruthFor(frame.Frame, joint.Joint)
		if !ok {
			joint.Report.WithinBound = true
			continue
		}
		truthQ, _ := fixed.RescaleQuat(sample.Rotation, p.options.WorkingScale, model.RoundHalfUp)
		truthPos, _ := fixed.RescaleVec(sample.Position, p.options.WorkingScale, model.RoundHalfUp)
		rotationDeviation := fixed.QuatDeltaPPM(joint.WorldRotation, truthQ)
		positionDeviation := fixed.VecDeltaPPM(joint.Position, truthPos)
		joint.Report.TruthBacked = true
		joint.Report.ActualDeviationPPM = &rotationDeviation
		joint.Report.PositionDeviationPPM = &positionDeviation
		joint.Report.WithinBound = rotationDeviation <= joint.Report.BoundPPM
		if !p.options.DisableTruthCheck && rotationDeviation > joint.Report.BoundPPM {
			return &model.Error{
				Code: model.CodeBound, Version: p.doc.SourceVersion, Joint: joint.Joint, Frame: frame.Frame,
				Message: fmt.Sprintf("observed rotation deviation %d ppm exceeds derived upper bound %d ppm", rotationDeviation, joint.Report.BoundPPM),
			}
		}
		if !p.options.DisableTruthCheck && rotationDeviation > p.doc.Tolerance.RotationPPM {
			return &model.Error{
				Code: model.CodeBound, Version: p.doc.SourceVersion, Joint: joint.Joint, Frame: frame.Frame,
				Message: fmt.Sprintf("rotation deviation %d ppm exceeds declared tolerance %d ppm", rotationDeviation, p.doc.Tolerance.RotationPPM),
			}
		}
	}
	return nil
}

func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

type Timer struct {
	Started time.Time
}
