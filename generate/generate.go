package generate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"lipsync/domain"
	"lipsync/truth"
)

const ProjectID = "proj-lipsync-deterministic"

type Config struct {
	OutDir string
	Seed   int64
}

type Report struct {
	OutDir      string            `json:"out_dir"`
	Seed        int64             `json:"seed"`
	ProjectID   string            `json:"project_id"`
	Truth       string            `json:"truth"`
	Fingerprint string            `json:"fingerprint"`
	Projects    map[string]string `json:"projects"`
	PCMS        map[string]string `json:"pcm"`
}

type patternPart struct {
	Category domain.Category
	Frames   int64
}

type canonicalDialogue struct {
	ID         string
	Shard      int
	Speaker    string
	Channel    int
	StartFrame int64
	Pattern    []patternPart
	Phonemes   []domain.Phoneme
	PCM        domain.PCMInfo
}

// ResetOutput removes and recreates the generated artifact directory. It
// refuses obviously unsafe paths.
func ResetOutput(dir string) error {
	clean := filepath.Clean(dir)
	if clean == "." || clean == "/" || clean == "" {
		return fmt.Errorf("refusing to clear unsafe output directory %q", dir)
	}
	if err := os.RemoveAll(clean); err != nil {
		return err
	}
	return os.MkdirAll(clean, 0o755)
}

// Generate writes deterministic sample artifacts and returns the paths needed
// by the loader and verifier.
func Generate(cfg Config) (*Report, error) {
	if cfg.OutDir == "" {
		return nil, fmt.Errorf("output directory is required")
	}
	if err := ResetOutput(cfg.OutDir); err != nil {
		return nil, err
	}
	seed := cfg.Seed
	dialogues := canonicalDialogues()
	report := &Report{
		OutDir: cfg.OutDir, Seed: seed, ProjectID: ProjectID,
		Projects: make(map[string]string), PCMS: make(map[string]string),
	}

	pcmDir := filepath.Join(cfg.OutDir, "pcm")
	if err := os.MkdirAll(pcmDir, 0o755); err != nil {
		return nil, err
	}
	for i := range dialogues {
		data, err := renderPCM(dialogues[i], seed)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		dialogues[i].PCM = domain.PCMInfo{
			File:       fmt.Sprintf("../pcm/%s.pcm", dialogues[i].ID),
			Encoding:   "s16le",
			SampleRate: 48000,
			Channels:   1,
			Samples:    int64(len(data) / 2),
			SHA256:     hex.EncodeToString(sum[:]),
		}
		p := filepath.Join(pcmDir, dialogues[i].ID+".pcm")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, err
		}
		report.PCMS[dialogues[i].ID] = p
	}

	truthManifest, err := buildTruth(dialogues, seed)
	if err != nil {
		return nil, err
	}
	if err := truthManifest.Seal(); err != nil {
		return nil, err
	}
	truthPath := filepath.Join(cfg.OutDir, "truth.json")
	if err := writeJSON(truthPath, truthManifest); err != nil {
		return nil, err
	}
	report.Truth = truthPath
	report.Fingerprint = truthManifest.Fingerprint

	for _, ver := range []string{"v1", "v2", "v3"} {
		dir := filepath.Join(cfg.OutDir, ver)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		doc := projectDocument(ver, dialogues)
		path := filepath.Join(dir, "project.json")
		if err := writeJSON(path, doc); err != nil {
			return nil, err
		}
		report.Projects[ver] = path
	}
	manifestPath := filepath.Join(cfg.OutDir, "manifest.json")
	if err := writeJSON(manifestPath, report); err != nil {
		return nil, err
	}
	return report, nil
}

