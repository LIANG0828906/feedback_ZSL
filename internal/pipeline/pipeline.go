package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"skeletonretarget/internal/checkpoint"
	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/retarget"
	"skeletonretarget/internal/state"
)

type Strategy string

const (
	Load        Strategy = "load"
	Incremental Strategy = "incremental"
	Recovery    Strategy = "recovery"
)

var ErrInterrupted = errors.New("run interrupted after committed interval")

type EditKind string

const (
	EditConstraint EditKind = "constraint"
	EditMapping    EditKind = "mapping"
	EditPrecision  EditKind = "precision"
	EditTopology   EditKind = "topology"
)

type Edit struct {
	Kind       EditKind
	Joint      string
	Target     string
	Parent     string
	Reason     string
	Constraint *model.Constraint
	Mapping    *model.Mapping
}

type Options struct {
	DBPath       string
	IntervalSize int
	Retarget     retarget.Options
	ParamVersion string
	StopAfter    int
	ResetOnLoad  bool
}

func DefaultOptions(dbPath string) Options {
	return Options{
		DBPath: dbPath, IntervalSize: 4, Retarget: retarget.DefaultOptions(),
	}
}

type IntervalRun struct {
	IntervalID int      `json:"intervalId"`
	Frames     []int    `json:"frames"`
	CacheKey   string   `json:"cacheKey"`
	CacheHit   bool     `json:"cacheHit"`
	State      string   `json:"state"`
	Deps       []string `json:"dependencies"`
}

type Invalidation struct {
	IntervalID int      `json:"intervalId"`
	Frames     []int    `json:"frames"`
	Joints     []string `json:"joints"`
	Reasons    []string `json:"reasons"`
}

type Comparison struct {
	Left       string   `json:"left"`
	Right      string   `json:"right"`
	Equal      bool     `json:"equal"`
	Mismatches []string `json:"mismatches,omitempty"`
}

type Report struct {
	Path             Strategy                `json:"path"`
	ActionHash       string                  `json:"actionHash"`
	TopologyVersion  string                  `json:"topologyVersion"`
	MappingVersion   string                  `json:"mappingVersion"`
	ParamVersion     string                  `json:"paramVersion"`
	AlgorithmVersion string                  `json:"algorithmVersion"`
	Intervals        []IntervalRun           `json:"intervals"`
	Invalidations    []Invalidation          `json:"invalidations"`
	Frames           []retarget.FrameResult  `json:"frames"`
	Failures         []retarget.FrameFailure `json:"failures,omitempty"`
	Queue            retarget.QueueStats     `json:"queue"`
	RecoveredTo      int                     `json:"recoveredTo"`
	Complete         bool                    `json:"complete"`
	Interrupted      bool                    `json:"interrupted"`
}

type intervalPlan struct {
	ID         int
	FrameIdx   []int
	FrameStart int
	FrameEnd   int
	Deps       []string
	DepHash    string
	CacheKey   string
}

type runPlan struct {
	actionHash      string
	topologyVersion string
	mappingVersion  string
	paramVersion    string
	algorithm       string
	intervals       []intervalPlan
}

