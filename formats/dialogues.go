package formats

import (
	"encoding/json"
	"fmt"

	"lipsync/domain"
	"lipsync/units"
)

type dialogueDialect struct {
	Version      string
	DialogueKeys map[string]string
	PCMKeys      map[string]string
	PhonemeKeys  map[string]string
	TimeBase     string
	CostScale    string
}

func dialectFor(version string) dialogueDialect {
	switch version {
	case "v1":
		return dialogueDialect{
			Version: version,
			DialogueKeys: map[string]string{
				"id": "dialogue_id", "channel": "channel_id", "voice": "voice_id",
				"priority": "priority", "start": "start_sample",
			},
			PCMKeys: map[string]string{
				"sampleRate": "sample_rate", "samples": "samples", "duration": "duration_ms",
				"channels": "channels",
			},
			PhonemeKeys: map[string]string{
				"id": "phoneme_id", "symbol": "symbol", "start": "start_sample",
				"duration": "duration_samples", "cost": "cost_permille",
			},
			TimeBase:  domain.TimeBaseSamples,
			CostScale: domain.BudgetScalePermille,
		}
	case "v2":
		return dialogueDialect{
			Version: version,
			DialogueKeys: map[string]string{
				"id": "id", "channel": "channelId", "voice": "voiceId",
				"priority": "priority", "start": "startMs",
			},
			PCMKeys: map[string]string{
				"sampleRate": "sampleRate", "samples": "samples", "duration": "durationMs",
				"channels": "channels",
			},
			PhonemeKeys: map[string]string{
				"id": "id", "symbol": "symbol", "start": "startMs",
				"duration": "durationMs", "cost": "costBasisPoints",
			},
			TimeBase:  domain.TimeBaseMillis,
			CostScale: domain.BudgetScaleBasisPoint,
		}
	default:
		return dialogueDialect{
			Version: version,
			DialogueKeys: map[string]string{
				"id": "dialogue_id", "channel": "channel_id", "voice": "voice_id",
				"priority": "priority", "start": "start_sample",
			},
			PCMKeys: map[string]string{
				"sampleRate": "sample_rate", "samples": "samples", "duration": "duration_ms",
				"channels": "channels",
			},
			PhonemeKeys: map[string]string{
				"id": "phoneme_id", "symbol": "symbol", "start": "start_sample",
				"duration": "duration_samples", "cost": "cost_basis_points",
			},
			TimeBase:  domain.TimeBaseSamples,
			CostScale: domain.BudgetScaleBasisPoint,
		}
	}
}

