package truth

import (
	"encoding/json"
	"fmt"
	"os"

	"lipsync/domain"
)

const Schema = "lipsync-truth/v1"

type Phoneme struct {
	ID              string          `json:"id"`
	StartFrame      int64           `json:"start_frame"`
	EndFrame        int64           `json:"end_frame"`
	StartSample     int64           `json:"start_sample"`
	EndSample       int64           `json:"end_sample"`
	Category        domain.Category `json:"category"`
	WeightPermyriad int64           `json:"weight_permyriad"`
}

type Package struct {
	ID                 string   `json:"id"`
	DialogueID         string   `json:"dialogue_id"`
	ShardIndex         int      `json:"shard_index"`
	Channel            int      `json:"channel"`
	StartFrame         int64    `json:"start_frame"`
	EndFrame           int64    `json:"end_frame"`
	PhonemeIDs         []string `json:"phoneme_ids"`
	Status             string   `json:"status"`
	DispatchStartFrame int64    `json:"dispatch_start_frame"`
	DispatchEndFrame   int64    `json:"dispatch_end_frame"`
}

type Dialogue struct {
	ID          string    `json:"dialogue_id"`
	ShardIndex  int       `json:"shard_index"`
	ContentHash string    `json:"content_hash"`
	Phonemes    []Phoneme `json:"phonemes"`
	Packages    []Package `json:"packages"`
}

type Frame struct {
	Frame      int64    `json:"frame"`
	PackageIDs []string `json:"package_ids"`
}

type Event struct {
	Frame      int64  `json:"frame"`
	PackageID  string `json:"package_id"`
	DialogueID string `json:"dialogue_id"`
	ShardIndex int    `json:"shard_index"`
	Channel    int    `json:"channel"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	RetryFrame int64  `json:"retry_frame,omitempty"`
}

type Manifest struct {
	Schema           string     `json:"schema"`
	ProjectID        string     `json:"project_id"`
	Seed             int64      `json:"seed"`
	Fingerprint      string     `json:"fingerprint"`
	AlgorithmVersion string     `json:"algorithm_version"`
	Dialogues        []Dialogue `json:"dialogues"`
	Packages         []Package  `json:"packages"`
	Frames           []Frame    `json:"frames"`
	Events           []Event    `json:"events"`
}

func (m *Manifest) ComputeFingerprint() (string, error) {
	cp := *m
	cp.Fingerprint = ""
	return domain.Hash(cp)
}

func (m *Manifest) Seal() error {
	fp, err := m.ComputeFingerprint()
	if err != nil {
		return err
	}
	m.Fingerprint = fp
	return nil
}

func (m *Manifest) VerifyFingerprint() error {
	want, err := m.ComputeFingerprint()
	if err != nil {
		return err
	}
	if m.Fingerprint == "" || want != m.Fingerprint {
		return &domain.Error{
			Code: domain.CodeDiff, Field: "truth.fingerprint",
			Expected: want, Actual: m.Fingerprint,
			Message: "truth manifest fingerprint mismatch",
		}
	}
	return nil
}

func LoadFile(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read truth manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode truth manifest: %w", err)
	}
	if m.Schema != Schema {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: "$.schema", Field: "schema",
			Version: m.Schema, Value: m.Schema,
			Message: "unsupported truth schema",
		}
	}
	if err := m.VerifyFingerprint(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) Dialogue(id string) *Dialogue {
	for i := range m.Dialogues {
		if m.Dialogues[i].ID == id {
			return &m.Dialogues[i]
		}
	}
	return nil
}
