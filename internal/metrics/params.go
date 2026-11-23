// Package metrics implements PSNR, fixed-point SSIM and per-block error
// statistics, including window parameter validation and the overlap-band
// rules used when merging block-level evaluations back into a frame result.
package metrics

import (
	"vqa/internal/errs"
)

func errBand(required, actual int) *errs.Error {
	return errs.New(errs.EBand, "overlap band narrower than required").
		WithInt("required_width", int64(required)).
		WithInt("actual_width", int64(actual)).
		With("field", "halo")
}

// Params is the frozen parameter snapshot recorded with every committed block.
type Params struct {
	Version     int `json:"version"`
	Window      int `json:"window"`
	Stride      int `json:"stride"`
	BlockSize   int `json:"block_size"`
	FrameWidth  int `json:"frame_width"`
	FrameHeight int `json:"frame_height"`
	Channels    int `json:"channels"`
	Halo        int `json:"halo"`
}

// ParamVersion is bumped whenever the meaning of a parameter field changes.
const ParamVersion = 1

// DefaultParams returns the standard evaluation parameters.
func DefaultParams(blockSize, w, h, ch int) Params {
	return Params{
		Version:     ParamVersion,
		Window:      7,
		Stride:      2,
		BlockSize:   blockSize,
		FrameWidth:  w,
		FrameHeight: h,
		Channels:    ch,
		Halo:        8,
	}
}

// Validate enforces window/stride bounds (E_PARA) and the overlap-band
// relationship (E_BAND). The band must extend at least window-1 pixels on each
// side so every SSIM window touching a block edge is fully owned by one block.
func (p Params) Validate() error {
	bad := func(field string, value int) *errs.Error {
		return errs.New(errs.EPara, "evaluation parameter out of range").
			With("field", field).WithInt("value", int64(value))
	}
	if p.Window <= 0 || p.Window > p.BlockSize {
		return bad("window", p.Window)
	}
	if p.Stride <= 0 || p.Stride > p.Window {
		return bad("stride", p.Stride)
	}
	if p.Window > p.FrameWidth || p.Window > p.FrameHeight {
		return bad("window", p.Window)
	}
	if p.BlockSize <= 0 || p.FrameWidth%p.BlockSize != 0 || p.FrameHeight%p.BlockSize != 0 {
		return bad("block_size", p.BlockSize)
	}
	required := p.RequiredHalo()
	if p.Halo < required {
		return errs.New(errs.EBand, "overlap band narrower than required").
			WithInt("required_width", int64(required)).
			WithInt("actual_width", int64(p.Halo)).
			With("field", "halo")
	}
	return nil
}

// RequiredHalo is the minimum per-side overlap band implied by the window and
// stride. Windows whose top-left lies up to window-1 pixels before the block
// origin still intersect the block, hence the band follows the window.
func (p Params) RequiredHalo() int {
	required := p.Window - 1
	if p.Stride > required {
		required = p.Stride
	}
	return required
}
