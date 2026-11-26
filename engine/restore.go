package engine

import (
	"context"
	"encoding/json"

	"dialoguefit/model"
	"dialoguefit/state"
)

type RestoreReport struct {
	RestoredDialogues int                       `json:"restored_dialogues"`
	Invalidations     []model.InvalidationEntry `json:"invalidations"`
}

// Restore loads only complete, version-matching dialogue snapshots. Any
// content/version/fingerprint mismatch or torn phoneme set invalidates the
// whole dialogue and deletes its rows; callers recompute that dialogue from
// phoneme zero rather than mixing old and new segments.
func (e *Engine) Restore(ctx context.Context) (*RestoreReport, error) {
	report := &RestoreReport{Invalidations: []model.InvalidationEntry{}}
	if e.Store == nil {
		return report, nil
	}
	for di := range e.Project.Dialogues {
		d := &e.Project.Dialogues[di]
		records, err := e.Store.LoadDialogueSegments(ctx, d.ID)
		if err != nil {
			return report, err
		}
		contentHash := model.ContentHash(*d)
		paramVersion := model.ParamVersion(e.Project.Envelope, d.Params)
		truthFingerprint := e.TruthFingerprint(d.ID)
		invalidReason := ""
		invalidField := ""
		if len(records) != len(d.Phonemes) {
			invalidReason = "checkpoint-incomplete"
			invalidField = "phoneme_index"
		} else {
			for i, rec := range records {
				switch {
				case rec.PhonemeIndex != i || rec.PhonemeID != d.Phonemes[i].ID:
					invalidReason, invalidField = "checkpoint-order", "phoneme_index"
				case rec.ContentHash != contentHash:
					invalidReason, invalidField = "content-hash", "content_hash"
				case rec.ParamVersion != paramVersion:
					invalidReason, invalidField = "param-version", "param_version"
				case rec.AlgorithmVersion != model.AlgorithmVersion:
					invalidReason, invalidField = "algorithm-version", "algorithm_version"
				case truthFingerprint != "" && rec.TruthFingerprint != truthFingerprint:
					invalidReason, invalidField = "truth-fingerprint", "truth_fingerprint"
				case rec.State != "committed":
					invalidReason, invalidField = "state", "state"
				}
				if invalidReason != "" {
					break
				}
			}
		}
		if invalidReason != "" {
			report.Invalidations = append(report.Invalidations, model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: 0, PhonemeID: "",
				Reason: invalidReason, Node: invalidField,
			})
			if err := e.Store.DeleteDialogue(ctx, d.ID); err != nil {
				return report, err
			}
			continue
		}

		phonemeResults := make([]model.PhonemeResult, 0, len(records))
		frameByIndex := map[int]model.Frame{}
		stress := make([]model.StressMark, 0)
		segments := make([]model.StretchSegment, 0, len(records))
		var payloadErr error
		for _, rec := range records {
			var pr model.PhonemeResult
			if err := json.Unmarshal([]byte(rec.PayloadJSON), &pr); err != nil {
				payloadErr = err
				break
			}
			phonemeResults = append(phonemeResults, pr)
			for _, f := range pr.Frames {
				frameByIndex[f.Index] = f
			}
			if pr.Stress != nil {
				stress = append(stress, *pr.Stress)
			}
			if pr.Segment != nil {
				segments = append(segments, *pr.Segment)
			}
			e.Cache.Put(pr)
		}
		if payloadErr != nil {
			report.Invalidations = append(report.Invalidations, model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: 0, Reason: "payload-decode", Node: "payload_json",
			})
			_ = e.Store.DeleteDialogue(ctx, d.ID)
			continue
		}
		frames := sortedFrameMap(frameByIndex)
		commitSequence := make([]string, len(d.Phonemes))
		for i, ph := range d.Phonemes {
			commitSequence[i] = d.ID + "/" + ph.ID
		}
		result := &model.DialogueResult{
			DialogueID: d.ID, ContentHash: contentHash, ParamVersion: paramVersion,
			AlgorithmVersion: model.AlgorithmVersion, TruthFingerprint: truthFingerprint,
			Params: d.Params, Frames: frames, Stress: stress, Segments: segments,
			PhonemeResults: phonemeResults, CommitSequence: commitSequence, State: "restored",
		}
		e.mu.Lock()
		e.Results[d.ID] = result
		e.States[d.ID] = &state.Machine{DialogueID: d.ID, Current: state.Committed}
		delete(e.Failures, d.ID)
		e.mu.Unlock()
		report.RestoredDialogues++
	}
	return report, nil
}

func (e *Engine) RunMissing(ctx context.Context) (*model.InvalidationReport, error) {
	aggregate := &model.InvalidationReport{Entries: []model.InvalidationEntry{}}
	for _, d := range e.Project.Dialogues {
		e.mu.Lock()
		_, exists := e.Results[d.ID]
		failed := e.States[d.ID] != nil && e.States[d.ID].Current == state.Failed
		e.mu.Unlock()
		if exists && !failed {
			aggregate.ReusedDialogues++
			continue
		}
		rep, err := e.RetryDialogue(ctx, d.ID)
		if err != nil {
			return aggregate, err
		}
		if rep != nil {
			aggregate.Entries = append(aggregate.Entries, rep.Entries...)
			aggregate.RecomputedPhonemes += rep.RecomputedPhonemes
			aggregate.ReusedPhonemes += rep.ReusedPhonemes
		}
		aggregate.ComputedDialogues++
	}
	return aggregate, nil
}
