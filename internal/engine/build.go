package engine

import (
	"fmt"
	"unicode"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

const (
	FaultOrder        = "order_inversion"
	FaultRate         = "rate_conflict"
	FaultLipCategory  = "lip_category_out_of_range"
	FaultLipTolerance = "lip_tolerance_exceeded"
	FaultStage        = "stage_failure"
	FaultStagePanic   = "stage_panic"
)

type Options struct {
	FPS               int
	Fault             string
	ToleranceOverride int
	OverrideTolerance bool
}

func DefaultOptions(p *domain.Project) Options {
	return Options{FPS: p.TargetFPS}
}

func BuildAll(p *domain.Project, opts Options) ([]*domain.PartitionResult, error) {
	results := make([]*domain.PartitionResult, 0)
	for di := range p.Dialogues {
		for si := range p.Dialogues[di].Segments {
			r, err := BuildPartition(p, di, si, opts)
			if err != nil {
				return nil, err
			}
			results = append(results, r)
		}
	}
	return results, nil
}

func partitionRef(p *domain.Project, di, si int) (domain.Dialogue, domain.Segment, string) {
	d := p.Dialogues[di]
	s := d.Segments[si]
	pid := domain.MakePartitionID(d.ID, s.Index)
	return d, s, pid
}

func refError(err error, d domain.Dialogue, pid string, index int) error {
	if de, ok := domain.AsError(err); ok {
		return de.WithPartition(d.ID, pid, index)
	}
	return &domain.Error{Code: domain.CodeStage, DialogueID: d.ID, PartitionID: pid, PartitionIndex: index, Message: err.Error()}
}

func BuildPartition(p *domain.Project, di, si int, opts Options) (result *domain.PartitionResult, err error) {
	d, sourceSeg, pid := partitionRef(p, di, si)
	seg := sourceSeg
	seg.Phonemes = append([]domain.Phoneme(nil), sourceSeg.Phonemes...)
	seg.CharWeights = append([]int(nil), sourceSeg.CharWeights...)
	seg.TransitionOverrides = append([]domain.TransitionOverride(nil), sourceSeg.TransitionOverrides...)

	defer func() {
		if rec := recover(); rec != nil {
			stageErr := domain.StageError("recovered partition worker panic: %v", rec)
			err = stageErr.WithPartition(d.ID, pid, seg.Index)
			result = nil
		}
	}()

	if opts.Fault == FaultStage {
		return nil, domain.StageError("injected deterministic partition stage failure").WithPartition(d.ID, pid, seg.Index)
	}
	if opts.Fault == FaultStagePanic {
		panic("injected deterministic worker panic")
	}
	if opts.Fault == FaultOrder && len(seg.Phonemes) >= 2 {
		seg.Phonemes[0], seg.Phonemes[1] = seg.Phonemes[1], seg.Phonemes[0]
	}
	if opts.Fault == FaultRate && len(seg.Phonemes) > 1 {
		seg.Phonemes[1].StartFrame += 7
	}
	if opts.Fault == FaultLipCategory && len(seg.Phonemes) > 0 {
		seg.Phonemes[0].CategoryCode = "OUT_OF_TABLE_9"
	}

	mode := quant.NormalizeMode(p.RoundingMode)
	if err := validateSourceTimeline(p, d, pid, seg, mode); err != nil {
		return nil, err
	}

	fps := opts.FPS
	if fps <= 0 {
		fps = p.TargetFPS
	}
	if fps <= 0 || p.SampleRateHz <= 0 {
		return nil, (&domain.Error{Code: domain.CodeRate, Message: "fps and sample rate must be positive"}).WithPartition(d.ID, pid, seg.Index)
	}
	tolerance := p.TransitionToleranceFrames
	if opts.OverrideTolerance {
		tolerance = opts.ToleranceOverride
	}
	if opts.Fault == FaultLipTolerance {
		tolerance = 0
	}

	frameCount, rem, qerr := quant.SamplesToFrames(pid+".duration_samples", seg.DurationSamples, p.SampleRateHz, fps, mode)
	if qerr != nil {
		return nil, refError(qerr, d, pid, seg.Index)
	}
	_ = rem
	if frameCount <= 0 {
		frameCount = seg.DurationFrames
	}

	phonemeFrames := make([]domain.Phoneme, len(seg.Phonemes))
	copy(phonemeFrames, seg.Phonemes)
	for i := range phonemeFrames {
		start, _, err := quant.SamplesToFrames(fmt.Sprintf("%s.phonemes[%d].start", pid, i), phonemeFrames[i].StartSample, p.SampleRateHz, fps, mode)
		if err != nil {
			return nil, refError(err, d, pid, seg.Index)
		}
		end, _, err := quant.SamplesToFrames(fmt.Sprintf("%s.phonemes[%d].end", pid, i), phonemeFrames[i].EndSample, p.SampleRateHz, fps, mode)
		if err != nil {
			return nil, refError(err, d, pid, seg.Index)
		}
		phonemeFrames[i].StartFrame = start
		phonemeFrames[i].EndFrame = end
		if phonemeFrames[i].EndFrame < phonemeFrames[i].StartFrame {
			phonemeFrames[i].EndFrame = phonemeFrames[i].StartFrame
		}
		if phonemeFrames[i].EndFrame > frameCount {
			phonemeFrames[i].EndFrame = frameCount
		}
	}

	speechStart := 0
	speechEnd := frameCount
	if len(phonemeFrames) > 0 {
		speechStart = phonemeFrames[0].StartFrame
		speechEnd = phonemeFrames[len(phonemeFrames)-1].EndFrame
	}
	intervals, charFrames, compressions, err := scheduleChars(seg.Text, seg.CharWeights, frameCount, speechStart, speechEnd, p.MinReadableFrames)
	if err != nil {
		return nil, refError(err, d, pid, seg.Index)
	}

	categoryByCode := make(map[string]domain.Category, len(p.CategoryTable))
	for _, c := range p.CategoryTable {
		categoryByCode[c.Code] = c
	}
	for _, ph := range seg.Phonemes {
		if _, ok := categoryByCode[ph.CategoryCode]; !ok {
			de := domain.LipError("phoneme %q references unknown lip category %q", ph.Symbol, ph.CategoryCode)
			de.Field = "phonemes[].category"
			de.Frame = ph.StartFrame
			de.LeftCategory = ph.CategoryCode
			return nil, de.WithPartition(d.ID, pid, seg.Index)
		}
	}
	for _, ov := range seg.TransitionOverrides {
		if _, ok := categoryByCode[ov.BridgeCategory]; !ok && ov.BridgeCategory != "" {
			de := domain.LipError("transition override references unknown bridge category %q", ov.BridgeCategory)
			de.Frame = ov.AtFrame
			de.LeftCategory = ov.FromCategory
			de.RightCategory = ov.ToCategory
			return nil, de.WithPartition(d.ID, pid, seg.Index)
		}
		if ov.OffsetFrames > tolerance || -ov.OffsetFrames > tolerance {
			de := domain.LipError("explicit transition offset %d exceeds tolerance %d", ov.OffsetFrames, tolerance)
			de.Frame = ov.AtFrame
			de.LeftCategory = ov.FromCategory
			de.RightCategory = ov.ToCategory
			return nil, de.WithPartition(d.ID, pid, seg.Index)
		}
	}

	lipFrames, transitions, err := buildLipTrack(pid, d.ID, seg.Index, frameCount, phonemeFrames, categoryByCode, seg.TransitionOverrides, tolerance)
	if err != nil {
		return nil, err
	}

	contentHash := ContentHash(p, di, si)
	fpsKey := FPSKey(fps)
	upstream := UpstreamKey(contentHash, fpsKey)
	cacheKey := CacheKey(contentHash, upstream)
	r := &domain.PartitionResult{
		DialogueID:        d.ID,
		DialogueIndex:     di,
		PartitionID:       pid,
		PartitionIndex:    seg.Index,
		FrameCount:        frameCount,
		LipSequenceLength: len(lipFrames),
		SourceSchema:      p.SchemaVersion,
		ContentHash:       contentHash,
		UpstreamKey:       upstream,
		CacheKey:          cacheKey,
		QuantParamVersion: quant.ParamVersion,
		AlgorithmVersion:  domain.AlgorithmVersion,
		CharIntervals:     intervals,
		CharFrames:        charFrames,
		LipFrames:         lipFrames,
		Transitions:       transitions,
		Compressions:      compressions,
	}
	r.FrameDigests = buildFrameDigests(r)
	r.Digest = ResultDigest(r)
	return r, nil
}

func validateSourceTimeline(p *domain.Project, d domain.Dialogue, pid string, seg domain.Segment, mode quant.RoundingMode) error {
	sourceFPS := p.SourceFPS
	if sourceFPS <= 0 {
		sourceFPS = p.TargetFPS
	}
	if p.SampleRateHz <= 0 {
		return (&domain.Error{Code: domain.CodeRate, Field: "sample_rate_hz", Message: "sample rate must be positive"}).WithPartition(d.ID, pid, seg.Index)
	}
	if p.TargetFPS <= 0 {
		return (&domain.Error{Code: domain.CodeRate, Field: "target_fps", Message: "frame rate must be positive"}).WithPartition(d.ID, pid, seg.Index)
	}
	for i, ph := range seg.Phonemes {
		if ph.StartSample >= ph.EndSample {
			de := &domain.Error{Code: domain.CodeOrder, Field: fmt.Sprintf("phonemes[%d].sample_range", i), Frame: ph.StartFrame, Message: "phoneme start must precede end"}
			return de.WithPartition(d.ID, pid, seg.Index)
		}
		if i > 0 {
			prev := seg.Phonemes[i-1]
			if ph.Index < prev.Index || ph.StartSample < prev.StartSample || ph.StartSample < prev.EndSample {
				de := &domain.Error{Code: domain.CodeOrder, Field: fmt.Sprintf("phonemes[%d]", i), Frame: ph.StartFrame, LeftCategory: prev.Symbol, RightCategory: ph.Symbol, Message: "phoneme order inversion or overlap"}
				return de.WithPartition(d.ID, pid, seg.Index)
			}
		}
		wantStart, _, err := quant.SamplesToFrames(fmt.Sprintf("phonemes[%d].start_sample", i), ph.StartSample, p.SampleRateHz, sourceFPS, mode)
		if err != nil {
			return err
		}
		wantEnd, _, err := quant.SamplesToFrames(fmt.Sprintf("phonemes[%d].end_sample", i), ph.EndSample, p.SampleRateHz, sourceFPS, mode)
		if err != nil {
			return err
		}
		if wantStart != ph.StartFrame || wantEnd != ph.EndFrame {
			de := &domain.Error{
				Code: domain.CodeRate, Field: fmt.Sprintf("phonemes[%d].sample_frame", i), Frame: ph.StartFrame,
				Operands: []int64{ph.StartSample, int64(p.SampleRateHz), int64(sourceFPS)},
				Expected: []int{wantStart, wantEnd}, Actual: []int{ph.StartFrame, ph.EndFrame},
				Message: "sample rate/fps quantization contradicts recorded frame boundary",
			}
			return de.WithPartition(d.ID, pid, seg.Index)
		}
	}
	return nil
}

func scheduleChars(text string, weights []int, frameCount, speechStart, speechEnd, minReadable int) ([]domain.CharInterval, []domain.FrameCharRange, []domain.CompressionRecord, error) {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return nil, nil, nil, domain.SchemaError("text", "segment text must not be empty")
	}
	byteStarts := make([]int, n+1)
	offset := 0
	for i, r := range runes {
		byteStarts[i] = offset
		offset += len(string(r))
		byteStarts[i+1] = offset
	}
	if len(weights) != n {
		return nil, nil, nil, domain.SchemaError("character_weights", "weight count %d does not match rune count %d", len(weights), n)
	}
	if speechStart < 0 || speechEnd > frameCount || speechEnd <= speechStart || speechEnd-speechStart < n {
		speechStart = 0
		speechEnd = frameCount
	}
	spokenFrames := speechEnd - speechStart
	if spokenFrames < n {
		conflicts := make([]string, 0, n)
		for i, r := range runes {
			conflicts = append(conflicts, fmt.Sprintf("char=%d rune=%q needs at least 1 frame but partition has %d frames", i, r, frameCount))
		}
		return nil, nil, nil, domain.ReadError(conflicts, "not enough frames to cover every character")
	}

	lengths := allocateWeighted(spokenFrames, weights)
	desired := make([]int, n)
	for i := range lengths {
		desired[i] = lengths[i]
		if minReadable > 1 && desired[i] < minReadable {
			desired[i] = minReadable
		}
	}
	lengths = desired
	compressions := []domain.CompressionRecord{}
	target := spokenFrames
	for sumInts(lengths) > target {
		choice := -1
		choicePriority := -1
		for i, r := range runes {
			if lengths[i] <= 1 {
				continue
			}
			pr := compressionPriority(r, weights[i])
			if choice == -1 || pr > choicePriority ||
				(pr == choicePriority && weights[i] < weights[choice]) ||
				(pr == choicePriority && weights[i] == weights[choice] && i < choice) {
				choice = i
				choicePriority = pr
			}
		}
		if choice == -1 {
			conflicts := make([]string, 0, n)
			for i, r := range runes {
				conflicts = append(conflicts, fmt.Sprintf("char=%d rune=%q interval=%d frames min_readable=%d", i, r, lengths[i], minReadable))
			}
			return nil, nil, nil, domain.ReadError(conflicts, "readable-frame constraints cannot be compressed further")
		}
		start := sumInts(lengths[:choice])
		before := lengths[choice]
		frame := speechStart + start + before - 1
		lengths[choice]--
		compressions = append(compressions, domain.CompressionRecord{
			Frame: frame, CharIndex: choice, Char: string(runes[choice]), Priority: choicePriority,
			BeforeLength: before, AfterLength: lengths[choice],
			Rule: fmt.Sprintf("priority=%d class=%s; removed trailing dwell frame", choicePriority, charClass(runes[choice])),
		})
	}
	for sumInts(lengths) < target {
		choice := 0
		for i := 1; i < n; i++ {
			if weights[i] > weights[choice] || (weights[i] == weights[choice] && i < choice) {
				choice = i
			}
		}
		lengths[choice]++
	}

	intervals := make([]domain.CharInterval, n)
	charFrames := make([]domain.FrameCharRange, frameCount)
	start := speechStart
	for i, r := range runes {
		end := start + lengths[i]
		if i == 0 {
			start = 0
		}
		if i == n-1 {
			end = frameCount
		}
		intervals[i] = domain.CharInterval{
			CharIndex: i, RuneStart: i, RuneEnd: i + 1, ByteStart: byteStarts[i], ByteEnd: byteStarts[i+1],
			Char: string(r), WeightPermyriad: weights[i], StartFrame: start, EndFrame: end,
		}
		for f := start; f < end; f++ {
			charFrames[f] = domain.FrameCharRange{
				Frame: f, CharIndex: i, RuneStart: i, RuneEnd: i + 1,
				ByteStart: byteStarts[i], ByteEnd: byteStarts[i+1], Char: string(r),
				IntervalStartFrame: start, IntervalEndFrame: end,
			}
		}
		start = end
	}
	return intervals, charFrames, compressions, nil
}

