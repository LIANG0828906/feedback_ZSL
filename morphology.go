package maskqa

import "fmt"

func radiusAt(rf *IntField, x, y int) int {
	r := rf.At(x, y)
	if r <= 0 {
		return 1
	}
	return r
}

func AdaptiveDilate(src *Mask, rf *IntField) *Mask {
	dst := NewMask(src.Width, src.Height)
	for y := 0; y < src.Height; y++ {
		for x := 0; x < src.Width; x++ {
			r := radiusAt(rf, x, y)
			found := byte(0)
			for _, off := range DiskOffsets(r) {
				if src.At(x+off.dx, y+off.dy) == 1 {
					found = 1
					break
				}
			}
			dst.Set(x, y, found)
		}
	}
	return dst
}

func AdaptiveErode(src *Mask, rf *IntField) (*Mask, []int, []int) {
	dst := NewMask(src.Width, src.Height)
	votes := make([]int, src.Width*src.Height)
	areas := make([]int, src.Width*src.Height)
	for y := 0; y < src.Height; y++ {
		for x := 0; x < src.Width; x++ {
			r := radiusAt(rf, x, y)
			offsets := DiskOffsets(r)
			fg := 0
			for _, off := range offsets {
				if src.At(x+off.dx, y+off.dy) == 1 {
					fg++
				}
			}
			idx := y*src.Width + x
			votes[idx] = fg
			areas[idx] = len(offsets)
			if fg == len(offsets) {
				dst.Data[idx] = 1
			}
		}
	}
	return dst, votes, areas
}

func AdaptiveClose(src *Mask, rf *IntField) *Mask {
	dilated := AdaptiveDilate(src, rf)
	eroded, _, _ := AdaptiveErode(dilated, rf)
	return eroded
}

func AdaptiveOpenEvidence(src *Mask, rf *IntField) (*Mask, []int, []int) {
	eroded, votes, areas := AdaptiveErode(src, rf)
	dilated := AdaptiveDilate(eroded, rf)
	return dilated, votes, areas
}

func AdaptiveOpen(src *Mask, rf *IntField) *Mask {
	out, _, _ := AdaptiveOpenEvidence(src, rf)
	return out
}

func RunOpenIterations(rough, firstClosed *Mask, rf *IntField, p Params, block BlockCoord) (*OpenOutput, error) {
	if p.MaxIterations < 1 {
		return nil, paraError("max_iterations", 1, 30, p.MaxIterations, "iteration limit must be positive")
	}
	out := &OpenOutput{Block: block, Mask: NewMask(block.Width, block.Height), MinChanges: int(^uint(0) >> 1)}
	previousFinal := rough.Clone()
	current := firstClosed.Clone()
	var lastVotes, lastAreas []int
	for round := 1; round <= p.MaxIterations; round++ {
		var closed *Mask
		var closeChanges int
		if round == 1 {
			closed = firstClosed.Clone()
			closeChanges = XORCount(rough, closed)
		} else {
			beforeClose := current.Clone()
			closed = AdaptiveClose(beforeClose, rf)
			closeChanges = XORCount(beforeClose, closed)
		}
		opened, votes, areas := AdaptiveOpenEvidence(closed, rf)
		lastVotes, lastAreas = votes, areas
		openChanges := XORCount(closed, opened)
		roundChanges := XORCount(previousFinal, opened)
		record := RoundRecord{Round: round, CloseChanges: closeChanges, OpenChanges: openChanges, RoundChanges: roundChanges}
		out.Rounds = append(out.Rounds, record)
		if roundChanges < out.MinChanges {
			out.MinChanges = roundChanges
		}
		previousFinal = opened.Clone()
		current = opened
		if roundChanges <= p.ChangeThreshold {
			record.Converged = true
			out.Rounds[len(out.Rounds)-1].Converged = true
			out.Converged = true
			break
		}
	}

	for ly := 0; ly < block.Height; ly++ {
		for lx := 0; lx < block.Width; lx++ {
			x, y := block.OriginX+lx, block.OriginY+ly
			globalIdx := y*rough.Width + x
			localIdx := ly*block.Width + lx
			out.Mask.Data[localIdx] = current.At(x, y)
			area := 1
			vote := 0
			if lastAreas != nil {
				area = lastAreas[globalIdx]
				vote = lastVotes[globalIdx]
			}
			rem := (int64(vote) * p.FixedScale) % int64(area)
			micro := rem * p.TraceScale / int64(area)
			trace := PixelTrace{
				X: x, Y: y, BlockX: block.IndexX, BlockY: block.IndexY,
				MorphologyMicro: micro, MorphForegroundVotes: vote, MorphKernelArea: area,
				Truncations: []Truncation{{
					Level: "morphology", X: x, Y: y, Operation: "disk_accumulator_coverage_div",
					Quotient: int64(vote) * p.FixedScale / int64(area), Remainder: rem, Denominator: int64(area),
					ContributionMicro: micro, TruncatedAt: "fixed_scale*foreground_votes/kernel_area",
				}},
			}
			out.Traces = append(out.Traces, trace)
		}
	}
	if !out.Converged {
		return out, &QAError{Code: EIter, Field: "max_iterations", ObjectID: block.ID(), Message: fmt.Sprintf("morphology did not converge after %d rounds", p.MaxIterations), Round: p.MaxIterations, MinChanges: out.MinChanges, Actual: p.MaxIterations}
	}
	return out, nil
}

func ComposeBlock(openMask, closeMask, rough *Mask, p Params, block BlockCoord) (*ComposeOutput, error) {
	totalWeight := p.ComposeOpenWeight + 2*p.ComposeAuxWeight
	if totalWeight <= 0 {
		return nil, paraError("compose_weights", "positive total", "positive total", totalWeight, "composition weight sum must be positive")
	}
	out := &ComposeOutput{Block: block, Mask: NewMask(block.Width, block.Height)}
	for ly := 0; ly < block.Height; ly++ {
		for lx := 0; lx < block.Width; lx++ {
			x, y := block.OriginX+lx, block.OriginY+ly
			openV := int(openMask.At(x, y))
			closeV := int(closeMask.At(x, y))
			roughV := int(rough.At(x, y))
			weighted := openV*p.ComposeOpenWeight + (closeV+roughV)*p.ComposeAuxWeight
			scoreNum := int64(weighted) * 1000
			score := scoreNum / int64(totalWeight)
			rem := scoreNum % int64(totalWeight)
			micro := rem * p.TraceScale / int64(totalWeight)
			bit := byte(0)
			if int(score) >= p.ComposeThresholdPermille {
				bit = 1
			}
			out.Mask.Set(lx, ly, bit)
			trace := PixelTrace{
				X: x, Y: y, BlockX: block.IndexX, BlockY: block.IndexY,
				CompositionMicro: micro,
				Truncations: []Truncation{{
					Level: "composition", X: x, Y: y, Operation: "weighted_candidate_permille_div",
					Quotient: score, Remainder: rem, Denominator: int64(totalWeight),
					ContributionMicro: micro, TruncatedAt: "weighted_votes_1000/total_weight",
				}},
			}
			out.Traces = append(out.Traces, trace)
		}
	}
	return out, nil
}
