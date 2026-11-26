// Package generator owns the fixed-seed synthetic dialogue project and its
// truth bundle. The truth is constructed here, not read back from the engine.
package generator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dialogueaudio/codec"
	"dialogueaudio/fixed"
	"dialogueaudio/model"
)

// DefaultSeed is the repository's reproducibility contract.
const DefaultSeed int64 = 20261127

// TruthFrame is an independently constructed expected frame value.
type TruthFrame struct {
	FrameIndex     int   `json:"frame_index"`
	StartSample    int64 `json:"start_sample"`
	EndSample      int64 `json:"end_sample"`
	SampleCount    int   `json:"sample_count"`
	RawEnergy      int64 `json:"raw_energy"`
	WeightedEnergy int64 `json:"weighted_energy"`
	PeakOffset     int   `json:"peak_offset"`
	EnvelopeQ16    int64 `json:"envelope_q16"`
}

// TruthClip is the per-segment expected envelope and calibration target.
type TruthClip struct {
	ClipID                  string       `json:"clip_id"`
	ContentHash             string       `json:"content_hash"`
	Category                string       `json:"category"`
	Frames                  []TruthFrame `json:"frames"`
	SegmentEnergy           int64        `json:"segment_energy"`
	SegmentEnvelopeQ16      int64        `json:"segment_envelope_q16"`
	ExpectedAdjustmentDBQ16 int64        `json:"expected_adjustment_db_q16"`
	PeakOffset              int          `json:"peak_offset"`
}

// TruthBundle is the only accuracy oracle for generated samples.
type TruthBundle struct {
	Seed                 int64       `json:"seed"`
	SampleRate           int         `json:"sample_rate"`
	FrameLenSamples      int64       `json:"frame_len_samples"`
	HopSamples           int64       `json:"hop_samples"`
	SessionGainQ16       int64       `json:"session_gain_q16"`
	TargetLoudnessQ16    int64       `json:"target_loudness_q16"`
	MaxAdjustmentDBQ16   int64       `json:"max_adjustment_db_q16"`
	EnvelopeToleranceQ16 int64       `json:"envelope_tolerance_q16"`
	Clips                []TruthClip `json:"clips"`
}

// FileRecord is a manifest entry with content digest.
type FileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// Manifest makes the generated bundle self-describing and tamper-obvious.
type Manifest struct {
	Format          string                      `json:"format"`
	Seed            int64                       `json:"seed"`
	ProjectID       string                      `json:"project_id"`
	ClipCount       int                         `json:"clip_count"`
	FrameCount      int                         `json:"frame_count"`
	TruthFile       string                      `json:"truth_file"`
	Files           []FileRecord                `json:"files"`
	DowngradeLosses map[string]codec.LossReport `json:"downgrade_losses,omitempty"`
}

// SampleSet contains both in-memory and serialized sample artifacts.
type SampleSet struct {
	Seed     int64
	Project  model.Project
	Truth    TruthBundle
	Manifest Manifest
	Files    map[string][]byte
}

