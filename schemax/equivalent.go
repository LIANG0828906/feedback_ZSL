package schemax

import (
	"fmt"
	"reflect"

	"iconflow/model"
)

func SemanticDifferences(want, got *model.Project) []string {
	var diffs []string
	if want.ProjectID != got.ProjectID {
		diffs = append(diffs, "project_id")
	}
	if want.Seed != got.Seed {
		diffs = append(diffs, "seed")
	}
	if want.BlockWidth != got.BlockWidth || want.BlockHeight != got.BlockHeight {
		diffs = append(diffs, "block_size")
	}
	if want.ThresholdPermyriad != got.ThresholdPermyriad {
		diffs = append(diffs, fmt.Sprintf("threshold %d != %d", want.ThresholdPermyriad, got.ThresholdPermyriad))
	}
	if !reflect.DeepEqual(want.ChannelOrder, got.ChannelOrder) {
		diffs = append(diffs, "channel_order")
	}
	if want.Quant != got.Quant {
		diffs = append(diffs, fmt.Sprintf("quant %+v != %+v", want.Quant, got.Quant))
	}
	if !reflect.DeepEqual(want.Rules, got.Rules) {
		diffs = append(diffs, "rules")
	}
	if len(want.Images) != len(got.Images) {
		diffs = append(diffs, "image_count")
		return diffs
	}
	for i := range want.Images {
		w := want.Images[i]
		g := got.Images[i]
		if w.ID != g.ID || w.Raster.Width != g.Raster.Width || w.Raster.Height != g.Raster.Height {
			diffs = append(diffs, "image_metadata:"+w.ID)
			continue
		}
		if !reflect.DeepEqual(w.Raster.Flat, g.Raster.Flat) {
			diffs = append(diffs, "raster_flat:"+w.ID)
		}
	}
	if len(want.Truth) != len(got.Truth) {
		diffs = append(diffs, "truth_count")
	} else {
		for key, wantTruth := range want.Truth {
			gotTruth := got.Truth[key]
			if gotTruth == nil || wantTruth.Foreground != gotTruth.Foreground ||
				len(wantTruth.Components) != len(gotTruth.Components) {
				diffs = append(diffs, "truth:"+key)
			}
		}
	}
	return diffs
}

func Equivalent(want, got *model.Project) bool {
	return len(SemanticDifferences(want, got)) == 0
}
