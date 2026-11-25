package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"lipsync/domain"
	"lipsync/quant"
)

type LossItem struct {
	Code          domain.Code `json:"code"`
	Path          string      `json:"path"`
	SourceVersion string      `json:"source_version"`
	TargetVersion string      `json:"target_version"`
	Action        string      `json:"action"`
	OldValue      string      `json:"old_value,omitempty"`
	NewValue      string      `json:"new_value,omitempty"`
	Reason        string      `json:"reason"`
}

type LossReport struct {
	Items []LossItem `json:"items"`
}

func (r *LossReport) Add(item LossItem) {
	item.Code = domain.CodeLossy
	r.Items = append(r.Items, item)
}

func (r *LossReport) HasLosses() bool { return r != nil && len(r.Items) > 0 }

func (r *LossReport) AsError() error {
	if !r.HasLosses() {
		return nil
	}
	return &domain.Error{
		Code: domain.CodeLossy, Field: "loss_report",
		Value:   strconv.Itoa(len(r.Items)),
		Message: fmt.Sprintf("downgrade produced %d lossy field(s)", len(r.Items)),
	}
}

// Marshal exports the internal model into the requested schema. The loss
// report is returned even when all fields are representable.
func Marshal(p *domain.Project, target Version) ([]byte, *LossReport, error) {
	if p == nil {
		return nil, nil, &domain.Error{Code: domain.CodeSchema, Message: "nil project"}
	}
	if !target.Valid() {
		return nil, nil, &domain.Error{
			Code: domain.CodeSchema, Field: "target_version", Value: string(target),
			Message: "unknown target schema version",
		}
	}
	report := &LossReport{}
	codes, grouped := categoryRepresentation(p)
	if len(grouped) > 0 && target != V3 {
		report.Add(LossItem{
			Path: "$.category_groups", SourceVersion: p.SourceVersion, TargetVersion: string(target),
			Action:   "flatten category groups into a code table",
			OldValue: fmt.Sprintf("%d group(s)", len(grouped)), NewValue: "flat code map",
			Reason: "target schema has no grouped category representation",
		})
	}
	if target == V1 {
		for di, d := range sortedDialogues(p) {
			for pi, ph := range sortedPhonemes(d.Phonemes) {
				if ph.AffinityPresent || ph.Affinity != 1 {
					report.Add(LossItem{
						Path:          fmt.Sprintf("$.dialogues[%d].phonemes[%d].affinity", di, pi),
						SourceVersion: p.SourceVersion, TargetVersion: string(target),
						Action: "omit transition affinity", OldValue: strconv.FormatFloat(ph.Affinity, 'g', -1, 64),
						NewValue: "schema default 1",
						Reason:   "v1 has no transition affinity field",
					})
				}
			}
		}
	}

	var out map[string]any
	var err error
	switch target {
	case V1:
		out, err = exportV1(p, codes, report)
	case V2:
		out, err = exportV2(p, codes, report)
	case V3:
		out, err = exportV3(p, codes, grouped, report)
	}
	if err != nil {
		return nil, report, err
	}
	out["schema"] = string(target)
	out["project_id"] = p.ID
	out["truth_file"] = p.TruthFile
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	b = append(b, '\n')
	return b, report, nil
}

// Convert is the explicit version conversion API. It marshals to the target
// schema and immediately loads it back, guaranteeing that the target loader
// accepts the produced artifact.
func Convert(p *domain.Project, target Version) (*domain.Project, *LossReport, error) {
	b, report, err := Marshal(p, target)
	if err != nil {
		return nil, nil, err
	}
	out, err := Load(b)
	if err != nil {
		return nil, nil, err
	}
	return out, report, nil
}

func sortedDialogues(p *domain.Project) []domain.Dialogue {
	ds := append([]domain.Dialogue(nil), p.Dialogues...)
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].ShardIndex != ds[j].ShardIndex {
			return ds[i].ShardIndex < ds[j].ShardIndex
		}
		return ds[i].ID < ds[j].ID
	})
	return ds
}

func sortedPhonemes(ps []domain.Phoneme) []domain.Phoneme {
	cp := append([]domain.Phoneme(nil), ps...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].StartFrame != cp[j].StartFrame {
			return cp[i].StartFrame < cp[j].StartFrame
		}
		return cp[i].ID < cp[j].ID
	})
	return cp
}

func categoryCode(ph domain.Phoneme) string {
	if ph.CategoryCode != "" {
		return ph.CategoryCode
	}
	switch ph.Category {
	case domain.CategoryVowel:
		return "V"
	case domain.CategoryConsonant:
		return "C"
	default:
		return "S"
	}
}

