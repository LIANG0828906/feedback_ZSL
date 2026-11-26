package pipeline

import (
	"fmt"

	"msworkbench/internal/e"
	"msworkbench/internal/quant"
)

// CompareReports compares quantized band pixels, reconstruction, method and
// stable stage sequence. The first mismatch is returned as E_DIFF.
func CompareReports(want, got *Report) error {
	if len(want.Blocks) != len(got.Blocks) {
		return &e.Error{Code: e.Diff, Expected: fmt.Sprintf("%d blocks", len(want.Blocks)), Actual: fmt.Sprintf("%d blocks", len(got.Blocks)), Message: "block count mismatch"}
	}
	for i := range want.Blocks {
		wb, gb := want.Blocks[i], got.Blocks[i]
		block := wb.Block.String()
		if wb.Block != gb.Block {
			return &e.Error{Code: e.Diff, Block: block, Expected: wb.Block.String(), Actual: gb.Block.String(), Message: "block order mismatch"}
		}
		if wb.Quant.Decision.Method != gb.Quant.Decision.Method {
			return &e.Error{Code: e.Diff, Block: block, Field: "method", Expected: string(wb.Quant.Decision.Method), Actual: string(gb.Quant.Decision.Method), Message: "selected quantization method mismatch"}
		}
		for bandID, wbr := range wb.Quant.Bands {
			gbr := gb.Quant.Bands[bandID]
			if gbr == nil {
				return &e.Error{Code: e.Diff, Block: block, Band: bandID, Message: "band missing in candidate"}
			}
			for px := 0; px < len(wbr.Values); px++ {
				if wbr.Values[px] != gbr.Values[px] {
					return &e.Error{Code: e.Diff, Block: block, Band: bandID, Field: fmt.Sprintf("pixel[%d]", px), Expected: fmt.Sprint(wbr.Values[px]), Actual: fmt.Sprint(gbr.Values[px]), Message: "quantized pixel mismatch"}
				}
			}
		}
		for px := 0; px < len(wb.Recon.Pixels); px++ {
			if wb.Recon.Pixels[px].LinearBP != gb.Recon.Pixels[px].LinearBP {
				band := firstDifferentBand(wb.Quant, gb.Quant, px)
				return &e.Error{Code: e.Diff, Block: block, Band: band, Field: fmt.Sprintf("recon.pixel[%d].linear_bp", px), Expected: fmt.Sprint(wb.Recon.Pixels[px].LinearBP), Actual: fmt.Sprint(gb.Recon.Pixels[px].LinearBP), Message: "reconstructed chromaticity mismatch"}
			}
		}
	}
	if len(want.LogicalSequence) != len(got.LogicalSequence) {
		return &e.Error{Code: e.Diff, Field: "logical_sequence", Expected: fmt.Sprint(len(want.LogicalSequence)), Actual: fmt.Sprint(len(got.LogicalSequence)), Message: "logical commit sequence length mismatch"}
	}
	for i := range want.LogicalSequence {
		if want.LogicalSequence[i] != got.LogicalSequence[i] {
			return &e.Error{Code: e.Diff, Field: "logical_sequence", Expected: want.LogicalSequence[i], Actual: got.LogicalSequence[i], Message: "commit sequence mismatch at position"}
		}
	}
	ws := physicalSequence(want)
	gs := physicalSequence(got)
	if len(ws) != len(gs) {
		return &e.Error{Code: e.Diff, Field: "physical_sequence", Expected: fmt.Sprint(ws), Actual: fmt.Sprint(gs), Message: "physical stage sequence length mismatch"}
	}
	for i := range ws {
		if ws[i] != gs[i] {
			return &e.Error{Code: e.Diff, Field: "physical_sequence", Expected: ws[i], Actual: gs[i], Message: "physical commit order mismatch"}
		}
	}
	return nil
}

// ComparePixels compares only pixel quanta and reconstruction. It is used
// across sparse/medium/dense tiers, whose method markers intentionally differ.
func ComparePixels(want, got *Report) error {
	if len(want.Blocks) != len(got.Blocks) {
		return &e.Error{Code: e.Diff, Expected: fmt.Sprintf("%d blocks", len(want.Blocks)), Actual: fmt.Sprintf("%d blocks", len(got.Blocks)), Message: "block count mismatch"}
	}
	for i := range want.Blocks {
		wb, gb := want.Blocks[i], got.Blocks[i]
		block := wb.Block.String()
		if wb.Block != gb.Block {
			return &e.Error{Code: e.Diff, Block: block, Expected: wb.Block.String(), Actual: gb.Block.String(), Message: "block order mismatch"}
		}
		for bandID, wbr := range wb.Quant.Bands {
			gbr := gb.Quant.Bands[bandID]
			if gbr == nil {
				return &e.Error{Code: e.Diff, Block: block, Band: bandID, Message: "band missing in candidate"}
			}
			for px := 0; px < len(wbr.Values); px++ {
				if wbr.Values[px] != gbr.Values[px] {
					return &e.Error{Code: e.Diff, Block: block, Band: bandID, Field: fmt.Sprintf("pixel[%d]", px), Expected: fmt.Sprint(wbr.Values[px]), Actual: fmt.Sprint(gbr.Values[px]), Message: "cross-tier quantized pixel mismatch"}
				}
			}
		}
		for px := 0; px < len(wb.Recon.Pixels); px++ {
			if wb.Recon.Pixels[px].LinearBP != gb.Recon.Pixels[px].LinearBP {
				band := firstDifferentBand(wb.Quant, gb.Quant, px)
				return &e.Error{Code: e.Diff, Block: block, Band: band, Field: fmt.Sprintf("recon.pixel[%d]", px), Message: "cross-tier reconstruction mismatch"}
			}
		}
	}
	return nil
}

func physicalSequence(r *Report) []string {
	out := make([]string, 0, len(r.Records))
	for _, rec := range r.Records {
		out = append(out, rec.Block+"/"+rec.Stage)
	}
	return out
}

func firstDifferentBand(want, got *quant.Result, pixel int) string {
	for bandID, wbr := range want.Bands {
		gbr := got.Bands[bandID]
		if gbr == nil || pixel >= len(wbr.Values) || pixel >= len(gbr.Values) {
			return bandID
		}
		if wbr.Values[pixel] != gbr.Values[pixel] {
			return bandID
		}
	}
	return "_reconstruction"
}
