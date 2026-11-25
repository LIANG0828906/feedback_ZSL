package model

import (
	"fmt"
	"math"
	"sort"
)

const (
	AlgorithmVersion = "reconstruction-engine/1.0.0"
	SchemaVersion    = "atelier.scenario/1"
)

var RarityKinds = []string{"common", "uncommon", "rare"}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Bounds struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

func (b Bounds) Width() float64  { return b.MaxX - b.MinX }
func (b Bounds) Height() float64 { return b.MaxY - b.MinY }

func (b Bounds) Contains(p Point) bool {
	const eps = 1e-9
	return p.X >= b.MinX-eps && p.X <= b.MaxX+eps && p.Y >= b.MinY-eps && p.Y <= b.MaxY+eps
}

type Polygon struct {
	Vertices []Point `json:"vertices"`
}

func (p Polygon) Bounds() Bounds {
	if len(p.Vertices) == 0 {
		return Bounds{}
	}
	b := Bounds{
		MinX: p.Vertices[0].X,
		MinY: p.Vertices[0].Y,
		MaxX: p.Vertices[0].X,
		MaxY: p.Vertices[0].Y,
	}
	for _, v := range p.Vertices[1:] {
		b.MinX = math.Min(b.MinX, v.X)
		b.MinY = math.Min(b.MinY, v.Y)
		b.MaxX = math.Max(b.MaxX, v.X)
		b.MaxY = math.Max(b.MaxY, v.Y)
	}
	return b
}

func (p Polygon) Area() float64 {
	if len(p.Vertices) < 4 {
		return 0
	}
	var twice float64
	for i := 0; i < len(p.Vertices)-1; i++ {
		a, b := p.Vertices[i], p.Vertices[i+1]
		twice += a.X*b.Y - b.X*a.Y
	}
	return math.Abs(twice) / 2
}

func (p Polygon) Centroid() Point {
	var x, y float64
	n := len(p.Vertices)
	if n > 1 && nearPoint(p.Vertices[0], p.Vertices[n-1]) {
		n--
	}
	if n == 0 {
		return Point{}
	}
	for _, v := range p.Vertices[:n] {
		x += v.X
		y += v.Y
	}
	return Point{X: x / float64(n), Y: y / float64(n)}
}

func nearPoint(a, b Point) bool {
	const eps = 1e-9
	return math.Abs(a.X-b.X) <= eps && math.Abs(a.Y-b.Y) <= eps
}

type ZoneParams struct {
	DensityScale float64 `json:"densityScale"`
	RarityShift  float64 `json:"rarityShift"`
}

type Zone struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Ordinal int        `json:"ordinal"`
	Polygon Polygon    `json:"polygon"`
	Params  ZoneParams `json:"params"`
}

type Exclusion struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Reason  string  `json:"reason"`
	Polygon Polygon `json:"polygon"`
}

type TerrainRaster struct {
	CellSize float64  `json:"cellSize"`
	OriginX  float64  `json:"originX"`
	OriginY  float64  `json:"originY"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Types    []string `json:"types"`
}

func (r TerrainRaster) CellAtXY(x, y float64) (row, col int, ok bool) {
	if r.CellSize <= 0 {
		return 0, 0, false
	}
	col = int(math.Floor((x - r.OriginX) / r.CellSize))
	row = int(math.Floor((y - r.OriginY) / r.CellSize))
	if col < 0 || row < 0 || col >= r.Width || row >= r.Height {
		return 0, 0, false
	}
	return row, col, true
}

func (r TerrainRaster) At(row, col int) string {
	if row < 0 || col < 0 || row >= r.Height || col >= r.Width {
		return ""
	}
	return r.Types[row*r.Width+col]
}

func (r TerrainRaster) AtXY(x, y float64) string {
	row, col, ok := r.CellAtXY(x, y)
	if !ok {
		return ""
	}
	return r.At(row, col)
}

type Region struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Boundary   Polygon       `json:"boundary"`
	Zones      []Zone        `json:"zones"`
	Exclusions []Exclusion   `json:"exclusions"`
	Terrain    TerrainRaster `json:"terrain"`
}

type SamplingConfig struct {
	SampleGridStep      float64 `json:"sampleGridStep"`
	SamplesPerZone      int     `json:"samplesPerZone"`
	HeatmapCellSize     float64 `json:"heatmapCellSize"`
	HeatmapTileSize     int     `json:"heatmapTileSize"`
	MaxParallelism      int     `json:"maxParallelism"`
	PerWorkerBufferSize int     `json:"perWorkerBufferSize"`
	QuantizationStep    float64 `json:"quantizationStep"`
}

type Observation struct {
	ID               string         `json:"id"`
	ZoneID           string         `json:"zoneId"`
	X                float64        `json:"x"`
	Y                float64        `json:"y"`
	GridRow          int            `json:"gridRow"`
	GridCol          int            `json:"gridCol"`
	Terrain          string         `json:"terrain"`
	Density          float64        `json:"density"`
	QuantizedDensity float64        `json:"quantizedDensity"`
	RarityCounts     map[string]int `json:"rarityCounts"`
}

type TruthPoint struct {
	ID      string  `json:"id"`
	ZoneID  string  `json:"zoneId"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Terrain string  `json:"terrain"`
	Rarity  string  `json:"rarity"`
	Density float64 `json:"density"`
}

