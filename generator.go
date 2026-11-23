package orchestrator

import (
	"fmt"
	"math/rand"
)

// DefaultSeed is the fixed seed of the deterministic sample generator.
const DefaultSeed int64 = 20240607

var (
	speakers = []string{"Narrator", "Ava", "Ben", "Chloe"}
	charPool = []rune("天地玄黄宇宙洪荒日月盈昃辰宿列张寒来暑往秋收冬藏云腾致雨露结为霜金生丽水玉出昆冈")
	sentEnds = []rune{'。', '！', '？'}
)

// Generate deterministically produces a project with numDialogues
// dialogues plus its ground-truth manifest. The truth is computed
// with the reference scheduling algorithm at generation time.
func Generate(seed int64, numDialogues int) (*Project, *Error) {
	if numDialogues < 2 {
		return nil, newError(EPara, "", "num_dialogues", "need at least 2 dialogues")
	}
	rng := rand.New(rand.NewSource(seed))
	p := &Project{
		ID:               fmt.Sprintf("proj-%d", seed),
		SampleRate:       48000,
		ToleranceSamples: 4,
	}

	var cursor int64 = 48000 // 1s lead-in
	for i := 0; i < numDialogues; i++ {
		id := fmt.Sprintf("dlg-%03d", i+1)
		text := genText(rng)
		nChars := len([]rune(text))
		duration := int64(nChars)*2400 + rng.Int63n(4800)
		seg := SegmentRef{ID: "seg-" + id, StartSample: cursor, EndSample: cursor + duration}
		d := Dialogue{
			ID:      id,
			Speaker: speakers[rng.Intn(len(speakers))],
			Text:    text,
			Segment: seg,
			Rhythm:  make([]float64, nChars),
		}
		for j := range d.Rhythm {
			d.Rhythm[j] = 0.6 + rng.Float64()*1.4
		}
		// Every third dialogue declares a manual presentation range
		// strictly inside the voice segment.
		if i%3 == 2 {
			shrink := duration/8 + rng.Int63n(duration/8+1)
			d.Manual = &SampleRange{Start: seg.StartSample, End: seg.EndSample - shrink}
		}
		// Script interruptions on a deterministic subset.
		if i%3 == 1 {
			start, end := presentRange(&d)
			t := start + (end-start)/2
			d.InterruptAt = &t
		}
		p.Dialogues = append(p.Dialogues, d)
		cursor = seg.EndSample + 4800 + rng.Int63n(19200)
	}

	// Queue order: fixed rotation to make the constraint non-trivial.
	order := make([]string, numDialogues)
	for i := range p.Dialogues {
		order[i] = p.Dialogues[(i+1)%numDialogues].ID
	}
	p.Policy = Policy{Interruptible: true, CutoffLimitSamples: 1, QueueOrder: order}

	// Compute truth with the reference algorithm; size the cutoff
	// limit so every scripted interruption can finish its tail.
	var maxRequired int64
	truths := make([]TruthEntry, numDialogues)
	cuts := make([]*InterruptResult, numDialogues)
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		sched, err := ScheduleDialogue(d)
		if err != nil {
			return nil, err
		}
		truths[i] = TruthEntry{
			DialogueID:   d.ID,
			PresentStart: sched.PresentStart,
			PresentEnd:   sched.PresentEnd,
		}
		if d.InterruptAt != nil {
			ir, ierr := ComputeInterrupt(d, sched, *d.InterruptAt, 1<<62)
			if ierr != nil {
				return nil, ierr
			}
			cuts[i] = ir
			if req := ir.CutoffEnd - ir.InterruptAt; req > maxRequired {
				maxRequired = req
			}
		}
	}
	p.Policy.CutoffLimitSamples = maxRequired + 4800
	for i := range p.Dialogues {
		if cuts[i] != nil {
			truths[i].Interrupted = true
			truths[i].CutoffEnd = cuts[i].CutoffEnd
			truths[i].ResumeIndex = cuts[i].ResumeIndex
			truths[i].ResumeStart = cuts[i].ResumeStart
		}
	}
	p.Truth = truths
	return p, nil
}

// genText builds 2-4 short sentences from the deterministic pool.
func genText(rng *rand.Rand) string {
	nSent := 2 + rng.Intn(3)
	var out []rune
	for s := 0; s < nSent; s++ {
		n := 4 + rng.Intn(7)
		for j := 0; j < n; j++ {
			out = append(out, charPool[rng.Intn(len(charPool))])
		}
		out = append(out, sentEnds[rng.Intn(len(sentEnds))])
	}
	return string(out)
}
