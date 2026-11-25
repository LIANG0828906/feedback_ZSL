package edit

import (
	"fmt"

	"lipsync/domain"
	"lipsync/quant"
)

// ApplySequence applies edits to a clone of the project. Edit order is
// significant and deterministic. The resulting project can be passed through
// any pipeline mode.
func ApplySequence(p *domain.Project, edits ...domain.Edit) (*domain.Project, error) {
	if p == nil {
		return nil, &domain.Error{Code: domain.CodeParam, Message: "nil project"}
	}
	out := p.Clone()
	for i, e := range edits {
		if err := Apply(out, e); err != nil {
			return nil, domain.Annotate(err, func(de *domain.Error) {
				if de.Field == "" {
					de.Field = fmt.Sprintf("edits[%d]", i)
				}
			})
		}
	}
	return out, nil
}

func Apply(p *domain.Project, e domain.Edit) error {
	if p == nil {
		return &domain.Error{Code: domain.CodeParam, Message: "nil project"}
	}
	switch e.Kind {
	case domain.EditFPS:
		return applyFPS(p, e)
	case domain.EditBoundary:
		return applyBoundary(p, e)
	case domain.EditWeight:
		return applyWeight(p, e)
	default:
		return &domain.Error{
			Code: domain.CodeParam, Field: "edit.kind", Value: string(e.Kind),
			Message: "unknown edit kind",
		}
	}
}

func applyBoundary(p *domain.Project, e domain.Edit) error {
	d, ph, err := findPhoneme(p, e)
	if err != nil {
		return err
	}
	if e.NewStartFrame < 0 || e.NewEndFrame <= e.NewStartFrame {
		return &domain.Error{
			Code: domain.CodeOrder, Field: "edit.boundary",
			DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: ph.ID,
			Expected: "start >= 0 and end > start",
			Actual:   fmt.Sprintf("%d..%d", e.NewStartFrame, e.NewEndFrame),
		}
	}
	ph.StartFrame = e.NewStartFrame
	ph.EndFrame = e.NewEndFrame
	ph.SourceBasis = "frame"
	ph.SourceStart = ph.StartFrame
	ph.SourceEnd = ph.EndFrame
	ss, err := quant.FrameToSample(ph.StartFrame, d.PCM.SampleRate, p.Render.TargetFPS,
		p.Render.Rounding, "sample_start")
	if err != nil {
		return err
	}
	se, err := quant.FrameToSample(ph.EndFrame, d.PCM.SampleRate, p.Render.TargetFPS,
		p.Render.Rounding, "sample_end")
	if err != nil {
		return err
	}
	if ss.Truncated || se.Truncated {
		ph.SampleStart, ph.SampleEnd = 0, 0
	} else {
		ph.SampleStart, ph.SampleEnd = ss.Value, se.Value
	}
	p.Traces = append(p.Traces, domain.Trace{
		Path:          fmt.Sprintf("dialogues.%s.phonemes.%s.boundary", d.ID, ph.ID),
		SourceVersion: p.SourceVersion, From: "previous", To: fmt.Sprintf("%d..%d", ph.StartFrame, ph.EndFrame),
		Rule: "explicit boundary edit",
	})
	return nil
}

func applyWeight(p *domain.Project, e domain.Edit) error {
	if e.NewWeight < 0 {
		return &domain.Error{
			Code: domain.CodeParam, Field: "edit.weight", Value: fmt.Sprintf("%d", e.NewWeight),
			Message: "weight must not be negative",
		}
	}
	d, ph, err := findPhoneme(p, e)
	if err != nil {
		return err
	}
	ph.WeightPermyriad = e.NewWeight
	scale := ph.SourceWeightScale
	if scale == 0 {
		scale = p.WeightScale
	}
	if scale > 0 {
		w, err := quant.WeightFromPermyriad(ph.WeightPermyriad, scale, p.Render.Rounding, "source_weight")
		if err != nil {
			return err
		}
		ph.SourceWeight = w.Value
		ph.SourceWeightScale = scale
	}
	p.Traces = append(p.Traces, domain.Trace{
		Path:          fmt.Sprintf("dialogues.%s.phonemes.%s.weight", d.ID, ph.ID),
		SourceVersion: p.SourceVersion, From: "previous", To: fmt.Sprintf("%d", ph.WeightPermyriad),
		Rule: "explicit duration-weight edit",
	})
	return nil
}