// Build constructs the complete deterministic sample set without touching the
// filesystem.
func Build(seed int64) (*SampleSet, error) {
	if seed < 0 {
		return nil, fmt.Errorf("seed must be non-negative")
	}
	const (
		sampleRate  = 48000
		frameLen    = int64(480)
		hop         = int64(480)
		clipSamples = int64(5760)
	)
	project := model.Project{
		ProjectID:          fmt.Sprintf("voice-project-%d", seed),
		SourceVersion:      3,
		SampleRate:         sampleRate,
		FrameLenSamples:    frameLen,
		HopSamples:         hop,
		SessionGainQ16:     72090, // ~1.1 linear
		MaxAdjustmentDBQ16: 6 * fixed.Q16,
		CalibrationEnabled: true,
	}
	type clipSpec struct {
		id        string
		label     string
		category  string
		weightQ16 int64
		gainQ16   int64
		groupPath []string
		mark      string
	}
	type sceneSpec struct {
		id    string
		name  string
		clips []clipSpec
	}
	sceneSpecs := []sceneSpec{
		{id: "s01", name: "Arrival", clips: []clipSpec{
			{id: "c01", label: "dock_greeting", category: "dialogue", weightQ16: 65536, gainQ16: 65536, groupPath: []string{"VO"}, mark: "review"},
			{id: "c02", label: "combat_call", category: "shout", weightQ16: 72090, gainQ16: 62259, groupPath: []string{"VO", "Combat"}, mark: ""},
		}},
		{id: "s02", name: "Corridor", clips: []clipSpec{
			{id: "c03", label: "radio_hint", category: "dialogue", weightQ16: 65536, gainQ16: 68813, groupPath: []string{"VO", "Radio"}, mark: ""},
			{id: "c04", label: "wounded_whisper", category: "whisper", weightQ16: 58982, gainQ16: 58982, groupPath: []string{"Foley"}, mark: "locked"},
		}},
		{id: "s03", name: "Extraction", clips: []clipSpec{
			{id: "c05", label: "pilot_shout", category: "shout", weightQ16: 72090, gainQ16: 72090, groupPath: []string{"VO", "Combat"}, mark: ""},
			{id: "c06", label: "final_whisper", category: "whisper", weightQ16: 58982, gainQ16: 55706, groupPath: []string{"VO"}, mark: ""},
		}},
	}
	clipOrdinal := 0
	startCursor := int64(0)
	for _, scene := range sceneSpecs {
		project.Scenes = append(project.Scenes, model.Scene{ID: scene.id, Name: scene.name})
		for _, spec := range scene.clips {
			samples := synthPCM(seed, clipOrdinal, spec.category, int(clipSamples))
			start := startCursor
			end := start + clipSamples
			startCursor = end + 1440 // 30 ms inter-clip gap
			clip := model.Clip{
				ID: spec.id, Label: spec.label, SceneID: scene.id, SceneName: scene.name,
				GroupPath: append([]string(nil), spec.groupPath...), Category: spec.category,
				StartSample: start, EndSample: end, GainQ16: spec.gainQ16,
				LoudnessWeightQ16: spec.weightQ16, CalibrationMark: spec.mark, PCM: samples,
				ContentHash: model.ContentHashForPCM(samples, sampleRate),
			}
			project.Clips = append(project.Clips, clip)
			clipOrdinal++
		}
	}
	project.SortClips()
	truth, err := buildTruth(project, seed)
	if err != nil {
		return nil, err
	}
	loudnesses := make([]int64, 0, len(truth.Clips))
	for _, clip := range truth.Clips {
		loudnesses = append(loudnesses, clip.SegmentEnvelopeQ16)
	}
	sort.Slice(loudnesses, func(i, j int) bool { return loudnesses[i] < loudnesses[j] })
	median := loudnesses[len(loudnesses)/2]
	project.TargetLoudnessQ16 = median + fixed.Q16 // 1 dB above median
	truth.TargetLoudnessQ16 = project.TargetLoudnessQ16
	for i := range truth.Clips {
		truth.Clips[i].ExpectedAdjustmentDBQ16 = project.TargetLoudnessQ16 - truth.Clips[i].SegmentEnvelopeQ16
	}
	set := &SampleSet{
		Seed: seed, Project: project, Truth: truth,
		Files: make(map[string][]byte),
	}
	if err := set.serialize(); err != nil {
		return nil, err
	}
	return set, nil
}

func buildTruth(project model.Project, seed int64) (TruthBundle, error) {
	truth := TruthBundle{
		Seed: seed, SampleRate: project.SampleRate,
		FrameLenSamples: project.FrameLenSamples, HopSamples: project.HopSamples,
		SessionGainQ16:       project.SessionGainQ16,
		MaxAdjustmentDBQ16:   project.MaxAdjustmentDBQ16,
		EnvelopeToleranceQ16: 4,
	}
	for _, clip := range project.Clips {
		clipTruth := TruthClip{
			ClipID: clip.ID, ContentHash: clip.ContentHash, Category: clip.Category,
		}
		duration := clip.DurationSamples()
		frameIndex := 0
		energySum := int64(0)
		for start := int64(0); start < duration; start += project.HopSamples {
			end := start + project.FrameLenSamples
			if end > duration {
				end = duration
			}
			count := int(end - start)
			var sumSquares int64
			peakAbs := int64(-1)
			peakOffset := 0
			for offset, sample := range clip.PCM[start:end] {
				value := int64(sample)
				square := value * value
				sumSquares += square
				abs := value
				if abs < 0 {
					abs = -abs
				}
				if abs > peakAbs {
					peakAbs = abs
					peakOffset = offset
				}
			}
			raw := sumSquares / int64(count)
			session := mulDivTrunc(raw, project.SessionGainQ16, fixed.Q16)
			session = mulDivTrunc(session, project.SessionGainQ16, fixed.Q16)
			gained := mulDivTrunc(session, clip.GainQ16, fixed.Q16)
			gained = mulDivTrunc(gained, clip.GainQ16, fixed.Q16)
			weighted := mulDivTrunc(gained, clip.LoudnessWeightQ16, fixed.Q16)
			db, err := referenceDB(weighted)
			if err != nil {
				return truth, fmt.Errorf("truth clip %s frame %d: %w", clip.ID, frameIndex, err)
			}
			clipTruth.Frames = append(clipTruth.Frames, TruthFrame{
				FrameIndex: frameIndex, StartSample: clip.StartSample + start,
				EndSample: clip.StartSample + end, SampleCount: count,
				RawEnergy: raw, WeightedEnergy: weighted,
				PeakOffset: peakOffset, EnvelopeQ16: db,
			})
			energySum += weighted
			frameIndex++
		}
		segmentEnergy := energySum / int64(len(clipTruth.Frames))
		db, err := referenceDB(segmentEnergy)
		if err != nil {
			return truth, err
		}
		clipTruth.SegmentEnergy = segmentEnergy
		clipTruth.SegmentEnvelopeQ16 = db
		truth.Clips = append(truth.Clips, clipTruth)
	}
	return truth, nil
}

