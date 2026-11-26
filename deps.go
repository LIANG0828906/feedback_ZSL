package dialoguecal

import (
	"fmt"
	"sort"
)

type CacheKeys struct {
	ContentHash    string
	UpstreamKey    string
	FeatureKey     string
	CalibrationKey string
}

type DependencyNode struct {
	ID       string
	Kind     string
	Metadata map[string]string
}

type DependencyEdge struct {
	From   string
	To     string
	Reason string
}

type DependencyGraph struct {
	Nodes []DependencyNode
	Edges []DependencyEdge
}

// BuildDependencyGraph materializes the relationships that drive
// invalidation. The graph is descriptive: the cache keys remain the authority
// for deciding whether persisted work can be reused.
func BuildDependencyGraph(p *Project) (*DependencyGraph, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	intervals, err := BuildIntervals(p)
	if err != nil {
		return nil, err
	}
	g := &DependencyGraph{}
	addNode := func(id, kind string, meta map[string]string) {
		g.Nodes = append(g.Nodes, DependencyNode{ID: id, Kind: kind, Metadata: meta})
	}
	addEdge := func(from, to, reason string) {
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Reason: reason})
	}
	projectID := "project:" + p.ID
	addNode(projectID, "project", map[string]string{
		"targetLoudness": fmt.Sprint(p.TargetLoudness),
		"quantization":   p.QuantizationVersion,
		"parallelBudget": p.ParallelVersion,
	})
	for _, d := range p.Dialogues {
		audioID := "audio:" + d.ID
		addNode(audioID, "audio", map[string]string{"pcmHash": PCMSHA256(d.PCM)})
		addEdge(audioID, projectID, "project_input")
		for _, s := range d.Segments {
			segID := "segment:" + d.ID + "/" + s.ID
			addNode(segID, "segment", map[string]string{
				"startSample": fmt.Sprint(s.StartSample), "endSample": fmt.Sprint(s.EndSample),
				"gain": fmt.Sprint(s.Gain),
			})
			addEdge(audioID, segID, "audio_window")
			addEdge(segID, projectID, "declared_boundaries_and_gain")
		}
	}
	for _, iv := range intervals {
		keys, err := IntervalCacheKeys(p, iv)
		if err != nil {
			return nil, err
		}
		segID := "segment:" + iv.DialogueID + "/" + iv.SegmentID
		featureID := "feature:" + iv.ID
		calID := "calibration:" + iv.ID
		markerID := "marker:" + iv.DialogueID + "/" + iv.SegmentID
		addNode(featureID, "frame_feature", map[string]string{
			"featureKey": keys.FeatureKey, "contentHash": keys.ContentHash,
			"upstreamKey": keys.UpstreamKey,
			"startFrame":  fmt.Sprint(iv.StartFrame), "endFrame": fmt.Sprint(iv.EndFrame),
		})
		addNode(calID, "calibration", map[string]string{
			"calibrationKey": keys.CalibrationKey,
			"startFrame":     fmt.Sprint(iv.StartFrame), "endFrame": fmt.Sprint(iv.EndFrame),
		})
		addNode(markerID, "calibration_marker", map[string]string{"segment": iv.SegmentID})
		addEdge(projectID, featureID, "quantization_and_parallel_budget_version")
		addEdge(segID, featureID, "boundaries_phonemes_and_loudness_weights")
		addEdge(featureID, calID, "frame_energy_and_weight")
		addEdge(projectID, calID, "target_loudness_and_gain_ceiling")
		addEdge(segID, calID, "segment_gain_scale")
		addEdge(calID, markerID, "downstream_calibration_marker")
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Reason < g.Edges[j].Reason
	})
	return g, nil
}

func intervalContentHash(p *Project, iv Interval) (string, error) {
	d := dialogueByID(p, iv.DialogueID)
	if d == nil {
		return "", &Error{Code: CodeSchema, Path: "dialogues", Message: "unknown dialogue " + iv.DialogueID}
	}
	seg := segmentByID(d, iv.SegmentID)
	if seg == nil {
		return "", &Error{Code: CodeSchema, Path: "segments", SegmentID: iv.SegmentID, Message: "unknown segment"}
	}
	parts := []string{
		"interval-content-v1", d.ID, seg.ID,
		fmt.Sprint(seg.StartSample), fmt.Sprint(seg.EndSample),
		PCMSHA256(d.PCM),
	}
	for _, ph := range seg.Phonemes {
		parts = append(parts, "ph", ph.ID, fmt.Sprint(ph.StartSample), fmt.Sprint(ph.EndSample), ph.Class, fmt.Sprint(ph.Weight))
	}
	return HashStrings(parts...), nil
}

