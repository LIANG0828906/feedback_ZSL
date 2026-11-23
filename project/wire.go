package project

type wireGrid struct {
	Cols      *int     `json:"cols"`
	Rows      *int     `json:"rows"`
	BlockCols *int     `json:"blockCols"`
	BlockRows *int     `json:"blockRows"`
	Spacing   *float64 `json:"spacing"`
}

type wireGridSize struct {
	Cols *int `json:"cols"`
	Rows *int `json:"rows"`
}

type wireVec2 struct {
	X *float64 `json:"x"`
	Z *float64 `json:"z"`
}

type wireMaterial struct {
	ID    *int        `json:"id"`
	Name  *string     `json:"name"`
	Color *[3]float64 `json:"color"`
}

type wireBake struct {
	Subdivisions          *int     `json:"subdivisions"`
	QuantMax              *int     `json:"quantMax"`
	SeamMaxIterations     *int     `json:"seamMaxIterations"`
	SeamAngleThresholdDeg *float64 `json:"seamAngleThresholdDeg"`
	SeamMaterialBandCells *float64 `json:"seamMaterialBandCells"`
	ParamsVersion         *string  `json:"paramsVersion"`
	MaxWorkers            *int     `json:"maxWorkers"`
	MaxVerticesPerThread  *int     `json:"maxVerticesPerThread"`
}

type wireFileSpec struct {
	File     *string `json:"file"`
	Encoding *string `json:"encoding"`
	Cols     *int    `json:"cols"`
	Rows     *int    `json:"rows"`
}

type wireMaterialSpec struct {
	File     *string `json:"file"`
	Encoding *string `json:"encoding"`
	Cols     *int    `json:"cols"`
	Rows     *int    `json:"rows"`
	Layers   *int    `json:"layers"`
	QuantMax *int    `json:"quantMax"`
}

type wireScatter struct {
	Seed           *int64   `json:"seed"`
	DensityPerCell *float64 `json:"densityPerCell"`
	MinMask        *int     `json:"minMask"`
	MaxInstances   *int     `json:"maxInstances"`
}

type wireBlock struct {
	ID          *string           `json:"id"`
	Col         *int              `json:"col"`
	Row         *int              `json:"row"`
	Grid        *wireGridSize     `json:"grid"`
	Origin      *wireVec2         `json:"origin"`
	Height      *wireFileSpec     `json:"height"`
	Material    *wireMaterialSpec `json:"material"`
	Passability *wireFileSpec     `json:"passability"`
	Scatter     *wireScatter      `json:"scatter"`
}

type wireSeam struct {
	ID    *string `json:"id"`
	A     *string `json:"a"`
	B     *string `json:"b"`
	EdgeA *string `json:"edgeA"`
	EdgeB *string `json:"edgeB"`
}

type wireManifest struct {
	SchemaVersion *int            `json:"schemaVersion"`
	Seed          *int64          `json:"seed"`
	Grid          *wireGrid       `json:"grid"`
	Materials     *[]wireMaterial `json:"materials"`
	Bake          *wireBake       `json:"bake"`
	Blocks        *[]wireBlock    `json:"blocks"`
	Seams         *[]wireSeam     `json:"seams"`
	TruthFile     *string         `json:"truthFile"`
}

type wireHeightTruth struct {
	Block     *string  `json:"block"`
	Col       *int     `json:"col"`
	Row       *int     `json:"row"`
	Value     *float64 `json:"value"`
	Tolerance *float64 `json:"tolerance"`
}

type wireRatioTruth struct {
	Block     *string  `json:"block"`
	Layer     *int     `json:"layer"`
	Ratio     *float64 `json:"ratio"`
	Tolerance *float64 `json:"tolerance"`
}

type wireCountTruth struct {
	Block     *string `json:"block"`
	Count     *int    `json:"count"`
	Tolerance *int    `json:"tolerance"`
}

type wireSeamTruth struct {
	A           *string  `json:"a"`
	B           *string  `json:"b"`
	MaxAngleDeg *float64 `json:"maxAngleDeg"`
	Tolerance   *float64 `json:"tolerance"`
}

type wireTruth struct {
	SchemaVersion  *int               `json:"schemaVersion"`
	HeightSamples  *[]wireHeightTruth `json:"heightSamples"`
	MaterialRatios *[]wireRatioTruth  `json:"materialRatios"`
	InstanceCounts *[]wireCountTruth  `json:"instanceCounts"`
	Seams          *[]wireSeamTruth   `json:"seams"`
}

type wireEdit struct {
	Seq   *int     `json:"seq"`
	Block *string  `json:"block"`
	Kind  *string  `json:"kind"`
	Col   *int     `json:"col"`
	Row   *int     `json:"row"`
	Layer *int     `json:"layer"`
	Value *float64 `json:"value"`
}

type wireEditFile struct {
	SchemaVersion *int        `json:"schemaVersion"`
	Edits         *[]wireEdit `json:"edits"`
}
