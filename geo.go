package ortho

import (
	"encoding/json"
	"fmt"
	"math"
)

// Stage 描述校正链中的一个几何变换阶段。
// Forward 将输入图坐标映射到输出图坐标（用于真值控制点校验），
// Inverse 将输出图坐标反映射回输入图坐标（用于重采样）。
type Stage interface {
	ID() string
	Version() string
	Validate() *Error
	OutSize(inW, inH int) (int, int)
	Forward(x, y float64) (float64, float64)
	Inverse(x, y float64) (float64, float64)
	CanonicalParams() []byte
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// ---------------- 阶段 1：透视单应 ----------------

// HomographyParams 为行优先 3x3 单应矩阵。
type HomographyParams struct {
	H [9]float64 `json:"h"`
}

type Homography struct {
	P   HomographyParams
	inv [9]float64
}

func NewHomography(p HomographyParams) *Homography {
	s := &Homography{P: p}
	a := p.H
	det := a[0]*(a[4]*a[8]-a[5]*a[7]) - a[1]*(a[3]*a[8]-a[5]*a[6]) + a[2]*(a[3]*a[7]-a[4]*a[6])
	if det != 0 {
		s.inv[0] = (a[4]*a[8] - a[5]*a[7]) / det
		s.inv[1] = (a[2]*a[7] - a[1]*a[8]) / det
		s.inv[2] = (a[1]*a[5] - a[2]*a[4]) / det
		s.inv[3] = (a[5]*a[6] - a[3]*a[8]) / det
		s.inv[4] = (a[0]*a[8] - a[2]*a[6]) / det
		s.inv[5] = (a[2]*a[3] - a[0]*a[5]) / det
		s.inv[6] = (a[3]*a[7] - a[4]*a[6]) / det
		s.inv[7] = (a[1]*a[6] - a[0]*a[7]) / det
		s.inv[8] = (a[0]*a[4] - a[1]*a[3]) / det
	}
	return s
}

func (s *Homography) ID() string      { return "homography" }
func (s *Homography) Version() string { return "homography/v1" }

func (s *Homography) Validate() *Error {
	for i, v := range s.P.H {
		if !finite(v) {
			return &Error{Code: EParam, Stage: s.ID(), Field: fmt.Sprintf("H[%d]", i), Msg: "matrix element must be finite"}
		}
	}
	a := s.P.H
	det := a[0]*(a[4]*a[8]-a[5]*a[7]) - a[1]*(a[3]*a[8]-a[5]*a[6]) + a[2]*(a[3]*a[7]-a[4]*a[6])
	if math.Abs(det) < 1e-9 {
		return &Error{Code: EParam, Stage: s.ID(), Field: "H", Msg: "homography matrix is singular"}
	}
	if math.Abs(a[8]) < 1e-12 {
		return &Error{Code: EParam, Stage: s.ID(), Field: "H[8]", Msg: "h33 must be non-zero"}
	}
	return nil
}

func (s *Homography) OutSize(inW, inH int) (int, int) { return inW, inH }

func (s *Homography) Forward(x, y float64) (float64, float64) {
	a := s.P.H
	d := a[6]*x + a[7]*y + a[8]
	return (a[0]*x + a[1]*y + a[2]) / d, (a[3]*x + a[4]*y + a[5]) / d
}

func (s *Homography) Inverse(x, y float64) (float64, float64) {
	a := s.inv
	d := a[6]*x + a[7]*y + a[8]
	return (a[0]*x + a[1]*y + a[2]) / d, (a[3]*x + a[4]*y + a[5]) / d
}

func (s *Homography) CanonicalParams() []byte {
	b, _ := json.Marshal(s.P)
	return b
}

// ---------------- 阶段 2：径向畸变校正 ----------------

// RadialParams 描述以 (Cx,Cy) 为中心、Radius 归一化的多项式径向模型。
// 正向映射：p' = c + (p-c) * (1 + K1*r^2 + K2*r^4)，r = |p-c| / Radius。
type RadialParams struct {
	K1     float64 `json:"k1"`
	K2     float64 `json:"k2"`
	Cx     float64 `json:"cx"`
	Cy     float64 `json:"cy"`
	Radius float64 `json:"radius"`
}

type Radial struct {
	P RadialParams
}

func NewRadial(p RadialParams) *Radial { return &Radial{P: p} }

func (s *Radial) ID() string      { return "radial" }
func (s *Radial) Version() string { return "radial/v1" }

func (s *Radial) Validate() *Error {
	bad := func(field, msg string) *Error {
		return &Error{Code: EParam, Stage: s.ID(), Field: field, Msg: msg}
	}
	if !finite(s.P.K1) || math.Abs(s.P.K1) > 1 {
		return bad("K1", "must be finite with |K1| <= 1")
	}
	if !finite(s.P.K2) || math.Abs(s.P.K2) > 1 {
		return bad("K2", "must be finite with |K2| <= 1")
	}
	if !finite(s.P.Cx) || math.Abs(s.P.Cx) > 1<<15 {
		return bad("Cx", "must be finite within +/-32768")
	}
	if !finite(s.P.Cy) || math.Abs(s.P.Cy) > 1<<15 {
		return bad("Cy", "must be finite within +/-32768")
	}
	if !finite(s.P.Radius) || s.P.Radius <= 0 || s.P.Radius > 1<<15 {
		return bad("Radius", "must be in (0, 32768]")
	}
	return nil
}

func (s *Radial) OutSize(inW, inH int) (int, int) { return inW, inH }

func (s *Radial) gain(r float64) float64 {
	r2 := r * r
	return 1 + s.P.K1*r2 + s.P.K2*r2*r2
}

func (s *Radial) Forward(x, y float64) (float64, float64) {
	dx, dy := x-s.P.Cx, y-s.P.Cy
	r := math.Hypot(dx, dy) / s.P.Radius
	g := s.gain(r)
	return s.P.Cx + dx*g, s.P.Cy + dy*g
}

// Inverse 通过牛顿法求解 r*g(r)=rq，固定迭代次数保证确定性。
func (s *Radial) Inverse(x, y float64) (float64, float64) {
	qx, qy := x-s.P.Cx, y-s.P.Cy
	rq := math.Hypot(qx, qy) / s.P.Radius
	r := rq
	for i := 0; i < 50; i++ {
		r2 := r * r
		g := 1 + s.P.K1*r2 + s.P.K2*r2*r2
		gp := 2*s.P.K1*r + 4*s.P.K2*r2*r
		hp := g + r*gp
		if hp == 0 {
			break
		}
		r -= (r*g - rq) / hp
		if r < 0 {
			r = 0
		}
	}
	g := s.gain(r)
	if g == 0 {
		return x, y
	}
	return s.P.Cx + qx/g, s.P.Cy + qy/g
}

func (s *Radial) CanonicalParams() []byte {
	b, _ := json.Marshal(s.P)
	return b
}

// ---------------- 阶段 3：仿射缩放平移 ----------------

type AffineParams struct {
	Sx float64 `json:"sx"`
	Sy float64 `json:"sy"`
	Tx float64 `json:"tx"`
	Ty float64 `json:"ty"`
}

type Affine struct {
	P AffineParams
}

func NewAffine(p AffineParams) *Affine { return &Affine{P: p} }

func (s *Affine) ID() string      { return "affine" }
func (s *Affine) Version() string { return "affine/v1" }

func (s *Affine) Validate() *Error {
	bad := func(field, msg string) *Error {
		return &Error{Code: EParam, Stage: s.ID(), Field: field, Msg: msg}
	}
	if !finite(s.P.Sx) || s.P.Sx < 0.05 || s.P.Sx > 20 {
		return bad("Sx", "must be in [0.05, 20]")
	}
	if !finite(s.P.Sy) || s.P.Sy < 0.05 || s.P.Sy > 20 {
		return bad("Sy", "must be in [0.05, 20]")
	}
	if !finite(s.P.Tx) || math.Abs(s.P.Tx) > 1<<15 {
		return bad("Tx", "must be finite within +/-32768")
	}
	if !finite(s.P.Ty) || math.Abs(s.P.Ty) > 1<<15 {
		return bad("Ty", "must be finite within +/-32768")
	}
	return nil
}

// OutSize 由缩放参数推导输出尺寸。
func (s *Affine) OutSize(inW, inH int) (int, int) {
	return int(math.Round(float64(inW) * s.P.Sx)), int(math.Round(float64(inH) * s.P.Sy))
}

func (s *Affine) Forward(x, y float64) (float64, float64) {
	return s.P.Sx*x + s.P.Tx, s.P.Sy*y + s.P.Ty
}

func (s *Affine) Inverse(x, y float64) (float64, float64) {
	return (x - s.P.Tx) / s.P.Sx, (y - s.P.Ty) / s.P.Sy
}

func (s *Affine) CanonicalParams() []byte {
	b, _ := json.Marshal(s.P)
	return b
}
