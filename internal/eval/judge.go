package eval

import "semanticshading/internal/model"

type Judgment struct {
	Label       int    `json:"label"`
	Name        string `json:"name"`
	MeanQ8      int64  `json:"mean_q8"`
	ThresholdQ8 int64  `json:"threshold_q8"`
	Accepted    bool   `json:"accepted"`
}

func Judge(regions []model.RegionMetrics, thresholdQ8 int64) []Judgment {
	result := make([]Judgment, 0, len(regions))
	for _, region := range regions {
		result = append(result, Judgment{
			Label:       region.Label,
			Name:        region.Name,
			MeanQ8:      region.MeanDistanceQ8,
			ThresholdQ8: thresholdQ8,
			Accepted:    region.MeanDistanceQ8 <= thresholdQ8,
		})
	}
	return result
}

func AcceptedLabels(judgments []Judgment) []int {
	result := make([]int, 0)
	for _, judgment := range judgments {
		if judgment.Accepted {
			result = append(result, judgment.Label)
		}
	}
	return result
}
