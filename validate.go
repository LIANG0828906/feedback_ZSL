package lipsync

import (
	"encoding/json"
	"fmt"
)

func NormalizeProject(p *Project) {
	if p == nil {
		return
	}
	if p.AlgorithmVersion == "" {
		p.AlgorithmVersion = AlgorithmVersion
	}
	if p.Recipe.RecipeVersion == "" {
		p.Recipe.RecipeVersion = "recipe-v1"
	}
	if p.Recipe.Quantization == "" {
		p.Recipe.Quantization = QuantizeDown
	}
	if p.Recipe.MaxDeferFrames == 0 {
		p.Recipe.MaxDeferFrames = 8
	}
	for _, d := range p.Dialogues {
		if d == nil {
			continue
		}
		if d.PCM.Channels == 0 {
			d.PCM.Channels = 1
		}
		for i := range d.Phonemes {
			if d.Phonemes[i].Index == 0 && i > 0 {
				d.Phonemes[i].Index = i
			}
			if d.Phonemes[i].WeightUnit == "" {
				d.Phonemes[i].WeightUnit = WeightPermille
			}
		}
	}
}

func ValidateProject(p *Project) error {
	if p == nil {
		return SchemaError("project", "project is nil")
	}
	NormalizeProject(p)
	if p.ID == "" {
		return SchemaError("id", "project id is required")
	}
	r := p.Recipe
	if r.FPS <= 0 {
		return SchemaError("recipe.fps", "target fps must be positive, got %d", r.FPS)
	}
	if r.SpeakerChannels <= 0 {
		return SchemaError("recipe.speakerChannels", "speaker channel cap must be positive, got %d", r.SpeakerChannels)
	}
	if r.FramePackBudget < 0 {
		return SchemaError("recipe.framePackBudget", "frame pack budget must be non-negative, got %d", r.FramePackBudget)
	}
	if r.MaxPhonemesPerPack <= 0 {
		return SchemaError("recipe.maxPhonemesPerPack", "max phonemes per pack must be positive, got %d", r.MaxPhonemesPerPack)
	}
	if r.MinVisibleFrames <= 0 {
		return SchemaError("recipe.minVisibleFrames", "minimum visible frames must be positive, got %d", r.MinVisibleFrames)
	}
	if r.MergeAffinityThreshold < 0 || r.MergeAffinityThreshold > 10000 {
		return SchemaError("recipe.mergeAffinityThreshold", "affinity threshold must be in [0,10000], got %d", r.MergeAffinityThreshold)
	}
	if r.MaxDeferFrames < 0 {
		return SchemaError("recipe.maxDeferFrames", "max defer frames must be non-negative, got %d", r.MaxDeferFrames)
	}
	switch r.Quantization {
	case QuantizeDown, QuantizeUp, QuantizeNearest:
	default:
		return SchemaError("recipe.quantization", "unsupported quantization direction %q", r.Quantization)
	}
	if len(p.Dialogues) == 0 {
		return SchemaError("dialogues", "at least one dialogue is required")
	}

	seenShards := map[int]string{}
	for di, d := range p.Dialogues {
		if d == nil {
			return SchemaError(fmt.Sprintf("dialogues[%d]", di), "dialogue is nil")
		}
		if d.ID == "" {
			return DialogueError(ECSchema, d, fmt.Sprintf("dialogues[%d].id", di), "dialogue id is required")
		}
		if priorID, dup := seenShards[d.ShardIndex]; dup {
			return DialogueError(ECSchema, d, "shardIndex",
				"shard index %d already assigned to dialogue %q", d.ShardIndex, priorID)
		}
		seenShards[d.ShardIndex] = d.ID
		if d.PCM.Channels != 1 {
			return DialogueError(ECSchema, d, "pcm.channels", "generator requires mono PCM, got %d channels", d.PCM.Channels)
		}
		if d.PCM.SampleRate <= 0 {
			return DialogueError(ECSchema, d, "pcm.sampleRate", "sample rate must be positive, got %d", d.PCM.SampleRate)
		}
		if d.PCM.SampleCount <= 0 {
			return DialogueError(ECSchema, d, "pcm.sampleCount", "sample count must be positive, got %d", d.PCM.SampleCount)
		}
		if len(d.Phonemes) == 0 {
			return DialogueError(ECSchema, d, "phonemes", "at least one phoneme is required")
		}

		for i, ph := range d.Phonemes {
			path := fmt.Sprintf("phonemes[%d]", i)
			if ph.ID == "" {
				return DialogueError(ECSchema, d, path+".id", "phoneme id is required")
			}
			if ph.StartSample < 0 {
				return DialogueError(ECSchema, d, path+".startSample", "start sample must be non-negative")
			}
			if ph.EndSample > d.PCM.SampleCount {
				return DialogueError(ECSchema, d, path+".endSample",
					"end sample %d exceeds PCM sample count %d", ph.EndSample, d.PCM.SampleCount)
			}
			if ph.EndSample <= ph.StartSample {
				return DialogueError(ECOrder, d, path+".endSample",
					"phoneme %q has inverted/empty interval [%d,%d)", ph.ID, ph.StartSample, ph.EndSample)
			}
			if i > 0 {
				prev := d.Phonemes[i-1]
				if ph.Index < prev.Index {
					return DialogueError(ECOrder, d, path+".index",
						"phoneme index decreases after %q", prev.ID)
				}
				if ph.StartSample < prev.EndSample || ph.StartSample <= prev.StartSample {
					return DialogueError(ECOrder, d, path+".startSample",
						"phoneme %q interval [%d,%d) overlaps or is not strictly after %q [%d,%d)",
						ph.ID, ph.StartSample, ph.EndSample, prev.ID, prev.StartSample, prev.EndSample)
				}
			}
			if ph.Class == "" {
				return DialogueError(ECSchema, d, path+".class", "phoneme class is required")
			}
			switch ph.Class {
			case ClassVowel, ClassConsonant, ClassSilence:
			default:
				return DialogueError(ECLabel, d, path+".class",
					"phoneme %q has unsupported class %q", ph.ID, ph.Class)
			}
			if ph.Weight <= 0 {
				return DialogueError(ECSchema, d, path+".weight", "weight must be positive, got %d", ph.Weight)
			}
			switch ph.WeightUnit {
			case WeightPermille, WeightPermyriad:
			default:
				return DialogueError(ECSchema, d, path+".weightUnit", "unsupported weight unit %q", ph.WeightUnit)
			}
			if _, _, _, err := NormalizeWeight(ph.Weight, ph.WeightUnit); err != nil {
				return DialogueError(ECSchema, d, path+".weight", "%v", err)
			}
			if ph.AffinityToNext != nil && (*ph.AffinityToNext < 0 || *ph.AffinityToNext > 10000) {
				return DialogueError(ECSchema, d, path+".affinityToNext",
					"affinity must be in [0,10000], got %d", *ph.AffinityToNext)
			}
		}
	}
	return nil
}