func mulDivTrunc(a, b, c int64) int64 {
	return (a * b) / c
}

func referenceDB(energy int64) (int64, error) {
	db, _, err := fixed.EnergyToDBQ16(energy, "truth.envelope")
	return db, err
}

// synthPCM is integer-only and seed-stable across Go releases.
func synthPCM(seed int64, ordinal int, category string, count int) []int16 {
	state := uint64(seed)*0x9E3779B97F4A7C15 + uint64(ordinal+1)*0xD1B54A32D192ED03
	if state == 0 {
		state = 0xA0761D6478BD642F
	}
	period := 96 + ordinal*9
	ampBase := int64(6500 + ordinal*150)
	switch category {
	case "shout":
		ampBase += 400
	case "whisper":
		ampBase -= 300
	}
	out := make([]int16, count)
	for i := 0; i < count; i++ {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		state *= 0x2545F4914F6CDD1D
		noise := int64((state>>17)%20001) - 10000
		triPhase := (i + ordinal*17) % (period * 2)
		var tri int64
		if triPhase < period {
			tri = int64(triPhase)*5000/int64(period) - 5000
		} else {
			tri = 5000 - int64(triPhase-period)*5000/int64(period)
		}
		frame := i / 480
		variation := int64((uint64(frame+1) * uint64(ordinal+3) * 2654435761) % 401)
		amp := ampBase + variation
		wave := (tri + noise) / 2
		value := wave * amp / 10000
		if value > 32767 {
			value = 32767
		}
		if value < -32768 {
			value = -32768
		}
		out[i] = int16(value)
	}
	return out
}

func (s *SampleSet) serialize() error {
	truthData, err := json.MarshalIndent(s.Truth, "", "  ")
	if err != nil {
		return err
	}
	s.Files["truth.json"] = append(truthData, '\n')
	losses := make(map[string]codec.LossReport)
	for version := 1; version <= 3; version++ {
		result, err := codec.Export(s.Project, version)
		if err != nil {
			return fmt.Errorf("export v%d: %w", version, err)
		}
		name := fmt.Sprintf("schema_v%d.json", version)
		s.Files[name] = result.Data
		if len(result.Losses.Items) > 0 {
			losses[fmt.Sprintf("v%d", version)] = result.Losses
		}
	}
	manifest := Manifest{
		Format: "dialogue-audio-sample-set", Seed: s.Seed, ProjectID: s.Project.ProjectID,
		ClipCount: len(s.Project.Clips), FrameCount: totalFrames(s.Truth),
		TruthFile: "truth.json", DowngradeLosses: losses,
	}
	names := make([]string, 0, len(s.Files))
	for name := range s.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := s.Files[name]
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, FileRecord{
			Path: name, SHA256: hex.EncodeToString(sum[:]), Bytes: len(data),
		})
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	s.Manifest = manifest
	s.Files["manifest.json"] = append(manifestData, '\n')
	return nil
}

func totalFrames(truth TruthBundle) int {
	total := 0
	for _, clip := range truth.Clips {
		total += len(clip.Frames)
	}
	return total
}

// Write materializes all generated files using temp-file + rename.
func (s *SampleSet) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(s.Files))
	for name := range s.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(dir, name)
		temp := target + ".tmp"
		if err := os.WriteFile(temp, s.Files[name], 0o644); err != nil {
			return err
		}
		if err := os.Rename(temp, target); err != nil {
			return err
		}
	}
	return nil
}

// ManifestPath is a convenience for diagnostics.
func ManifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }

// TruthFilename returns the conventional truth file name.
func TruthFilename(dir string) string { return filepath.Join(dir, "truth.json") }

// SchemaFilename returns the conventional schema file name.
func SchemaFilename(dir string, version int) string {
	return filepath.Join(dir, fmt.Sprintf("schema_v%d.json", version))
}

// ValidateManifest detects sample tampering.
func ValidateManifest(dir string) error {
	data, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	for _, record := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(dir, record.Path))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), record.SHA256) {
			return fmt.Errorf("manifest mismatch for %s", record.Path)
		}
	}
	return nil
}
