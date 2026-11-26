package maskqa

// LoadResult contains the unified model and field-level audit trail.
type LoadResult struct {
	Project     *Project           `json:"project"`
	Traces      []FieldTrace       `json:"traces"`
	Conversions []TruncationRecord `json:"conversions"`
}

// ExportResult contains a versioned artifact and downgrade audit entries.
type ExportResult struct {
	Schema      string             `json:"schema"`
	Data        []byte             `json:"-"`
	Lossy       []LossyIssue       `json:"lossy,omitempty"`
	Conversions []TruncationRecord `json:"conversions,omitempty"`
}

// LossyIssue describes a field or structure that cannot be represented.
type LossyIssue struct {
	Code        string `json:"code"`
	Field       string `json:"field"`
	Message     string `json:"message"`
	Disposition string `json:"disposition"`
	Value       any    `json:"value,omitempty"`
}

type maskV1 struct {
	W      *int   `json:"w"`
	H      *int   `json:"h"`
	PixB64 string `json:"pix_b64"`
}

type strokeV1 struct {
	No              *int   `json:"stroke_no"`
	StrokeID        string `json:"stroke_id,omitempty"`
	OrderPriority   *int   `json:"order_priority"`
	X0              *int   `json:"x0"`
	Y0              *int   `json:"y0"`
	X1              *int   `json:"x1"`
	Y1              *int   `json:"y1"`
	BrushWidthPx    *int   `json:"brush_width_px"`
	WeightPermille  *int64 `json:"cover_weight_permille"`
	WeightRemainder *int64 `json:"weight_remainder_bp,omitempty"`
}

type truthV1 struct {
	DomainIdx      *int     `json:"domain_idx"`
	CXMilli        *int     `json:"cx_milli"`
	CYMilli        *int     `json:"cy_milli"`
	PixelArea      *int     `json:"pixel_area"`
	BoxX0          *int     `json:"box_x0"`
	BoxY0          *int     `json:"box_y0"`
	BoxX1          *int     `json:"box_x1"`
	BoxY1          *int     `json:"box_y1"`
	HoleCnt        *int     `json:"hole_cnt"`
	RingWidthMilli *int     `json:"ring_width_milli"`
	StrokeRefs     []string `json:"stroke_refs"`
}

type dtoV1 struct {
	Schema        string      `json:"schema"`
	ProjID        *string     `json:"proj_id"`
	SeedNo        *int64      `json:"seed_no"`
	CanvasW       *int        `json:"canvas_w"`
	CanvasH       *int        `json:"canvas_h"`
	TileSz        int         `json:"tile_sz,omitempty"`
	IconPNGB64    string      `json:"icon_png_b64,omitempty"`
	CandidateMask *maskV1     `json:"candidate_mask"`
	ReferenceMask *maskV1     `json:"reference_mask"`
	StrokeRecords []strokeV1  `json:"stroke_records"`
	TruthItems    []truthV1   `json:"truth_items"`
	Tolerances    *Tolerances `json:"tolerances,omitempty"`
	Recovery      *recoveryV1 `json:"x_maskqa_recovery,omitempty"`
}

type recoveryStrokeV1 struct {
	StrokeID          string         `json:"stroke_id"`
	Group             string         `json:"group"`
	HardnessPermyriad int            `json:"hardness_permyriad"`
	Direction         DirectionClass `json:"direction"`
	DirectionCode     int            `json:"direction_code"`
	HasHardness       bool           `json:"has_hardness"`
	HasDirection      bool           `json:"has_direction"`
}

type recoveryTruthV1 struct {
	DomainNo      int            `json:"domain_no"`
	Direction     DirectionClass `json:"direction"`
	DirectionCode int            `json:"direction_code"`
}

type recoveryV1 struct {
	Strokes []recoveryStrokeV1 `json:"strokes"`
	Truth   []recoveryTruthV1  `json:"truth"`
}

type maskV2 struct {
	Width        *int   `json:"width"`
	Height       *int   `json:"height"`
	PixelsBase64 string `json:"pixelsBase64"`
}

type strokeV2 struct {
	StrokeID            string `json:"strokeId"`
	Sequence            *int   `json:"sequence"`
	CompositionPriority *int   `json:"compositionPriority"`
	GroupName           string `json:"groupName,omitempty"`
	StartOffsetX        *int64 `json:"startOffsetX"`
	StartOffsetY        *int64 `json:"startOffsetY"`
	EndOffsetX          *int64 `json:"endOffsetX"`
	EndOffsetY          *int64 `json:"endOffsetY"`
	WidthPx             *int   `json:"widthPx"`
	WeightPermyriad     *int64 `json:"weightPermyriad"`
	HardnessPermyriad   *int   `json:"hardnessPermyriad"`
	DirectionCode       *int   `json:"directionCode,omitempty"`
}

type truthV2 struct {
	DomainNo       *int     `json:"domainNo"`
	CentroidXMilli *int     `json:"centroidXMilli"`
	CentroidYMilli *int     `json:"centroidYMilli"`
	Area           *int     `json:"area"`
	MinX           *int     `json:"minX"`
	MinY           *int     `json:"minY"`
	MaxX           *int     `json:"maxX"`
	MaxY           *int     `json:"maxY"`
	Holes          *int     `json:"holes"`
	BandWidthMilli *int     `json:"bandWidthMilli"`
	DirectionCode  *int     `json:"directionCode"`
	StrokeIDs      []string `json:"strokeIds"`
}

type dtoV2 struct {
	SchemaVersion string      `json:"schemaVersion"`
	ProjectID     *string     `json:"projectId"`
	Seed          *int64      `json:"seed"`
	CanvasWidth   *int        `json:"canvasWidth"`
	CanvasHeight  *int        `json:"canvasHeight"`
	TileSize      int         `json:"tileSize,omitempty"`
	IconPNGBase64 string      `json:"iconPngBase64,omitempty"`
	Candidate     *maskV2     `json:"candidateMask"`
	Reference     *maskV2     `json:"referenceMask"`
	Strokes       []strokeV2  `json:"strokes"`
	TruthDomains  []truthV2   `json:"truthDomains"`
	Tolerances    *Tolerances `json:"tolerances,omitempty"`
}

type maskV3 struct {
	WidthPx     *int   `json:"width_px"`
	HeightPx    *int   `json:"height_px"`
	Gray8Base64 string `json:"gray8_base64"`
}

type strokeV3 struct {
	ID                string         `json:"id"`
	Seq               *int           `json:"seq"`
	Group             string         `json:"group"`
	Priority          *int           `json:"priority"`
	Start             *Point         `json:"start"`
	End               *Point         `json:"end"`
	WidthPx           *int           `json:"width_px"`
	HardnessPermyriad *int           `json:"hardness_permyriad"`
	WeightPermyriad   *int           `json:"weight_permyriad"`
	Direction         DirectionClass `json:"direction"`
	DirectionCode     *int           `json:"direction_code,omitempty"`
}

type truthV3 = DomainTruth

type dtoV3 struct {
	SchemaVersion string      `json:"schema_version"`
	ID            *string     `json:"id"`
	Seed          *int64      `json:"seed"`
	Width         *int        `json:"width"`
	Height        *int        `json:"height"`
	TileSize      int         `json:"tile_size,omitempty"`
	IconPNGB64    string      `json:"icon_png_base64,omitempty"`
	Candidate     *maskV3     `json:"candidate_mask"`
	Reference     *maskV3     `json:"reference_mask"`
	Strokes       []strokeV3  `json:"strokes"`
	Truth         []truthV3   `json:"truth"`
	Tolerances    *Tolerances `json:"tolerances,omitempty"`
}