func parseDialogues(top object, version string, projectSampleRate, frameRate int64, rounding units.Rounding, channels []domain.Channel) ([]domain.Dialogue, []domain.Truncation, error) {
	raw, err := require(top, "dialogues", "dialogues")
	if err != nil {
		return nil, nil, err
	}
	items, err := asArray(raw, "dialogues")
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, domain.NewError(domain.CodeSchema, "dialogues", "at least one dialogue is required")
	}
	dialect := dialectFor(version)
	channelSet := make(map[string]bool, len(channels))
	for _, channel := range channels {
		channelSet[channel.ID] = true
	}
	seenDialogues := map[string]bool{}
	seenPacketIDs := map[string]bool{}
	var dialogues []domain.Dialogue
	var truncations []domain.Truncation
	for i, item := range items {
		path := fmt.Sprintf("dialogues[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, nil, err
		}
		id, err := requiredString(obj, dialect.DialogueKeys["id"], path+"."+dialect.DialogueKeys["id"])
		if err != nil {
			return nil, nil, err
		}
		if seenDialogues[id] {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + "." + dialect.DialogueKeys["id"], Message: "duplicate dialogue id"}
		}
		seenDialogues[id] = true
		channelID, err := requiredString(obj, dialect.DialogueKeys["channel"], path+"."+dialect.DialogueKeys["channel"])
		if err != nil {
			return nil, nil, err
		}
		if err := validateDialogueChannel(channelID, channelSet, path+"."+dialect.DialogueKeys["channel"]); err != nil {
			return nil, nil, err
		}
		voiceID, err := requiredString(obj, dialect.DialogueKeys["voice"], path+"."+dialect.DialogueKeys["voice"])
		if err != nil {
			return nil, nil, err
		}
		priority, err := requiredInt(obj, dialect.DialogueKeys["priority"], path+"."+dialect.DialogueKeys["priority"])
		if err != nil {
			return nil, nil, err
		}
		startSource, err := requiredInt(obj, dialect.DialogueKeys["start"], path+"."+dialect.DialogueKeys["start"])
		if err != nil {
			return nil, nil, err
		}
		if startSource < 0 {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + "." + dialect.DialogueKeys["start"], ChannelID: channelID, Message: "dialogue start cannot be negative"}
		}
		startConversion, err := sourceTimeToFrames(startSource, dialect.TimeBase, projectSampleRate, frameRate, rounding, path+"."+dialect.DialogueKeys["start"])
		if err != nil {
			return nil, nil, withChannel(err, channelID)
		}
		units.RecordTruncation(&truncations, path+"."+dialect.DialogueKeys["start"], dialect.TimeBase, "frames", startConversion, channelID, "")

		pcmRaw, err := require(obj, "pcm", path+".pcm")
		if err != nil {
			return nil, nil, err
		}
		pcmObj, err := asObject(pcmRaw, path+".pcm")
		if err != nil {
			return nil, nil, err
		}
		pcmSampleRate, err := requiredInt(pcmObj, dialect.PCMKeys["sampleRate"], path+".pcm."+dialect.PCMKeys["sampleRate"])
		if err != nil {
			return nil, nil, err
		}
		if pcmSampleRate != projectSampleRate {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".pcm." + dialect.PCMKeys["sampleRate"], ChannelID: channelID, Message: "PCM sample rate does not match project sample rate"}
		}
		samples, err := requiredInt(pcmObj, dialect.PCMKeys["samples"], path+".pcm."+dialect.PCMKeys["samples"])
		if err != nil {
			return nil, nil, err
		}
		durationMs, err := requiredInt(pcmObj, dialect.PCMKeys["duration"], path+".pcm."+dialect.PCMKeys["duration"])
		if err != nil {
			return nil, nil, err
		}
		pcmChannels, err := requiredInt(pcmObj, dialect.PCMKeys["channels"], path+".pcm."+dialect.PCMKeys["channels"])
		if err != nil {
			return nil, nil, err
		}
		if samples < 0 || durationMs < 0 || pcmChannels != 1 {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".pcm", ChannelID: channelID, Message: "PCM must be non-negative mono audio"}
		}

		phonemeRaw, err := require(obj, "phonemes", path+".phonemes")
		if err != nil {
			return nil, nil, err
		}
		phonemeItems, err := asArray(phonemeRaw, path+".phonemes")
		if err != nil {
			return nil, nil, err
		}
		if len(phonemeItems) == 0 {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".phonemes", ChannelID: channelID, Message: "at least one phoneme is required"}
		}
		phonemes := make([]domain.Phoneme, 0, len(phonemeItems))
		for pi, phonemeRaw := range phonemeItems {
			phonemePath := fmt.Sprintf("%s.phonemes[%d]", path, pi)
			phonemeObj, err := asObject(phonemeRaw, phonemePath)
			if err != nil {
				return nil, nil, err
			}
			phonemeID, err := requiredString(phonemeObj, dialect.PhonemeKeys["id"], phonemePath+"."+dialect.PhonemeKeys["id"])
			if err != nil {
				return nil, nil, err
			}
			if seenPacketIDs[phonemeID] {
				return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: phonemePath + "." + dialect.PhonemeKeys["id"], ChannelID: channelID, PacketID: phonemeID, Message: "duplicate phoneme/packet id"}
			}
			seenPacketIDs[phonemeID] = true
			symbol, err := requiredString(phonemeObj, dialect.PhonemeKeys["symbol"], phonemePath+"."+dialect.PhonemeKeys["symbol"])
			if err != nil {
				return nil, nil, err
			}
			phonemeStart, err := requiredInt(phonemeObj, dialect.PhonemeKeys["start"], phonemePath+"."+dialect.PhonemeKeys["start"])
			if err != nil {
				return nil, nil, err
			}
			phonemeDuration, err := requiredInt(phonemeObj, dialect.PhonemeKeys["duration"], phonemePath+"."+dialect.PhonemeKeys["duration"])
			if err != nil {
				return nil, nil, err
			}
			if phonemeStart < 0 || phonemeDuration <= 0 {
				return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: phonemePath, ChannelID: channelID, PacketID: phonemeID, Message: "phoneme start must be non-negative and duration must be positive"}
			}
			costSource, err := requiredInt(phonemeObj, dialect.PhonemeKeys["cost"], phonemePath+"."+dialect.PhonemeKeys["cost"])
			if err != nil {
				return nil, nil, err
			}
			if costSource <= 0 {
				return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: phonemePath + "." + dialect.PhonemeKeys["cost"], ChannelID: channelID, PacketID: phonemeID, Message: "packet cost must be positive"}
			}
			phonemeStartConv, err := sourceTimeToFrames(phonemeStart, dialect.TimeBase, projectSampleRate, frameRate, rounding, phonemePath+"."+dialect.PhonemeKeys["start"])
			if err != nil {
				return nil, nil, withChannel(err, channelID)
			}
			units.RecordTruncation(&truncations, phonemePath+"."+dialect.PhonemeKeys["start"], dialect.TimeBase, "frames", phonemeStartConv, channelID, phonemeID)
			phonemeDurationConv, err := sourceTimeToFrames(phonemeDuration, dialect.TimeBase, projectSampleRate, frameRate, rounding, phonemePath+"."+dialect.PhonemeKeys["duration"])
			if err != nil {
				return nil, nil, withChannel(err, channelID)
			}
			units.RecordTruncation(&truncations, phonemePath+"."+dialect.PhonemeKeys["duration"], dialect.TimeBase, "frames", phonemeDurationConv, channelID, phonemeID)
			if phonemeDurationConv.Output <= 0 {
				return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: phonemePath + "." + dialect.PhonemeKeys["duration"], ChannelID: channelID, PacketID: phonemeID, Message: "phoneme duration rounds to zero frames"}
			}
			costBP := costSource
			if dialect.CostScale == domain.BudgetScalePermille {
				costConv, err := units.PermilleToBasisPoints(costSource, rounding, phonemePath+"."+dialect.PhonemeKeys["cost"])
				if err != nil {
					return nil, nil, withChannel(err, channelID)
				}
				costBP = costConv.Output
				units.RecordTruncation(&truncations, phonemePath+"."+dialect.PhonemeKeys["cost"], domain.BudgetScalePermille, domain.BudgetScaleBasisPoint, costConv, channelID, phonemeID)
			}
			phoneme := domain.Phoneme{
				ID:              phonemeID,
				Symbol:          symbol,
				StartFrame:      phonemeStartConv.Output,
				DurationFrames:  phonemeDurationConv.Output,
				CostBasisPoints: costBP,
			}
			if dialect.TimeBase == domain.TimeBaseSamples {
				phoneme.StartSample = phonemeStart
				phoneme.DurationSamples = phonemeDuration
			} else {
				phoneme.StartMs = phonemeStart
				phoneme.DurationMs = phonemeDuration
			}
			phonemes = append(phonemes, phoneme)
		}
		dialogue := domain.Dialogue{
			ID:         id,
			ChannelID:  channelID,
			VoiceID:    voiceID,
			Priority:   int(priority),
			StartFrame: startConversion.Output,
			PCM: domain.PCMDescription{
				SampleRate: pcmSampleRate,
				Samples:    samples,
				DurationMs: durationMs,
				Channels:   int(pcmChannels),
			},
			Phonemes: phonemes,
		}
		dialogues = append(dialogues, dialogue)
	}
	return dialogues, truncations, nil
}

