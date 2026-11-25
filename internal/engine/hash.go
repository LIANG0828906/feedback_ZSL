package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func canonicalHash(prefix string, value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(append([]byte(prefix), b...))
	return hex.EncodeToString(sum[:])
}

type partitionHashInput struct {
	ProjectID                 string
	SampleRateHz              int
	MinReadableFrames         int
	TransitionToleranceFrames int
	RoundingMode              string
	WeightScale               int
	CategoryTable             []domain.Category
	DialogueID                string
	DialogueSpeaker           string
	Segment                   domain.Segment
}

func ContentHash(p *domain.Project, dialogueIndex, segmentIndex int) string {
	d := p.Dialogues[dialogueIndex]
	s := d.Segments[segmentIndex]
	return canonicalHash("content-v1", partitionHashInput{
		ProjectID:    p.ProjectID,
		SampleRateHz: p.SampleRateHz, MinReadableFrames: p.MinReadableFrames,
		TransitionToleranceFrames: p.TransitionToleranceFrames, RoundingMode: p.RoundingMode,
		WeightScale: p.WeightScale, CategoryTable: p.CategoryTable,
		DialogueID: d.ID, DialogueSpeaker: d.Speaker, Segment: s,
	})
}

func FPSKey(fps int) string {
	return canonicalHash("fps-v1", struct {
		FPS          int    `json:"fps"`
		QuantVersion string `json:"quant_version"`
	}{fps, quant.ParamVersion})
}

func UpstreamKey(contentHash, fpsKey string) string {
	return canonicalHash("upstream-v1", struct {
		ContentHash string `json:"content_hash"`
		FPSKey      string `json:"fps_key"`
	}{contentHash, fpsKey})
}

func CacheKey(contentHash, upstreamKey string) string {
	return canonicalHash("cache-v1", struct {
		ContentHash      string `json:"content_hash"`
		QuantVersion     string `json:"quant_version"`
		UpstreamKey      string `json:"upstream_key"`
		AlgorithmVersion string `json:"algorithm_version"`
	}{contentHash, quant.ParamVersion, upstreamKey, domain.AlgorithmVersion})
}

func ResultDigest(r *domain.PartitionResult) string {
	copy := *r
	copy.ContentHash = ""
	copy.UpstreamKey = ""
	copy.CacheKey = ""
	copy.Digest = ""
	copy.CommitSequence = 0
	copy.SourceSchema = ""
	return canonicalHash("result-v1", copy)
}