func allocateWeighted(total int, weights []int) []int {
	n := len(weights)
	out := make([]int, n)
	positiveSum := 0
	for _, w := range weights {
		if w < 1 {
			w = 1
		}
		positiveSum += w
	}
	remaining := total - n
	type rem struct {
		i int
		r int
	}
	rems := make([]rem, n)
	allocated := 0
	for i, w := range weights {
		if w < 1 {
			w = 1
		}
		num := w * remaining
		q := num / positiveSum
		r := num % positiveSum
		out[i] = 1 + q
		allocated += q
		rems[i] = rem{i, r}
	}
	left := remaining - allocated
	for left > 0 {
		best := 0
		for i := 1; i < n; i++ {
			if rems[i].r > rems[best].r || (rems[i].r == rems[best].r && i < best) {
				best = i
			}
		}
		out[rems[best].i]++
		rems[best].r = -1
		left--
	}
	return out
}

func compressionPriority(r rune, weight int) int {
	switch {
	case unicode.IsSpace(r):
		return 1000
	case unicode.IsPunct(r):
		return 900
	default:
		priority := 500 - weight/20
		if priority < 10 {
			priority = 10
		}
		return priority
	}
}

func charClass(r rune) string {
	switch {
	case unicode.IsSpace(r):
		return "space"
	case unicode.IsPunct(r):
		return "punctuation"
	default:
		return "spoken-grapheme"
	}
}