func sourceTimeToFrames(value int64, timeBase string, sampleRate, frameRate int64, rounding units.Rounding, field string) (units.Conversion, error) {
	if timeBase == domain.TimeBaseMillis {
		return units.MillisToFrames(value, frameRate, rounding, field)
	}
	return units.SamplesToFrames(value, sampleRate, frameRate, rounding, field)
}

func parseRecoveryV2(top object) (domain.RecoveryPolicy, []domain.DefaultFill, error) {
	policy := domain.RecoveryPolicy{
		Strategy:            domain.StrategyNextFrame,
		RecoveryOffsetFrame: 1,
		MinContinuousFrames: 2,
		AllowAbandon:        true,
		WindowFrames:        4,
	}
	var fills []domain.DefaultFill
	raw, exists := top["preemption"]
	if !exists || string(raw) == "null" {
		defaults := []struct {
			path  string
			value any
		}{
			{"preemption.strategy", policy.Strategy},
			{"preemption.recovery_offset_frames", policy.RecoveryOffsetFrame},
			{"preemption.min_continuous_frames", policy.MinContinuousFrames},
			{"preemption.allow_abandon", policy.AllowAbandon},
			{"preemption.window_frames", policy.WindowFrames},
		}
		for _, item := range defaults {
			fills = append(fills, domain.DefaultFill{Path: item.path, OldValue: nil, DefaultValue: item.value, Source: "protocol:v2-default", Version: "v2"})
		}
		return policy, fills, validateRecovery(policy, "preemption")
	}
	obj, err := asObject(raw, "preemption")
	if err != nil {
		return policy, nil, err
	}
	if value, ok := obj["strategy"]; ok {
		if err := json.Unmarshal(value, &policy.Strategy); err != nil {
			return policy, nil, &domain.Error{Code: domain.CodeSchema, Path: "preemption.strategy", Message: "expected string"}
		}
	} else {
		fills = append(fills, domain.DefaultFill{Path: "preemption.strategy", OldValue: nil, DefaultValue: policy.Strategy, Source: "protocol:v2-default", Version: "v2"})
	}
	if value, set, err := optionalInt(obj, "minContinuousFrames", "preemption.minContinuousFrames", int64(policy.MinContinuousFrames)); err != nil {
		return policy, nil, err
	} else if set {
		policy.MinContinuousFrames = int(value)
	} else {
		fills = append(fills, domain.DefaultFill{Path: "preemption.min_continuous_frames", OldValue: nil, DefaultValue: policy.MinContinuousFrames, Source: "protocol:v2-default", Version: "v2"})
	}
	if value, set, err := optionalInt(obj, "windowFrames", "preemption.windowFrames", policy.WindowFrames); err != nil {
		return policy, nil, err
	} else if set {
		policy.WindowFrames = value
	} else {
		fills = append(fills, domain.DefaultFill{Path: "preemption.window_frames", OldValue: nil, DefaultValue: policy.WindowFrames, Source: "protocol:v2-default", Version: "v2"})
	}
	if value, set, err := optionalInt(obj, "recoveryOffsetFrames", "preemption.recoveryOffsetFrames", policy.RecoveryOffsetFrame); err != nil {
		return policy, nil, err
	} else if set {
		policy.RecoveryOffsetFrame = value
	} else {
		fills = append(fills, domain.DefaultFill{Path: "preemption.recovery_offset_frames", OldValue: nil, DefaultValue: policy.RecoveryOffsetFrame, Source: "protocol:v2-default", Version: "v2"})
	}
	if value, set, err := optionalBool(obj, "allowAbandon", "preemption.allowAbandon", policy.AllowAbandon); err != nil {
		return policy, nil, err
	} else if set {
		policy.AllowAbandon = value
	} else {
		fills = append(fills, domain.DefaultFill{Path: "preemption.allow_abandon", OldValue: nil, DefaultValue: policy.AllowAbandon, Source: "protocol:v2-default", Version: "v2"})
	}
	return policy, fills, validateRecovery(policy, "preemption")
}

