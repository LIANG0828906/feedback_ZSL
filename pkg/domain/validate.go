package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// LoadProject decodes and validates a dialogue project. Malformed JSON maps to
// E_DECODE; missing or structurally invalid fields map to E_SCHEMA; ordering
// violations map to E_ORDER.
func LoadProject(data []byte) (*Project, error) {
	if !json.Valid(data) {
		return nil, &Error{Code: ErrDecode, Reason: "malformed JSON"}
	}
	if err := checkProjectShape(data); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var project Project
	if err := dec.Decode(&project); err != nil {
		return nil, &Error{Code: ErrSchema, Reason: "decode schema: " + err.Error(), Cause: err}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, &Error{Code: ErrDecode, Reason: "trailing JSON value"}
		}
		return nil, &Error{Code: ErrDecode, Reason: "trailing JSON: " + err.Error(), Cause: err}
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

func checkProjectShape(data []byte) error {
	root, err := object(data, "$")
	if err != nil {
		return err
	}
	if err := requireKeys(root, "$", "version", "seed", "recipe", "dialogues"); err != nil {
		return err
	}
	recipe, err := object(root["recipe"], "$.recipe")
	if err != nil {
		return err
	}
	if err := requireKeys(recipe, "$.recipe", "version", "frame_rate", "quantize",
		"affinity_threshold", "transition_tolerance", "max_phonemes_per_package",
		"max_packages_per_frame", "max_packages_per_channel_frame", "max_defer_frames",
		"algorithm_version", "channels"); err != nil {
		return err
	}
	var channels []json.RawMessage
	if err := json.Unmarshal(recipe["channels"], &channels); err != nil {
		return &Error{Code: ErrSchema, Path: "$.recipe.channels", Reason: "must be an array", Cause: err}
	}
	for i, raw := range channels {
		obj, err := object(raw, fmt.Sprintf("$.recipe.channels[%d]", i))
		if err != nil {
			return err
		}
		if err := requireKeys(obj, fmt.Sprintf("$.recipe.channels[%d]", i), "id", "slots"); err != nil {
			return err
		}
	}
	var dialogues []json.RawMessage
	if err := json.Unmarshal(root["dialogues"], &dialogues); err != nil {
		return &Error{Code: ErrSchema, Path: "$.dialogues", Reason: "must be an array", Cause: err}
	}
	for i, raw := range dialogues {
		base := fmt.Sprintf("$.dialogues[%d]", i)
		obj, err := object(raw, base)
		if err != nil {
			return err
		}
		if err := requireKeys(obj, base, "id", "channel", "pcm", "phonemes"); err != nil {
			return err
		}
		var dialogueID string
		_ = json.Unmarshal(obj["id"], &dialogueID)
		pcm, err := object(obj["pcm"], base+".pcm")
		if err != nil {
			return err
		}
		if err := requireKeys(pcm, base+".pcm", "format", "sample_rate", "bits_per_sample", "channels", "sample_count"); err != nil {
			return err
		}
		var phonemes []json.RawMessage
		if err := json.Unmarshal(obj["phonemes"], &phonemes); err != nil {
			return &Error{Code: ErrSchema, Path: base + ".phonemes", Reason: "must be an array", Cause: err}
		}
		for j, praw := range phonemes {
			pbase := fmt.Sprintf("%s.phonemes[%d]", base, j)
			pobj, err := object(praw, pbase)
			if err != nil {
				return err
			}
			var phonemeID string
			_ = json.Unmarshal(pobj["id"], &phonemeID)
			if err := requireKeys(pobj, pbase, "id", "start_sample", "end_sample",
				"category", "weight_permille", "affinity_to_next"); err != nil {
				var de *Error
				if asError(err, &de) {
					de.PhonemeID = phonemeID
					de.DialogueID = dialogueID
				}
				return err
			}
		}
	}
	return nil
}

func asError(err error, target **Error) bool {
	de, ok := err.(*Error)
	if ok {
		*target = de
	}
	return ok
}

func object(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if len(raw) == 0 {
		return nil, &Error{Code: ErrSchema, Path: path, Reason: "must be an object"}
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &Error{Code: ErrSchema, Path: path, Reason: "must be an object"}
	}
	return obj, nil
}

func requireKeys(obj map[string]json.RawMessage, path string, keys ...string) error {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			return &Error{Code: ErrSchema, Path: path + "." + key, Reason: "required field missing"}
		}
	}
	return nil
}

