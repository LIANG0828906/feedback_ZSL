// Package model defines the stable, serializable data structures exchanged by
// every stage of the collection-point reconstruction pipeline.
package model

// Rarity levels are a fixed ordered partition of the reconstructed points.
const (
	RarityCommon = iota
	RarityUncommon
	RarityRare
	NumRarity
)

// RarityName maps a rarity index to a stable string identifier.
var RarityName = [NumRarity]string{"common", "uncommon", "rare"}

// Point is a single 2D coordinate expressed in region-local world units.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Polygon is a closed simple polygon. The first and last vertices are stored
// explicitly so closure can be validated by the loader.
type Polygon struct {
	Vertices []Point `json:"vertices"`
}

// Rect is an axis aligned rectangle used for zones and no-collect areas.
type Rect struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

// Zone is one design stratum inside a region.
type Zone struct {
	ID             int       `json:"id"`
	Bounds         Rect      `json:"bounds"`
	TerrainWeights []float64 `json:"terrainWeights"`
	TrueDensity    float64   `json:"trueDensity"`
	RarityMix      []float64 `json:"rarityMix"`
	ParamVersion   int64     `json:"paramVersion"`
}

// NoCollectZone is a rectangular forbidden area inside a zone.
type NoCollectZone struct {
	ID     int  `json:"id"`
	ZoneID int  `json:"zoneId"`
	Bounds Rect `json:"bounds"`
}

// TruthPoint is a hidden generator-only collection point.
type TruthPoint struct {
	Coord   Point `json:"coord"`
	ZoneID  int   `json:"zoneId"`
	Terrain int   `json:"terrain"`
	Rarity  int   `json:"rarity"`
}

// Observation is a quantized density reading at one sample grid point.
type Observation struct {
	GridIndex     int       `json:"gridIndex"`
	Coord         Point     `json:"coord"`
	ZoneID        int       `json:"zoneId"`
	Terrain       int       `json:"terrain"`
	Density       float64   `json:"density"`
	RarityDensity []float64 `json:"rarityDensity"`
	QuantStep     float64   `json:"quantStep"`
}

// SampleCell is a candidate sample grid point with its validity flags.
type SampleCell struct {
	Index       int   `json:"index"`
	Coord       Point `json:"coord"`
	ZoneID      int   `json:"zoneId"`
	Terrain     int   `json:"terrain"`
	InNoCollect bool  `json:"inNoCollect"`
	Valid       bool  `json:"valid"`
}

// Region is the full deterministic dataset for one terrain region.
type Region struct {
	ID            int             `json:"id"`
	Name          string          `json:"name"`
	Seed          uint64          `json:"seed"`
	Boundary      Polygon         `json:"boundary"`
	BBox          Rect            `json:"bbox"`
	RasterCell    float64         `json:"rasterCell"`
	RasterNX      int             `json:"rasterNx"`
	RasterNY      int             `json:"rasterNy"`
	TerrainRaster [][]int         `json:"terrainRaster"`
	Zones         []Zone          `json:"zones"`
	NoCollect     []NoCollectZone `json:"noCollect"`
	SampleGrid    []SampleCell    `json:"sampleGrid"`
	Observations  []Observation   `json:"observations"`
	Truth         []TruthPoint    `json:"truth"`
	QuantStep     float64         `json:"quantStep"`
	GridSpacing   float64         `json:"gridSpacing"`
}

// DensityEstimate is the per-zone inverse density result.
type DensityEstimate struct {
	ZoneID       int       `json:"zoneId"`
	CellCount    int       `json:"cellCount"`
	Density      float64   `json:"density"`
	StdError     float64   `json:"stdError"`
	RarityMix    []float64 `json:"rarityMix"`
	RarityCounts []int     `json:"rarityCounts"`
	RarityResid  []int     `json:"rarityResid"`
	CellIndices  []int     `json:"cellIndices"`
}

// ReconstructedPoint is one rebuilt collection point.
type ReconstructedPoint struct {
	Coord   Point `json:"coord"`
	ZoneID  int   `json:"zoneId"`
	Terrain int   `json:"terrain"`
	Rarity  int   `json:"rarity"`
}

// HeatTile is a fixed-raster density heatmap cell tagged with its owning zone.
type HeatTile struct {
	IX      int     `json:"ix"`
	IY      int     `json:"iy"`
	Center  Point   `json:"center"`
	ZoneID  int     `json:"zoneId"`
	Density float64 `json:"density"`
}

// ZoneResult is the complete recomputed artifact set for one zone.
type ZoneResult struct {
	ZoneID       int                  `json:"zoneId"`
	CellIndices  []int                `json:"cellIndices"`
	Estimate     DensityEstimate      `json:"estimate"`
	Points       []ReconstructedPoint `json:"points"`
	PointDigest  string               `json:"pointDigest"`
	HeatTiles    []HeatTile           `json:"heatTiles"`
	HeatDigest   string               `json:"heatDigest"`
	ContentHash  string               `json:"contentHash"`
	ParamVersion int64                `json:"paramVersion"`
	AlgoVersion  string               `json:"algoVersion"`
}

// InvalidationEntry describes why a zone was invalidated or reused.
type InvalidationEntry struct {
	ZoneID int    `json:"zoneId"`
	Reason string `json:"reason"`
	Reused bool   `json:"reused"`
	Edge   string `json:"edge"`
}

// BoundRecord is one zone row of the bias propagation report.
type BoundRecord struct {
	ZoneID          int     `json:"zoneId"`
	QuantErrorBound float64 `json:"quantErrorBound"`
	StdErrorBound   float64 `json:"stdErrorBound"`
	RarityResidual  int     `json:"rarityResidual"`
	HeatErrorBound  float64 `json:"heatErrorBound"`
	CumQuantBound   float64 `json:"cumQuantBound"`
	CumStdBound     float64 `json:"cumStdBound"`
	CumRarityBound  int     `json:"cumRarityBound"`
	CumHeatBound    float64 `json:"cumHeatBound"`
}

// PipelineReport bundles all verification-facing outputs of a run.
type PipelineReport struct {
	RegionID      int                 `json:"regionId"`
	Results       []ZoneResult        `json:"results"`
	Invalidations []InvalidationEntry `json:"invalidations"`
	HitSequence   []string            `json:"hitSequence"`
	Bounds        []BoundRecord       `json:"bounds"`
	AlgoVersion   string              `json:"algoVersion"`
}