func canonicalDialogues() []canonicalDialogue {
	specs := []struct {
		id      string
		shard   int
		speaker string
		channel int
		start   int64
		pattern []patternPart
	}{
		{
			id: "d01", shard: 0, speaker: "A", channel: 0, start: 0,
			pattern: []patternPart{
				{domain.CategoryVowel, 8}, {domain.CategoryConsonant, 5}, {domain.CategoryVowel, 7},
				{domain.CategorySilence, 4}, {domain.CategoryConsonant, 6}, {domain.CategoryVowel, 8},
				{domain.CategoryConsonant, 5}, {domain.CategoryVowel, 7}, {domain.CategorySilence, 3},
			},
		},
		{
			id: "d02", shard: 1, speaker: "A", channel: 0, start: 1,
			pattern: []patternPart{
				{domain.CategoryVowel, 7}, {domain.CategoryConsonant, 5}, {domain.CategorySilence, 3},
				{domain.CategoryVowel, 8}, {domain.CategoryConsonant, 6}, {domain.CategoryVowel, 7},
				{domain.CategoryConsonant, 5}, {domain.CategoryVowel, 6},
			},
		},
		{
			id: "d03", shard: 2, speaker: "B", channel: 1, start: 2,
			pattern: []patternPart{
				{domain.CategoryConsonant, 5}, {domain.CategoryVowel, 8}, {domain.CategoryConsonant, 6},
				{domain.CategorySilence, 3}, {domain.CategoryVowel, 7}, {domain.CategoryConsonant, 5},
				{domain.CategoryVowel, 8}, {domain.CategoryConsonant, 5}, {domain.CategoryVowel, 6},
			},
		},
		{
			id: "d04", shard: 3, speaker: "B", channel: 1, start: 4,
			pattern: []patternPart{
				{domain.CategoryVowel, 6}, {domain.CategoryConsonant, 6}, {domain.CategoryVowel, 7},
				{domain.CategorySilence, 3}, {domain.CategoryConsonant, 5}, {domain.CategoryVowel, 8},
				{domain.CategoryConsonant, 5}, {domain.CategoryVowel, 7},
			},
		},
	}

	out := make([]canonicalDialogue, 0, len(specs))
	for _, s := range specs {
		var frames int64
		for _, p := range s.pattern {
			frames += p.Frames
		}
		total := s.start + frames
		d := canonicalDialogue{
			ID: s.id, Shard: s.shard, Speaker: s.speaker, Channel: s.channel,
			StartFrame: s.start, Pattern: s.pattern,
		}
		cursor := s.start
		for pi, part := range s.pattern {
			weight := (part.Frames * 10000 / total) / 10 * 10
			if weight < 10 {
				weight = 10
			}
			next := domain.CategorySilence
			if pi+1 < len(s.pattern) {
				next = s.pattern[pi+1].Category
			}
			affinity := transitionAffinity(part.Category, next)
			d.Phonemes = append(d.Phonemes, domain.Phoneme{
				ID:         fmt.Sprintf("%s-p%02d", s.id, pi+1),
				StartFrame: cursor, EndFrame: cursor + part.Frames,
				SampleStart: cursor * 960, SampleEnd: (cursor + part.Frames) * 960,
				Category: part.Category, CategoryCode: categoryCode(part.Category),
				WeightPermyriad: weight, Affinity: affinity, AffinityPresent: true,
				SourceBasis: "frame", SourceStart: cursor, SourceEnd: cursor + part.Frames,
				SourceWeight: weight / 10, SourceWeightScale: 1000,
			})
			cursor += part.Frames
		}
		out = append(out, d)
	}
	return out
}

func categoryCode(c domain.Category) string {
	switch c {
	case domain.CategoryVowel:
		return "V"
	case domain.CategoryConsonant:
		return "C"
	default:
		return "S"
	}
}

func transitionAffinity(a, b domain.Category) float64 {
	if a == domain.CategorySilence || b == domain.CategorySilence {
		return 0.1
	}
	switch {
	case a == domain.CategoryVowel && b == domain.CategoryVowel:
		return 0.9
	case a == domain.CategoryVowel:
		return 0.8
	case b == domain.CategoryVowel:
		return 0.85
	default:
		return 0.7
	}
}