func Run(ctx context.Context, doc *model.Document, options Options, strategy Strategy, edits []Edit) (*Report, error) {
	if doc == nil {
		return nil, &model.Error{Code: model.CodeSchema, Frame: -1, Message: "document is nil"}
	}
	if options.IntervalSize <= 0 {
		options.IntervalSize = 4
	}
	if options.Retarget.WorkingScale == 0 {
		options.Retarget = retarget.DefaultOptions()
	}
	if options.DBPath == "" {
		return nil, &model.Error{Code: model.CodeState, Frame: -1, Message: "checkpoint database path is required"}
	}
	plan, err := buildPlan(doc, options)
	if err != nil {
		return nil, err
	}
	store, openErr := checkpoint.Open(options.DBPath)
	if openErr != nil {
		return nil, openErr
	}
	defer store.Close()
	if strategy == Load || options.ResetOnLoad {
		if resetErr := store.Reset(ctx); resetErr != nil {
			return nil, resetErr
		}
	}
	if beginErr := store.Begin(ctx, checkpoint.Meta{ActionHash: plan.actionHash, AlgorithmVersion: plan.algorithm}); beginErr != nil {
		return nil, beginErr
	}
	report := &Report{
		Path: strategy, ActionHash: plan.actionHash, TopologyVersion: plan.topologyVersion,
		MappingVersion: plan.mappingVersion, ParamVersion: plan.paramVersion, AlgorithmVersion: plan.algorithm,
	}
	if strategy != Load && len(edits) > 0 {
		invalidationList, first := affects(doc, plan, options, edits)
		if first >= 0 {
			if invalidateErr := store.InvalidateFrom(ctx, first, "explicit dependency edit"); invalidateErr != nil {
				return nil, invalidateErr
			}
			report.Invalidations = invalidationList
		}
	}
	expected := make(map[int]intervalPlan, len(plan.intervals))
	for _, interval := range plan.intervals {
		expected[interval.ID] = interval
	}
	resume, resumeErr := store.Resume(ctx, checkpoint.Meta{ActionHash: plan.actionHash, AlgorithmVersion: plan.algorithm}, func(record checkpoint.IntervalRecord) bool {
		interval, ok := expected[record.IntervalID]
		return ok && record.CacheKey == interval.CacheKey &&
			record.TopologyVersion == plan.topologyVersion &&
			record.ParamVersion == plan.paramVersion &&
			record.DependencyHash == interval.DepHash &&
			record.AlgorithmVersion == plan.algorithm
	})
	if resumeErr != nil {
		return nil, resumeErr
	}
	if resume.InvalidatedFrom > 0 || (resume.Invalidated && resume.InvalidatedFrom == 0) {
		report.Invalidations = mergeInvalidations(report.Invalidations, invalidationFrom(doc, plan, resume.InvalidatedFrom, "checkpoint "+resume.Reason))
	}
	cached := make(map[int]checkpoint.IntervalRecord, len(resume.Records))
	for _, record := range resume.Records {
		cached[record.IntervalID] = record
	}
	report.RecoveredTo = len(resume.Records)
	committed := 0
	var runErr error
	for _, interval := range plan.intervals {
		if options.StopAfter > 0 && committed >= options.StopAfter {
			report.Interrupted = true
			runErr = ErrInterrupted
			break
		}
		if err := ctx.Err(); err != nil {
			report.Interrupted = true
			runErr = err
			break
		}
		intervalReport := IntervalRun{
			IntervalID: interval.ID, Frames: append([]int(nil), interval.FrameIdx...),
			CacheKey: interval.CacheKey, Deps: append([]string(nil), interval.Deps...),
		}
		if record, ok := cached[interval.ID]; ok {
			var result retarget.Result
			if err := json.Unmarshal(record.Payload, &result); err != nil {
				return nil, &model.Error{Code: model.CodeDecode, Frame: interval.FrameStart, Message: "cached interval payload is corrupt", Cause: err}
			}
			report.Frames = append(report.Frames, result.Frames...)
			intervalReport.CacheHit = true
			intervalReport.State = string(state.Committed)
			report.Intervals = append(report.Intervals, intervalReport)
			committed++
			continue
		}
		machine := state.New(state.PendingLoad, interval.FrameStart)
		if transitionErr := machine.Transition(state.PendingRetarget, interval.FrameStart); transitionErr != nil {
			return nil, transitionErr
		}
		if transitionErr := machine.Transition(state.Retargeting, interval.FrameStart); transitionErr != nil {
			return nil, transitionErr
		}
		result, computeErr := retarget.ComputeFramesContext(ctx, doc, options.Retarget, interval.FrameIdx)
		if computeErr != nil {
			_ = machine.Transition(state.Failed, interval.FrameStart)
			intervalReport.State = string(state.Failed)
			report.Intervals = append(report.Intervals, intervalReport)
			report.Queue = addQueue(report.Queue, maybeQueue(result))
			if result != nil {
				report.Failures = append(report.Failures, result.Failures...)
			}
			return report, computeErr
		}
		report.Queue = addQueue(report.Queue, result.Queue)
		if len(result.Failures) > 0 {
			_ = machine.Transition(state.Failed, interval.FrameStart)
			intervalReport.State = string(state.Failed)
			report.Intervals = append(report.Intervals, intervalReport)
			report.Failures = append(report.Failures, result.Failures...)
			first := result.Failures[0]
			return report, &model.Error{Code: first.Code, Version: doc.SourceVersion, Frame: first.Frame, Message: first.Message}
		}
		if len(result.Frames) != len(interval.FrameIdx) {
			_ = machine.Transition(state.Failed, interval.FrameStart)
			report.Intervals = append(report.Intervals, intervalReport)
			return report, &model.Error{Code: model.CodeFrame, Frame: interval.FrameStart, Message: "interval returned an incomplete frame set"}
		}
		if err := ctx.Err(); err != nil {
			report.Interrupted = true
			return report, err
		}
		payload, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, marshalErr
		}
		summary, summaryErr := intervalSummary(result)
		if summaryErr != nil {
			return nil, summaryErr
		}
		record, commitErr := store.CommitInterval(ctx, checkpoint.IntervalRecord{
			IntervalID: interval.ID, FrameStart: interval.FrameStart, FrameEnd: interval.FrameEnd,
			State: string(state.Committed), Payload: payload, Summary: summary,
			Dependencies: interval.Deps, DependencyHash: interval.DepHash, CacheKey: interval.CacheKey,
			TopologyVersion: plan.topologyVersion, ParamVersion: plan.paramVersion,
			AlgorithmVersion: plan.algorithm, StateProgress: interval.FrameEnd,
		})
		if commitErr != nil {
			_ = machine.Transition(state.Failed, interval.FrameStart)
			return nil, commitErr
		}
		_ = record
		if transitionErr := machine.Transition(state.Committed, interval.FrameEnd); transitionErr != nil {
			return nil, transitionErr
		}
		intervalReport.State = string(state.Committed)
		report.Intervals = append(report.Intervals, intervalReport)
		report.Frames = append(report.Frames, result.Frames...)
		committed++
	}
	sort.Slice(report.Frames, func(i, j int) bool { return report.Frames[i].Frame < report.Frames[j].Frame })
	sort.Slice(report.Intervals, func(i, j int) bool { return report.Intervals[i].IntervalID < report.Intervals[j].IntervalID })
	report.Complete = len(report.Frames) == len(doc.Clip.Frames)
	return report, runErr
}

