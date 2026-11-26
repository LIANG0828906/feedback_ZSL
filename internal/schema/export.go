package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	"lip-pipeline-prototype/internal/convert"
	"lip-pipeline-prototype/internal/domain"
)

type ExportOptions struct {
	IncludeShadow bool `json:"include_shadow"`
}

type ExportResult struct {
	Version     domain.SchemaVersion      `json:"version"`
	Data        []byte                    `json:"-"`
	Losses      []*domain.Error           `json:"losses"`
	Conversions []domain.ConversionRecord `json:"conversions"`
	Shadow      *Shadow                   `json:"shadow,omitempty"`
}

func Export(project *domain.Project, version domain.SchemaVersion, opts ExportOptions) (*ExportResult, error) {
	switch version {
	case domain.SchemaV1:
		return exportV1(project, opts)
	case domain.SchemaV2:
		return exportV2(project, opts)
	case domain.SchemaV3:
		return exportV3(project)
	default:
		return nil, schemaError("$.schema", "unknown export version "+string(version))
	}
}

func sortedGroups(project *domain.Project) []string {
	set := map[string]struct{}{}
	for _, d := range project.Dialogues {
		for _, s := range d.Shards {
			for _, p := range s.Phonemes {
				if p.EncodingGroup != "" {
					set[p.EncodingGroup] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func categoryMaps(project *domain.Project) (map[string]string, []map[string]any) {
	flat := map[string]string{}
	groups := map[string]map[string]string{}
	for _, d := range project.Dialogues {
		for _, s := range d.Shards {
			for _, p := range s.Phonemes {
				code := p.CategoryCode
				if code == "" {
					code = string(p.Category)
				}
				flat[code] = string(p.Category)
				group := p.EncodingGroup
				if group == "" {
					group = "default"
				}
				if groups[group] == nil {
					groups[group] = map[string]string{}
				}
				groups[group][code] = string(p.Category)
			}
		}
	}
	groupIDs := make([]string, 0, len(groups))
	for id := range groups {
		groupIDs = append(groupIDs, id)
	}
	sort.Strings(groupIDs)
	var tables []map[string]any
	for _, id := range groupIDs {
		tables = append(tables, map[string]any{
			"group_id":   id,
			"group_name": id,
			"encodings":  groups[id],
		})
	}
	return flat, tables
}

func newShadow(project *domain.Project) *Shadow {
	shadow := &Shadow{Affinities: map[string]int{}, EncodingGroups: map[string]string{}, ParamVersion: project.Params.ParamVersion}
	for _, d := range project.Dialogues {
		for _, s := range d.Shards {
			for _, p := range s.Phonemes {
				if p.AffinityExplicit {
					shadow.Affinities[p.ID] = p.AffinityToNext
				}
				if p.EncodingGroup != "" {
					shadow.EncodingGroups[p.ID] = p.EncodingGroup
				}
			}
		}
	}
	return shadow
}

func marshalDocument(doc map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

func exportV3(project *domain.Project) (*ExportResult, error) {
	_, tables := categoryMaps(project)
	groupsV3 := make([]map[string]any, 0, len(tables))
	for _, table := range tables {
		groupsV3 = append(groupsV3, map[string]any{
			"id": table["group_id"], "label": table["group_name"], "encodings": table["encodings"],
		})
	}
	dialogues := []any{}
	for _, d := range project.Dialogues {
		shards := []any{}
		for _, s := range d.Shards {
			phonemes := []any{}
			for _, p := range s.Phonemes {
				ph := map[string]any{
					"id": p.ID, "index": p.Index, "code": p.CategoryCode, "group": p.EncodingGroup,
					"timing": map[string]any{"time_base": "samples", "start_sample": p.StartSample, "end_sample": p.EndSample},
					"weight": map[string]any{"scale": "permyriad", "value": p.WeightPermyriad},
				}
				if p.Index < len(s.Phonemes)-1 {
					ph["transition"] = map[string]any{"affinity_to_next": p.AffinityToNext}
				}
				phonemes = append(phonemes, ph)
			}
			shards = append(shards, map[string]any{
				"id": s.ID, "index": s.Index, "channel_id": s.ChannelID,
				"total_samples": s.TotalSamples, "pcm": s.PCM, "phonemes": phonemes,
			})
		}
		dialogues = append(dialogues, map[string]any{
			"id": d.ID, "title": d.Title, "speaker_id": d.Speaker,
			"channel_id": d.ChannelID, "pcm": d.PCM, "shards": shards,
		})
	}
	doc := map[string]any{
		"schema": domain.SchemaV3, "project_id": project.ID, "seed": project.Seed,
		"pipeline": map[string]any{
			"sample_rate": project.Params.SampleRate, "target_fps": project.Params.FPS,
			"quantization_rule": project.Params.QuantizationRule,
			"param_version":     project.Params.ParamVersion,
			"transition": map[string]any{
				"tolerance":                          project.Params.TransitionTolerance,
				"affinity_merge_threshold_permyriad": project.Params.AffinityMergeThreshold,
			},
			"packing": map[string]any{"max_phonemes_per_pack": project.Params.MaxPhonemesPerPack},
			"preview": map[string]any{
				"latency_budget_ms":   project.Params.PreviewLatencyBudgetMS,
				"frame_stride":        project.Params.PreviewFrameStride,
				"max_transition_scan": project.Params.PreviewMaxTransitionScan,
			},
		},
		"channels":        project.Channels,
		"category_groups": groupsV3,
		"dialogues":       dialogues,
	}
	data, err := marshalDocument(doc)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV3, Data: data}, nil
}

func exportV2(project *domain.Project, opts ExportOptions) (*ExportResult, error) {
	_, tables := categoryMaps(project)
	ledger := convert.NewLedger()
	losses := []*domain.Error{}
	shadow := newShadow(project)

	dialogues := []any{}
	for _, d := range project.Dialogues {
		shards := []any{}
		for _, s := range d.Shards {
			phonemes := []any{}
			for _, p := range s.Phonemes {
				path := fmt.Sprintf("dialogues.%s.shards.%s.phonemes.%s.affinity_to_next", d.ID, s.ID, p.ID)
				if p.Index < len(s.Phonemes)-1 {
					disposition := "omit_native_field;v2_loader_uses_default_6000"
					if opts.IncludeShadow {
						disposition = "omit_native_field;preserve_in:" + ShadowKey + ".affinities"
					}
					losses = append(losses, &domain.Error{
						Code: domain.CodeLossy, Path: path, Field: "transition_affinity",
						Message: "v2 cannot natively express transition affinity", OldValue: p.AffinityToNext,
						Disposition: disposition, SourceVersion: domain.SchemaV2,
					})
				}
				phonemes = append(phonemes, map[string]any{
					"phoneme_id": p.ID, "index": p.Index,
					"start_sample": p.StartSample, "end_sample": p.EndSample,
					"category_code": p.CategoryCode, "category_group": p.EncodingGroup,
					"weight_permyriad": p.WeightPermyriad,
				})
			}
			shards = append(shards, map[string]any{
				"shard_id": s.ID, "shard_index": s.Index, "total_samples": s.TotalSamples,
				"phoneme_annotations": phonemes,
			})
		}
		dialogues = append(dialogues, map[string]any{
			"dialogue_id": d.ID, "title": d.Title, "speaker_id": d.Speaker,
			"channel_id": d.ChannelID, "recording_shards": shards,
		})
	}

	doc := map[string]any{
		"schema": domain.SchemaV2, "project_id": project.ID, "seed": project.Seed,
		"render_config": map[string]any{
			"target_fps": project.Params.FPS, "sample_rate": project.Params.SampleRate,
			"time_base": "samples", "weight_scale": "permyriad",
			"quantization_rule":                  project.Params.QuantizationRule,
			"transition_tolerance":               project.Params.TransitionTolerance,
			"max_phonemes_per_pack":              project.Params.MaxPhonemesPerPack,
			"affinity_merge_threshold_permyriad": project.Params.AffinityMergeThreshold,
			"preview_latency_budget_ms":          project.Params.PreviewLatencyBudgetMS,
			"preview_frame_stride":               project.Params.PreviewFrameStride,
			"max_transition_scan":                project.Params.PreviewMaxTransitionScan,
			"param_version":                      project.Params.ParamVersion,
		},
		"speaker_channels": channelSnake(project.Channels),
		"category_tables":  tables,
		"dialogues":        dialogues,
	}
	if opts.IncludeShadow {
		doc[ShadowKey] = shadow
	}
	data, err := marshalDocument(doc)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV2, Data: data, Losses: losses, Shadow: shadow, Conversions: ledger.Records}, nil
}

func exportV1(project *domain.Project, opts ExportOptions) (*ExportResult, error) {
	flat, _ := categoryMaps(project)
	ledger := convert.NewLedger()
	losses := []*domain.Error{}
	shadow := newShadow(project)

	for _, group := range sortedGroups(project) {
		losses = append(losses, &domain.Error{
			Code: domain.CodeLossy, Path: "$.categoryTables." + group, Field: "category_group",
			Message: "v1 has only a flat categoryCodes table", OldValue: group,
			Disposition:   lossyDisposition(opts.IncludeShadow, "flatten_into_categoryCodes;preserve_group_in_shadow"),
			SourceVersion: domain.SchemaV1,
		})
	}

	dialogues := []any{}
	for _, d := range project.Dialogues {
		shards := []any{}
		for _, s := range d.Shards {
			totalMS, err := convert.SamplesToMS(s.TotalSamples, project.Params.SampleRate, "$."+s.ID+".totalSamples", ledger)
			if err != nil {
				return nil, err
			}
			phonemes := []any{}
			for _, p := range s.Phonemes {
				startMS, err := convert.SamplesToMS(p.StartSample, project.Params.SampleRate, "$."+p.ID+".startMs", ledger)
				if err != nil {
					return nil, err
				}
				endMS, err := convert.SamplesToMS(p.EndSample, project.Params.SampleRate, "$."+p.ID+".endMs", ledger)
				if err != nil {
					return nil, err
				}
				permille, residual := convert.PermyriadToPermille(p.WeightPermyriad, "$."+p.ID+".weightPermille", ledger)
				if residual != 0 {
					losses = append(losses, &domain.Error{
						Code: domain.CodeLossy, Path: "$." + p.ID + ".weightPermille", Field: "weight_permyriad",
						Message: "weight scale conversion truncates a non-zero remainder", OldValue: p.WeightPermyriad,
						DefaultValue: permille, Residual: int64(residual),
						Disposition: "floor_to_permille;record_residual", SourceVersion: domain.SchemaV1,
					})
				}
				if p.Index < len(s.Phonemes)-1 {
					losses = append(losses, &domain.Error{
						Code: domain.CodeLossy, Path: "$." + p.ID + ".transitionAffinity", Field: "transition_affinity",
						Message: "v1 cannot natively express transition affinity", OldValue: p.AffinityToNext,
						Disposition:   lossyDisposition(opts.IncludeShadow, "omit_native_field;preserve_in_shadow"),
						SourceVersion: domain.SchemaV1,
					})
				}
				phonemes = append(phonemes, map[string]any{
					"phonemeId": p.ID, "index": p.Index, "startMs": startMS, "endMs": endMS,
					"kindCode": p.CategoryCode, "weightPermille": permille,
				})
			}
			shards = append(shards, map[string]any{
				"shardId": s.ID, "shardIndex": s.Index, "totalDurationMs": totalMS,
				"phonemes": phonemes,
			})
		}
		dialogues = append(dialogues, map[string]any{
			"dialogueId": d.ID, "title": d.Title, "speaker": d.Speaker,
			"channelId": d.ChannelID, "shards": shards,
		})
	}

	doc := map[string]any{
		"schema": domain.SchemaV1, "projectId": project.ID, "seed": project.Seed,
		"render": map[string]any{
			"frameRate": project.Params.FPS, "sampleRate": project.Params.SampleRate,
			"timeBase": "milliseconds", "weightScale": "permille",
			"quantization":               project.Params.QuantizationRule,
			"transitionTolerance":        project.Params.TransitionTolerance,
			"maxPhonemesPerPack":         project.Params.MaxPhonemesPerPack,
			"affinityThresholdPermyriad": project.Params.AffinityMergeThreshold,
			"previewLatencyBudgetMs":     project.Params.PreviewLatencyBudgetMS,
			"previewFrameStride":         project.Params.PreviewFrameStride,
			"previewMaxTransitionScan":   project.Params.PreviewMaxTransitionScan,
			"paramVersion":               project.Params.ParamVersion,
		},
		"speakerChannels": channelCamel(project.Channels),
		"categoryCodes":   flat,
		"dialogues":       dialogues,
	}
	if opts.IncludeShadow {
		doc[ShadowKey] = shadow
	}
	data, err := marshalDocument(doc)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV1, Data: data, Losses: losses, Shadow: shadow, Conversions: ledger.Records}, nil
}

func lossyDisposition(shadow bool, base string) string {
	if shadow {
		return base
	}
	return "strict_native:" + base
}

func channelSnake(in []domain.ChannelSpec) []map[string]any {
	out := []map[string]any{}
	for _, c := range in {
		out = append(out, map[string]any{"channel_id": c.ID, "speaker_id": c.Speaker, "concurrency_slots": c.Slots})
	}
	return out
}

func channelCamel(in []domain.ChannelSpec) []map[string]any {
	out := []map[string]any{}
	for _, c := range in {
		out = append(out, map[string]any{"id": c.ID, "speaker": c.Speaker, "concurrencySlots": c.Slots})
	}
	return out
}
