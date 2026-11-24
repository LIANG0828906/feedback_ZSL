package segment

type Marker struct {
	ID    int32   `json:"id"`
	Class uint8   `json:"class"` // 1=foreground, 0=background
	X     int     `json:"x"`
	Y     int     `json:"y"`
	Score float64 `json:"score"`
	Area  int     `json:"area"`
}

type MarkerResult struct {
	Labels        []int32   `json:"labels"`
	Markers       []Marker  `json:"markers"`
	Residual      []float64 `json:"residual"`
	FGThreshold   float64   `json:"fg_threshold"`
	BGThreshold   float64   `json:"bg_threshold"`
	OtsuThreshold float64   `json:"otsu_threshold"`
}

type RAGEdge struct {
	A              int32   `json:"a"`
	B              int32   `json:"b"`
	MinGradient    float64 `json:"min_gradient"`
	AreaA          int     `json:"area_a"`
	AreaB          int     `json:"area_b"`
	AreaDifference int     `json:"area_difference"`
	Cost           float64 `json:"cost"`
	Feasible       bool    `json:"feasible"`
	Reason         string  `json:"reason,omitempty"`
	BoundaryPixels int     `json:"boundary_pixels"`
}

type MergeEvent struct {
	Step   int     `json:"step"`
	A      int32   `json:"a"`
	B      int32   `json:"b"`
	RootA  int32   `json:"root_a"`
	RootB  int32   `json:"root_b"`
	Cost   float64 `json:"cost"`
	Reason string  `json:"reason"`
}

type RegionInfo struct {
	Label   int32   `json:"label"`
	Class   uint8   `json:"class"`
	Area    int     `json:"area"`
	SeedIDs []int32 `json:"seed_ids"`
}

type Result struct {
	Width            int          `json:"width"`
	Height           int          `json:"height"`
	Gradient         []float64    `json:"gradient"`
	Markers          MarkerResult `json:"markers"`
	InitialLabels    []int32      `json:"initial_labels"`
	Labels           []int32      `json:"labels"`
	Regions          []RegionInfo `json:"regions"`
	Edges            []RAGEdge    `json:"edges"`
	MergeLog         []MergeEvent `json:"merge_log"`
	RawMask          []byte       `json:"raw_mask"`
	FloodSamples     int          `json:"flood_samples"`
	QueueSampleRatio float64      `json:"queue_sample_ratio"`
}
