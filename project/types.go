package project

type Vec2 struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

type GridConfig struct {
	Cols      int     `json:"cols"`
	Rows      int     `json:"rows"`
	BlockCols int     `json:"blockCols"`
	BlockRows int     `json:"blockRows"`
	Spacing   float64 `json:"spacing"`
}

type GridSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type Material struct {
	ID    int        `json:"id"`
	Name  string     `json:"name"`
	Color [3]float64 `json:"color"`
}

type BakeConfig struct {
	Subdivisions          int     `json:"subdivisions"`
	QuantMax              int     `json:"quantMax"`
	SeamMaxIterations     int     `json:"seamMaxIterations"`
	SeamAngleThresholdDeg float64 `json:"seamAngleThresholdDeg"`
	SeamMaterialBandCells float64 `json:"seamMaterialBandCells"`
	ParamsVersion         string  `json:"paramsVersion"`
	MaxWorkers            int     `json:"maxWorkers"`
	MaxVerticesPerThread  int     `json:"maxVerticesPerThread"`
}

type FileSpec struct {
	File     string `json:"file"`
	Encoding string `json:"encoding"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
}

type MaterialSpec struct {
	File     string `json:"file"`
	Encoding string `json:"encoding"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	Layers   int    `json:"layers"`
	QuantMax int    `json:"quantMax"`
}

type ScatterConfig struct {
	Seed           int64   `json:"seed"`
	DensityPerCell float64 `json:"densityPerCell"`
	MinMask        int     `json:"minMask"`
	MaxInstances   int     `json:"maxInstances"`
}

type Block struct {
	ID          string        `json:"id"`
	Col         int           `json:"col"`
	Row         int           `json:"row"`
	Grid        GridSize      `json:"grid"`
	Origin      Vec2          `json:"origin"`
	Height      FileSpec      `json:"height"`
	Material    MaterialSpec  `json:"material"`
	Passability FileSpec      `json:"passability"`
	Scatter     ScatterConfig `json:"scatter"`
}

// Seam is an explicit bidirectional constraint. EdgeA/EdgeB values are
// right, left, top or bottom.
type Seam struct {
	ID    string `json:"id"`
	A     string `json:"a"`
	B     string `json:"b"`
	EdgeA string `json:"edgeA"`
	EdgeB string `json:"edgeB"`
}

type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Seed          int64      `json:"seed"`
	Grid          GridConfig `json:"grid"`
	Materials     []Material `json:"materials"`
	Bake          BakeConfig `json:"bake"`
	Blocks        []Block    `json:"blocks"`
	Seams         []Seam     `json:"seams"`
	TruthFile     string     `json:"truthFile"`
}

type HeightTruth struct {
	Block     string  `json:"block"`
	Col       int     `json:"col"`
	Row       int     `json:"row"`
	Value     float64 `json:"value"`
	Tolerance float64 `json:"tolerance"`
}

type RatioTruth struct {
	Block     string  `json:"block"`
	Layer     int     `json:"layer"`
	Ratio     float64 `json:"ratio"`
	Tolerance float64 `json:"tolerance"`
}

type CountTruth struct {
	Block     string `json:"block"`
	Count     int    `json:"count"`
	Tolerance int    `json:"tolerance"`
}

type SeamAngleTruth struct {
	A           string  `json:"a"`
	B           string  `json:"b"`
	MaxAngleDeg float64 `json:"maxAngleDeg"`
	Tolerance   float64 `json:"tolerance"`
}

type Truth struct {
	SchemaVersion  int              `json:"schemaVersion"`
	HeightSamples  []HeightTruth    `json:"heightSamples"`
	MaterialRatios []RatioTruth     `json:"materialRatios"`
	InstanceCounts []CountTruth     `json:"instanceCounts"`
	Seams          []SeamAngleTruth `json:"seams"`
}

// EditRecord is persisted in data/edits.json. Material edits are quantized
// weights in the block's material quantization range.
type EditRecord struct {
	Seq   int     `json:"seq"`
	Block string  `json:"block"`
	Kind  string  `json:"kind"`
	Col   int     `json:"col"`
	Row   int     `json:"row"`
	Layer int     `json:"layer,omitempty"`
	Value float64 `json:"value"`
}

type EditFile struct {
	SchemaVersion int          `json:"schemaVersion"`
	Edits         []EditRecord `json:"edits"`
}

type BlockData struct {
	Def     Block
	Height  []float64
	Weights []uint16
	Mask    []byte
}

type Project struct {
	Root        string
	Manifest    Manifest
	Truth       Truth
	Blocks      map[string]*BlockData
	BlockOrder  []string
	Edits       []EditRecord
	ContentHash string
	TruthStale  bool
}

func (p *Project) Block(id string) (*BlockData, bool) {
	b, ok := p.Blocks[id]
	return b, ok
}

func (p *Project) Layers() int {
	return len(p.Manifest.Materials)
}
