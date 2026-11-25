package iconmask

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
)

const AlgorithmVersion = "iconmask-core/v1"

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type PointInt struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Coord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

func (r Rect) Empty() bool {
	return r.Width <= 0 || r.Height <= 0
}

// Mask is a single-channel, row-major binary mask. Valid foreground values are
// exactly 1; background values are exactly 0.
type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   []byte `json:"data"`
}

func NewMask(width, height int) Mask {
	if width < 0 || height < 0 {
		return Mask{}
	}
	return Mask{Width: width, Height: height, Data: make([]byte, width*height)}
}

func (m Mask) At(x, y int) byte {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return 0
	}
	return m.Data[y*m.Width+x]
}

func (m Mask) Set(x, y int, value byte) {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return
	}
	m.Data[y*m.Width+x] = value
}

func (m Mask) Clone() Mask {
	out := Mask{Width: m.Width, Height: m.Height, Data: make([]byte, len(m.Data))}
	copy(out.Data, m.Data)
	return out
}

func (m Mask) Count() int {
	n := 0
	for _, v := range m.Data {
		if v != 0 {
			n++
		}
	}
	return n
}

func (m Mask) Empty() bool {
	return m.Width <= 0 || m.Height <= 0 || len(m.Data) != m.Width*m.Height
}

func (m Mask) Equal(other Mask) bool {
	if m.Width != other.Width || m.Height != other.Height {
		return false
	}
	return bytes.Equal(m.Data, other.Data)
}

func (m Mask) DiffPixels(other Mask) int {
	if m.Width != other.Width || m.Height != other.Height {
		return maxInt(m.Width*m.Height, other.Width*other.Height)
	}
	n := 0
	for i := range m.Data {
		if (m.Data[i] != 0) != (other.Data[i] != 0) {
			n++
		}
	}
	return n
}

func (m Mask) FirstDiff(other Mask) *PointInt {
	if m.Width != other.Width || m.Height != other.Height {
		return &PointInt{}
	}
	for i := range m.Data {
		if (m.Data[i] != 0) != (other.Data[i] != 0) {
			return &PointInt{X: i % m.Width, Y: i / m.Width}
		}
	}
	return nil
}

func (m Mask) Binary() bool {
	if len(m.Data) != m.Width*m.Height {
		return false
	}
	for _, v := range m.Data {
		if v != 0 && v != 1 {
			return false
		}
	}
	return true
}

func (m Mask) SHA256() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%d:", m.Width, m.Height)
	_, _ = h.Write(m.Data)
	return hex.EncodeToString(h.Sum(nil))
}

type ComponentTruth struct {
	ID       string `json:"id"`
	Centroid Point  `json:"centroid"`
	Area     int    `json:"area"`
	BBox     Rect   `json:"bbox"`
	Holes    int    `json:"holes"`
}

// BoundarySegment covers the normalized arclength interval [Start, End) of one
// closed boundary loop ("outer" or "hole-N"). Curvature is the local analytic
// curvature magnitude at the segment midpoint.
type BoundarySegment struct {
	ID          string  `json:"id"`
	ComponentID string  `json:"componentId"`
	Loop        string  `json:"loop"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Curvature   float64 `json:"curvature"`
	Points      []Point `json:"points,omitempty"`
}

type Truth struct {
	ReferenceMask Mask              `json:"referenceMask"`
	Components    []ComponentTruth  `json:"components"`
	Segments      []BoundarySegment `json:"segments"`
}

type ImageSample struct {
	ID          string `json:"id"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Seed        int64  `json:"seed"`
	ImagePNG    []byte `json:"imagePng"`
	InitialMask Mask   `json:"initialMask"`
	Truth       Truth  `json:"truth"`
}

type Project struct {
	Format           string        `json:"format"`
	Generator        string        `json:"generator"`
	GeneratorVersion string        `json:"generatorVersion"`
	AlgorithmVersion string        `json:"algorithmVersion"`
	Seed             int64         `json:"seed"`
	Images           []ImageSample `json:"images"`
}

type RegionStats struct {
	Count     int              `json:"count"`
	TotalArea int              `json:"totalArea"`
	Regions   []ComponentTruth `json:"regions"`
}

type AccuracyReport struct {
	TruthArea         int     `json:"truthArea"`
	ResultArea        int     `json:"resultArea"`
	Intersection      int     `json:"intersection"`
	MissingForeground int     `json:"missingForeground"`
	SurplusForeground int     `json:"surplusForeground"`
	IoU               float64 `json:"iou"`
	PixelAccuracy     float64 `json:"pixelAccuracy"`
}

func ComputeAccuracy(reference, result Mask) AccuracyReport {
	r := AccuracyReport{}
	if reference.Width != result.Width || reference.Height != result.Height {
		return r
	}
	for i := range reference.Data {
		t := reference.Data[i] != 0
		v := result.Data[i] != 0
		if t {
			r.TruthArea++
		}
		if v {
			r.ResultArea++
		}
		switch {
		case t && v:
			r.Intersection++
		case t && !v:
			r.MissingForeground++
		case !t && v:
			r.SurplusForeground++
		}
	}
	union := r.TruthArea + r.ResultArea - r.Intersection
	if union > 0 {
		r.IoU = float64(r.Intersection) / float64(union)
	}
	if total := reference.Width * reference.Height; total > 0 {
		r.PixelAccuracy = float64(total-r.MissingForeground-r.SurplusForeground) / float64(total)
	}
	return r
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