// IntervalCacheKeys makes dependencies explicit. Raw feature identity cannot
// be changed by gain/target edits; those enter only the calibration key.
func IntervalCacheKeys(p *Project, iv Interval) (CacheKeys, error) {
	d := dialogueByID(p, iv.DialogueID)
	if d == nil {
		return CacheKeys{}, &Error{Code: CodeSchema, Path: "dialogues", Message: "unknown dialogue " + iv.DialogueID}
	}
	seg := segmentByID(d, iv.SegmentID)
	if seg == nil {
		return CacheKeys{}, &Error{Code: CodeSchema, Path: "segments", SegmentID: iv.SegmentID, Message: "unknown segment"}
	}
	content, err := intervalContentHash(p, iv)
	if err != nil {
		return CacheKeys{}, err
	}
	upstream := HashStrings("upstream-v1", p.ID, d.ID, p.QuantizationVersion, p.AlgorithmVersion)
	feature := HashStrings(
		"feature-v1", content, iv.ID, p.QuantizationVersion, p.ParallelVersion,
		fmt.Sprint(p.FrameRateHz), fmt.Sprint(p.SampleRate),
		fmt.Sprint(p.MaxParallelism), fmt.Sprint(p.WorkerBufferSamples), fmt.Sprint(p.QueueBudget),
		upstream,
	)
	calibration := HashStrings(
		"calibration-v1", feature, fmt.Sprint(seg.Gain), fmt.Sprint(p.SessionGain),
		fmt.Sprint(p.TargetLoudness), fmt.Sprint(p.GainUpperBound), fmt.Sprint(p.Tolerance),
		p.ConversionVersion,
	)
	return CacheKeys{
		ContentHash: content, UpstreamKey: upstream, FeatureKey: feature, CalibrationKey: calibration,
	}, nil
}

type InvalidationItem struct {
	IntervalID      string
	DialogueID      string
	SegmentID       string
	StartFrame      int
	EndFrame        int
	Reason          string
	FeatureReusable bool
	MarkerPaths     []string
}

type InvalidationReport struct {
	BeforeFingerprint string
	AfterFingerprint  string
	Items             []InvalidationItem
	Reused            []string
}

func (r *InvalidationReport) Has(intervalID string) bool {
	for _, item := range r.Items {
		if item.IntervalID == intervalID {
			return true
		}
	}
	return false
}

