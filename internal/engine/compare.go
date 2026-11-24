package engine

import (
	"fmt"
	"math"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

// Compare performs an exact frame-ledger comparison. The first mismatch is
// reported with frame, block and field values from both sides.
func Compare(left, right *model.RunReport) error {
	if left == nil || right == nil {
		return apperr.New(apperr.Diff, "cannot compare a nil verification report")
	}
	if len(left.Frames) != len(right.Frames) {
		frame := min(len(left.Frames), len(right.Frames))
		e := apperr.New(apperr.Diff, "verification paths produced different frame ledger lengths")
		e.Field = "frame_count"
		e.Frame = frame
		e.Left = len(left.Frames)
		e.Right = len(right.Frames)
		return e
	}
	for i := range left.Frames {
		a, b := &left.Frames[i], &right.Frames[i]
		if a.Summary != b.Summary {
			if block, ok := firstDifferentBlock(a, b); ok {
				return diffAtBlock(i, block.ref(), "block_hash", block.leftHash, block.rightHash)
			}
			return diffAt(i, 0, "summary", a.Summary, b.Summary)
		}
		if a.Verdict != b.Verdict {
			return diffAtBlock(i, firstAbnormalBlock(a), "verdict", a.Verdict, b.Verdict)
		}
		if !NearlyEqual(a.ResidualEnergy, b.ResidualEnergy) {
			return diffAt(i, 0, "residual_energy", a.ResidualEnergy, b.ResidualEnergy)
		}
		if !NearlyEqual(a.ResidualEnergyNext, b.ResidualEnergyNext) {
			return diffAt(i, 0, "residual_energy_next", a.ResidualEnergyNext, b.ResidualEnergyNext)
		}
		if !NearlyEqual(a.NoiseDispersion, b.NoiseDispersion) {
			return diffAt(i, 0, "noise_dispersion", a.NoiseDispersion, b.NoiseDispersion)
		}
		if !NearlyEqual(a.NoiseMean, b.NoiseMean) {
			return diffAt(i, 0, "noise_mean", a.NoiseMean, b.NoiseMean)
		}
		if !NearlyEqual(a.BBoxShiftPixels, b.BBoxShiftPixels) {
			return diffAt(i, 0, "bbox_shift_pixels", a.BBoxShiftPixels, b.BBoxShiftPixels)
		}
		if a.ContentHash != b.ContentHash {
			return diffAt(i, 0, "content_hash", a.ContentHash, b.ContentHash)
		}
		if len(a.Blocks) != len(b.Blocks) {
			return diffAt(i, 0, "block_count", len(a.Blocks), len(b.Blocks))
		}
		for j := range a.Blocks {
			if a.Blocks[j].Hash != b.Blocks[j].Hash {
				block := a.Blocks[j]
				return diffAtBlock(i, model.BlockRef{Col: block.Col, Row: block.Row, X: block.X, Y: block.Y, W: block.W, H: block.H}, "block_hash", a.Blocks[j].Hash, b.Blocks[j].Hash)
			}
			if math.Abs(a.Blocks[j].Residual-b.Blocks[j].Residual) > 1e-12 {
				block := a.Blocks[j]
				return diffAtBlock(i, model.BlockRef{Col: block.Col, Row: block.Row, X: block.X, Y: block.Y, W: block.W, H: block.H}, "block_residual", a.Blocks[j].Residual, b.Blocks[j].Residual)
			}
			if math.Abs(a.Blocks[j].NoiseVariance-b.Blocks[j].NoiseVariance) > 1e-12 {
				block := a.Blocks[j]
				return diffAtBlock(i, model.BlockRef{Col: block.Col, Row: block.Row, X: block.X, Y: block.Y, W: block.W, H: block.H}, "block_noise_variance", a.Blocks[j].NoiseVariance, b.Blocks[j].NoiseVariance)
			}
		}
	}
	return nil
}

type blockDiff struct {
	block     model.BlockRef
	leftHash  string
	rightHash string
}

func (d blockDiff) ref() model.BlockRef { return d.block }

func firstDifferentBlock(a, b *model.FrameResult) (blockDiff, bool) {
	limit := min(len(a.Blocks), len(b.Blocks))
	for i := 0; i < limit; i++ {
		if a.Blocks[i].Hash != b.Blocks[i].Hash {
			block := a.Blocks[i]
			return blockDiff{
				block:    model.BlockRef{Col: block.Col, Row: block.Row, X: block.X, Y: block.Y, W: block.W, H: block.H},
				leftHash: a.Blocks[i].Hash, rightHash: b.Blocks[i].Hash,
			}, true
		}
	}
	return blockDiff{}, false
}

func firstAbnormalBlock(frame *model.FrameResult) model.BlockRef {
	if len(frame.AbnormalBlocks) > 0 {
		return frame.AbnormalBlocks[0]
	}
	if len(frame.ChangedBlocks) > 0 {
		return frame.ChangedBlocks[0]
	}
	return model.BlockRef{}
}

func diffAt(frame, block int, field string, left, right any) error {
	return diffAtBlock(frame, model.BlockRef{Col: block, Row: block, X: 0, Y: 0, W: 0, H: 0}, field, left, right)
}

func diffAtBlock(frame int, block model.BlockRef, field string, left, right any) error {
	e := apperr.New(apperr.Diff, "verification paths disagree")
	e.Field = field
	e.Frame = frame
	e.Block = block.AppErr()
	e.Left = left
	e.Right = right
	e.Details = map[string]any{
		"frame": frame,
		"block": fmt.Sprintf("col=%d,row=%d,x=%d,y=%d", block.Col, block.Row, block.X, block.Y),
		"field": field,
	}
	return e
}