func LoadProjectJSON(data []byte) (*Project, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, SchemaError("$", "invalid JSON: %v", err)
	}
	if err := requireFields(raw, "$", "id", "seed", "recipe", "dialogues"); err != nil {
		return nil, err
	}
	var recipe map[string]json.RawMessage
	if err := json.Unmarshal(raw["recipe"], &recipe); err != nil {
		return nil, SchemaError("recipe", "recipe must be an object: %v", err)
	}
	if err := requireFields(recipe, "recipe",
		"fps", "maxPhonemesPerPack", "minVisibleFrames", "speakerChannels",
		"framePackBudget", "quantization", "mergeAffinityThreshold",
		"maxDeferFrames", "recipeVersion"); err != nil {
		return nil, err
	}
	var rawDialogues []map[string]json.RawMessage
	if err := json.Unmarshal(raw["dialogues"], &rawDialogues); err != nil {
		return nil, SchemaError("dialogues", "dialogues must be an array of objects: %v", err)
	}
	for i, rd := range rawDialogues {
		dialogueID := rawString(rd["id"])
		dpath := fmt.Sprintf("dialogues[%d]", i)
		if err := requireFields(rd, dpath, "id", "shardIndex", "pcm", "phonemes"); err != nil {
			if domainErr, ok := AsError(err); ok && dialogueID != "" {
				domainErr.DialogueID = dialogueID
			}
			return nil, err
		}
		var pcm map[string]json.RawMessage
		if err := json.Unmarshal(rd["pcm"], &pcm); err != nil {
			return nil, DialogueError(ECSchema, &Dialogue{ID: dialogueID}, dpath+".pcm", "pcm must be an object")
		}
		if err := requireFields(pcm, dpath+".pcm", "encoding", "channels", "sampleRate", "sampleCount", "seed"); err != nil {
			if domainErr, ok := AsError(err); ok && dialogueID != "" {
				domainErr.DialogueID = dialogueID
			}
			return nil, err
		}
		var phonemes []map[string]json.RawMessage
		if err := json.Unmarshal(rd["phonemes"], &phonemes); err != nil {
			return nil, DialogueError(ECSchema, &Dialogue{ID: dialogueID}, dpath+".phonemes", "phonemes must be an array")
		}
		for j, ph := range phonemes {
			ppath := fmt.Sprintf("%s.phonemes[%d]", dpath, j)
			if err := requireFields(ph, ppath, "id", "index", "startSample", "endSample",
				"class", "weight", "weightUnit"); err != nil {
				if domainErr, ok := AsError(err); ok && dialogueID != "" {
					domainErr.DialogueID = dialogueID
				}
				return nil, err
			}
		}
	}

	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, SchemaError("$", "decode project: %v", err)
	}
	if err := ValidateProject(&p); err != nil {
		return nil, err
	}
	EnsureDialogueHashes(&p)
	return &p, nil
}

func requireFields(obj map[string]json.RawMessage, path string, fields ...string) error {
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok || string(raw) == "null" {
			return SchemaError(path+"."+field, "missing required field %q", field)
		}
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}
