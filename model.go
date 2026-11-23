package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SegmentRef references a voice segment by absolute sample points.
type SegmentRef struct {
	ID          string `json:"id"`
	StartSample int64  `json:"start_sample"`
	EndSample   int64  `json:"end_sample"`
}

// SampleRange is a half-open [Start, End) sample range.
type SampleRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Dialogue is one character line with rhythm weights per character.
type Dialogue struct {
	ID          string       `json:"id"`
	Speaker     string       `json:"speaker"`
	Text        string       `json:"text"`
	Segment     SegmentRef   `json:"segment"`
	Rhythm      []float64    `json:"rhythm"`
	Manual      *SampleRange `json:"manual,omitempty"`
	InterruptAt *int64       `json:"interrupt_at,omitempty"`
}

// Chars returns the dialogue text as runes (one rhythm weight per rune).
func (d *Dialogue) Chars() []rune { return []rune(d.Text) }

// Policy declares playback and interruption strategy.
type Policy struct {
	Interruptible      bool     `json:"interruptible"`
	CutoffLimitSamples int64    `json:"cutoff_limit_samples"`
	QueueOrder         []string `json:"queue_order"`
}

// TruthEntry is the ground-truth expectation for one dialogue.
type TruthEntry struct {
	DialogueID   string `json:"dialogue_id"`
	PresentStart int64  `json:"present_start"`
	PresentEnd   int64  `json:"present_end"`
	Interrupted  bool   `json:"interrupted"`
	CutoffEnd    int64  `json:"cutoff_end,omitempty"`
	ResumeIndex  int    `json:"resume_index,omitempty"`
	ResumeStart  int64  `json:"resume_start,omitempty"`
}

// Project is one dialogue engineering project.
type Project struct {
	ID               string       `json:"id"`
	SampleRate       int          `json:"sample_rate"`
	ToleranceSamples int64        `json:"tolerance_samples"`
	Dialogues        []Dialogue   `json:"dialogues"`
	Policy           Policy       `json:"policy"`
	Truth            []TruthEntry `json:"truth"`
}

// DialogueByID returns the dialogue pointer or nil.
func (p *Project) DialogueByID(id string) *Dialogue {
	for i := range p.Dialogues {
		if p.Dialogues[i].ID == id {
			return &p.Dialogues[i]
		}
	}
	return nil
}

// TruthByID returns the truth entry or nil.
func (p *Project) TruthByID(id string) *TruthEntry {
	for i := range p.Truth {
		if p.Truth[i].DialogueID == id {
			return &p.Truth[i]
		}
	}
	return nil
}

// ContentHash is a stable hash over dialogues and policy, used to
// detect stale checkpoints on restart.
func (p *Project) ContentHash() string {
	payload := struct {
		Dialogues []Dialogue `json:"dialogues"`
		Policy    Policy     `json:"policy"`
	}{p.Dialogues, p.Policy}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

const projectFile = "project.json"

// SaveProject writes the project manifest (including truth) to dir.
func SaveProject(dir string, p *Project) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, projectFile), raw, 0o644)
}

// LoadProject reads the project manifest from dir.
func LoadProject(dir string) (*Project, error) {
	raw, err := os.ReadFile(filepath.Join(dir, projectFile))
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	return &p, nil
}