func buildPlan(doc *model.Document, options Options) (*runPlan, *model.Error) {
	derivedParam := paramFingerprint(doc, options)
	paramVersion := derivedParam
	if options.ParamVersion != "" {
		paramVersion = options.ParamVersion + ":" + derivedParam
	}
	plan := &runPlan{
		actionHash: doc.ContentHash(), topologyVersion: doc.TopologyHash(),
		mappingVersion: doc.MappingHash(), paramVersion: paramVersion,
		algorithm: retarget.AlgorithmVersion,
	}
	frames := doc.Clip.Frames
	upstream := "root:" + plan.actionHash
	for start := 0; start < len(frames); start += options.IntervalSize {
		end := start + options.IntervalSize
		if end > len(frames) {
			end = len(frames)
		}
		intervalID := len(plan.intervals)
		frameIdx := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			frameIdx = append(frameIdx, frames[i].Index)
		}
		deps, depErr := intervalDependencies(doc, options, intervalID, frameIdx, upstream)
		if depErr != nil {
			return nil, depErr
		}
		depBytes, _ := json.Marshal(deps)
		depSum := sha256.Sum256(depBytes)
		depHash := hex.EncodeToString(depSum[:])
		cacheKey := cacheKey(plan.actionHash, intervalID, plan.topologyVersion, plan.paramVersion, depHash, upstream)
		plan.intervals = append(plan.intervals, intervalPlan{
			ID: intervalID, FrameIdx: frameIdx, FrameStart: frames[start].Index, FrameEnd: frames[end-1].Index,
			Deps: deps, DepHash: depHash, CacheKey: cacheKey,
		})
		upstream = cacheKey
	}
	return plan, nil
}

func paramFingerprint(doc *model.Document, options Options) string {
	h := sha256.New()
	fmt.Fprintf(h, "scale=%d;tol=%d/%d/%d;mapping=%s",
		options.Retarget.WorkingScale, doc.Tolerance.NormPPM, doc.Tolerance.RoundTripPPM,
		doc.Tolerance.RotationPPM, doc.MappingHash())
	return hex.EncodeToString(h.Sum(nil))
}

func intervalDependencies(doc *model.Document, options Options, intervalID int, frameIndices []int, upstream string) ([]string, *model.Error) {
	deps := []string{
		"action:" + doc.ContentHash(),
		"mapping:" + doc.MappingHash(),
		"topology:" + doc.TopologyHash(),
		"precision:" + fmt.Sprintf("%d", options.Retarget.WorkingScale),
		"upstream:" + upstream,
		fmt.Sprintf("interval:%d", intervalID),
		fmt.Sprintf("frames:%d-%d", frameIndices[0], frameIndices[len(frameIndices)-1]),
	}
	active, err := activeConstraints(doc, options.Retarget.WorkingScale, frameIndices)
	if err != nil {
		return nil, err
	}
	deps = append(deps, active...)
	sort.Strings(deps)
	return deps, nil
}

