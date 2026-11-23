// Package envelope detects speech-segment boundaries from the PCM
// envelope. All positions are integer samples; detected boundaries are
// quantised to the frame-hop grid and every quantisation error is
// recorded for the error-propagation report.
package envelope

import (
	"fmt"

	"captionx/internal/errs"
	"captionx/internal/model"
)

// DetectedSegment is one observed speech region.
type DetectedSegment struct {
	Start model.BoundaryObservation
	End   model.BoundaryObservation
}

// Detection holds the full detection outcome for a project.
type Detection struct {
	Segments []DetectedSegment // parallel to the manifest segments
}

// ValidateFrameParams enforces the legal frame-geometry ranges, returning
// E_PARA with the legal range on violation.
func ValidateFrameParams(fp model.FrameParams) error {
	if fp.SampleRate < 1000 || fp.SampleRate > 384000 {
		return errs.Para("frame.sampleRate",
			fmt.Sprintf("must be in [1000,384000], got %d", fp.SampleRate))
	}
	if fp.FrameLength < 16 || fp.FrameLength > fp.SampleRate {
		return errs.Para("frame.frameLength",
			fmt.Sprintf("must be in [16,%d], got %d", fp.SampleRate, fp.FrameLength))
	}
	if fp.FrameHop < 1 || fp.FrameHop > fp.FrameLength {
		return errs.Para("frame.frameHop",
			fmt.Sprintf("must be in [1,%d], got %d", fp.FrameLength, fp.FrameHop))
	}
	return nil
}

// frameEnergies computes the mean absolute amplitude per frame. Frame i
// covers samples [i*hop, i*hop+frameLen).
func frameEnergies(samples []int16, frameLen, hop int64) []int64 {
	n := int64(len(samples))
	if n < frameLen {
		return nil
	}
	nFrames := (n-frameLen)/hop + 1
	out := make([]int64, nFrames)
	for f := int64(0); f < nFrames; f++ {
		base := f * hop
		var acc int64
		for i := base; i < base+frameLen; i++ {
			v := int64(samples[i])
			if v < 0 {
				v = -v
			}
			acc += v
		}
		out[f] = acc / frameLen
	}
	return out
}

// quantize rounds raw to the nearest hop multiple and reports the error.
func quantize(raw, hop int64) (q, quantErr int64) {
	q = (raw + hop/2) / hop * hop
	quantErr = q - raw
	if quantErr < 0 {
		quantErr = -quantErr
	}
	return q, quantErr
}

// Detect locates speech segments in samples and matches them, in timeline
// order, to the declared manifest segments. Boundaries are first estimated
// at sample resolution inside the threshold-crossing frame, then quantised
// to the hop grid; each quantisation error is recorded.
func Detect(samples []int16, fp model.FrameParams, declared []model.SegmentManifest) (*Detection, error) {
	if err := ValidateFrameParams(fp); err != nil {
		return nil, err
	}
	energies := frameEnergies(samples, fp.FrameLength, fp.FrameHop)
	if len(energies) == 0 {
		return nil, errs.Para("audio.samples", "audio shorter than one frame")
	}
	var maxE, maxAbs int64
	for _, e := range energies {
		if e > maxE {
			maxE = e
		}
	}
	for _, s := range samples {
		v := int64(s)
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
	}
	frameThr := maxE / 8
	sampleThr := maxAbs / 16

	type run struct{ first, last int64 } // frame indices, inclusive
	var runs []run
	for i, e := range energies {
		if e > frameThr {
			if len(runs) > 0 && int64(i)-runs[len(runs)-1].last <= 2 {
				runs[len(runs)-1].last = int64(i) // bridge single-frame dips
			} else {
				runs = append(runs, run{int64(i), int64(i)})
			}
		}
	}
	if len(runs) != len(declared) {
		return nil, errs.Schema(-1, "segments",
			fmt.Sprintf("detected %d speech regions, manifest declares %d", len(runs), len(declared)))
	}

	det := &Detection{}
	for i, rn := range runs {
		// Sample-accurate refinement inside the boundary frames.
		rawStart := rn.first * fp.FrameHop
		endLimit := (rn.last+1)*fp.FrameHop + fp.FrameLength
		if endLimit > int64(len(samples)) {
			endLimit = int64(len(samples))
		}
		for s := rawStart; s < endLimit; s++ {
			if abs16(samples[s]) > sampleThr {
				rawStart = s
				break
			}
		}
		rawEnd := endLimit
		for s := endLimit - 1; s >= rawStart; s-- {
			if abs16(samples[s]) > sampleThr {
				rawEnd = s + 1
				break
			}
		}
		qs, es := quantize(rawStart, fp.FrameHop)
		qe, ee := quantize(rawEnd, fp.FrameHop)
		det.Segments = append(det.Segments, DetectedSegment{
			Start: model.BoundaryObservation{
				SegmentID: declared[i].ID, Kind: "start",
				Raw: rawStart, Quantized: qs, QuantError: es,
			},
			End: model.BoundaryObservation{
				SegmentID: declared[i].ID, Kind: "end",
				Raw: rawEnd, Quantized: qe, QuantError: ee,
			},
		})
	}
	return det, nil
}

func abs16(v int16) int64 {
	if v < 0 {
		return -int64(v)
	}
	return int64(v)
}
