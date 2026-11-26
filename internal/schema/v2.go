package schema

import (
	"encoding/json"
	"fmt"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

func loadV2(data []byte, root map[string]json.RawMessage) (*LoadResult, error) {
	report := domain.LoadReport{Version: domain.SchemaV2}
	shadow := parseShadow(root)

	projectID, err := requireString(root, "$", "project_id")
	if err != nil {
		return nil, err
	}
	renderRaw, err := requireField(root, "$", "render_config")
	if err != nil {
		return nil, err
	}
	render, err := decodeObject(renderRaw, "$.render_config")
	if err != nil {
		return nil, err
	}
	if tb, err := requireString(render, "$.render_config", "time_base"); err != nil {
		return nil, err
	} else if tb != "samples" {
		return nil, schemaError("$.render_config.time_base", "v2 requires sample time base")
	}
	if ws, err := requireString(render, "$.render_config", "weight_scale"); err != nil {
		return nil, err
	} else if ws != "permyriad" {
		return nil, schemaError("$.render_config.weight_scale", "v2 requires permyriad weight scale")
	}
	fps, err := requireInt(render, "$.render_config", "target_fps")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requireInt(render, "$.render_config", "sample_rate")
	if err != nil {
		return nil, err
	}
	tolerance, err := requireInt(render, "$.render_config", "transition_tolerance")
	if err != nil {
		return nil, err
	}
	maxPacks, err := requireInt(render, "$.render_config", "max_phonemes_per_pack")
	if err != nil {
		return nil, err
	}

	codeCategory := map[string]domain.Category{}
	codeGroup := map[string]string{}
	tableRaw, err := requireField(root, "$", "category_tables")
	if err != nil {
		return nil, err
	}
	tableItems, err := decodeArray(tableRaw, "$.category_tables")
	if err != nil {
		return nil, err
	}
	for i, item := range tableItems {
		path := fmt.Sprintf("$.category_tables[%d]", i)
		m, err := decodeObject(item, path)
		if err != nil {
			return nil, err
		}
		groupID, err := requireString(m, path, "group_id")
		if err != nil {
			return nil, err
		}
		encRaw, err := requireField(m, path, "encodings")
		if err != nil {
			return nil, err
		}
		enc := map[string]string{}
		if err := json.Unmarshal(encRaw, &enc); err != nil {
			return nil, schemaError(path+".encodings", "expected code to category map")
		}
		for code, categoryText := range enc {
			category, ok := categoryFromText(categoryText)
			if !ok {
				return nil, schemaError(path+".encodings."+code, "unknown category "+categoryText)
			}
			codeCategory[code] = category
			codeGroup[code] = groupID
		}
	}

	channels, err := loadChannelsSnake(root)
	if err != nil {
		return nil, err
	}
	dialogueRaw, err := requireField(root, "$", "dialogues")
	if err != nil {
		return nil, err
	}
	dialogueItems, err := decodeArray(dialogueRaw, "$.dialogues")
	if err != nil {
		return nil, err
	}

	params := domain.DefaultParams()
	params.SampleRate = int(sampleRate)
	params.FPS = int(fps)
	params.TransitionTolerance = int(tolerance)
	params.MaxPhonemesPerPack = int(maxPacks)
	params.ParamVersion = optionalString(render, "param_version", "params-v2-import")
	params.AffinityMergeThreshold = optionalInt(render, "affinity_merge_threshold_permyriad", params.AffinityMergeThreshold)
	params.PreviewLatencyBudgetMS = optionalInt(render, "preview_latency_budget_ms", params.PreviewLatencyBudgetMS)
	params.PreviewFrameStride = optionalInt(render, "preview_frame_stride", params.PreviewFrameStride)

	project := &domain.Project{
		ID: projectID, Params: params, Channels: channels, SourceVersion: domain.SchemaV2,
		GeneratedAt: fixedLoadTime(),
	}
	if seedRaw, ok := root["seed"]; ok {
		_ = json.Unmarshal(seedRaw, &project.Seed)
	}

	for di, ditem := range dialogueItems {
		dpath := fmt.Sprintf("$.dialogues[%d]", di)
		dm, err := decodeObject(ditem, dpath)
		if err != nil {
			return nil, err
		}
		did, err := requireString(dm, dpath, "dialogue_id")
		if err != nil {
			return nil, err
		}
		channelID, err := requireString(dm, dpath, "channel_id")
		if err != nil {
			return nil, err
		}
		dlg := domain.Dialogue{
			ID: did, Title: optionalString(dm, "title", did), Speaker: optionalString(dm, "speaker_id", ""),
			ChannelID: channelID, SourceVersion: domain.SchemaV2,
		}
		shardsRaw, err := requireField(dm, dpath, "recording_shards")
		if err != nil {
			return nil, err
		}
		shardItems, err := decodeArray(shardsRaw, dpath+".recording_shards")
		if err != nil {
			return nil, err
		}
		var pcmTotal int64
		for si, sitem := range shardItems {
			spath := fmt.Sprintf("%s.recording_shards[%d]", dpath, si)
			sm, err := decodeObject(sitem, spath)
			if err != nil {
				return nil, err
			}
			sid, err := requireString(sm, spath, "shard_id")
			if err != nil {
				return nil, err
			}
			sidx, err := requireInt(sm, spath, "shard_index")
			if err != nil {
				return nil, err
			}
			total, err := requireInt(sm, spath, "total_samples")
			if err != nil {
				return nil, err
			}
			phRaw, err := requireField(sm, spath, "phoneme_annotations")
			if err != nil {
				return nil, err
			}
			phItems, err := decodeArray(phRaw, spath+".phoneme_annotations")
			if err != nil {
				return nil, err
			}
			shard := domain.Shard{ID: sid, DialogueID: did, Index: int(sidx), ChannelID: channelID, TotalSamples: total}
			for pi, pitem := range phItems {
				ppath := fmt.Sprintf("%s.phoneme_annotations[%d]", spath, pi)
				pm, err := decodeObject(pitem, ppath)
				if err != nil {
					return nil, err
				}
				pid, err := requireString(pm, ppath, "phoneme_id")
				if err != nil {
					return nil, err
				}
				idx, err := requireInt(pm, ppath, "index")
				if err != nil {
					return nil, err
				}
				start, err := requireInt(pm, ppath, "start_sample")
				if err != nil {
					return nil, err
				}
				end, err := requireInt(pm, ppath, "end_sample")
				if err != nil {
					return nil, err
				}
				code, err := requireString(pm, ppath, "category_code")
				if err != nil {
					return nil, err
				}
				weight, err := requireInt(pm, ppath, "weight_permyriad")
				if err != nil {
					return nil, err
				}
				category, ok := codeCategory[code]
				if !ok {
					return nil, schemaError(ppath+".category_code", "code absent from category_tables: "+code)
				}
				if err := checkPermyriad(int(weight), ppath+".weight_permyriad"); err != nil {
					return nil, err
				}
				group := optionalString(pm, "category_group", "")
				if group == "" {
					group = codeGroup[code]
					fill(&report, ppath+".category_group", nil, group, "derive group from grouped category table")
				}
				affinityPath := ppath + ".transition_affinity"
				fill(&report, affinityPath, nil, DefaultUpgradedAffinity, "v2 has no affinity field; use declared upgrade default")
				shard.Phonemes = append(shard.Phonemes, domain.Phoneme{
					ID: pid, DialogueID: did, ShardID: sid, Index: int(idx),
					StartSample: start, EndSample: end, Category: category, CategoryCode: code,
					EncodingGroup: group, WeightPermyriad: int(weight),
					AffinityToNext: DefaultUpgradedAffinity, AffinityExplicit: false,
				})
			}
			if len(shard.Phonemes) > 0 {
				shard.Phonemes[len(shard.Phonemes)-1].AffinityToNext = 0
			}
			shard.PCM = pcmForShard(shard, params.SampleRate, project.Seed)
			pcmTotal += shard.TotalSamples
			dlg.Shards = append(dlg.Shards, shard)
		}
		dlg.PCM = pcmForDialogue(dlg, params.SampleRate, project.Seed, pcmTotal)
		dlg.ContentHash = hashx.JSON(struct {
			ID     string
			Shards []domain.Shard
		}{dlg.ID, dlg.Shards})
		project.Dialogues = append(project.Dialogues, dlg)
	}

	applyShadowToProject(project, shadow)
	report.Conversions = nil
	return &LoadResult{Project: project, Report: report, Shadow: shadow}, nil
}

func loadChannelsSnake(root map[string]json.RawMessage) ([]domain.ChannelSpec, error) {
	raw, err := requireField(root, "$", "speaker_channels")
	if err != nil {
		return nil, err
	}
	items, err := decodeArray(raw, "$.speaker_channels")
	if err != nil {
		return nil, err
	}
	out := []domain.ChannelSpec{}
	for i, item := range items {
		path := fmt.Sprintf("$.speaker_channels[%d]", i)
		m, err := decodeObject(item, path)
		if err != nil {
			return nil, err
		}
		id, err := requireString(m, path, "channel_id")
		if err != nil {
			return nil, err
		}
		speaker, err := requireString(m, path, "speaker_id")
		if err != nil {
			return nil, err
		}
		slots, err := requireInt(m, path, "concurrency_slots")
		if err != nil {
			return nil, err
		}
		out = append(out, domain.ChannelSpec{ID: id, Speaker: speaker, Slots: int(slots)})
	}
	return out, nil
}

func checkPermyriad(value int, path string) error {
	if value < 0 || value > 10000 {
		return &domain.Error{Code: domain.CodeVal, Message: "weight must be in [0,10000]", Path: path, Actual: value}
	}
	return nil
}

func applyShadowToProject(project *domain.Project, shadow *Shadow) {
	if shadow == nil {
		return
	}
	for di := range project.Dialogues {
		for si := range project.Dialogues[di].Shards {
			sh := &project.Dialogues[di].Shards[si]
			for pi := range sh.Phonemes {
				ph := &sh.Phonemes[pi]
				if group, ok := shadow.EncodingGroups[ph.ID]; ok {
					ph.EncodingGroup = group
				}
				if affinity, ok := shadow.Affinities[ph.ID]; ok && pi < len(sh.Phonemes)-1 {
					ph.AffinityToNext = affinity
					ph.AffinityExplicit = true
				}
			}
		}
	}
	if shadow.ParamVersion != "" {
		project.Params.ParamVersion = shadow.ParamVersion
	}
}