func ValidateProject(p *Project) error {
	if p == nil {
		return &Error{Code: ErrSchema, Path: "$", Reason: "project is nil"}
	}
	if p.Version != 1 {
		return &Error{Code: ErrSchema, Path: "$.version", Reason: "unsupported schema version"}
	}
	if err := ValidateRecipe(&p.Recipe); err != nil {
		return err
	}
	if len(p.Dialogues) == 0 {
		return &Error{Code: ErrSchema, Path: "$.dialogues", Reason: "at least one dialogue required"}
	}
	channels := make(map[string]Channel, len(p.Recipe.Channels))
	for _, ch := range p.Recipe.Channels {
		channels[ch.ID] = ch
	}
	seenDialogue := make(map[string]struct{}, len(p.Dialogues))
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		base := fmt.Sprintf("$.dialogues[%d]", i)
		if d.ID == "" {
			return &Error{Code: ErrSchema, Path: base + ".id", Reason: "required field missing"}
		}
		if _, ok := seenDialogue[d.ID]; ok {
			return &Error{Code: ErrSchema, Path: base + ".id", DialogueID: d.ID, Reason: "duplicate dialogue id"}
		}
		seenDialogue[d.ID] = struct{}{}
		if _, ok := channels[d.Channel]; !ok {
			return &Error{Code: ErrPara, Path: base + ".channel", DialogueID: d.ID, Field: "channel", Reason: "unknown channel"}
		}
		if err := validatePCM(d.PCM, base+".pcm", d.ID); err != nil {
			return err
		}
		if len(d.Phonemes) == 0 {
			return &Error{Code: ErrSchema, Path: base + ".phonemes", DialogueID: d.ID, Reason: "at least one phoneme required"}
		}
		seenPhoneme := make(map[string]struct{}, len(d.Phonemes))
		var previousEnd int64
		var previousID string
		for j := range d.Phonemes {
			ph := &d.Phonemes[j]
			pbase := fmt.Sprintf("%s.phonemes[%d]", base, j)
			if ph.ID == "" {
				return &Error{Code: ErrSchema, Path: pbase + ".id", DialogueID: d.ID, Reason: "required field missing"}
			}
			if _, ok := seenPhoneme[ph.ID]; ok {
				return &Error{Code: ErrSchema, Path: pbase + ".id", DialogueID: d.ID, PhonemeID: ph.ID, Reason: "duplicate phoneme id"}
			}
			seenPhoneme[ph.ID] = struct{}{}
			if !CategoryAllowed(ph.Category) {
				return &Error{Code: ErrSchema, Path: pbase + ".category", DialogueID: d.ID, PhonemeID: ph.ID, Reason: "category not in {vowel,consonant,silence}"}
			}
			if ph.WeightPermille <= 0 {
				return &Error{Code: ErrSchema, Path: pbase + ".weight_permille", DialogueID: d.ID, PhonemeID: ph.ID, Reason: "weight must be positive"}
			}
			if ph.StartSample < 0 || ph.EndSample <= ph.StartSample {
				return &Error{Code: ErrOrder, Path: pbase, DialogueID: d.ID, PhonemeID: ph.ID, PhonemeA: previousID, PhonemeB: ph.ID, Reason: "phoneme interval must be positive and increasing"}
			}
			if ph.EndSample > d.PCM.SampleCount {
				return &Error{Code: ErrOrder, Path: pbase + ".end_sample", DialogueID: d.ID, PhonemeID: ph.ID, Reason: "phoneme exceeds PCM sample count"}
			}
			if j > 0 && ph.StartSample < previousEnd {
				return &Error{Code: ErrOrder, Path: pbase + ".start_sample", DialogueID: d.ID, PhonemeID: ph.ID, PhonemeA: previousID, PhonemeB: ph.ID, Reason: "phoneme intervals overlap or are not strictly increasing"}
			}
			if ph.AffinityToNext < 0 || ph.AffinityToNext > 1000 {
				return &Error{Code: ErrPara, Path: pbase + ".affinity_to_next", DialogueID: d.ID, PhonemeA: ph.ID, PhonemeB: nextID(d.Phonemes, j), Field: "affinity_to_next", Reason: "must be in [0,1000]"}
			}
			previousEnd = ph.EndSample
			previousID = ph.ID
		}
	}
	return nil
}