func categoryRepresentation(p *domain.Project) (map[string]domain.Category, map[string][]string) {
	codes := make(map[string]domain.Category)
	used := make(map[string]struct{})
	for _, d := range p.Dialogues {
		for _, ph := range d.Phonemes {
			code := categoryCode(ph)
			codes[code] = ph.Category
			used[code] = struct{}{}
		}
	}
	grouped := make(map[string][]string)
	if p.CategoryEncoding == "grouped" {
		for _, g := range p.CategoryGroups {
			for _, code := range g.Codes {
				if _, ok := used[code]; ok {
					grouped[g.Name] = append(grouped[g.Name], code)
					delete(used, code)
				}
			}
		}
	}
	for _, ph := range sortedPhonemes(allPhonemes(p)) {
		code := categoryCode(ph)
		if _, ok := used[code]; !ok {
			continue
		}
		name := groupName(ph.Category)
		grouped[name] = append(grouped[name], code)
		delete(used, code)
	}
	return codes, grouped
}

func allPhonemes(p *domain.Project) []domain.Phoneme {
	var out []domain.Phoneme
	for _, d := range p.Dialogues {
		out = append(out, d.Phonemes...)
	}
	return out
}

func groupName(c domain.Category) string {
	switch c {
	case domain.CategoryVowel:
		return "vowels"
	case domain.CategoryConsonant:
		return "consonants"
	default:
		return "silences"
	}
}

func exportRenderV1(r domain.RenderConfig) map[string]any {
	return map[string]any{
		"fps":                         r.TargetFPS,
		"rounding":                    string(r.Rounding),
		"frame_budget":                r.FrameBudget,
		"channel_slots":               r.ChannelSlots,
		"min_visible_frames":          r.MinVisibleFrames,
		"max_phonemes_per_package":    r.MaxPhonemesPerPackage,
		"affinity_threshold_permille": int64(math.Round(r.AffinityThreshold * 1000)),
		"max_delay_frames":            r.MaxDelayFrames,
		"samples_per_frame":           r.SamplesPerFrame,
	}
}

func exportRenderV2(r domain.RenderConfig) map[string]any {
	return map[string]any{
		"target_fps":               r.TargetFPS,
		"sample_rounding":          string(r.Rounding),
		"frame_budget":             r.FrameBudget,
		"channel_slots":            r.ChannelSlots,
		"min_visible_frames":       r.MinVisibleFrames,
		"max_phonemes_per_package": r.MaxPhonemesPerPackage,
		"affinity_threshold":       r.AffinityThreshold,
		"max_delay_frames":         r.MaxDelayFrames,
		"samples_per_frame":        r.SamplesPerFrame,
	}
}

func exportRenderV3(r domain.RenderConfig) map[string]any {
	return map[string]any{
		"target_fps":              r.TargetFPS,
		"quantize":                string(r.Rounding),
		"budget_per_frame":        r.FrameBudget,
		"channel_slots":           r.ChannelSlots,
		"min_visible":             r.MinVisibleFrames,
		"max_phones_per_pack":     r.MaxPhonemesPerPackage,
		"affinity_floor_permille": int64(math.Round(r.AffinityThreshold * 1000)),
		"max_delay_frames":        r.MaxDelayFrames,
		"samples_per_frame":       r.SamplesPerFrame,
	}
}

func exportV1(p *domain.Project, codes map[string]domain.Category, report *LossReport) (map[string]any, error) {
	cats := make(map[string]string, len(codes))
	for code, cat := range codes {
		cats[code] = string(cat)
	}
	var ds []any
	for di, d := range sortedDialogues(p) {
		var phones []any
		for pi, ph := range sortedPhonemes(d.Phonemes) {
			w, err := quant.WeightFromPermyriad(ph.WeightPermyriad, 1000, p.Render.Rounding, "weight_milli")
			if err != nil {
				return nil, err
			}
			if w.Truncated {
				report.Add(LossItem{
					Path:          fmt.Sprintf("$.dialogues[%d].phonemes[%d].weight_milli", di, pi),
					SourceVersion: p.SourceVersion, TargetVersion: string(V1),
					Action: "truncate duration weight", OldValue: strconv.FormatInt(ph.WeightPermyriad, 10),
					NewValue: strconv.FormatInt(w.Value*10, 10),
					Reason:   "per-mille scale cannot represent permyriad remainder",
				})
			}
			ss, se, hasSamples, err := sampleInterval(d, p, ph)
			if err != nil {
				return nil, err
			}
			pm := map[string]any{
				"id": ph.ID, "start_frame": ph.StartFrame, "end_frame": ph.EndFrame,
				"category": categoryCode(ph), "weight_milli": w.Value,
			}
			if hasSamples {
				pm["start_sample"] = ss
				pm["end_sample"] = se
			}
			phones = append(phones, pm)
		}
		ds = append(ds, map[string]any{
			"id": d.ID, "shard": d.ShardIndex, "speaker": d.Speaker, "channel": d.Channel,
			"pcm": map[string]any{
				"file": d.PCM.File, "encoding": d.PCM.Encoding,
				"sample_rate": d.PCM.SampleRate, "channels": d.PCM.Channels,
				"samples": d.PCM.Samples, "sha256": d.PCM.SHA256,
			},
			"phonemes": phones,
		})
	}
	return map[string]any{
		"time_basis": "frame", "weight_scale": 1000, "render": exportRenderV1(p.Render),
		"categories": cats, "dialogues": ds,
	}, nil
}

