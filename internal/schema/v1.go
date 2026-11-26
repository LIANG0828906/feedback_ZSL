package schema

import (
	"encoding/json"
	"fmt"

	"lip-pipeline-prototype/internal/convert"
	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

const DefaultUpgradedAffinity = 6000
const DefaultFlatGroup = "upgraded-flat"

func loadV1(data []byte, root map[string]json.RawMessage) (*LoadResult, error) {
	report := domain.LoadReport{Version: domain.SchemaV1}
	shadow := parseShadow(root)

	projectID, err := requireString(root, "$", "projectId")
	if err != nil {
		return nil, err
	}
	renderRaw, err := requireField(root, "$", "render")
	if err != nil {
		return nil, err
	}
	render, err := decodeObject(renderRaw, "$.render")
	if err != nil {
		return nil, err
	}
	fps, err := requireInt(render, "$.render", "frameRate")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requireInt(render, "$.render", "sampleRate")
	if err != nil {
		return nil, err
	}
	tolerance, err := requireInt(render, "$.render", "transitionTolerance")
	if err != nil {
		return nil, err
	}
	maxPacks, err := requireInt(render, "$.render", "maxPhonemesPerPack")
	if err != nil {
		return nil, err
	}

	categoriesRaw, err := requireField(root, "$", "categoryCodes")
	if err != nil {
		return nil, err
	}
	categoryCodes := map[string]string{}
	if err := json.Unmarshal(categoriesRaw, &categoryCodes); err != nil {
		return nil, schemaError("$.categoryCodes", "expected flat code to category map")
	}

	channelItemsRaw, err := requireField(root, "$", "speakerChannels")
	if err != nil {
		return nil, err
	}
	channelItems, err := decodeArray(channelItemsRaw, "$.speakerChannels")
	if err != nil {
		return nil, err
	}
	channels := []domain.ChannelSpec{}
	for i, item := range channelItems {
		path := fmt.Sprintf("$.speakerChannels[%d]", i)
		m, err := decodeObject(item, path)
		if err != nil {
			return nil, err
		}
		id, err := requireString(m, path, "id")
		if err != nil {
			return nil, err
		}
		speaker, err := requireString(m, path, "speaker")
		if err != nil {
			return nil, err
		}
		slots, err := requireInt(m, path, "concurrencySlots")
		if err != nil {
			return nil, err
		}
		channels = append(channels, domain.ChannelSpec{ID: id, Speaker: speaker, Slots: int(slots)})
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
	params.MaxPhonemesPerPack = int(maxPacks)
	params.ParamVersion = optionalString(render, "paramVersion", "params-v1-import")
	if v := optionalInt(render, "affinityThresholdPermyriad", params.AffinityMergeThreshold); v != 0 {
		params.AffinityMergeThreshold = v
	}
	params.PreviewLatencyBudgetMS = optionalInt(render, "previewLatencyBudgetMs", params.PreviewLatencyBudgetMS)
	params.PreviewFrameStride = optionalInt(render, "previewFrameStride", params.PreviewFrameStride)

	project := &domain.Project{
		ID: projectID, Params: params, Channels: channels, SourceVersion: domain.SchemaV1,
		GeneratedAt: fixedLoadTime(),
	}
	if seedRaw, ok := root["seed"]; ok {
		_ = json.Unmarshal(seedRaw, &project.Seed)
	}

	ledger := convert.NewLedger()
	for di, ditem := range dialogueItems {
		dpath := fmt.Sprintf("$.dialogues[%d]", di)
		dm, err := decodeObject(ditem, dpath)
		if err != nil {
			return nil, err
		}
		did, err := requireString(dm, dpath, "dialogueId")
		if err != nil {
			return nil, err
		}
		title, _ := requireString(dm, dpath, "title")
		channelID, err := requireString(dm, dpath, "channelId")
		if err != nil {
			return nil, err
		}
		speaker := optionalString(dm, "speaker", "")
		dlg := domain.Dialogue{ID: did, Title: title, Speaker: speaker, ChannelID: channelID, SourceVersion: domain.SchemaV1}

		shardRaw, err := requireField(dm, dpath, "shards")
		if err != nil {
			return nil, err
		}
		shardItems, err := decodeArray(shardRaw, dpath+".shards")
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
			sid, err := requireString(sm, spath, "shardId")
			if err != nil {
				return nil, err
			}
			shardIndex, err := requireInt(sm, spath, "shardIndex")
			if err != nil {
				return nil, err
			}
			totalMS, err := requireInt(sm, spath, "totalDurationMs")
			if err != nil {
				return nil, err
			}
			totalSamples, err := convert.MSToSamples(totalMS, params.SampleRate, spath+".totalDurationMs", ledger)
			if err != nil {
				return nil, err
			}
			phRaw, err := requireField(sm, spath, "phonemes")
			if err != nil {
				return nil, err
			}
			phItems, err := decodeArray(phRaw, spath+".phonemes")
			if err != nil {
				return nil, err
			}
			shard := domain.Shard{ID: sid, DialogueID: did, Index: int(shardIndex), ChannelID: channelID, TotalSamples: totalSamples}
			for pi, pitem := range phItems {
				ppath := fmt.Sprintf("%s.phonemes[%d]", spath, pi)
				pm, err := decodeObject(pitem, ppath)
				if err != nil {
					return nil, err
				}
				pid, err := requireString(pm, ppath, "phonemeId")
				if err != nil {
					return nil, err
				}
				index, err := requireInt(pm, ppath, "index")
				if err != nil {
					return nil, err
				}
				startMS, err := requireInt(pm, ppath, "startMs")
				if err != nil {
					return nil, err
				}
				endMS, err := requireInt(pm, ppath, "endMs")
				if err != nil {
					return nil, err
				}
				code, err := requireString(pm, ppath, "kindCode")
				if err != nil {
					return nil, err
				}
				weight, err := requireInt(pm, ppath, "weightPermille")
				if err != nil {
					return nil, err
				}
				categoryText, ok := categoryCodes[code]
				if !ok {
					return nil, schemaError(ppath+".kindCode", "kind code "+code+" is absent from $.categoryCodes")
				}
				startSample, err := convert.MSToSamples(startMS, params.SampleRate, ppath+".startMs", ledger)
				if err != nil {
					return nil, err
				}
				endSample, err := convert.MSToSamples(endMS, params.SampleRate, ppath+".endMs", ledger)
				if err != nil {
					return nil, err
				}
				if err := convert.CheckWeightRange(int(weight), 1000, ppath+".weightPermille"); err != nil {
					return nil, err
				}
				weight10k := convert.PermilleToPermyriad(int(weight), ppath+".weightPermille", ledger)
				groupPath := ppath + ".categoryGroup"
				fill(&report, groupPath, nil, DefaultFlatGroup, "v1 has a flat category table; assign upgraded group")
				affinityPath := ppath + ".transitionAffinity"
				fill(&report, affinityPath, nil, DefaultUpgradedAffinity, "v1 has no transition affinity; use declared upgrade default")
				shard.Phonemes = append(shard.Phonemes, domain.Phoneme{
					ID: pid, DialogueID: did, ShardID: sid, Index: int(index),
					StartSample: startSample, EndSample: endSample,
					Category: domain.Category(categoryText), CategoryCode: code,
					EncodingGroup: DefaultFlatGroup, WeightPermyriad: weight10k,
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

	if shadow != nil {
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

	report.Conversions = ledger.Records
	return &LoadResult{Project: project, Report: report, Shadow: shadow}, nil
}

func optionalString(m map[string]json.RawMessage, name, def string) string {
	raw, ok := m[name]
	if !ok {
		return def
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}
