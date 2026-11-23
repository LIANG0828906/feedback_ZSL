package engine

import (
	"maskpipeline/internal/model"
	"maskpipeline/internal/segment"
)

type segmentOptions struct {
	params            model.Params
	truthLabels       []byte
	truthAreas        map[int]int
	truthRects        map[int]model.Rect
	budget            int
	maxAreaRatio      float64
	outsideTruthRatio float64
}

func runSegment(im *model.Image, o segmentOptions) (*segment.Result, error) {
	return segment.Run(im, segment.Options{
		Params:            o.params,
		TruthLabels:       o.truthLabels,
		TruthAreas:        o.truthAreas,
		TruthRects:        o.truthRects,
		IterationBudget:   o.budget,
		MaxAreaRatio:      o.maxAreaRatio,
		OutsideTruthRatio: o.outsideTruthRatio,
	})
}
