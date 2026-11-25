package schema

type V1Layer struct {
	LayerID        string  `json:"layer_id"`
	Order          int     `json:"order"`
	WeightTable    []int64 `json:"weight_table"`
	BinHeightCM    int64   `json:"bin_height_cm"`
	HeightOriginCM int64   `json:"height_origin_cm,omitempty"`
	SlopeTable     []int64 `json:"slope_table,omitempty"`
	BinSlopeRad    int64   `json:"bin_slope_rad,omitempty"`
	SlopeOriginRad int64   `json:"slope_origin_rad,omitempty"`
	Tint           int64   `json:"tint"`
}

type V1Wire struct {
	FormatVersion int       `json:"format_version"`
	ProjectID     string    `json:"project_id"`
	HeightUnit    string    `json:"height_unit"`
	SlopeUnit     string    `json:"slope_unit"`
	Normalization string    `json:"normalization"`
	Seed          int64     `json:"seed"`
	ChunkCols     int       `json:"chunk_cols"`
	ChunkRows     int       `json:"chunk_rows"`
	ChunkSizePx   int       `json:"chunk_size_px"`
	Layers        []V1Layer `json:"layers"`
}

type V2CurvePt struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type V2Layer struct {
	LayerId     string      `json:"layerId"`
	Order       int         `json:"order"`
	Driver      string      `json:"driver"`
	CurvePoints []V2CurvePt `json:"curvePoints"`
	Tint        int64       `json:"tint"`
}

type V2Wire struct {
	FormatVer     int       `json:"formatVer"`
	ProjectId     string    `json:"projectId"`
	HeightUnit    string    `json:"heightUnit"`
	SlopeUnit     string    `json:"slopeUnit"`
	Normalization string    `json:"normalization"`
	Seed          int64     `json:"seed"`
	ChunkCols     int       `json:"chunkCols"`
	ChunkRows     int       `json:"chunkRows"`
	ChunkSizePx   int       `json:"chunkSizePx"`
	SlopeCapDeg   *int64    `json:"slopeCapDeg,omitempty"`
	Layers        []V2Layer `json:"layers"`
}

type V3CurvePt struct {
	X int64 `json:"X"`
	Y int64 `json:"Y"`
}

type V3Noise struct {
	Seed      int64 `json:"Seed"`
	Amplitude int64 `json:"Amplitude"`
	ScaleM    int64 `json:"ScaleM"`
}

type V3Layer struct {
	LayerID     string      `json:"LayerID"`
	Order       int         `json:"Order"`
	Driver      string      `json:"Driver"`
	CurvePoints []V3CurvePt `json:"CurvePoints"`
	Tint        int64       `json:"Tint"`
	Noise       *V3Noise    `json:"Noise,omitempty"`
	SlopeCapDeg *int64      `json:"SlopeCapDeg,omitempty"`
}

type V3Wire struct {
	FormatVersion int       `json:"FormatVersion"`
	ProjectID     string    `json:"ProjectID"`
	HeightUnit    string    `json:"HeightUnit"`
	SlopeUnit     string    `json:"SlopeUnit"`
	Normalization string    `json:"Normalization"`
	Seed          int64     `json:"Seed"`
	ChunkCols     int       `json:"ChunkCols"`
	ChunkRows     int       `json:"ChunkRows"`
	ChunkSizePx   int       `json:"ChunkSizePx"`
	Layers        []V3Layer `json:"Layers"`
}
