package projectio

// These wire types deliberately use different naming and organization for each
// schema version. Pointers are used where presence itself must be validated.

type v1Document struct {
	Schema      string `json:"schema"`
	ProjectCode string `json:"project_code,omitempty"`
	ProjectName string `json:"project_name"`
	Seed        *int64 `json:"seed"`
	Geometry    *struct {
		Width       *int `json:"width"`
		Height      *int `json:"height"`
		BlockWidth  *int `json:"block_width"`
		BlockHeight *int `json:"block_height"`
	} `json:"geometry"`
	Quantization *struct {
		PermilleMax *int64  `json:"permille_max"`
		Rounding    *string `json:"rounding"`
	} `json:"quantization"`
	Exposure *struct {
		ReferenceMicros *int64  `json:"reference_micros"`
		StackMicros     []int64 `json:"stack_micros"`
	} `json:"exposure"`
	WhitePointPermille *[3]int64 `json:"white_point_permille"`
	XMSWhitePointBP    *[3]int64 `json:"x_ms_white_point_bp,omitempty"`
	FullWellDose       *int64    `json:"full_well_dose"`
	Bands              []v1Band  `json:"bands"`
}

type v1Band struct {
	BandID           *string   `json:"band_id"`
	Name             *string   `json:"name"`
	CenterNM         *int      `json:"center_nm"`
	ResponsePermille *[3]int64 `json:"response_permille"`
	GroupName        string    `json:"group_name,omitempty"`
	XMSGainPPM       *int64    `json:"x_ms_gain_ppm,omitempty"`
	XMSGroupPath     []string  `json:"x_ms_group_path,omitempty"`
	XMSResponseBP    *[3]int64 `json:"x_ms_response_bp,omitempty"`
}

type v2Document struct {
	Format   string `json:"format"`
	Metadata *struct {
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
		Seed      *int64 `json:"seed"`
	} `json:"metadata"`
	Canvas *struct {
		Width      *int `json:"width_px"`
		Height     *int `json:"height_px"`
		TileWidth  *int `json:"tile_width_px"`
		TileHeight *int `json:"tile_height_px"`
	} `json:"canvas"`
	QuantSpec *struct {
		BasisMax *int64  `json:"basis_max"`
		Truncate *string `json:"truncation_direction"`
	} `json:"quant_spec"`
	ExposureMetadata *struct {
		ReferenceMS    *int64  `json:"reference_ms"`
		StackMS        []int64 `json:"stack_ms"`
		XMSReferenceUS *int64  `json:"x_ms_reference_us,omitempty"`
		XMSStackUS     []int64 `json:"x_ms_stack_us,omitempty"`
	} `json:"exposure_metadata"`
	WhitePointBP  *[3]int64 `json:"white_point_bp"`
	FullWellDose  *int64    `json:"full_well_dose"`
	SpectralBands []v2Band  `json:"spectral_bands"`
}

type v2Band struct {
	BandID                 *string   `json:"band_id"`
	Label                  *string   `json:"label"`
	CenterNanometers       *int      `json:"center_nanometers"`
	SpectralFamily         string    `json:"spectral_family,omitempty"`
	SpectralSubfamily      string    `json:"spectral_subfamily,omitempty"`
	GainCorrectionPermille *int64    `json:"gain_correction_permille,omitempty"`
	TargetResponseBP       *[3]int64 `json:"target_response_bp"`
	XMSGroupPath           []string  `json:"x_ms_group_path,omitempty"`
	XMSGainPPM             *int64    `json:"x_ms_gain_ppm,omitempty"`
}

type v3Document struct {
	SchemaVersion string `json:"schemaVersion"`
	CompositionID string `json:"compositionId"`
	DisplayName   string `json:"displayName"`
	RandomSeed    *int64 `json:"randomSeed"`
	Canvas        *struct {
		W     *int `json:"w"`
		H     *int `json:"h"`
		TileW *int `json:"tileW"`
		TileH *int `json:"tileH"`
	} `json:"canvas"`
	Quantization *struct {
		Denominator *int64  `json:"denominator"`
		Rounding    *string `json:"rounding"`
	} `json:"quantization"`
	WhitePoint *struct {
		XYZBP *[3]int64 `json:"xyzBp"`
	} `json:"whitePoint"`
	FullWellDose   *int64            `json:"fullWellDose"`
	ExposureGroups []v3ExposureGroup `json:"exposureGroups"`
}

type v3ExposureGroup struct {
	ExposureID     *string  `json:"exposureId"`
	ExposureMicros *int64   `json:"exposureMicros"`
	Reference      bool     `json:"reference"`
	SpectralTree   []v3Node `json:"spectralTree"`
}

type v3Node struct {
	NodeID   string   `json:"nodeId"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Children []v3Node `json:"children,omitempty"`
	Band     *v3Leaf  `json:"band,omitempty"`
}

type v3Leaf struct {
	BandID     *string   `json:"bandId"`
	Name       *string   `json:"name"`
	CenterNM   *int      `json:"centerNm"`
	GainPPM    *int64    `json:"gainPpm"`
	ResponseBP *[3]int64 `json:"responseBp"`
}
