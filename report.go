package maskqa

import "sort"

func BuildErrorReport(s *Sample, final *Mask, gradients []*GradientOutput, edges []*EdgeOutput, radii []*RadiusOutput, opens []*OpenOutput, composes []*ComposeOutput, p Params, includePixels bool) (*ErrorReport, error) {
	count := s.Width * s.Height
	merged := make([]PixelTrace, count)
	for y := 0; y < s.Height; y++ {
		for x := 0; x < s.Width; x++ {
			idx := y*s.Width + x
			merged[idx].X = x
			merged[idx].Y = y
		}
	}
	merge := func(traces []PixelTrace) {
		for _, t := range traces {
			if t.X < 0 || t.Y < 0 || t.X >= s.Width || t.Y >= s.Height {
				continue
			}
			idx := t.Y*s.Width + t.X
			m := &merged[idx]
			m.X, m.Y = t.X, t.Y
			m.BlockX, m.BlockY = t.BlockX, t.BlockY
			m.GradientMicro += t.GradientMicro
			m.ThresholdMicro += t.ThresholdMicro
			m.RadiusMicro += t.RadiusMicro
			m.MorphologyMicro += t.MorphologyMicro
			m.CompositionMicro += t.CompositionMicro
			m.GradientMagnitude = t.GradientMagnitude
			if t.Threshold != 0 {
				m.Threshold = t.Threshold
			}
			if t.SegmentID != "" {
				m.SegmentID = t.SegmentID
				m.Radius = t.Radius
			}
			if t.MorphKernelArea != 0 {
				m.MorphKernelArea = t.MorphKernelArea
				m.MorphForegroundVotes = t.MorphForegroundVotes
			}
			m.Truncations = append(m.Truncations, t.Truncations...)
		}
	}
	for _, g := range gradients {
		if g != nil {
			merge(g.Traces)
		}
	}
	for _, e := range edges {
		if e != nil {
			merge(e.Traces)
		}
	}
	for _, r := range radii {
		if r != nil {
			merge(r.Traces)
		}
	}
	for _, o := range opens {
		if o != nil {
			merge(o.Traces)
		}
	}
	for _, c := range composes {
		if c != nil {
			merge(c.Traces)
		}
	}

	report := &ErrorReport{
		SampleID: s.ID, PixelCount: count,
		LevelContributionMicro: map[string]int64{},
		MaxByLevel:             map[string]PixelTrace{},
	}
	levels := []string{"gradient", "threshold", "radius", "morphology", "composition"}
	var globalBest int64 = -1
	for i := range merged {
		t := &merged[i]
		if t.BlockX == 0 && t.BlockY == 0 {
			for _, b := range s.Blocks {
				rect := Rect{X: b.OriginX, Y: b.OriginY, Width: b.Width, Height: b.Height}
				if rect.Contains(t.X, t.Y) {
					t.BlockX, t.BlockY = b.IndexX, b.IndexY
					break
				}
			}
		}
		t.finalize(p)
		for li, level := range levels {
			v := t.Contributions[li]
			report.LevelContributionMicro[level] += v
			report.FiveLevelSumMicro += v
			if current, ok := report.MaxByLevel[level]; !ok || v > levelValue(current, li) {
				report.MaxByLevel[level] = *t
			}
			if v > globalBest {
				globalBest = v
				cp := *t
				report.GlobalMax = &cp
			}
		}
	}
	report.ReferenceMismatchPixels = XORCount(final, s.Truth.Mask)
	report.ReferenceMismatchRate = float64(report.ReferenceMismatchPixels) / float64(count)
	report.InitialMismatchPixels = XORCount(s.Rough, s.Truth.Mask)
	report.InitialMismatchRate = float64(report.InitialMismatchPixels) / float64(count)
	if includePixels {
		report.Pixels = merged
	}
	return report, nil
}

func levelValue(t PixelTrace, idx int) int64 {
	if idx < 0 || idx >= len(t.Contributions) {
		return -1
	}
	return t.Contributions[idx]
}

