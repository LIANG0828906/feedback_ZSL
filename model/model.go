// Package model defines the single internal dialogue project representation.
package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"dialogueaudio/fixed"
)

// DefaultTrace records an upgrade-time field filling.
type DefaultTrace struct {
	Path         string `json:"path"`
	OldValue     any    `json:"old_value"`
	DefaultValue any    `json:"default_value"`
	Source       string `json:"source"`
}

// Scene is an organizational grouping. GroupPath is intentionally richer for
// v3 and is reported as lossy when exported to lower schemas.
type Scene struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	GroupPath []string `json:"group_path,omitempty"`
}

// Clip is a mono PCM dialogue segment.
type Clip struct {
	ID                string   `json:"id"`
	Label             string   `json:"label"`
	SceneID           string   `json:"scene_id"`
	SceneName         string   `json:"scene_name"`
	GroupPath         []string `json:"group_path,omitempty"`
	Category          string   `json:"category"`
	StartSample       int64    `json:"start_sample"`
	EndSample         int64    `json:"end_sample"`
	GainQ16           int64    `json:"gain_q16"`
	LoudnessWeightQ16 int64    `json:"loudness_weight_q16"`
	CalibrationMark   string   `json:"calibration_mark,omitempty"`
	PCM               []int16  `json:"-"`
	ContentHash       string   `json:"content_hash"`
}

// DurationSamples returns the clip duration.
func (c Clip) DurationSamples() int64 { return c.EndSample - c.StartSample }

// FrameCount returns the number of logical frames for frameLen/hop over this
// clip. A final partial frame is included.
func (c Clip) FrameCount(frameLen, hop int64) int {
	if frameLen <= 0 || hop <= 0 {
		return 0
	}
	duration := c.DurationSamples()
	if duration <= 0 {
		return 0
	}
	count := 0
	for start := int64(0); start < duration; start += hop {
		count++
	}
	return count
}

// Project is the unified internal model. All time is in samples; gains,
// weights and loudness values use Q16 fixed point.
type Project struct {
	ProjectID          string         `json:"project_id"`
	SourceVersion      int            `json:"source_version"`
	SampleRate         int            `json:"sample_rate"`
	FrameLenSamples    int64          `json:"frame_len_samples"`
	HopSamples         int64          `json:"hop_samples"`
	TargetLoudnessQ16  int64          `json:"target_loudness_q16"`
	SessionGainQ16     int64          `json:"session_gain_q16"`
	MaxAdjustmentDBQ16 int64          `json:"max_adjustment_db_q16"`
	CalibrationEnabled bool           `json:"calibration_enabled"`
	Scenes             []Scene        `json:"scenes"`
	Clips              []Clip         `json:"clips"`
	ConversionTraces   []fixed.Trace  `json:"conversion_traces,omitempty"`
	Defaults           []DefaultTrace `json:"defaults,omitempty"`
}

// Clone returns a deep-enough copy for editing without mutating the source.
func (p Project) Clone() Project {
	out := p
	out.Scenes = append([]Scene(nil), p.Scenes...)
	for i := range out.Scenes {
		out.Scenes[i].GroupPath = append([]string(nil), out.Scenes[i].GroupPath...)
	}
	out.Clips = append([]Clip(nil), p.Clips...)
	for i := range out.Clips {
		out.Clips[i].GroupPath = append([]string(nil), p.Clips[i].GroupPath...)
		out.Clips[i].PCM = append([]int16(nil), p.Clips[i].PCM...)
	}
	out.ConversionTraces = append([]fixed.Trace(nil), p.ConversionTraces...)
	out.Defaults = append([]DefaultTrace(nil), p.Defaults...)
	return out
}

// SortClips sorts clips by scene and start position to provide stable output.
func (p *Project) SortClips() {
	sort.SliceStable(p.Clips, func(i, j int) bool {
		if p.Clips[i].SceneID != p.Clips[j].SceneID {
			return p.Clips[i].SceneID < p.Clips[j].SceneID
		}
		if p.Clips[i].StartSample != p.Clips[j].StartSample {
			return p.Clips[i].StartSample < p.Clips[j].StartSample
		}
		return p.Clips[i].ID < p.Clips[j].ID
	})
}

// SceneByID returns a scene or false.
func (p Project) SceneByID(id string) (Scene, bool) {
	for _, scene := range p.Scenes {
		if scene.ID == id {
			return scene, true
		}
	}
	return Scene{}, false
}

// ClipByID returns a clip or false.
func (p Project) ClipByID(id string) (Clip, bool) {
	for _, clip := range p.Clips {
		if clip.ID == id {
			return clip, true
		}
	}
	return Clip{}, false
}

// ClipIDs returns canonical clip order.
func (p Project) ClipIDs() []string {
	ids := make([]string, len(p.Clips))
	for i, clip := range p.Clips {
		ids[i] = clip.ID
	}
	return ids
}

// ContentHashForPCM hashes the mono PCM payload only. Gain/target edits do not
// masquerade as content edits.
func ContentHashForPCM(samples []int16, sampleRate int) string {
	h := sha256.New()
	var buf [2]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint16(buf[:], uint16(sample))
		_, _ = h.Write(buf[:])
	}
	var rate [8]byte
	binary.LittleEndian.PutUint64(rate[:], uint64(sampleRate))
	_, _ = h.Write(rate[:])
	return hex.EncodeToString(h.Sum(nil))
}

// PopulateContentHashes recomputes all content hashes.
func (p *Project) PopulateContentHashes() {
	for i := range p.Clips {
		p.Clips[i].ContentHash = ContentHashForPCM(p.Clips[i].PCM, p.SampleRate)
	}
}

// Validate performs model-level invariant checks used after schema mapping.
func (p Project) Validate() error {
	if p.SampleRate <= 0 {
		return fmt.Errorf("sample_rate must be positive")
	}
	if p.FrameLenSamples <= 0 || p.HopSamples <= 0 {
		return fmt.Errorf("frame_len and hop must be positive")
	}
	if len(p.Clips) == 0 {
		return fmt.Errorf("project has no clips")
	}
	seen := make(map[string]struct{}, len(p.Clips))
	for _, clip := range p.Clips {
		if clip.ID == "" {
			return fmt.Errorf("clip id is required")
		}
		if _, ok := seen[clip.ID]; ok {
			return fmt.Errorf("duplicate clip id %s", clip.ID)
		}
		seen[clip.ID] = struct{}{}
		if clip.EndSample <= clip.StartSample {
			return fmt.Errorf("clip %s has invalid boundary", clip.ID)
		}
		if clip.GainQ16 <= 0 || clip.LoudnessWeightQ16 <= 0 {
			return fmt.Errorf("clip %s has non-positive gain or weight", clip.ID)
		}
		if clip.ContentHash == "" {
			return fmt.Errorf("clip %s has empty content hash", clip.ID)
		}
	}
	return nil
}
