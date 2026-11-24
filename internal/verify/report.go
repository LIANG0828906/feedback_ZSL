package verify

import (
	"sort"

	"screenverify/internal/model"
)

func (e *Engine) BuildReport(recovery *RecoveryInfo) *Report {
	n := e.set.Truth.FrameCount
	r := &Report{
		Clip:             e.set.Truth.Clip,
		TruthFingerprint: e.truthFP,
		ContentRoot:      e.contentRoot,
		AlgorithmVersion: model.AlgorithmVersion,
		Params:           e.params,
		ParamVersion:     ParamVersion(e.params),
		Watermarks:       make([]WatermarkEvidence, 0, n),
		Timelines:        make([]TimelineEvidence, 0, n),
		Residuals:        make([]ResidualEvidence, 0, n),
		Noises:           make([]NoiseEvidence, 0, n),
		Edges:            make([]EdgeEvidence, 0, n),
		Decisions:        make([]DecisionEvidence, 0, n),
		CommitSequence:   append([]CommitItem(nil), e.commits...),
		Invalidations:    append([]Invalidation(nil), e.lastInvalidations...),
		DependencyGraph:  DependencyGraph(),
		Failures:         append([]FailureRecord(nil), e.failures...),
		Recovery:         recovery,
	}
	for frame := 1; frame <= n; frame++ {
		if v, err := e.watermark(frame); err == nil {
			r.Watermarks = append(r.Watermarks, v)
		}
		if v, err := e.timeline(frame); err == nil {
			r.Timelines = append(r.Timelines, v)
		}
		if v, err := e.residual(frame); err == nil {
			r.Residuals = append(r.Residuals, v)
		}
		if v, err := e.noise(frame); err == nil {
			r.Noises = append(r.Noises, v)
		}
		if v, err := e.edge(frame); err == nil {
			r.Edges = append(r.Edges, v)
		}
		if v, err := e.decision(frame); err == nil {
			r.Decisions = append(r.Decisions, v)
		}
	}
	r.Intervals = LocateIntervals(r.Decisions, e.params.FrameTolerance)
	r.SubmissionSequence = e.canonicalSubmission()
	r.Stats = e.statsSnapshot()
	return r
}

func (e *Engine) decision(frame int) (DecisionEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvDecision})
	if !ok {
		return DecisionEvidence{}, errMissing(frame, EvDecision)
	}
	return v.(DecisionEvidence), nil
}

func errMissing(frame int, t EvidenceType) error {
	return &missingEvidenceError{frame: frame, typ: t}
}

type missingEvidenceError struct {
	frame int
	typ   EvidenceType
}

func (e *missingEvidenceError) Error() string {
	return "missing evidence frame=" + itoa(e.frame) + " type=" + string(e.typ)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func (e *Engine) canonicalSubmission() []CommitItem {
	out := []CommitItem{}
	ordinal := 0
	for frame := 1; frame <= e.set.Truth.FrameCount; frame++ {
		for _, typ := range allTypes() {
			id := NodeID{Frame: frame, Type: typ}
			e.stateMu.Lock()
			key, ok := e.keys[id]
			e.stateMu.Unlock()
			if !ok || key == "" {
				continue
			}
			out = append(out, CommitItem{
				Ordinal: ordinal, BatchID: "final-state", Frame: frame,
				EvidenceType: typ, CacheKey: key,
			})
			ordinal++
		}
	}
	return out
}

func (e *Engine) statsSnapshot() Stats {
	e.stateMu.Lock()
	s := e.stats
	e.stateMu.Unlock()
	s.QueueWaits = e.limiter.Waits()
	if s.QueueWaits == nil {
		s.QueueWaits = []QueueWait{}
	}
	return s
}

func (r *Report) Snapshot() Snapshot {
	return Snapshot{
		Decisions:          append([]DecisionEvidence(nil), r.Decisions...),
		Noises:             append([]NoiseEvidence(nil), r.Noises...),
		Intervals:          append([]Interval(nil), r.Intervals...),
		CommitSequence:     append([]CommitItem(nil), r.CommitSequence...),
		SubmissionSequence: append([]CommitItem(nil), r.SubmissionSequence...),
	}
}

func SortInvalidations(in []Invalidation) []Invalidation {
	out := append([]Invalidation(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Frame != out[j].Frame {
			return out[i].Frame < out[j].Frame
		}
		return typeRank(out[i].EvidenceType) < typeRank(out[j].EvidenceType)
	})
	return out
}