// PlanInvalidation compares two projects using cache keys and, when supplied,
// the actually persisted checkpoint keys. Gain and target edits leave feature
// keys intact and invalidate only the calibration state; a frame-rate edit
// changes interval identities and therefore invalidates features.
func PlanInvalidation(before, after *Project, persisted []IntervalResult) (*InvalidationReport, error) {
	if before == nil || after == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project in invalidation plan"}
	}
	beforeIntervals, err := BuildIntervals(before)
	if err != nil {
		return nil, err
	}
	afterIntervals, err := BuildIntervals(after)
	if err != nil {
		return nil, err
	}
	beforeByID := make(map[string]Interval, len(beforeIntervals))
	beforeKeys := make(map[string]CacheKeys, len(beforeIntervals))
	for _, iv := range beforeIntervals {
		beforeByID[iv.ID] = iv
		k, err := IntervalCacheKeys(before, iv)
		if err != nil {
			return nil, err
		}
		beforeKeys[iv.ID] = k
	}
	persistedByID := make(map[string]IntervalResult, len(persisted))
	for _, r := range persisted {
		persistedByID[r.IntervalID] = r
	}
	havePersistedSnapshot := persisted != nil
	report := &InvalidationReport{
		BeforeFingerprint: CanonicalSemantic(before),
		AfterFingerprint:  CanonicalSemantic(after),
	}
	seen := make(map[string]bool, len(afterIntervals))
	for _, iv := range afterIntervals {
		seen[iv.ID] = true
		keys, err := IntervalCacheKeys(after, iv)
		if err != nil {
			return nil, err
		}
		old, exists := beforeByID[iv.ID]
		oldKeys, haveOldKeys := beforeKeys[iv.ID]
		if !exists {
			report.Items = append(report.Items, InvalidationItem{
				IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
				StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
				Reason: "new_or_requantized_interval", FeatureReusable: false,
				MarkerPaths: []string{fmt.Sprintf("segments.%s.calibration", iv.SegmentID)},
			})
			continue
		}
		if !haveOldKeys || oldKeys.FeatureKey != keys.FeatureKey {
			reason := "feature_dependency_changed"
			if old.StartFrame != iv.StartFrame || old.EndFrame != iv.EndFrame {
				reason = "frame_rate_or_boundary_changed"
			}
			report.Items = append(report.Items, InvalidationItem{
				IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
				StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
				Reason: reason, FeatureReusable: false,
				MarkerPaths: []string{fmt.Sprintf("segments.%s.calibration", iv.SegmentID)},
			})
			continue
		}
		if oldKeys.CalibrationKey != keys.CalibrationKey {
			reason := "calibration_dependency_changed"
			d := dialogueByID(after, iv.DialogueID)
			oldD := dialogueByID(before, iv.DialogueID)
			if d != nil && oldD != nil {
				seg := segmentByID(d, iv.SegmentID)
				oldSeg := segmentByID(oldD, iv.SegmentID)
				switch {
				case seg != nil && oldSeg != nil && seg.Gain != oldSeg.Gain:
					reason = "segment_gain_changed"
				case before.TargetLoudness != after.TargetLoudness:
					reason = "target_loudness_changed"
				case before.SessionGain != after.SessionGain:
					reason = "session_gain_changed"
				case before.Tolerance != after.Tolerance:
					reason = "tolerance_changed"
				}
			}
			report.Items = append(report.Items, InvalidationItem{
				IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
				StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
				Reason: reason, FeatureReusable: true,
				MarkerPaths: []string{fmt.Sprintf("segments.%s.calibration", iv.SegmentID)},
			})
			continue
		}
		reuse := true
		if havePersistedSnapshot {
			r, ok := persistedByID[iv.ID]
			if !ok || r.FeatureKey != keys.FeatureKey || r.CalibrationKey != keys.CalibrationKey {
				reuse = false
			}
		} else {
			reuse = false
		}
		if reuse {
			report.Reused = append(report.Reused, iv.ID)
		} else {
			report.Items = append(report.Items, InvalidationItem{
				IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
				StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
				Reason: "checkpoint_missing_or_version_mismatch", FeatureReusable: false,
				MarkerPaths: []string{fmt.Sprintf("segments.%s.calibration", iv.SegmentID)},
			})
		}
	}
	for _, iv := range beforeIntervals {
		if seen[iv.ID] {
			continue
		}
		report.Items = append(report.Items, InvalidationItem{
			IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
			StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
			Reason: "interval_removed", FeatureReusable: false,
			MarkerPaths: []string{fmt.Sprintf("segments.%s.calibration", iv.SegmentID)},
		})
	}
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].DialogueID != report.Items[j].DialogueID {
			return report.Items[i].DialogueID < report.Items[j].DialogueID
		}
		if report.Items[i].StartFrame != report.Items[j].StartFrame {
			return report.Items[i].StartFrame < report.Items[j].StartFrame
		}
		return report.Items[i].IntervalID < report.Items[j].IntervalID
	})
	sort.Strings(report.Reused)
	return report, nil
}

type EditKind string

const (
	EditSegmentGain EditKind = "segment_gain"
	EditTarget      EditKind = "target_loudness"
	EditFrameRate   EditKind = "frame_rate"
	EditSessionGain EditKind = "session_gain"
	EditTolerance   EditKind = "tolerance"
)

type ProjectEdit struct {
	Kind       EditKind
	SegmentID  string
	DialogueID string
	IntValue   int64
}

func ApplyEdit(p *Project, edit ProjectEdit) (*Project, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	cp := p.Clone()
	switch edit.Kind {
	case EditSegmentGain:
		found := false
		for di := range cp.Dialogues {
			d := &cp.Dialogues[di]
			if edit.DialogueID != "" && d.ID != edit.DialogueID {
				continue
			}
			for si := range d.Segments {
				if d.Segments[si].ID == edit.SegmentID {
					d.Segments[si].Gain = edit.IntValue
					found = true
				}
			}
		}
		if !found {
			return nil, &Error{Code: CodeSchema, Path: "segments", SegmentID: edit.SegmentID, Message: "edit references unknown segment"}
		}
	case EditTarget:
		cp.TargetLoudness = edit.IntValue
	case EditFrameRate:
		cp.FrameRateHz = int(edit.IntValue)
	case EditSessionGain:
		cp.SessionGain = edit.IntValue
	case EditTolerance:
		cp.Tolerance = edit.IntValue
	default:
		return nil, &Error{Code: CodeSchema, Path: "$.edit.kind", Message: "unknown edit kind"}
	}
	if err := ValidateProject(cp); err != nil {
		return nil, err
	}
	return cp, nil
}
