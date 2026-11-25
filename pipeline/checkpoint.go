package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// PersistedWindow is the checkpoint representation of a window. AcceptedSeqs
// are event arrival sequences; aggregate values are stored for inspection and
// resume verification.
type PersistedWindow struct {
	Key            WindowKey         `json:"key"`
	Status         WindowStatus      `json:"status"`
	AcceptedSeqs   []int64           `json:"accepted_seqs"`
	Aggregates     []AggregateResult `json:"aggregates"`
	RecomputeCount int64             `json:"recompute_count"`
}

// CheckpointSnapshot is the atomic unit written after a batch. LastEventSeq is
// updated only after every window and discard row has been written.
type CheckpointSnapshot struct {
	EngineVersion      string            `json:"engine_version"`
	ProjectFingerprint string            `json:"project_fingerprint"`
	ProjectID          string            `json:"project_id"`
	SchemaVersion      string            `json:"schema_version"`
	LastEventSeq       int64             `json:"last_event_seq"`
	WatermarkMs        *int64            `json:"watermark_ms,omitempty"`
	WatermarkEnabled   bool              `json:"watermark_enabled"`
	Windows            []PersistedWindow `json:"windows"`
	Discards           []DiscardRecord   `json:"discards"`
}

// ResumeInfo describes what the backend found.
type ResumeInfo struct {
	Found       bool   `json:"found"`
	Compatible  bool   `json:"compatible"`
	FullRebuild bool   `json:"full_rebuild"`
	ReasonCode  string `json:"reason_code,omitempty"`
	Note        string `json:"note,omitempty"`
	LastSeq     int64  `json:"last_seq"`
}

// CheckpointBackend is implemented by the SQLite store and by test doubles.
type CheckpointBackend interface {
	Reset() error
	Load(project *Project, fingerprint string) (*CheckpointSnapshot, ResumeInfo, error)
	Save(snapshot *CheckpointSnapshot) error
}

// ProjectFingerprint covers the normalized configuration and event stream.
// A checkpoint from another schema version, edit, or engine version is
// rejected rather than partially applied.
func ProjectFingerprint(p *Project) (string, error) {
	if p == nil {
		return "", NewBusinessError(CodeCheckpointStore, "nil project")
	}
	features := append([]FeatureDefinition(nil), p.Features...)
	sort.Slice(features, func(i, j int) bool { return features[i].ID < features[j].ID })
	payload := struct {
		EngineVersion    string              `json:"engine_version"`
		SchemaVersion    string              `json:"schema_version"`
		ProjectID        string              `json:"project_id"`
		TimeOriginMs     int64               `json:"time_origin_ms"`
		TimeTickMs       int64               `json:"time_tick_ms"`
		Alignment        WindowAlignment     `json:"alignment"`
		WeightScale      WeightScale         `json:"weight_scale"`
		WatermarkEnabled bool                `json:"watermark_enabled"`
		Features         []FeatureDefinition `json:"features"`
		Events           []ObservationEvent  `json:"events"`
	}{
		EngineVersion: EngineVersion, SchemaVersion: p.SchemaVersion,
		ProjectID: p.ProjectID, TimeOriginMs: p.TimeOriginMs, TimeTickMs: p.TimeTickMs,
		Alignment: p.Alignment, WeightScale: p.WeightScale,
		WatermarkEnabled: p.WatermarkEnabled, Features: features, Events: p.Events,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", NewBusinessError(CodeCheckpointStore, "fingerprint encode: %v", err).Wrap(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func sortPersistedWindows(windows []PersistedWindow) {
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Key.FeatureID != windows[j].Key.FeatureID {
			return windows[i].Key.FeatureID < windows[j].Key.FeatureID
		}
		return windows[i].Key.StartMs < windows[j].Key.StartMs
	})
}

func sortDiscards(discards []DiscardRecord) {
	sort.Slice(discards, func(i, j int) bool {
		if discards[i].EventSeq != discards[j].EventSeq {
			return discards[i].EventSeq < discards[j].EventSeq
		}
		if discards[i].FeatureID != discards[j].FeatureID {
			return discards[i].FeatureID < discards[j].FeatureID
		}
		return discards[i].Window.StartMs < discards[j].Window.StartMs
	})
}
