package segment

import (
	"iconbench/internal/imaging"
	"iconbench/internal/model"
)

func Segment(rgb *imaging.RGBImage, p model.Params) (*Result, error) {
	if err := model.ValidateParams(p); err != nil {
		return nil, err
	}
	w, h := rgb.Width, rgb.Height
	smoothed := imaging.GaussianBlurRGB(rgb, p.SmoothKernel)
	gradient := imaging.GradientMagnitude(smoothed, p.GradientOperator)
	markers := BuildMarkers(rgb, p)
	samples, initial := Flood(gradient, markers, w, h, p.FloodThreshold)
	merged, err := MergeRegions(initial, gradient, markers, w, h, p, 0)
	if err != nil {
		return nil, err
	}
	rawMask := BuildRawMask(initial, merged, markers)

	return &Result{
		Width: w, Height: h,
		Gradient:         gradient,
		Markers:          markers,
		InitialLabels:    initial,
		Labels:           merged.Labels,
		Regions:          merged.Regions,
		Edges:            merged.Edges,
		MergeLog:         merged.MergeLog,
		RawMask:          rawMask,
		FloodSamples:     samples,
		QueueSampleRatio: float64(samples) / float64(w*h),
	}, nil
}

func Flood(gradient []float64, markers MarkerResult, w, h int, threshold float64) (int, []int32) {
	return priorityFlood(gradient, markers.Labels, w, h, threshold)
}

func BuildRawMask(initial []int32, merged *MergeOutput, markers MarkerResult) []byte {
	rawMask := make([]byte, len(initial))
	classByRoot := map[int32]uint8{}
	for _, r := range merged.Regions {
		classByRoot[r.Label] = r.Class
	}
	for i, root := range merged.Labels {
		if root > 0 && classByRoot[root] == 1 {
			rawMask[i] = 1
		}
		if initial[i] == 0 && markers.Residual[i] >= markers.FGThreshold {
			rawMask[i] = 1
		}
	}
	return rawMask
}