func ValidateRecipe(r *Recipe) error {
	if r.Version == "" {
		return &Error{Code: ErrSchema, Path: "$.recipe.version", Reason: "required field missing"}
	}
	if r.AlgorithmVersion == "" {
		return &Error{Code: ErrSchema, Path: "$.recipe.algorithm_version", Reason: "required field missing"}
	}
	if _, ok := SupportedFrameRates[r.FrameRate]; !ok {
		return &Error{Code: ErrRate, Path: "$.recipe.frame_rate", Field: "frame_rate", Reason: "unsupported frame rate"}
	}
	if !QuantizeAllowed(r.Quantize) {
		return &Error{Code: ErrSchema, Path: "$.recipe.quantize", Field: "quantize", Reason: "must be floor, ceil or nearest"}
	}
	if r.AffinityThreshold < 0 || r.AffinityThreshold > 1000 {
		return &Error{Code: ErrPara, Path: "$.recipe.affinity_threshold", Field: "affinity_threshold", Reason: "must be in [0,1000]"}
	}
	if r.TransitionTolerance < 1 || r.TransitionTolerance > 16 {
		return &Error{Code: ErrPara, Path: "$.recipe.transition_tolerance", Field: "transition_tolerance", Reason: "must be in [1,16]"}
	}
	if r.MaxPhonemesPerPackage < 1 || r.MaxPhonemesPerPackage > 32 {
		return &Error{Code: ErrPara, Path: "$.recipe.max_phonemes_per_package", Field: "max_phonemes_per_package", Reason: "must be in [1,32]"}
	}
	if r.MaxPackagesPerFrame < 1 {
		return &Error{Code: ErrPara, Path: "$.recipe.max_packages_per_frame", Field: "max_packages_per_frame", Reason: "must be positive"}
	}
	if r.MaxPackagesPerChannelFrame < 1 {
		return &Error{Code: ErrPara, Path: "$.recipe.max_packages_per_channel_frame", Field: "max_packages_per_channel_frame", Reason: "must be positive"}
	}
	if r.MaxDeferFrames < 0 || r.MaxDeferFrames > 100000 {
		return &Error{Code: ErrPara, Path: "$.recipe.max_defer_frames", Field: "max_defer_frames", Reason: "must be in [0,100000]"}
	}
	if len(r.Channels) == 0 {
		return &Error{Code: ErrSchema, Path: "$.recipe.channels", Reason: "at least one channel required"}
	}
	seen := make(map[string]struct{}, len(r.Channels))
	for i, ch := range r.Channels {
		base := fmt.Sprintf("$.recipe.channels[%d]", i)
		if ch.ID == "" {
			return &Error{Code: ErrSchema, Path: base + ".id", Reason: "required field missing"}
		}
		if _, ok := seen[ch.ID]; ok {
			return &Error{Code: ErrSchema, Path: base + ".id", Reason: "duplicate channel id"}
		}
		seen[ch.ID] = struct{}{}
		if ch.Slots < 1 {
			return &Error{Code: ErrPara, Path: base + ".slots", Field: "slots", Reason: "must be positive"}
		}
	}
	return nil
}

func validatePCM(pcm PCMDescriptor, path, dialogueID string) error {
	if pcm.SampleRate <= 0 {
		return &Error{Code: ErrSchema, Path: path + ".sample_rate", DialogueID: dialogueID, Reason: "must be positive"}
	}
	if pcm.BitsPerSample != 16 {
		return &Error{Code: ErrDecode, Path: path + ".bits_per_sample", DialogueID: dialogueID, Field: "bits_per_sample", Reason: "prototype supports 16-bit PCM"}
	}
	if pcm.Channels != 1 {
		return &Error{Code: ErrDecode, Path: path + ".channels", DialogueID: dialogueID, Field: "channels", Reason: "prototype supports mono PCM"}
	}
	if !strings.EqualFold(pcm.Format, "s16le") {
		return &Error{Code: ErrDecode, Path: path + ".format", DialogueID: dialogueID, Field: "format", Reason: "prototype supports s16le"}
	}
	if pcm.SampleCount <= 0 {
		return &Error{Code: ErrSchema, Path: path + ".sample_count", DialogueID: dialogueID, Reason: "must be positive"}
	}
	return nil
}

func nextID(phonemes []Phoneme, i int) string {
	if i+1 < len(phonemes) {
		return phonemes[i+1].ID
	}
	return ""
}
