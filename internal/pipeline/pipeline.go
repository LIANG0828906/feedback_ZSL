package pipeline

import (
	"fmt"
	"time"

	"lip-pipeline-prototype/internal/convert"
	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

func shardError(code domain.ErrorCode, msg string, shard domain.Shard) *domain.Error {
	idx := shard.Index
	return &domain.Error{
		Code: code, Message: msg, DialogueID: shard.DialogueID,
		ShardID: shard.ID, ShardIndex: &idx,
	}
}

func annotateShard(err error, shard domain.Shard) error {
	de, ok := domain.AsError(err)
	if !ok {
		return err
	}
	if de.DialogueID == "" {
		de.DialogueID = shard.DialogueID
	}
	if de.ShardID == "" {
		de.ShardID = shard.ID
	}
	if de.ShardIndex == nil {
		idx := shard.Index
		de.ShardIndex = &idx
	}
	return de
}

func ValidateParams(params domain.Params) error {
	if params.SampleRate <= 0 {
		return &domain.Error{Code: domain.CodeVal, Message: "sample_rate must be positive", Path: "params.sample_rate", Field: "sample_rate", Actual: params.SampleRate}
	}
	if params.FPS <= 0 {
		return &domain.Error{Code: domain.CodeVal, Message: "fps must be positive", Path: "params.fps", Field: "fps", Actual: params.FPS}
	}
	if params.MaxPhonemesPerPack <= 0 {
		return &domain.Error{Code: domain.CodeVal, Message: "max_phonemes_per_pack must be positive", Path: "params.max_phonemes_per_pack", Field: "max_phonemes_per_pack", Actual: params.MaxPhonemesPerPack}
	}
	if params.TransitionTolerance < 0 {
		return &domain.Error{Code: domain.CodeVal, Message: "transition_tolerance must not be negative", Path: "params.transition_tolerance", Field: "transition_tolerance", Actual: params.TransitionTolerance}
	}
	if params.AffinityMergeThreshold < 0 || params.AffinityMergeThreshold > 10000 {
		return &domain.Error{Code: domain.CodeVal, Message: "affinity threshold must be in [0,10000]", Path: "params.affinity_merge_threshold_permyriad", Actual: params.AffinityMergeThreshold}
	}
	return nil
}

func ValidateShard(shard domain.Shard, params domain.Params) error {
	if err := ValidateParams(params); err != nil {
		return annotateShard(err, shard)
	}
	if shard.InjectFailure {
		err := shardError(domain.CodeStage, "injected shard stage failure", shard)
		err.Path = "fault_injection.shard_failure"
		err.Frame = int64Ptr(0)
		return err
	}
	if len(shard.Phonemes) == 0 {
		err := shardError(domain.CodeDecode, "shard has no phoneme annotations", shard)
		err.Path = "dialogues.shards.phonemes"
		return err
	}
	var previousEnd int64
	havePrev := false
	for i, ph := range shard.Phonemes {
		path := fmt.Sprintf("dialogues[%s].shards[%d].phonemes[%d]", shard.DialogueID, shard.Index, i)
		if ph.StartSample < 0 || ph.EndSample <= ph.StartSample {
			err := shardError(domain.CodeDecode, "phoneme has invalid sample interval", shard)
			err.Path = path
			err.Field = "start_sample/end_sample"
			err.OperandA = fmt.Sprint(ph.StartSample)
			err.OperandB = fmt.Sprint(ph.EndSample)
			return err
		}
		if havePrev && ph.StartSample < previousEnd {
			frame, qerr := convert.QuantizeFrame(ph.StartSample, params.FPS, params.SampleRate, path+".start_sample")
			if qerr != nil {
				return annotateShard(qerr, shard)
			}
			err := shardError(domain.CodeOrder, "phoneme intervals overlap", shard)
			err.Path = path + ".start_sample"
			err.Frame = &frame
			err.Field = "start_sample"
			err.OperandA = fmt.Sprint(previousEnd)
			err.OperandB = fmt.Sprint(ph.StartSample)
			err.Expected = "start_sample >= previous end_sample"
			err.Actual = ph.StartSample
			return err
		}
		previousEnd = ph.EndSample
		havePrev = true
	}
	last := shard.Phonemes[len(shard.Phonemes)-1]
	if shard.TotalSamples != last.EndSample {
		err := shardError(domain.CodeDecode, "declared total sample count does not match annotation list", shard)
		err.Path = "dialogues.shards.total_samples"
		err.Field = "total_samples"
		err.OperandA = fmt.Sprint(shard.TotalSamples)
		err.OperandB = fmt.Sprint(last.EndSample)
		err.Expected = last.EndSample
		err.Actual = shard.TotalSamples
		return err
	}
	if shard.PCM.SampleCount != 0 && shard.PCM.SampleCount != shard.TotalSamples {
		err := shardError(domain.CodeDecode, "PCM sample count does not match shard sample count", shard)
		err.Path = "dialogues.shards.pcm.sample_count"
		err.Field = "pcm.sample_count"
		err.OperandA = fmt.Sprint(shard.PCM.SampleCount)
		err.OperandB = fmt.Sprint(shard.TotalSamples)
		err.Expected = shard.TotalSamples
		err.Actual = shard.PCM.SampleCount
		return err
	}
	return nil
}

func int64Ptr(v int64) *int64 { return &v }

func PackPhonemes(shard domain.Shard, params domain.Params) ([]domain.Pack, error) {
	if err := ValidateShard(shard, params); err != nil {
		return nil, err
	}
	var packs []domain.Pack
	var current []domain.Phoneme
	packIndex := 0

	flush := func() {
		if len(current) == 0 {
			return
		}
		pack := domain.Pack{
			Index: packIndex, DialogueID: shard.DialogueID, ShardID: shard.ID,
			StartSample: current[0].StartSample,
			EndSample:   current[len(current)-1].EndSample,
		}
		for _, ph := range current {
			pack.PhonemeIDs = append(pack.PhonemeIDs, ph.ID)
			pack.Categories = append(pack.Categories, ph.Category)
		}
		type phonemeDigest struct {
			ID             string
			Start          int64
			End            int64
			Category       domain.Category
			Weight         int
			AffinityToNext int
		}
		phonemeDigests := make([]phonemeDigest, 0, len(current))
		for _, ph := range current {
			phonemeDigests = append(phonemeDigests, phonemeDigest{ph.ID, ph.StartSample, ph.EndSample, ph.Category, ph.WeightPermyriad, ph.AffinityToNext})
		}
		pack.Digest = hashx.JSON(struct {
			IDs      []string
			Start    int64
			End      int64
			Cats     []domain.Category
			Phonemes []phonemeDigest
			Max      int
			Thresh   int
		}{pack.PhonemeIDs, pack.StartSample, pack.EndSample, pack.Categories, phonemeDigests, params.MaxPhonemesPerPack, params.AffinityMergeThreshold})
		packs = append(packs, pack)
		packIndex++
		current = nil
	}

	for i, ph := range shard.Phonemes {
		if ph.Category == domain.CategorySilence {
			flush()
			current = []domain.Phoneme{ph}
			flush()
			continue
		}
		if len(current) > 0 {
			affinity := shard.Phonemes[i-1].AffinityToNext
			if len(current) >= params.MaxPhonemesPerPack || affinity < params.AffinityMergeThreshold {
				flush()
			}
		}
		current = append(current, ph)
		if len(current) >= params.MaxPhonemesPerPack {
			flush()
		}
	}
	flush()

	for i := range packs {
		startFrame, err := convert.QuantizeFrame(packs[i].StartSample, params.FPS, params.SampleRate, "pack.start_sample")
		if err != nil {
			return nil, annotateShard(err, shard)
		}
		endFrame, err := convert.QuantizeFrame(packs[i].EndSample, params.FPS, params.SampleRate, "pack.end_sample")
		if err != nil {
			return nil, annotateShard(err, shard)
		}
		packs[i].StartFrame = startFrame
		packs[i].EndFrame = endFrame
		packs[i].FrameCount = endFrame - startFrame
	}
	return packs, nil
}

func MouthForCategory(category domain.Category) domain.MouthClass {
	switch category {
	case domain.CategoryVowel:
		return domain.MouthVowel
	case domain.CategoryConsonant:
		return domain.MouthConsonant
	case domain.CategorySilence:
		return domain.MouthSilence
	default:
		return domain.MouthSilence
	}
}

func mouthFamily(mouth domain.MouthClass) int {
	switch mouth {
	case domain.MouthSilence:
		return 0
	case domain.MouthConsonant, domain.MouthBridge:
		return 1
	case domain.MouthVowel:
		return 2
	default:
		return 0
	}
}

func bridgeMouth(from, to domain.MouthClass) domain.MouthClass {
	ff, tf := mouthFamily(from), mouthFamily(to)
	if ff == 0 && tf == 2 || ff == 2 && tf == 0 {
		return domain.MouthConsonant
	}
	mid := (ff + tf) / 2
	if mid == 1 {
		return domain.MouthConsonant
	}
	return domain.MouthBridge
}

type phonemeFrameSpan struct {
	phoneme domain.Phoneme
	start   int64
	end     int64
	pack    int
}

func frameSpans(shard domain.Shard, params domain.Params, packs []domain.Pack) ([]phonemeFrameSpan, map[string]int, error) {
	packByPhoneme := map[string]int{}
	for _, pack := range packs {
		for _, id := range pack.PhonemeIDs {
			packByPhoneme[id] = pack.Index
		}
	}
	spans := make([]phonemeFrameSpan, 0, len(shard.Phonemes))
	for _, ph := range shard.Phonemes {
		start, err := convert.QuantizeFrame(ph.StartSample, params.FPS, params.SampleRate, ph.ID+".start_sample")
		if err != nil {
			return nil, nil, annotateShard(err, shard)
		}
		end, err := convert.QuantizeFrame(ph.EndSample, params.FPS, params.SampleRate, ph.ID+".end_sample")
		if err != nil {
			return nil, nil, annotateShard(err, shard)
		}
		if end <= start {
			err := shardError(domain.CodeDecode, "phoneme is shorter than one quantized frame", shard)
			err.Path = "phonemes." + ph.ID
			err.Frame = &start
			return nil, nil, err
		}
		spans = append(spans, phonemeFrameSpan{phoneme: ph, start: start, end: end, pack: packByPhoneme[ph.ID]})
	}
	return spans, packByPhoneme, nil
}

func transitionAtBoundary(shard domain.Shard, params domain.Params, spans []phonemeFrameSpan, i int) (*domain.TransitionMark, *domain.Error) {
	if i <= 0 || i >= len(spans) {
		return nil, nil
	}
	prevMouth := MouthForCategory(spans[i-1].phoneme.Category)
	nextMouth := MouthForCategory(spans[i].phoneme.Category)
	prevFamily, nextFamily := mouthFamily(prevMouth), mouthFamily(nextMouth)
	span := nextFamily - prevFamily
	if span < 0 {
		span = -span
	}
	if span <= 1 {
		return nil, nil
	}
	frame := spans[i].start
	if span > params.TransitionTolerance {
		err := shardError(domain.CodeLip, "category family span exceeds transition tolerance", shard)
		err.Frame = &frame
		err.PackIndex = intPtr(spans[i].pack)
		err.Field = "transition_tolerance"
		err.Expected = params.TransitionTolerance
		err.Actual = span
		err.WithDetail("from", prevMouth).WithDetail("to", nextMouth)
		return nil, err
	}
	mark := domain.TransitionMark{
		Frame: frame, DialogueID: shard.DialogueID, ShardID: shard.ID,
		PackIndex: spans[i].pack, From: prevMouth, To: nextMouth,
		Bridge: bridgeMouth(prevMouth, nextMouth), FamilySpan: span, Inserted: true,
	}
	return &mark, nil
}

func intPtr(v int) *int { return &v }

func BuildShard(shard domain.Shard, params domain.Params, dialogueHash string, source domain.SchemaVersion) (*domain.ShardResult, error) {
	packs, err := PackPhonemes(shard, params)
	if err != nil {
		return nil, err
	}
	spans, _, err := frameSpans(shard, params, packs)
	if err != nil {
		return nil, err
	}
	totalFrames, err := convert.QuantizeFrame(shard.TotalSamples, params.FPS, params.SampleRate, shard.ID+".total_samples")
	if err != nil {
		return nil, annotateShard(err, shard)
	}

	transitionsByFrame := map[int64]domain.TransitionMark{}
	var transitions []domain.TransitionMark
	for i := range spans {
		mark, terr := transitionAtBoundary(shard, params, spans, i)
		if terr != nil {
			return nil, terr
		}
		if mark != nil {
			transitions = append(transitions, *mark)
			transitionsByFrame[mark.Frame] = *mark
		}
	}

	frames := make([]domain.Frame, 0, totalFrames)
	spanIndex := 0
	for f := int64(0); f < totalFrames; f++ {
		for spanIndex < len(spans) && f >= spans[spanIndex].end {
			spanIndex++
		}
		if spanIndex >= len(spans) || f < spans[spanIndex].start {
			continue
		}
		span := spans[spanIndex]
		frame := domain.Frame{
			Index: f, DialogueID: shard.DialogueID, ShardID: shard.ID,
			PackIndex: span.pack, PhonemeID: span.phoneme.ID,
			Category: span.phoneme.Category, Mouth: MouthForCategory(span.phoneme.Category),
			Kind: domain.FrameStable,
		}
		if mark, ok := transitionsByFrame[f]; ok {
			frame.Kind = domain.FrameTransition
			frame.Mouth = mark.Bridge
			frame.TransitionFrom = mark.From
			frame.TransitionTo = mark.To
			frame.FamilySpan = mark.FamilySpan
		}
		frames = append(frames, frame)
	}

	contentHash := dialogueHash
	if contentHash == "" {
		contentHash = hashx.JSON(shard)
	}
	return &domain.ShardResult{
		DialogueID: shard.DialogueID, ShardID: shard.ID, ShardIndex: shard.Index,
		ChannelID: shard.ChannelID, TotalFrames: totalFrames, Packs: packs, Frames: frames,
		Transitions: transitions, ContentHash: contentHash, ParamVersion: params.ParamVersion,
		AlgorithmVersion: domain.AlgorithmVersion, SourceVersion: source,
	}, nil
}

func BuildPreview(shard domain.Shard, params domain.Params) (*domain.PreviewResult, error) {
	start := time.Now()
	packs, err := PackPhonemes(shard, params)
	if err != nil {
		return nil, err
	}
	spans, _, err := frameSpans(shard, params, packs)
	if err != nil {
		return nil, err
	}
	totalFrames, err := convert.QuantizeFrame(shard.TotalSamples, params.FPS, params.SampleRate, shard.ID+".total_samples")
	if err != nil {
		return nil, annotateShard(err, shard)
	}
	stride := params.PreviewFrameStride
	if stride <= 0 {
		stride = 1
	}

	var marks []domain.TransitionMark
	marksByFrame := map[int64]domain.TransitionMark{}
	scanFrames := 0
	for i := range spans {
		if i == 0 {
			continue
		}
		scanFrames++
		if scanFrames > params.PreviewMaxTransitionScan && params.PreviewMaxTransitionScan > 0 {
			err := shardError(domain.CodeVal, "preview transition scan budget exceeded", shard)
			err.Field = "preview_max_transition_scan"
			err.Expected = params.PreviewMaxTransitionScan
			err.Actual = scanFrames
			return nil, err
		}
		mark, terr := transitionAtBoundary(shard, params, spans, i)
		if terr != nil {
			return nil, terr
		}
		if mark != nil {
			marks = append(marks, *mark)
			marksByFrame[mark.Frame] = *mark
		}
	}

	indexSet := map[int64]struct{}{}
	for f := int64(0); f < totalFrames; f += int64(stride) {
		indexSet[f] = struct{}{}
	}
	if totalFrames > 0 {
		indexSet[totalFrames-1] = struct{}{}
	}
	for _, span := range spans {
		if span.start >= 0 && span.start < totalFrames {
			indexSet[span.start] = struct{}{}
		}
	}
	indices := make([]int64, 0, len(indexSet))
	for frame := range indexSet {
		indices = append(indices, frame)
	}
	indices = sortedInt64(indices)
	sampled := make([]domain.Frame, 0, len(indices))
	for _, f := range indices {
		sampled = append(sampled, previewFrameAt(f, shard, spans, marksByFrame))
	}
	evaluated := int64(len(sampled))

	elapsed := time.Since(start)
	budgetMS := params.PreviewLatencyBudgetMS
	budgetMicros := int64(budgetMS) * 1000
	elapsedMicros := elapsed.Microseconds()
	occupancy := int64(0)
	if budgetMicros > 0 {
		occupancy = elapsedMicros * 10000 / budgetMicros
	}
	previewFPS := params.FPS / stride
	if previewFPS <= 0 {
		previewFPS = 1
	}
	density := int64(0)
	if totalFrames > 0 {
		density = evaluated * 10000 / totalFrames
	}
	return &domain.PreviewResult{
		ShardID: shard.ID, Params: params, SampledFrames: sampled, Transitions: marks,
		Stats: domain.PreviewStats{
			SourceFPS: params.FPS, PreviewFPS: previewFPS, FrameStride: stride,
			SampleDensityPermyriad: int(density), TotalFrames: totalFrames,
			EvaluatedFrames: evaluated, SkippedFrames: totalFrames - evaluated,
			MaxTransitionScan: params.PreviewMaxTransitionScan, ActualTransitionScan: scanFrames,
			ElapsedMicros: elapsedMicros, BudgetMillis: budgetMS,
			BudgetOccupancyPermyriad: occupancy, WithinBudget: elapsedMicros <= budgetMicros,
			Basis: "wall_clock_microseconds; integer work_units=evaluated_frames+actual_transition_scan; latency_budget_ms",
		},
	}, nil
}

func previewFrameAt(f int64, shard domain.Shard, spans []phonemeFrameSpan, marks map[int64]domain.TransitionMark) domain.Frame {
	for _, span := range spans {
		if f >= span.start && f < span.end {
			frame := domain.Frame{
				Index: f, DialogueID: shard.DialogueID, ShardID: shard.ID, PackIndex: span.pack,
				PhonemeID: span.phoneme.ID, Category: span.phoneme.Category,
				Mouth: MouthForCategory(span.phoneme.Category), Kind: domain.FrameStable, Sampled: true,
			}
			if mark, ok := marks[f]; ok {
				frame.Kind = domain.FrameTransition
				frame.Mouth = mark.Bridge
				frame.TransitionFrom = mark.From
				frame.TransitionTo = mark.To
				frame.FamilySpan = mark.FamilySpan
			}
			return frame
		}
	}
	return domain.Frame{Index: f, ShardID: shard.ID, Kind: domain.FrameStable, Mouth: domain.MouthSilence, Sampled: true}
}

func ComparePreviewExact(preview *domain.PreviewResult, exact *domain.ShardResult) error {
	exactFrames := map[int64]domain.Frame{}
	for _, frame := range exact.Frames {
		exactFrames[frame.Index] = frame
	}
	for _, pf := range preview.SampledFrames {
		ef, ok := exactFrames[pf.Index]
		if !ok {
			return divergenceError(pf.Index, pf.PackIndex, "frame_index", pf, nil)
		}
		if ef.Mouth != pf.Mouth || ef.Category != pf.Category || ef.Kind != pf.Kind {
			pack := ef.PackIndex
			return divergenceError(pf.Index, pack, "mouth/category/kind", summarizeFrame(pf), summarizeFrame(ef))
		}
		if ef.TransitionFrom != pf.TransitionFrom || ef.TransitionTo != pf.TransitionTo {
			return divergenceError(pf.Index, ef.PackIndex, "transition_endpoints", summarizeFrame(pf), summarizeFrame(ef))
		}
	}

	exactMarks := map[int64]domain.TransitionMark{}
	for _, mark := range exact.Transitions {
		exactMarks[mark.Frame] = mark
	}
	previewMarks := map[int64]domain.TransitionMark{}
	for _, mark := range preview.Transitions {
		previewMarks[mark.Frame] = mark
	}
	var frames []int64
	for f := range exactMarks {
		frames = append(frames, f)
	}
	for f := range previewMarks {
		if _, ok := exactMarks[f]; !ok {
			frames = append(frames, f)
		}
	}
	frames = sortedInt64(frames)
	for _, f := range frames {
		em, eok := exactMarks[f]
		pm, pok := previewMarks[f]
		if eok != pok {
			pack := 0
			if eok {
				pack = em.PackIndex
			} else {
				pack = pm.PackIndex
			}
			return divergenceError(f, pack, "transition_insertion_presence", pm, em)
		}
		if eok && pok && (em.Bridge != pm.Bridge || em.From != pm.From || em.To != pm.To || em.PackIndex != pm.PackIndex) {
			return divergenceError(f, em.PackIndex, "transition_insertion_position_or_value", pm, em)
		}
	}
	return nil
}

func sortedInt64(values []int64) []int64 {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return values
}

func summarizeFrame(f domain.Frame) map[string]any {
	return map[string]any{
		"mouth": f.Mouth, "category": f.Category, "kind": f.Kind,
		"from": f.TransitionFrom, "to": f.TransitionTo, "pack": f.PackIndex,
	}
}

func divergenceError(frame int64, pack int, field string, preview, exact any) error {
	packCopy := pack
	return &domain.Error{
		Code: domain.CodeDiverge, Message: "preview and exact dispatch diverged",
		Frame: &frame, PackIndex: &packCopy, Field: field,
		Expected: exact, Actual: preview,
	}
}

func DispatchRecords(result *domain.ShardResult) []domain.DispatchRecord {
	records := make([]domain.DispatchRecord, 0, len(result.Frames))
	for _, frame := range result.Frames {
		records = append(records, domain.DispatchRecord{
			DialogueID: result.DialogueID, ShardID: result.ShardID, FrameIndex: frame.Index,
			PackIndex: frame.PackIndex, ChannelID: result.ChannelID, Mouth: frame.Mouth,
			Kind: frame.Kind,
		})
	}
	return records
}