func renderPCM(d canonicalDialogue, seed int64) ([]byte, error) {
	end := int64(0)
	frameCategory := make(map[int64]domain.Category)
	for _, ph := range d.Phonemes {
		if ph.EndFrame > end {
			end = ph.EndFrame
		}
		for f := ph.StartFrame; f < ph.EndFrame; f++ {
			frameCategory[f] = ph.Category
		}
	}
	const samplesPerFrame = 960
	data := make([]byte, end*samplesPerFrame*2)
	state := uint64(seed) + uint64(d.Shard+1)*0x9e3779b97f4a7c15
	for i := int64(0); i < end*samplesPerFrame; i++ {
		cat := frameCategory[i/samplesPerFrame]
		var sample int16
		if cat != domain.CategorySilence && cat != "" {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			sample = int16(state%1601) - 800
			if cat == domain.CategoryConsonant {
				sample = sample / 2
			}
		}
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}
	return data, nil
}

func buildTruth(dialogues []canonicalDialogue, seed int64) (*truth.Manifest, error) {
	m := &truth.Manifest{
		Schema: truth.Schema, ProjectID: ProjectID, Seed: seed,
		AlgorithmVersion: domain.AlgorithmVersion,
	}
	var allPackages []truth.Package
	for _, d := range dialogues {
		td := truth.Dialogue{ID: d.ID, ShardIndex: d.Shard}
		for _, ph := range d.Phonemes {
			td.Phonemes = append(td.Phonemes, truth.Phoneme{
				ID: ph.ID, StartFrame: ph.StartFrame, EndFrame: ph.EndFrame,
				StartSample: ph.SampleStart, EndSample: ph.SampleEnd,
				Category: ph.Category, WeightPermyriad: ph.WeightPermyriad,
			})
		}
		pkgs := referencePartition(d)
		for _, p := range pkgs {
			td.Packages = append(td.Packages, truth.Package{
				ID: p.ID, DialogueID: d.ID, ShardIndex: d.Shard, Channel: d.Channel,
				StartFrame: p.StartFrame, EndFrame: p.EndFrame, PhonemeIDs: p.PhonemeIDs,
			})
		}
		hash, err := domain.DialogueContentHash(ProjectID, domain.Dialogue{
			ID: d.ID, ShardIndex: d.Shard, Speaker: d.Speaker, Channel: d.Channel,
			PCM: d.PCM, Phonemes: d.Phonemes,
		})
		if err != nil {
			return nil, err
		}
		td.ContentHash = hash
		m.Dialogues = append(m.Dialogues, td)
		allPackages = append(allPackages, td.Packages...)
	}
	packages, frames, events := referenceSchedule(allPackages)
	m.Packages = packages
	m.Frames = frames
	m.Events = events
	return m, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func projectDocument(ver string, dialogues []canonicalDialogue) map[string]any {
	render := map[string]any{
		"target_fps":               int64(50),
		"frame_budget":             3,
		"channel_slots":            1,
		"min_visible_frames":       2,
		"max_phonemes_per_package": 3,
		"max_delay_frames":         8,
		"samples_per_frame":        960,
	}
	var ds []any
	switch ver {
	case "v1":
		for _, d := range dialogues {
			var phones []any
			for _, ph := range d.Phonemes {
				phones = append(phones, map[string]any{
					"id": ph.ID, "start_frame": ph.StartFrame, "end_frame": ph.EndFrame,
					"start_sample": ph.SampleStart, "end_sample": ph.SampleEnd,
					"category": ph.CategoryCode, "weight_milli": ph.WeightPermyriad / 10,
				})
			}
			ds = append(ds, map[string]any{
				"id": d.ID, "shard": d.Shard, "speaker": d.Speaker, "channel": d.Channel,
				"pcm": pcmV1(d.PCM), "phonemes": phones,
			})
		}
		return map[string]any{
			"schema": "dialogue/v1", "project_id": ProjectID, "time_basis": "frame",
			"weight_scale": 1000, "truth_file": "../truth.json",
			"render": mergeMaps(render, map[string]any{
				"fps": 50, "rounding": "floor", "affinity_threshold_permille": 500,
			}),
			"categories": map[string]any{"V": "vowel", "C": "consonant", "S": "silence"},
			"dialogues":  ds,
		}
	case "v2":
		for _, d := range dialogues {
			var phones []any
			for i, ph := range d.Phonemes {
				pm := map[string]any{
					"phoneme_id":    ph.ID,
					"start_ms":      ph.StartFrame * 1000 / 50,
					"end_ms":        ph.EndFrame * 1000 / 50,
					"start_sample":  ph.SampleStart,
					"end_sample":    ph.SampleEnd,
					"category_code": ph.CategoryCode,
					"weight_bp":     ph.WeightPermyriad,
				}
				if i+1 < len(d.Phonemes) {
					pm["transition_affinity"] = ph.Affinity
				}
				phones = append(phones, pm)
			}
			ds = append(ds, map[string]any{
				"dialogue_id": d.ID, "shard": d.Shard, "speaker": d.Speaker, "channel": d.Channel,
				"audio": pcmV2(d.PCM), "phonemes": phones,
			})
		}
		policy := mergeMaps(render, map[string]any{
			"sample_rounding": "floor", "affinity_threshold": 0.5,
		})
		delete(policy, "frame_budget")
		delete(policy, "max_phonemes_per_package")
		delete(policy, "min_visible_frames")
		policy["frame_budget"] = 3
		policy["max_phonemes_per_package"] = 3
		policy["min_visible_frames"] = 2
		return map[string]any{
			"schema": "dialogue/v2", "project_id": ProjectID, "time_basis": "millisecond",
			"duration_weight_scale": 10000, "truth_file": "../truth.json",
			"frame_policy": policy,
			"categories": map[string]any{
				"V": map[string]any{"kind": "vowel"},
				"C": map[string]any{"kind": "consonant"},
				"S": map[string]any{"kind": "silence"},
			},
			"dialogues": ds,
		}
	default:
		for _, d := range dialogues {
			var phones []any
			for _, ph := range d.Phonemes {
				phones = append(phones, map[string]any{
					"id": ph.ID, "from_frame": ph.StartFrame, "to_frame": ph.EndFrame,
					"sample_start": ph.SampleStart, "sample_end": ph.SampleEnd,
					"category": ph.CategoryCode, "duration_weight": ph.WeightPermyriad,
					"affinity": ph.Affinity,
				})
			}
			ds = append(ds, map[string]any{
				"id": d.ID, "shard_index": d.Shard, "speaker": d.Speaker, "channel": d.Channel,
				"pcm_meta": pcmV3(d.PCM), "phonemes": phones,
			})
		}
		v3render := map[string]any{
			"target_fps": 50, "quantize": "floor", "budget_per_frame": 3,
			"channel_slots": 1, "min_visible": 2, "max_phones_per_pack": 3,
			"affinity_floor_permille": 500, "max_delay_frames": 8,
			"samples_per_frame": 960,
		}
		return map[string]any{
			"schema": "dialogue/v3", "project_id": ProjectID, "time_basis": "frame",
			"duration_weight_scale": 10000, "truth_file": "../truth.json",
			"render": v3render,
			"category_groups": map[string]any{
				"vowels":     []any{map[string]any{"code": "V", "kind": "vowel"}},
				"consonants": []any{map[string]any{"code": "C", "kind": "consonant"}},
				"silences":   []any{map[string]any{"code": "S", "kind": "silence"}},
			},
			"dialogues": ds,
		}
	}
}

func mergeMaps(base, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func pcmV1(p domain.PCMInfo) map[string]any {
	return map[string]any{
		"file": p.File, "encoding": p.Encoding, "sample_rate": p.SampleRate,
		"channels": p.Channels, "samples": p.Samples, "sha256": p.SHA256,
	}
}

func pcmV2(p domain.PCMInfo) map[string]any {
	return map[string]any{
		"file": p.File, "codec": p.Encoding, "sample_rate": p.SampleRate,
		"channels": p.Channels, "sample_count": p.Samples, "content_sha256": p.SHA256,
	}
}

func pcmV3(p domain.PCMInfo) map[string]any {
	return map[string]any{
		"path": p.File, "encoding": p.Encoding, "sample_rate": p.SampleRate,
		"channels": p.Channels, "sample_count": p.Samples, "sha": p.SHA256,
	}
}
