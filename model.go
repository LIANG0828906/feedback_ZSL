package iconcut

import "image"

const AlgorithmVersion = "maskcut-algo-1.0.0"

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type IPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Size struct {
	W int `json:"w"`
	H int `json:"h"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) MaxX() int { return r.X + r.W }
func (r Rect) MaxY() int { return r.Y + r.H }

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.MaxX() && y >= r.Y && y < r.MaxY()
}

func (r Rect) Intersect(o Rect) Rect {
	x0 := maxInt(r.X, o.X)
	y0 := maxInt(r.Y, o.Y)
	x1 := minInt(r.MaxX(), o.MaxX())
	y1 := minInt(r.MaxY(), o.MaxY())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (r Rect) Expand(n, maxW, maxH int) Rect {
	x0 := maxInt(0, r.X-n)
	y0 := maxInt(0, r.Y-n)
	x1 := minInt(maxW, r.MaxX()+n)
	y1 := minInt(maxH, r.MaxY()+n)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

type RadialParams struct {
	CenterX float64 `json:"center_x"`
	CenterY float64 `json:"center_y"`
	Radius  float64 `json:"radius"`
	K1      float64 `json:"k1"`
	K2      float64 `json:"k2"`
}

type Geometry struct {
	Source     Size          `json:"source"`
	Corrected  Size          `json:"corrected"`
	Homography [3][3]float64 `json:"homography"`
	Radial     RadialParams  `json:"radial"`
	CoordScale int           `json:"coord_scale"`
}

type ControlPoint struct {
	Source    Point `json:"source"`
	Corrected Point `json:"corrected"`
}

type BBox struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type RegionTruth struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Color     [3]uint8 `json:"color"`
	Seed      IPoint   `json:"seed"`
	CentroidX float64  `json:"centroid_x"`
	CentroidY float64  `json:"centroid_y"`
	Area      int      `json:"area"`
	BBox      BBox     `json:"bbox"`
	Holes     int      `json:"holes"`
}

type SegmentParams struct {
	ColorTolerance    float64  `json:"color_tolerance"`
	GradientThreshold float64  `json:"gradient_threshold"`
	MorphSize         int      `json:"morph_size"`
	SeedPoints        []IPoint `json:"seed_points"`
}

type Budget struct {
	Workers           int `json:"workers"`
	MaxThreadPixels   int `json:"max_thread_pixels"`
	MaxQueuedTasks    int `json:"max_queued_tasks"`
	PreviewDelayMaxMS int `json:"preview_delay_max_ms"`
}

type Tolerances struct {
	ControlPX         float64 `json:"control_px"`
	MapPixelAbs       int     `json:"map_pixel_abs"`
	PreviewCentroidPX float64 `json:"preview_centroid_px"`
	PreviewAreaRel    float64 `json:"preview_area_rel"`
	RegionCentroidPX  float64 `json:"region_centroid_px"`
	RegionAreaRel     float64 `json:"region_area_rel"`
	HoleCountDelta    int     `json:"hole_count_delta"`
}

type Precision struct {
	CoordScale int `json:"coord_scale"`
}

type Declared struct {
	TileWidth   int           `json:"tile_width"`
	TileHeight  int           `json:"tile_height"`
	BandPX      int           `json:"band_px"`
	InterpOrder int           `json:"interp_order"`
	Segment     SegmentParams `json:"segment"`
	Budget      Budget        `json:"budget"`
	Tolerance   Tolerances    `json:"tolerance"`
	Precision   Precision     `json:"precision"`
}

type Manifest struct {
	SchemaVersion string         `json:"schema_version"`
	ImageID       string         `json:"image_id"`
	Seed          int64          `json:"seed"`
	Source        Size           `json:"source"`
	Corrected     Size           `json:"corrected"`
	Channels      int            `json:"channels"`
	Geometry      Geometry       `json:"geometry"`
	Controls      []ControlPoint `json:"controls"`
	Regions       []RegionTruth  `json:"regions"`
	Declared      Declared       `json:"declared"`
}

type Image struct {
	W        int
	H        int
	Channels int
	Pix      []byte
}

func NewImage(w, h, channels int) *Image {
	return &Image{W: w, H: h, Channels: channels, Pix: make([]byte, w*h*channels)}
}

func ImageFromRGBA(im *image.RGBA) *Image {
	b := im.Bounds()
	out := NewImage(b.Dx(), b.Dy(), 3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i := im.PixOffset(x, y)
			o := ((y-b.Min.Y)*out.W + x - b.Min.X) * 3
			out.Pix[o] = im.Pix[i]
			out.Pix[o+1] = im.Pix[i+1]
			out.Pix[o+2] = im.Pix[i+2]
		}
	}
	return out
}

func (im *Image) Valid() bool {
	return im != nil && im.W > 0 && im.H > 0 && im.Channels == 3 && len(im.Pix) == im.W*im.H*3
}

func (im *Image) Pixel(x, y int) [3]byte {
	i := (y*im.W + x) * 3
	return [3]byte{im.Pix[i], im.Pix[i+1], im.Pix[i+2]}
}

func (im *Image) SetPixel(x, y int, c [3]byte) {
	i := (y*im.W + x) * 3
	im.Pix[i], im.Pix[i+1], im.Pix[i+2] = c[0], c[1], c[2]
}

type LabelMask struct {
	W      int
	H      int
	Labels []uint8
}

func NewLabelMask(w, h int) *LabelMask {
	return &LabelMask{W: w, H: h, Labels: make([]uint8, w*h)}
}

func (m *LabelMask) At(x, y int) uint8     { return m.Labels[y*m.W+x] }
func (m *LabelMask) Set(x, y int, v uint8) { m.Labels[y*m.W+x] = v }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
