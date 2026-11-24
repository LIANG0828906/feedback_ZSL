package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func HashJSON(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

func HashJSONMust(v any) string {
	hash, err := HashJSON(v)
	if err != nil {
		panic(err)
	}
	return hash
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func PriorityVersion(seg Segment) string {
	type p struct {
		Higher string `json:"higher"`
		Lower  string `json:"lower"`
		Basis  string `json:"basis"`
	}
	rules := make([]p, len(seg.Priorities))
	for i, rule := range seg.Priorities {
		rules[i] = p{Higher: rule.Higher, Lower: rule.Lower, Basis: rule.Basis}
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Higher != rules[j].Higher {
			return rules[i].Higher < rules[j].Higher
		}
		if rules[i].Lower != rules[j].Lower {
			return rules[i].Lower < rules[j].Lower
		}
		return rules[i].Basis < rules[j].Basis
	})
	hash, _ := HashJSON(rules)
	return hash
}

func SnapshotOf(seg Segment) SegmentSnapshot {
	return SegmentSnapshot{
		SegmentID:   seg.ID,
		StartBeat:   seg.StartBeat,
		EndBeat:     seg.EndBeat,
		TempoBPM:    seg.TempoBPM,
		NoteDensity: seg.NoteDensity,
		Channels:    append([]Channel(nil), CloneSegment(seg).Channels...),
		Priorities:  append([]PriorityRule(nil), seg.Priorities...),
	}
}

func CheckpointContentHash(snapshot SegmentSnapshot, summary ResultSummary) (string, error) {
	payload := struct {
		Snapshot SegmentSnapshot `json:"snapshot"`
		Summary  ResultSummary   `json:"summary"`
	}{
		Snapshot: snapshot,
		Summary:  summary,
	}
	return HashJSON(payload)
}

func SegmentFromSnapshot(snapshot SegmentSnapshot) Segment {
	return Segment{
		ID:          snapshot.SegmentID,
		StartBeat:   snapshot.StartBeat,
		EndBeat:     snapshot.EndBeat,
		TempoBPM:    snapshot.TempoBPM,
		NoteDensity: snapshot.NoteDensity,
		Channels:    CloneSegment(Segment{Channels: snapshot.Channels}).Channels,
		Priorities:  append([]PriorityRule(nil), snapshot.Priorities...),
		Truth:       Truth{Interpolation: InterpolationLinear},
	}
}

func CheckpointSnapshot(seg Segment, result SegmentResult, ops []EditOp, params Parameters) (Checkpoint, error) {
	snapshot := SnapshotOf(seg)
	summary := ResultSummary{
		SegmentID:     result.SegmentID,
		SampleCount:   len(result.Samples),
		DecisionCount: countDecisions(result),
	}
	resultHash, err := HashJSON(result)
	if err != nil {
		return Checkpoint{}, err
	}
	summary.ResultHash = resultHash
	contentHash, err := CheckpointContentHash(snapshot, summary)
	if err != nil {
		return Checkpoint{}, err
	}
	opsHash, err := HashJSON(ops)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{
		BatchID:          "",
		SegmentID:        seg.ID,
		Snapshot:         snapshot,
		Summary:          summary,
		PriorityVersion:  PriorityVersion(seg),
		ContentHash:      contentHash,
		ParamVersion:     params.ParamVersion,
		AlgorithmVersion: params.AlgorithmVersion,
		Operations:       append([]EditOp(nil), ops...),
		OperationsHash:   opsHash,
	}, nil
}

func countDecisions(result SegmentResult) int {
	n := 0
	for _, sample := range result.Samples {
		n += len(sample.Decisions)
	}
	return n
}