func exportV2(p *domain.Project, codes map[string]domain.Category, report *LossReport) (map[string]any, error) {
	cats := make(map[string]any, len(codes))
	for code, cat := range codes {
		cats[code] = map[string]any{"kind": string(cat)}
	}
	var ds []any
	for di, d := range sortedDialogues(p) {
		var phones []any
		for pi, ph := range sortedPhonemes(d.Phonemes) {
			sm, err := quant.FrameToMillis(ph.StartFrame, p.Render.TargetFPS, p.Render.Rounding, "start_ms")
			if err != nil {
				return nil, err
			}
			em, err := quant.FrameToMillis(ph.EndFrame, p.Render.TargetFPS, p.Render.Rounding, "end_ms")
			if err != nil {
				return nil, err
			}
			if sm.Truncated || em.Truncated {
				report.Add(LossItem{
					Path:          fmt.Sprintf("$.dialogues[%d].phonemes[%d].start_ms", di, pi),
					SourceVersion: p.SourceVersion, TargetVersion: string(V2),
					Action:   "round frame boundary to milliseconds",
					OldValue: fmt.Sprintf("%d..%d frames", ph.StartFrame, ph.EndFrame),
					NewValue: fmt.Sprintf("%d..%d ms", sm.Value, em.Value),
					Reason:   "millisecond schema cannot represent the frame remainder exactly",
				})
			}
			ss, se, hasSamples, err := sampleInterval(d, p, ph)
			if err != nil {
				return nil, err
			}
			pm := map[string]any{
				"phoneme_id": ph.ID, "start_ms": sm.Value, "end_ms": em.Value,
				"category_code": categoryCode(ph), "weight_bp": ph.WeightPermyriad,
				"transition_affinity": ph.Affinity,
			}
			if hasSamples {
				pm["start_sample"] = ss
				pm["end_sample"] = se
			}
			phones = append(phones, pm)
		}
		ds = append(ds, map[string]any{
			"dialogue_id": d.ID, "shard": d.ShardIndex, "speaker": d.Speaker, "channel": d.Channel,
			"audio": map[string]any{
				"file": d.PCM.File, "codec": d.PCM.Encoding,
				"sample_rate": d.PCM.SampleRate, "channels": d.PCM.Channels,
				"sample_count": d.PCM.Samples, "content_sha256": d.PCM.SHA256,
			},
			"phonemes": phones,
		})
	}
	return map[string]any{
		"time_basis": "millisecond", "duration_weight_scale": 10000,
		"frame_policy": exportRenderV2(p.Render), "categories": cats, "dialogues": ds,
	}, nil
}

func exportV3(p *domain.Project, codes map[string]domain.Category, grouped map[string][]string, _ *LossReport) (map[string]any, error) {
	groups := make(map[string][]any)
	groupNames := make([]string, 0, len(grouped))
	for name := range grouped {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		codesInGroup := append([]string(nil), grouped[name]...)
		sort.Strings(codesInGroup)
		for _, code := range codesInGroup {
			groups[name] = append(groups[name], map[string]any{"code": code, "kind": string(codes[code])})
		}
	}
	var ds []any
	for _, d := range sortedDialogues(p) {
		var phones []any
		for _, ph := range sortedPhonemes(d.Phonemes) {
			ss, se, hasSamples, err := sampleInterval(d, p, ph)
			if err != nil {
				return nil, err
			}
			pm := map[string]any{
				"id": ph.ID, "from_frame": ph.StartFrame, "to_frame": ph.EndFrame,
				"category": categoryCode(ph), "duration_weight": ph.WeightPermyriad,
				"affinity": ph.Affinity,
			}
			if hasSamples {
				pm["sample_start"] = ss
				pm["sample_end"] = se
			}
			phones = append(phones, pm)
		}
		ds = append(ds, map[string]any{
			"id": d.ID, "shard_index": d.ShardIndex, "speaker": d.Speaker, "channel": d.Channel,
			"pcm_meta": map[string]any{
				"path": d.PCM.File, "encoding": d.PCM.Encoding,
				"sample_rate": d.PCM.SampleRate, "channels": d.PCM.Channels,
				"sample_count": d.PCM.Samples, "sha": d.PCM.SHA256,
			},
			"phonemes": phones,
		})
	}
	return map[string]any{
		"time_basis": "frame", "duration_weight_scale": 10000,
		"render": exportRenderV3(p.Render), "category_groups": groups, "dialogues": ds,
	}, nil
}

func sampleInterval(d domain.Dialogue, p *domain.Project, ph domain.Phoneme) (int64, int64, bool, error) {
	ss, err := quant.FrameToSample(ph.StartFrame, d.PCM.SampleRate,
		p.Render.TargetFPS, p.Render.Rounding, "sample_start")
	if err != nil {
		return 0, 0, false, err
	}
	se, err := quant.FrameToSample(ph.EndFrame, d.PCM.SampleRate,
		p.Render.TargetFPS, p.Render.Rounding, "sample_end")
	if err != nil {
		return 0, 0, false, err
	}
	if ss.Truncated || se.Truncated {
		return 0, 0, false, nil
	}
	return ss.Value, se.Value, true, nil
}