func activeConstraints(doc *model.Document, scale int64, frameIndices []int) ([]string, *model.Error) {
	var deps []string
	seen := make(map[string]bool)
	for _, frameIndex := range frameIndices {
		idx := doc.FrameIndexOf(frameIndex)
		if idx < 0 {
			return nil, &model.Error{Code: model.CodeOrder, Version: doc.SourceVersion, Frame: frameIndex, Message: "dependency frame is missing"}
		}
		frame := doc.Clip.Frames[idx]
		for i, joint := range doc.Skeleton.Joints {
			raw, _ := fixed.RescaleQuat(frame.Rotations[i], scale, model.RoundHalfUp)
			normalized, normErr := fixed.Normalize(raw, model.RoundHalfUp)
			if normErr != nil {
				normErr.Version = doc.SourceVersion
				normErr.Joint = joint.ID
				normErr.Frame = frameIndex
				return nil, normErr
			}
			angle, angleErr := fixed.AngleRadians(normalized.Q)
			if angleErr != nil {
				return nil, angleErr
			}
			minAngle := float64(joint.Constraint.MinRad) / float64(joint.Constraint.Scale)
			maxAngle := float64(joint.Constraint.MaxRad) / float64(joint.Constraint.Scale)
			if angle < minAngle-1e-12 || angle > maxAngle+1e-12 {
				key := fmt.Sprintf("constraint:%s:%d/%d@%d", joint.ID, joint.Constraint.MinRad, joint.Constraint.MaxRad, frameIndex)
				if !seen[key] {
					seen[key] = true
					deps = append(deps, key)
				}
			}
		}
	}
	sort.Strings(deps)
	return deps, nil
}

func cacheKey(actionHash string, intervalID int, topologyVersion, paramVersion, depHash, upstream string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s",
		actionHash, intervalID, topologyVersion, paramVersion, depHash, upstream)))
	return hex.EncodeToString(sum[:])
}

func affects(doc *model.Document, plan *runPlan, options Options, edits []Edit) ([]Invalidation, int) {
	byInterval := make(map[int]map[string]bool)
	reasons := make(map[int]map[string]bool)
	first := -1
	allTargets := make([]string, 0, len(doc.Target.Joints))
	for _, joint := range doc.Target.Joints {
		allTargets = append(allTargets, joint.ID)
	}
	intervalOfFrame := make(map[int]intervalPlan)
	for _, interval := range plan.intervals {
		for _, frame := range interval.FrameIdx {
			intervalOfFrame[frame] = interval
		}
	}
	for _, edit := range edits {
		reason := edit.Reason
		if reason == "" {
			reason = string(edit.Kind)
		}
		switch edit.Kind {
		case EditConstraint:
			affectedFrames := constraintFrames(doc, options.Retarget.WorkingScale, edit.Joint)
			affectedJoints := sourceAffectedTargets(doc, edit.Joint)
			for frame := range affectedFrames {
				interval := intervalOfFrame[frame]
				if first < 0 || interval.ID < first {
					first = interval.ID
				}
				if byInterval[interval.ID] == nil {
					byInterval[interval.ID] = make(map[string]bool)
					reasons[interval.ID] = make(map[string]bool)
				}
				for _, joint := range affectedJoints {
					byInterval[interval.ID][joint] = true
				}
				reasons[interval.ID][reason] = true
			}
		case EditMapping, EditPrecision, EditTopology:
			joints := allTargets
			if edit.Target != "" {
				joints = targetDescendants(doc, edit.Target)
			}
			if edit.Joint != "" {
				joints = sourceAffectedTargets(doc, edit.Joint)
			}
			for _, interval := range plan.intervals {
				if first < 0 || interval.ID < first {
					first = interval.ID
				}
				if byInterval[interval.ID] == nil {
					byInterval[interval.ID] = make(map[string]bool)
					reasons[interval.ID] = make(map[string]bool)
				}
				for _, joint := range joints {
					byInterval[interval.ID][joint] = true
				}
				reasons[interval.ID][reason] = true
			}
		}
	}
	if first < 0 {
		return nil, -1
	}
	var out []Invalidation
	for _, interval := range plan.intervals {
		if interval.ID < first {
			continue
		}
		jointSet := byInterval[interval.ID]
		reasonSet := reasons[interval.ID]
		if jointSet == nil {
			jointSet = make(map[string]bool)
			for _, joint := range allTargets {
				jointSet[joint] = true
			}
		}
		if reasonSet == nil {
			reasonSet = make(map[string]bool)
			reasonSet["downstream cache-key dependency"] = true
		}
		joints := make([]string, 0, len(jointSet))
		for joint := range jointSet {
			joints = append(joints, joint)
		}
		sort.Strings(joints)
		why := make([]string, 0, len(reasonSet))
		for reason := range reasonSet {
			why = append(why, reason)
		}
		sort.Strings(why)
		out = append(out, Invalidation{
			IntervalID: interval.ID, Frames: append([]int(nil), interval.FrameIdx...),
			Joints: joints, Reasons: why,
		})
	}
	return out, first
}

