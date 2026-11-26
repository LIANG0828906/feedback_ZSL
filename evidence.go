package maskqa

type Truncation struct {
	Level             string `json:"level"`
	X                 int    `json:"x"`
	Y                 int    `json:"y"`
	Operation         string `json:"operation"`
	RawNumerator      int64  `json:"raw_numerator,omitempty"`
	Quotient          int64  `json:"quotient,omitempty"`
	Remainder         int64  `json:"remainder"`
	Denominator       int64  `json:"denominator"`
	ContributionMicro int64  `json:"contribution_micro"`
	TruncatedAt       string `json:"truncated_at"`
}

type PixelTrace struct {
	X                    int          `json:"x"`
	Y                    int          `json:"y"`
	BlockX               int          `json:"block_x"`
	BlockY               int          `json:"block_y"`
	GradientMicro        int64        `json:"gradient_micro"`
	ThresholdMicro       int64        `json:"threshold_micro"`
	RadiusMicro          int64        `json:"radius_micro"`
	MorphologyMicro      int64        `json:"morphology_micro"`
	CompositionMicro     int64        `json:"composition_micro"`
	Contributions        [5]int64     `json:"contributions"`
	DominantLevel        string       `json:"dominant_level"`
	Truncations          []Truncation `json:"truncations,omitempty"`
	GradientMagnitude    int          `json:"gradient_magnitude,omitempty"`
	Threshold            int          `json:"threshold,omitempty"`
	SegmentID            string       `json:"segment_id,omitempty"`
	Radius               int          `json:"radius,omitempty"`
	MorphForegroundVotes int          `json:"morph_foreground_votes,omitempty"`
	MorphKernelArea      int          `json:"morph_kernel_area,omitempty"`
}

func (t *PixelTrace) finalize(p Params) {
	t.Contributions = [5]int64{t.GradientMicro, t.ThresholdMicro, t.RadiusMicro, t.MorphologyMicro, t.CompositionMicro}
	levels := []string{"gradient", "threshold", "radius", "morphology", "composition"}
	var best int64
	t.DominantLevel = "none"
	for i, v := range t.Contributions {
		if v > best {
			best = v
			t.DominantLevel = levels[i]
		}
	}
}

type RoundRecord struct {
	Round        int  `json:"round"`
	CloseChanges int  `json:"close_changes"`
	OpenChanges  int  `json:"open_changes"`
	RoundChanges int  `json:"round_changes"`
	Converged    bool `json:"converged"`
}

type RadiusDecision struct {
	SegmentID        string  `json:"segment_id"`
	BlockID          string  `json:"block_id,omitempty"`
	BlockX           int     `json:"block_x,omitempty"`
	BlockY           int     `json:"block_y,omitempty"`
	PointCount       int     `json:"point_count"`
	Curvature        float64 `json:"curvature"`
	CurvatureQ       int64   `json:"curvature_q"`
	CurvatureRadiusQ int64   `json:"curvature_radius_q"`
	Bin              int     `json:"bin"`
	ErodeRadius      int     `json:"erode_radius"`
	DilateRadius     int     `json:"dilate_radius"`
	MappingBasis     string  `json:"mapping_basis"`
	TruncationMicro  int64   `json:"truncation_micro"`
}

type Segment struct {
	ID               string       `json:"id"`
	StartX           int          `json:"start_x"`
	StartY           int          `json:"start_y"`
	Points           []imagePoint `json:"points"`
	CurvatureQ       int64        `json:"curvature_q"`
	Curvature        float64      `json:"curvature"`
	CurvatureRadiusQ int64        `json:"curvature_radius_q"`
	BlockIDs         []string     `json:"block_ids"`
}

type GradientOutput struct {
	Block     BlockCoord   `json:"block"`
	Width     int          `json:"width"`
	Height    int          `json:"height"`
	Magnitude []int        `json:"magnitude"`
	Traces    []PixelTrace `json:"traces"`
}

type EdgeOutput struct {
	Block     BlockCoord   `json:"block"`
	Threshold int          `json:"threshold"`
	Band      *Mask        `json:"band"`
	Traces    []PixelTrace `json:"traces"`
}

type RadiusOutput struct {
	Block     BlockCoord       `json:"block"`
	Field     []byte           `json:"field"`
	Decisions []RadiusDecision `json:"decisions"`
	Traces    []PixelTrace     `json:"traces"`
}

type CloseOutput struct {
	Block       BlockCoord  `json:"block"`
	Mask        *Mask       `json:"mask"`
	Changes     int         `json:"changes"`
	KernelAreas map[int]int `json:"kernel_areas"`
}

type OpenOutput struct {
	Block      BlockCoord    `json:"block"`
	Mask       *Mask         `json:"mask"`
	Rounds     []RoundRecord `json:"rounds"`
	MinChanges int           `json:"min_changes"`
	Converged  bool          `json:"converged"`
	Traces     []PixelTrace  `json:"traces"`
}

type ComposeOutput struct {
	Block  BlockCoord   `json:"block"`
	Mask   *Mask        `json:"mask"`
	Traces []PixelTrace `json:"traces"`
}

type ErrorReport struct {
	SampleID                string                `json:"sample_id"`
	PixelCount              int                   `json:"pixel_count"`
	LevelContributionMicro  map[string]int64      `json:"level_contribution_micro"`
	FiveLevelSumMicro       int64                 `json:"five_level_sum_micro"`
	ReferenceMismatchPixels int                   `json:"reference_mismatch_pixels"`
	ReferenceMismatchRate   float64               `json:"reference_mismatch_rate"`
	InitialMismatchPixels   int                   `json:"initial_mismatch_pixels"`
	InitialMismatchRate     float64               `json:"initial_mismatch_rate"`
	MaxByLevel              map[string]PixelTrace `json:"max_by_level"`
	GlobalMax               *PixelTrace           `json:"global_max,omitempty"`
	Pixels                  []PixelTrace          `json:"pixels,omitempty"`
}
