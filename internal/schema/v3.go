package schema

import (
	"encoding/json"
	"fmt"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

func loadV3(data []byte, root map[string]json.RawMessage) (*LoadResult, error) {
	report := domain.LoadReport{Version: domain.SchemaV3}

	projectID, err := requireString(root, "$", "project_id")
	if err != nil {
		return nil, err
	}
	pipelineRaw, err := requireField(root, "$", "pipeline")
	if err != nil {
		return nil, err
	}
	pipeline, err := decodeObject(pipelineRaw, "$.pipeline")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requireInt(pipeline, "$.pipeline", "sample_rate")
	if err != nil {
		return nil, err
	}
	fps, err := requireInt(pipeline, "$.pipeline", "target_fps")
	if err != nil {
		return nil, err
	}
	transitionRaw, err := requireField(pipeline, "$.pipeline", "transition")
	if err != nil {
		return nil, err
	}
	transition, err := decodeObject(transitionRaw, "$.pipeline.transition")
	if err != nil {
		return nil, err
	}
	tolerance, err := requireInt(transition, "$.pipeline.transition", "tolerance")
	if err != nil {
		return nil, err
	}
	packingRaw, err := requireField(pipeline, "$.pipeline", "packing")
	if err != nil {
		return nil, err
	}
	packing, err := decodeObject(packingRaw, "$.pipeline.packing")
	if err != nil {
		return nil, err
	}
	maxPhonemes, err := requireInt(packing, "$.pipeline.packing", "max_phonemes_per_pack")
	if err != nil {
		return nil, err
	}

	codeCategory, codeGroup, err := loadV3Groups(root)
	if err != nil {
		return nil, err
	}
	channels, err := loadV3Channels(root)
	if err != nil {
		return nil, err
	}
	dialogueItemsRaw, err := requireField(root, "$", "dialogues")
	if err != nil {
		return nil, err
	}
	dialogueItems, err := decodeArray(dialogueItemsRaw, "$.dialogues")
	if err != nil {
		return nil, err
	}

	params := domain.DefaultParams()
	params.SampleRate = int(sampleRate)
	params.FPS = int(fps)
	params.TransitionTolerance = int(tolerance)
	params.MaxPhonemesPerPack = int(maxPhonemes)
	params.ParamVersion = optionalString(pipeline, "param_version", "params-v3")
	params.QuantizationRule = optionalString(pipeline, "quantization_rule", params.QuantizationRule)
	if threshold := optionalInt(transition, "affinity_merge_threshold_permyriad", params.AffinityMergeThreshold); threshold != 0 {
		params.AffinityMergeThreshold = threshold
	}
	if previewRaw, ok := pipeline["preview"]; ok {
		if pm, perr := decodeObject(previewRaw, "$.pipeline.preview"); perr == nil {
			params.PreviewLatencyBudgetMS = optionalInt(pm, "latency_budget_ms", params.PreviewLatencyBudgetMS)
			params.PreviewFrameStride = optionalInt(pm, "frame_stride", params.PreviewFrameStride)
			params.PreviewMaxTransitionScan = optionalInt(pm, "max_transition_scan", params.PreviewMaxTransitionScan)
		}
	}

	project := &domain.Project{
		ID: projectID, Params: params, Channels: channels, SourceVersion: domain.SchemaV3,
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
		did, err := requireString(dm, dpath, "id")
		if err != nil {
			return nil, err
		}
		channelID, err := requireString(dm, dpath, "channel_id")
		if err != nil {
			return nil, err
		}
		dlg := domain.Dialogue{
			ID: did, Title: optionalString(dm, "title", did), Speaker: optionalString(dm, "speaker_id", ""),
			ChannelID: channelID, SourceVersion: domain.SchemaV3,
		}
		shardItemsRaw, err := requireField(dm, dpath, "shards")
		if err != nil {
			return nil, err
		}
		shardItems, err := decodeArray(shardItemsRaw, dpath+".shards")
		if err != nil {
			return nil, err
		}
		var pcmTotal int64
		for si, sitem := range shardItems {
			spath := fmt.Sprintf("%s.shards[%d]", dpath, si)
			sm, err := decodeObject(sitem, spath)
			if err != nil {
				return nil, err
			}
			sid, err := requireString(sm, spath, "id")
			if err != nil {
				return nil, err
			}
			sidx, err := requireInt(sm, spath, "index")
			if err != nil {
				return nil, err
			}
			total, err := requireInt(sm, spath, "total_samples")
			if err != nil {
				return nil, err
			}
			phItemsRaw, err := requireField(sm, spath, "phonemes")
			if err != nil {
				return nil, err
			}
			phItems, err := decodeArray(phItemsRaw, spath+".phonemes")
			if err != nil {
				return nil, err
			}
			shard := domain.Shard{ID: sid, DialogueID: did, Index: int(sidx), ChannelID: channelID, TotalSamples: total}
			for pi, pitem := range phItems {
				ppath := fmt.Sprintf("%s.phonemes[%d]", spath, pi)
				pm, err := decodeObject(pitem, ppath)
				if err != nil {
					return nil, err
				}
				pid, err := requireString(pm, ppath, "id")
				if err != nil {
					return nil, err
				}
				idx, err := requireInt(pm, ppath, "index")
				if err != nil {
					return nil, err
				}
				timingRaw, err := requireField(pm, ppath, "timing")
				if err != nil {
					return nil, err
				}
				timing, err := decodeObject(timingRaw, ppath+".timing")
				if err != nil {
					return nil, err
				}
				start, err := requireInt(timing, ppath+".timing", "start_sample")
				if err != nil {
					return nil, err
				}
				end, err := requireInt(timing, ppath+".timing", "end_sample")
				if err != nil {
					return nil, err
				}
				weightRaw, err := requireField(pm, ppath, "weight")
				if err != nil {
					return nil, err
				}
				weightObj, err := decodeObject(weightRaw, ppath+".weight")
				if err != nil {
					return nil, err
				}
				weightValue, err := requireInt(weightObj, ppath+".weight", "value")
				if err != nil {
					return nil, err
				}
				if err := checkPermyriad(int(weightValue), ppath+".weight.value"); err != nil {
					return nil, err
				}
				code, err := requireString(pm, ppath, "code")
				if err != nil {
					return nil, err
				}
				category, ok := codeCategory[code]
				if !ok {
					return nil, schemaError(ppath+".code", "code absent from category_groups: "+code)
				}
				group := optionalString(pm, "group", codeGroup[code])
				affinity := DefaultUpgradedAffinity
				explicit := false
				if pi < len(phItems)-1 {
					trRaw, terr := requireField(pm, ppath, "transition")
					if terr != nil {
						return nil, terr
					}
					tr, derr := decodeObject(trRaw, ppath+".transition")
					if derr != nil {
						return nil, derr
					}
					aff, aerr := requireInt(tr, ppath+".transition", "affinity_to_next")
					if aerr != nil {
						return nil, aerr
					}
					affinity = int(aff)
					explicit = true
				}
				shard.Phonemes = append(shard.Phonemes, domain.Phoneme{
					ID: pid, DialogueID: did, ShardID: sid, Index: int(idx),
					StartSample: start, EndSample: end, Category: category, CategoryCode: code,
					EncodingGroup: group, WeightPermyriad: int(weightValue),
					AffinityToNext: affinity, AffinityExplicit: explicit,
				})
			}
			if len(shard.Phonemes) > 0 {
				last := &shard.Phonemes[len(shard.Phonemes)-1]
				last.AffinityToNext = 0
				last.AffinityExplicit = false
			}
			shard.PCM = pcmForShard(shard, params.SampleRate, project.Seed)
			if pcmRaw, ok := sm["pcm"]; ok {
				var pcm domain.PCMDescription
				if err := json.Unmarshal(pcmRaw, &pcm); err == nil && pcm.SampleCount == shard.TotalSamples {
					shard.PCM = pcm
				}
			}
			pcmTotal += shard.TotalSamples
			dlg.Shards = append(dlg.Shards, shard)
		}
		dlg.PCM = pcmForDialogue(dlg, params.SampleRate, project.Seed, pcmTotal)
		if pcmRaw, ok := dm["pcm"]; ok {
			var pcm domain.PCMDescription
			if err := json.Unmarshal(pcmRaw, &pcm); err == nil && pcm.SampleCount == pcmTotal {
				dlg.PCM = pcm
			}
		}
		dlg.ContentHash = hashx.JSON(struct {
			ID     string
			Shards []domain.Shard
		}{dlg.ID, dlg.Shards})
		project.Dialogues = append(project.Dialogues, dlg)
	}

	return &LoadResult{Project: project, Report: report}, nil
}

func loadV3Groups(root map[string]json.RawMessage) (map[string]domain.Category, map[string]string, error) {
	raw, err := requireField(root, "$", "category_groups")
	if err != nil {
		return nil, nil, err
	}
	items, err := decodeArray(raw, "$.category_groups")
	if err != nil {
		return nil, nil, err
	}
	codeCategory := map[string]domain.Category{}
	codeGroup := map[string]string{}
	for i, item := range items {
		path := fmt.Sprintf("$.category_groups[%d]", i)
		m, err := decodeObject(item, path)
		if err != nil {
			return nil, nil, err
		}
		groupID, err := requireString(m, path, "id")
		if err != nil {
			return nil, nil, err
		}
		encRaw, err := requireField(m, path, "encodings")
		if err != nil {
			return nil, nil, err
		}
		enc := map[string]string{}
		if err := json.Unmarshal(encRaw, &enc); err != nil {
			return nil, nil, schemaError(path+".encodings", "expected code map")
		}
		for code, text := range enc {
			category, ok := categoryFromText(text)
			if !ok {
				return nil, nil, schemaError(path+".encodings."+code, "unknown category")
			}
			codeCategory[code] = category
			codeGroup[code] = groupID
		}
	}
	return codeCategory, codeGroup, nil
}

func loadV3Channels(root map[string]json.RawMessage) ([]domain.ChannelSpec, error) {
	raw, err := requireField(root, "$", "channels")
	if err != nil {
		return nil, err
	}
	items, err := decodeArray(raw, "$.channels")
	if err != nil {
		return nil, err
	}
	out := []domain.ChannelSpec{}
	for i, item := range items {
		path := fmt.Sprintf("$.channels[%d]", i)
		m, err := decodeObject(item, path)
		if err != nil {
			return nil, err
		}
		id, err := requireString(m, path, "id")
		if err != nil {
			return nil, err
		}
		speaker := optionalString(m, "speaker_id", optionalString(m, "speaker", ""))
		slots, err := requireInt(m, path, "slots")
		if err != nil {
			return nil, err
		}
		out = append(out, domain.ChannelSpec{ID: id, Speaker: speaker, Slots: int(slots)})
	}
	return out, nil
}