func applyFPS(p *domain.Project, e domain.Edit) error {
	if e.NewFPS <= 0 {
		return &domain.Error{
			Code: domain.CodeRate, Field: "edit.new_fps", Value: fmt.Sprintf("%d", e.NewFPS),
			Message: "frame rate must be positive",
		}
	}
	oldFPS := p.Render.TargetFPS
	if oldFPS <= 0 {
		return &domain.Error{
			Code: domain.CodeRate, Field: "render.target_fps",
			Message: "source project has invalid frame rate",
		}
	}
	allIntegral := true
	for _, d := range p.Dialogues {
		if d.PCM.SampleRate <= 0 || d.PCM.SampleRate%e.NewFPS != 0 {
			allIntegral = false
			break
		}
	}
	for di := range p.Dialogues {
		d := &p.Dialogues[di]
		for pi := range d.Phonemes {
			ph := &d.Phonemes[pi]
			ss, err := quant.FrameToSample(ph.StartFrame, d.PCM.SampleRate, oldFPS,
				p.Render.Rounding, "old_sample_start")
			if err != nil {
				return err
			}
			se, err := quant.FrameToSample(ph.EndFrame, d.PCM.SampleRate, oldFPS,
				p.Render.Rounding, "old_sample_end")
			if err != nil {
				return err
			}
			ns, err := quant.SampleToFrame(ss.Value, d.PCM.SampleRate, e.NewFPS,
				p.Render.Rounding, "new_frame_start")
			if err != nil {
				return err
			}
			ne, err := quant.SampleToFrame(se.Value, d.PCM.SampleRate, e.NewFPS,
				p.Render.Rounding, "new_frame_end")
			if err != nil {
				return err
			}
			if ne.Value <= ns.Value {
				return &domain.Error{
					Code: domain.CodeOrder, Field: "edit.fps",
					DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: ph.ID,
					Message: "frame-rate edit collapsed a phoneme interval",
				}
			}
			ph.StartFrame, ph.EndFrame = ns.Value, ne.Value
			ph.SampleStart, ph.SampleEnd = ss.Value, se.Value
			ph.SourceBasis = "sample"
			ph.SourceStart, ph.SourceEnd = ss.Value, se.Value
		}
	}
	p.Render.TargetFPS = e.NewFPS
	if allIntegral && len(p.Dialogues) > 0 {
		p.Render.SamplesPerFrame = p.Dialogues[0].PCM.SampleRate / e.NewFPS
	} else {
		p.Render.SamplesPerFrame = 0
	}
	p.Traces = append(p.Traces, domain.Trace{
		Path: "render.target_fps", SourceVersion: p.SourceVersion,
		From: fmt.Sprintf("%d", oldFPS), To: fmt.Sprintf("%d", e.NewFPS),
		Rule: "explicit frame-rate edit with sample-domain requantization",
	})
	return nil
}

func findPhoneme(p *domain.Project, e domain.Edit) (*domain.Dialogue, *domain.Phoneme, error) {
	var d *domain.Dialogue
	for i := range p.Dialogues {
		if (e.DialogueID != "" && p.Dialogues[i].ID == e.DialogueID) ||
			(e.DialogueID == "" && p.Dialogues[i].ShardIndex == e.ShardIndex) {
			d = &p.Dialogues[i]
			break
		}
	}
	if d == nil {
		return nil, nil, &domain.Error{
			Code: domain.CodeParam, Field: "edit.dialogue_id",
			DialogueID: e.DialogueID, ShardIndex: e.ShardIndex,
			Message: "edited dialogue not found",
		}
	}
	for i := range d.Phonemes {
		if d.Phonemes[i].ID == e.PhonemeID {
			return d, &d.Phonemes[i], nil
		}
	}
	return nil, nil, &domain.Error{
		Code: domain.CodeParam, Field: "edit.phoneme_id",
		DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: e.PhonemeID,
		Message: "edited phoneme not found",
	}
}