type ZoneTruth struct {
	ZoneID       string             `json:"zoneId"`
	BaseDensity  float64            `json:"baseDensity"`
	Rarity       map[string]float64 `json:"rarity"`
	TerrainCount map[string]int     `json:"terrainCount"`
	PointCount   int                `json:"pointCount"`
}

type TruthManifest struct {
	RegionID string       `json:"regionId"`
	Zones    []ZoneTruth  `json:"zones"`
	Points   []TruthPoint `json:"points"`
}

type Scenario struct {
	SchemaVersion string         `json:"schemaVersion"`
	Seed          int64          `json:"seed"`
	Region        Region         `json:"region"`
	Config        SamplingConfig `json:"config"`
	Observations  []Observation  `json:"observations"`
	Truth         TruthManifest  `json:"truth"`
}

type DensityModel struct {
	RegionID     string
	Seed         int64
	Region       Region
	Config       SamplingConfig
	Observations []Observation
}

func ModelFromScenario(s Scenario) DensityModel {
	return DensityModel{
		RegionID:     s.Region.ID,
		Seed:         s.Seed,
		Region:       s.Region,
		Config:       s.Config,
		Observations: s.Observations,
	}
}

type SamplePoint struct {
	ID        string  `json:"id"`
	ZoneID    string  `json:"zoneId"`
	Terrain   string  `json:"terrain"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	GridRow   int     `json:"gridRow"`
	GridCol   int     `json:"gridCol"`
	Density   float64 `json:"density"`
	Quantized float64 `json:"quantizedDensity"`
}

type TerrainEstimate struct {
	Terrain       string             `json:"terrain"`
	SampleCount   int                `json:"sampleCount"`
	Area          float64            `json:"area"`
	Density       float64            `json:"density"`
	StandardError float64            `json:"standardError"`
	Rarity        map[string]float64 `json:"rarity"`
	RarityCounts  map[string]int     `json:"rarityCounts"`
}

type DensityEstimate struct {
	Density       float64            `json:"density"`
	Rarity        map[string]float64 `json:"rarity"`
	SampleCount   int                `json:"sampleCount"`
	EffectiveArea float64            `json:"effectiveArea"`
	Layers        []TerrainEstimate  `json:"layers"`
}

type ResourcePoint struct {
	ID      string  `json:"id"`
	ZoneID  string  `json:"zoneId"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Terrain string  `json:"terrain"`
	Rarity  string  `json:"rarity"`
}

type ZoneState struct {
	ZoneID        string          `json:"zoneId"`
	Ordinal       int             `json:"ordinal"`
	ParamsVersion string          `json:"paramsVersion"`
	SamplingKey   string          `json:"samplingKey"`
	EstimateKey   string          `json:"estimateKey"`
	PointsKey     string          `json:"pointsKey"`
	CacheKey      string          `json:"cacheKey"`
	ContentHash   string          `json:"contentHash"`
	Revision      uint64          `json:"revision"`
	Samples       []SamplePoint   `json:"samples"`
	Estimate      DensityEstimate `json:"estimate"`
	Points        []ResourcePoint `json:"points"`
	HeatSummary   string          `json:"heatSummary"`
}

type Heatmap struct {
	OriginX    float64   `json:"originX"`
	OriginY    float64   `json:"originY"`
	CellSize   float64   `json:"cellSize"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	TileSize   int       `json:"tileSize"`
	Values     []float64 `json:"values"`
	ZoneIDs    []string  `json:"zoneIds"`
	TileHashes []string  `json:"tileHashes"`
	Revision   uint64    `json:"revision"`
}

func (h *Heatmap) ValueAtXY(x, y float64) (float64, bool) {
	if h == nil || h.CellSize <= 0 {
		return 0, false
	}
	col := int(math.Floor((x - h.OriginX) / h.CellSize))
	row := int(math.Floor((y - h.OriginY) / h.CellSize))
	if col < 0 || row < 0 || col >= h.Width || row >= h.Height {
		return 0, false
	}
	return h.Values[row*h.Width+col], true
}

func SortedRarityKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func RarityLabel(ratio map[string]float64) string {
	best := RarityKinds[0]
	bestValue := -1.0
	for _, kind := range RarityKinds {
		if ratio[kind] > bestValue {
			best, bestValue = kind, ratio[kind]
		}
	}
	return best
}

func Clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func RoundTo(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Round(v/step) * step
}

func FormatPoint(x, y float64) string {
	return fmt.Sprintf("%.6f,%.6f", x, y)
}