func sumInts(v []int) int {
	total := 0
	for _, x := range v {
		total += x
	}
	return total
}

func buildLipTrack(pid, dialogueID string, partitionIndex, frameCount int, phonemes []domain.Phoneme, categories map[string]domain.Category, overrides []domain.TransitionOverride, tolerance int) ([]domain.LipFrame, []domain.Transition, error) {
	baseCategory := make([]string, frameCount)
	baseFamily := make([]string, frameCount)
	basePhoneme := make([]int, frameCount)
	overrideByFrame := make(map[int]domain.TransitionOverride, len(overrides))
	for _, ov := range overrides {
		overrideByFrame[ov.AtFrame] = ov
	}
	for f := 0; f < frameCount; f++ {
		category := "SIL"
		family := "silence"
		phonemeIndex := -1
		for _, ph := range phonemes {
			if f >= ph.StartFrame && f < ph.EndFrame {
				category = ph.CategoryCode
				if c, ok := categories[ph.CategoryCode]; ok {
					family = c.Family
				}
				phonemeIndex = ph.Index
				break
			}
		}
		baseCategory[f] = category
		baseFamily[f] = family
		basePhoneme[f] = phonemeIndex
	}

	lip := make([]domain.LipFrame, 0, frameCount+4)
	transitions := []domain.Transition{}
	seq := 0
	for f := 0; f < frameCount; f++ {
		if f > 0 && !domain.ContinuousFamily(baseFamily[f-1], baseFamily[f]) {
			if tolerance < 1 {
				de := domain.LipError("non-continuous lip families require an insertion but transition tolerance is %d", tolerance)
				de.Field = "transition_tolerance_frames"
				de.Frame = f
				de.LeftCategory = baseCategory[f-1]
				de.RightCategory = baseCategory[f]
				return nil, nil, de.WithPartition(dialogueID, pid, partitionIndex)
			}
			bridge := "TR"
			rule := "shortest-deterministic-bridge"
			reason := fmt.Sprintf("non-continuous families %s -> %s", baseFamily[f-1], baseFamily[f])
			if ov, ok := overrideByFrame[f]; ok {
				if ov.BridgeCategory != "" && ov.BridgeCategory != "TR" {
					bridge = ov.BridgeCategory
					rule = "schema-v3-explicit-override"
				}
				if ov.Reason != "" && ov.Reason != reason {
					reason = ov.Reason
					rule = "schema-v3-explicit-override"
				}
			}
			tr := domain.Transition{
				ID: fmt.Sprintf("%s-T%d", pid, f), DialogueID: dialogueID, PartitionID: pid,
				BoundaryFrame: f, InsertAtSequence: seq, OffsetFrames: 1,
				FromCategory: baseCategory[f-1], ToCategory: baseCategory[f], BridgeCategory: bridge,
				Rule: rule, Reason: reason,
			}
			transitions = append(transitions, tr)
			lip = append(lip, domain.LipFrame{
				BaseFrame: f, SequenceFrame: seq, PhonemeIndex: -1,
				Category: bridge, Family: categories[bridge].Family, Inserted: true, TransitionID: tr.ID,
			})
			seq++
		}
		lip = append(lip, domain.LipFrame{
			BaseFrame: f, SequenceFrame: seq, PhonemeIndex: basePhoneme[f],
			Category: baseCategory[f], Family: baseFamily[f],
		})
		seq++
	}
	return lip, transitions, nil
}

func buildFrameDigests(r *domain.PartitionResult) []domain.FrameDigest {
	out := make([]domain.FrameDigest, 0, len(r.LipFrames))
	for _, lf := range r.LipFrames {
		charIndex := -1
		if lf.BaseFrame >= 0 && lf.BaseFrame < len(r.CharFrames) {
			charIndex = r.CharFrames[lf.BaseFrame].CharIndex
		}
		d := domain.FrameDigest{
			BaseFrame: lf.BaseFrame, SequenceFrame: lf.SequenceFrame, CharIndex: charIndex,
			Category: lf.Category, Inserted: lf.Inserted,
		}
		d.Digest = canonicalHash("frame-digest-v1", d)
		out = append(out, d)
	}
	return out
}
