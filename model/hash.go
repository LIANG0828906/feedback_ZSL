package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func SHA256JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		sum := sha256.Sum256([]byte("marshal-error:" + err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type contentHashMaterial struct {
	ID         string    `json:"id"`
	Encoding   string    `json:"encoding"`
	SampleRate int       `json:"sample_rate"`
	Samples    []int16   `json:"samples"`
	Phonemes   []Phoneme `json:"phonemes"`
}

func ContentHash(d Dialogue) string {
	return SHA256JSON(contentHashMaterial{
		ID: d.ID, Encoding: d.PCM.Encoding, SampleRate: d.PCM.SampleRate,
		Samples: d.PCM.Samples, Phonemes: d.Phonemes,
	})
}

type paramHashMaterial struct {
	Envelope EnvelopeConfig `json:"envelope"`
	Fit      FitParams      `json:"fit"`
}

func ParamVersion(envelope EnvelopeConfig, fit FitParams) string {
	return SHA256JSON(paramHashMaterial{Envelope: envelope, Fit: fit})
}

func NodeCacheKey(contentHash, phonemeID, paramVersion, node string, upstream ...string) string {
	return SHA256JSON(struct {
		ContentHash  string   `json:"content_hash"`
		PhonemeID    string   `json:"phoneme_id"`
		ParamVersion string   `json:"param_version"`
		Node         string   `json:"node"`
		Upstream     []string `json:"upstream"`
	}{contentHash, phonemeID, paramVersion, node, upstream})
}

func FrameDigest(frames []Frame) string {
	return SHA256JSON(frames)
}

func TruthFingerprint(t TruthDialogue) string {
	return SHA256JSON(struct {
		FrameValues []float64        `json:"frame_values"`
		Stress      []TruthStress    `json:"stress"`
		Stretched   []StretchSegment `json:"stretched"`
	}{t.FrameValues, t.Stress, t.Stretched})
}