func constraintFrames(doc *model.Document, scale int64, jointID string) map[int]bool {
	affected := make(map[int]bool)
	jointIdx, ok := doc.Skeleton.Index()[jointID]
	if !ok {
		return affected
	}
	constraint := doc.Skeleton.Joints[jointIdx].Constraint
	for _, frame := range doc.Clip.Frames {
		raw, _ := fixed.RescaleQuat(frame.Rotations[jointIdx], scale, model.RoundHalfUp)
		normalized, err := fixed.Normalize(raw, model.RoundHalfUp)
		if err != nil {
			continue
		}
		angle, err := fixed.AngleRadians(normalized.Q)
		if err != nil {
			continue
		}
		minAngle := float64(constraint.MinRad) / float64(constraint.Scale)
		maxAngle := float64(constraint.MaxRad) / float64(constraint.Scale)
		if angle < minAngle-1e-12 || angle > maxAngle+1e-12 {
			affected[frame.Index] = true
		}
	}
	return affected
}

func sourceAffectedTargets(doc *model.Document, sourceJoint string) []string {
	descendants := make(map[string]bool)
	descendants[sourceJoint] = true
	changed := true
	for changed {
		changed = false
		for _, joint := range doc.Skeleton.Joints {
			if descendants[joint.Parent] && !descendants[joint.ID] {
				descendants[joint.ID] = true
				changed = true
			}
		}
	}
	seen := make(map[string]bool)
	var targets []string
	for _, mapping := range doc.Retarget.Mappings {
		if descendants[mapping.Source] && !seen[mapping.Target] {
			seen[mapping.Target] = true
			targets = append(targets, mapping.Target)
		}
	}
	for _, target := range append([]string(nil), targets...) {
		for _, descendant := range targetDescendants(doc, target) {
			if !seen[descendant] {
				seen[descendant] = true
				targets = append(targets, descendant)
			}
		}
	}
	sort.Strings(targets)
	return targets
}

func targetDescendants(doc *model.Document, target string) []string {
	descendants := []string{target}
	changed := true
	for changed {
		changed = false
		for _, joint := range doc.Target.Joints {
			for _, existing := range descendants {
				if joint.Parent == existing && !contains(descendants, joint.ID) {
					descendants = append(descendants, joint.ID)
					changed = true
					break
				}
			}
		}
	}
	sort.Strings(descendants)
	return descendants
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func invalidationFrom(doc *model.Document, plan *runPlan, first int, reason string) []Invalidation {
	var out []Invalidation
	for _, interval := range plan.intervals {
		if interval.ID < first {
			continue
		}
		joints := make([]string, 0, len(doc.Target.Joints))
		for _, joint := range doc.Target.Joints {
			joints = append(joints, joint.ID)
		}
		out = append(out, Invalidation{
			IntervalID: interval.ID, Frames: append([]int(nil), interval.FrameIdx...),
			Joints: joints, Reasons: []string{reason},
		})
	}
	return out
}

func mergeInvalidations(existing, incoming []Invalidation) []Invalidation {
	byID := make(map[int]Invalidation)
	for _, item := range existing {
		byID[item.IntervalID] = item
	}
	for _, item := range incoming {
		if old, ok := byID[item.IntervalID]; ok {
			old.Reasons = append(old.Reasons, item.Reasons...)
			old.Joints = union(old.Joints, item.Joints)
			byID[item.IntervalID] = old
		} else {
			byID[item.IntervalID] = item
		}
	}
	out := make([]Invalidation, 0, len(byID))
	for _, item := range byID {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IntervalID < out[j].IntervalID })
	return out
}