func parseRecoveryV3(top object) (domain.RecoveryPolicy, error) {
	raw, err := require(top, "preemption", "preemption")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	obj, err := asObject(raw, "preemption")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	strategy, err := requiredString(obj, "strategy", "preemption.strategy")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	offset, err := requiredInt(obj, "recovery_offset_frames", "preemption.recovery_offset_frames")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	minRun, err := requiredInt(obj, "min_continuous_frames", "preemption.min_continuous_frames")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	allowAbandon, err := requiredBool(obj, "allow_abandon", "preemption.allow_abandon")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	window, err := requiredInt(obj, "window_frames", "preemption.window_frames")
	if err != nil {
		return domain.RecoveryPolicy{}, err
	}
	policy := domain.RecoveryPolicy{
		Strategy:            strategy,
		RecoveryOffsetFrame: offset,
		MinContinuousFrames: int(minRun),
		AllowAbandon:        allowAbandon,
		WindowFrames:        window,
	}
	if err := validateRecovery(policy, "preemption"); err != nil {
		return domain.RecoveryPolicy{}, err
	}
	return policy, nil
}

func recoveryDefaultsV1() (domain.RecoveryPolicy, []domain.DefaultFill) {
	policy := domain.RecoveryPolicy{
		Strategy:            domain.StrategyNextFrame,
		RecoveryOffsetFrame: 1,
		MinContinuousFrames: 2,
		AllowAbandon:        true,
		WindowFrames:        4,
	}
	paths := []struct {
		path  string
		value any
	}{
		{"preemption.strategy", policy.Strategy},
		{"preemption.recovery_offset_frames", policy.RecoveryOffsetFrame},
		{"preemption.min_continuous_frames", policy.MinContinuousFrames},
		{"preemption.allow_abandon", policy.AllowAbandon},
		{"preemption.window_frames", policy.WindowFrames},
	}
	fills := make([]domain.DefaultFill, 0, len(paths))
	for _, item := range paths {
		fills = append(fills, domain.DefaultFill{
			Path: item.path, OldValue: nil, DefaultValue: item.value,
			Source: "protocol:v1-default", Version: "v1",
		})
	}
	return policy, fills
}
