package lipsync

import (
	"fmt"
)

type Edit struct {
	DialogueID    string `json:"dialogueId,omitempty"`
	Field         string `json:"field"`
	PhonemeIndex  int    `json:"phonemeIndex,omitempty"`
	BoundaryIndex int    `json:"boundaryIndex,omitempty"`
	IntValue      int    `json:"intValue"`
	RecipeVersion string `json:"recipeVersion,omitempty"`
	Description   string `json:"description,omitempty"`
}

func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Recipe = p.Recipe
	clone.Dialogues = make([]*Dialogue, len(p.Dialogues))
	for i, d := range p.Dialogues {
		if d == nil {
			continue
		}
		dc := *d
		dc.PCM = d.PCM
		if d.PCM.Samples != nil {
			dc.PCM.Samples = append([]int16(nil), d.PCM.Samples...)
		}
		dc.Phonemes = append([]Phoneme(nil), d.Phonemes...)
		for j := range dc.Phonemes {
			if d.Phonemes[j].AffinityToNext != nil {
				value := *d.Phonemes[j].AffinityToNext
				dc.Phonemes[j].AffinityToNext = &value
			}
		}
		clone.Dialogues[i] = &dc
	}
	return &clone
}

func EditProject(base *Project, edit Edit) (*Project, error) {
	p := CloneProject(base)
	if p == nil {
		return nil, SchemaError("project", "cannot edit nil project")
	}
	recipeEdit := false
	if edit.DialogueID == "" || len(edit.Field) >= 7 && edit.Field[:7] == "recipe." {
		if err := applyRecipeEdit(p, edit); err != nil {
			return nil, err
		}
		recipeEdit = true
	} else {
		d := findDialogue(p, edit.DialogueID)
		if d == nil {
			return nil, SchemaError("dialogueId", "dialogue %q not found", edit.DialogueID)
		}
		if err := applyPhonemeEdit(d, edit); err != nil {
			return nil, err
		}
	}
	if recipeEdit && edit.RecipeVersion != "" {
		p.Recipe.RecipeVersion = edit.RecipeVersion
	}
	EnsureDialogueHashes(p)
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func findDialogue(p *Project, id string) *Dialogue {
	for _, d := range p.Dialogues {
		if d != nil && d.ID == id {
			return d
		}
	}
	return nil
}

func FindDialogue(p *Project, id string) *Dialogue {
	return findDialogue(p, id)
}

func applyPhonemeEdit(d *Dialogue, edit Edit) error {
	idx := edit.PhonemeIndex
	switch edit.Field {
	case "sharedBoundary":
		b := edit.BoundaryIndex
		if b == 0 && edit.PhonemeIndex != 0 {
			b = edit.PhonemeIndex
		}
		if b < 0 || b >= len(d.Phonemes)-1 {
			return DialogueError(ECSchema, d, "boundaryIndex",
				"shared boundary %d outside [0,%d]", b, len(d.Phonemes)-2)
		}
		if edit.IntValue <= d.Phonemes[b].StartSample || edit.IntValue >= d.Phonemes[b+1].EndSample {
			return DialogueError(ECOrder, d, fmt.Sprintf("phonemes[%d].sharedBoundary", b),
				"boundary sample %d must be inside [%d,%d]", edit.IntValue,
				d.Phonemes[b].StartSample, d.Phonemes[b+1].EndSample)
		}
		d.Phonemes[b].EndSample = edit.IntValue
		d.Phonemes[b+1].StartSample = edit.IntValue
	case "startSample":
		if idx < 0 || idx >= len(d.Phonemes) {
			return DialogueError(ECSchema, d, "phonemeIndex", "phoneme index %d out of range", idx)
		}
		d.Phonemes[idx].StartSample = edit.IntValue
	case "endSample":
		if idx < 0 || idx >= len(d.Phonemes) {
			return DialogueError(ECSchema, d, "phonemeIndex", "phoneme index %d out of range", idx)
		}
		d.Phonemes[idx].EndSample = edit.IntValue
	case "weight":
		if idx < 0 || idx >= len(d.Phonemes) {
			return DialogueError(ECSchema, d, "phonemeIndex", "phoneme index %d out of range", idx)
		}
		d.Phonemes[idx].Weight = edit.IntValue
	case "affinityToNext":
		if idx < 0 || idx >= len(d.Phonemes)-1 {
			return DialogueError(ECSchema, d, "phonemeIndex", "affinity boundary index %d out of range", idx)
		}
		value := edit.IntValue
		d.Phonemes[idx].AffinityToNext = &value
	default:
		return DialogueError(ECSchema, d, "field", "unsupported edit field %q", edit.Field)
	}
	return nil
}

func applyRecipeEdit(p *Project, edit Edit) error {
	r := &p.Recipe
	switch edit.Field {
	case "recipe.fps":
		r.FPS = edit.IntValue
	case "recipe.maxPhonemesPerPack":
		r.MaxPhonemesPerPack = edit.IntValue
	case "recipe.minVisibleFrames":
		r.MinVisibleFrames = edit.IntValue
	case "recipe.speakerChannels":
		r.SpeakerChannels = edit.IntValue
	case "recipe.framePackBudget":
		r.FramePackBudget = edit.IntValue
	case "recipe.maxDeferFrames":
		r.MaxDeferFrames = edit.IntValue
	case "recipe.mergeAffinityThreshold":
		r.MergeAffinityThreshold = edit.IntValue
	case "recipe.recipeVersion":
		r.RecipeVersion = edit.RecipeVersion
		return nil
	default:
		return SchemaError("field", "unsupported recipe edit field %q", edit.Field)
	}
	if edit.RecipeVersion != "" {
		r.RecipeVersion = edit.RecipeVersion
	} else {
		r.RecipeVersion = fmt.Sprintf("%s+%s=%d", r.RecipeVersion, edit.Field, edit.IntValue)
	}
	return nil
}