func union(a, b []string) []string {
	seen := make(map[string]bool)
	for _, value := range a {
		seen[value] = true
	}
	for _, value := range b {
		seen[value] = true
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func intervalSummary(result *retarget.Result) (json.RawMessage, error) {
	type digest struct {
		Frame          int    `json:"frame"`
		RotationDigest string `json:"rotationDigest"`
		PositionDigest string `json:"positionDigest"`
		MaxBoundPPM    int64  `json:"maxBoundPpm"`
	}
	type summary struct {
		Frames []digest `json:"frames"`
	}
	out := summary{}
	for _, frame := range result.Frames {
		rHash := sha256.New()
		pHash := sha256.New()
		maxBound := int64(0)
		for _, joint := range frame.Joints {
			fmt.Fprintf(rHash, "%s:%d,%d,%d,%d;", joint.Joint, joint.WorldRotation.W, joint.WorldRotation.X, joint.WorldRotation.Y, joint.WorldRotation.Z)
			fmt.Fprintf(pHash, "%s:%d,%d,%d;", joint.Joint, joint.Position.X, joint.Position.Y, joint.Position.Z)
			if joint.Report.BoundPPM > maxBound {
				maxBound = joint.Report.BoundPPM
			}
		}
		out.Frames = append(out.Frames, digest{
			Frame: frame.Frame, RotationDigest: hex.EncodeToString(rHash.Sum(nil)),
			PositionDigest: hex.EncodeToString(pHash.Sum(nil)), MaxBoundPPM: maxBound,
		})
	}
	return json.Marshal(out)
}

func addQueue(a, b retarget.QueueStats) retarget.QueueStats {
	a.Submitted += b.Submitted
	a.FullWaits += b.FullWaits
	a.WaitNanos += b.WaitNanos
	if b.MaxQueueDepth > a.MaxQueueDepth {
		a.MaxQueueDepth = b.MaxQueueDepth
	}
	if b.WorkerCount > a.WorkerCount {
		a.WorkerCount = b.WorkerCount
	}
	if b.MaxJointState > a.MaxJointState {
		a.MaxJointState = b.MaxJointState
	}
	a.Waits = append(a.Waits, b.Waits...)
	return a
}

func maybeQueue(result *retarget.Result) retarget.QueueStats {
	if result == nil {
		return retarget.QueueStats{}
	}
	return result.Queue
}

func CompareFrames(left, right []retarget.FrameResult) Comparison {
	comparison := Comparison{Equal: true}
	if len(left) != len(right) {
		comparison.Equal = false
		comparison.Mismatches = append(comparison.Mismatches, fmt.Sprintf("frame count %d != %d", len(left), len(right)))
	}
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[i].Frame != right[i].Frame || left[i].TimeMS != right[i].TimeMS {
			comparison.Equal = false
			comparison.Mismatches = append(comparison.Mismatches, fmt.Sprintf("frame identity at index %d", i))
			continue
		}
		if len(left[i].Joints) != len(right[i].Joints) {
			comparison.Equal = false
			comparison.Mismatches = append(comparison.Mismatches, fmt.Sprintf("frame %d joint count differs", left[i].Frame))
			continue
		}
		for j := range left[i].Joints {
			lj, rj := left[i].Joints[j], right[i].Joints[j]
			if lj.Joint != rj.Joint || lj.WorldRotation != rj.WorldRotation ||
				lj.LocalRotation != rj.LocalRotation || lj.Position != rj.Position {
				comparison.Equal = false
				comparison.Mismatches = append(comparison.Mismatches,
					fmt.Sprintf("frame %d joint %s differs", left[i].Frame, lj.Joint))
			}
		}
	}
	if !comparison.Equal && len(comparison.Mismatches) > 12 {
		comparison.Mismatches = comparison.Mismatches[:12]
	}
	return comparison
}

func FramesDigest(frames []retarget.FrameResult) string {
	h := sha256.New()
	for _, frame := range frames {
		fmt.Fprintf(h, "%d/%d:", frame.Frame, frame.TimeMS)
		for _, joint := range frame.Joints {
			fmt.Fprintf(h, "%s=%d,%d,%d,%d|%d,%d,%d;", joint.Joint,
				joint.WorldRotation.W, joint.WorldRotation.X, joint.WorldRotation.Y, joint.WorldRotation.Z,
				joint.Position.X, joint.Position.Y, joint.Position.Z)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func SummarizeInvalidations(items []Invalidation) string {
	var parts []string
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("#%d[%v]{%s}", item.IntervalID, item.Frames, strings.Join(item.Reasons, ",")))
	}
	return strings.Join(parts, " ")
}