func CompareRunReports(expected, actual *RunReport) *DiffError {
	if expected == nil || actual == nil {
		return &DiffError{Code: EDiff, First: Difference{Field: "report", Message: "nil report"}}
	}
	expSeq := canonicalRefs(expected.CommitSequence)
	actSeq := canonicalRefs(actual.CommitSequence)
	for i := 0; i < minInt(len(expSeq), len(actSeq)); i++ {
		if expSeq[i].SampleID != actSeq[i].SampleID || expSeq[i].Stage != actSeq[i].Stage || expSeq[i].BlockX != actSeq[i].BlockX || expSeq[i].BlockY != actSeq[i].BlockY {
			d := Difference{SampleID: expSeq[i].SampleID, Stage: expSeq[i].Stage, Field: "commit_sequence", Expected: expSeq[i], Actual: actSeq[i], Message: "first commit order mismatch"}
			d.Block = blockFromRef(expSeq[i])
			return &DiffError{Code: EDiff, First: d, Differences: []Difference{d}}
		}
		if expSeq[i].ContentHash != actSeq[i].ContentHash {
			d := Difference{SampleID: expSeq[i].SampleID, Stage: expSeq[i].Stage, Field: "content_hash", Expected: expSeq[i].ContentHash, Actual: actSeq[i].ContentHash, Message: "stage content hash mismatch"}
			d.Block = blockFromRef(expSeq[i])
			return &DiffError{Code: EDiff, First: d, Differences: []Difference{d}}
		}
	}
	if len(expSeq) != len(actSeq) {
		ref := expSeq[minInt(len(expSeq), len(actSeq))-1]
		d := Difference{Field: "commit_sequence_length", Expected: len(expSeq), Actual: len(actSeq), Message: "commit sequence length mismatch"}
		d.SampleID = ref.SampleID
		d.Stage = ref.Stage
		d.Block = blockFromRef(ref)
		return &DiffError{Code: EDiff, First: d}
	}
	for _, es := range expected.SampleReports {
		var as *SampleRunReport
		for _, candidate := range actual.SampleReports {
			if candidate.SampleID == es.SampleID {
				as = candidate
				break
			}
		}
		if as == nil {
			return &DiffError{Code: EDiff, First: Difference{SampleID: es.SampleID, Field: "sample_report", Message: "missing sample report"}}
		}
		if diff := firstMaskDifference(es.SampleID, es.Final, as.Final, expected.Params.BlockSize); diff != nil {
			return &DiffError{Code: EDiff, First: *diff, Differences: []Difference{*diff}}
		}
		if !equalComponents(es.Components, as.Components) {
			b, st := locateFirstPixelBlock(es.Final, as.Final, expected.Params.BlockSize)
			d := Difference{SampleID: es.SampleID, Block: b, Stage: st, Field: "components", Expected: es.Components, Actual: as.Components, Message: "connected component statistics mismatch"}
			return &DiffError{Code: EDiff, First: d}
		}
	}
	return nil
}

func canonicalRefs(in []CommitRef) []CommitRef {
	out := append([]CommitRef(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func blockFromRef(r CommitRef) BlockCoord {
	return BlockCoord{IndexX: r.BlockX, IndexY: r.BlockY, Width: 0, Height: 0}
}

func firstMaskDifference(sampleID string, a, b *Mask, blockSize int) *Difference {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return &Difference{SampleID: sampleID, Stage: StageCompose, Field: "mask_dimensions", Expected: dims(a), Actual: dims(b), Message: "mask dimensions differ"}
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			if a.At(x, y) != b.At(x, y) {
				block, stage := locateFirstPixelBlock(a, b, blockSize)
				return &Difference{SampleID: sampleID, Block: block, Stage: stage, Field: "mask", X: x, Y: y, Expected: a.At(x, y), Actual: b.At(x, y), Message: "first pixel mismatch"}
			}
		}
	}
	return nil
}

func dims(m *Mask) map[string]int {
	if m == nil {
		return nil
	}
	return map[string]int{"width": m.Width, "height": m.Height}
}

func locateFirstPixelBlock(a, b *Mask, blockSize int) (BlockCoord, Stage) {
	if a == nil || b == nil {
		return BlockCoord{}, StageCompose
	}
	if blockSize <= 0 {
		blockSize = 32
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			if a.At(x, y) != b.At(x, y) {
				bx, by := x/blockSize, y/blockSize
				return BlockCoord{IndexX: bx, IndexY: by, OriginX: bx * blockSize, OriginY: by * blockSize}, StageCompose
			}
		}
	}
	return BlockCoord{}, StageCompose
}

func equalComponents(a, b []Component) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
