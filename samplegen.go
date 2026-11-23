package ortho

import (
	"encoding/json"
	"image"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

// SampleSeed 为样本生成器的固定种子。
const SampleSeed int64 = 42

// SampleWidth / SampleHeight 为生成的地形源图尺寸。
const (
	SampleWidth  = 320
	SampleHeight = 240
)

// StageParams 为三阶段链的完整参数集。
type StageParams struct {
	Homography  HomographyParams `json:"homography"`
	Radial      RadialParams     `json:"radial"`
	Affine      AffineParams     `json:"affine"`
	InterpOrder int              `json:"interp_order"`
	TileSize    int              `json:"tile_size"`
	Band        int              `json:"band"`
	Tolerance   float64          `json:"tolerance"`
}

// BuildStages 由参数集构造阶段链。
func BuildStages(p *StageParams) []Stage {
	return []Stage{
		NewHomography(p.Homography),
		NewRadial(p.Radial),
		NewAffine(p.Affine),
	}
}

// DefaultStageParams 返回与真值一致的标称参数。
func DefaultStageParams() *StageParams {
	return &StageParams{
		Homography: HomographyParams{H: [9]float64{
			1.02, 0.03, -12.0,
			-0.02, 0.98, 8.0,
			0.00008, -0.00005, 1.0,
		}},
		Radial: RadialParams{
			K1: 0.12, K2: 0.03,
			Cx: SampleWidth / 2, Cy: SampleHeight / 2,
			Radius: 100.0,
		},
		Affine:      AffineParams{Sx: 1.25, Sy: 1.25, Tx: 16.0, Ty: 10.0},
		InterpOrder: 1,
		TileSize:    64,
		Band:        2,
		Tolerance:   0.5,
	}
}

// SampleSet 记录生成样本的文件路径。
type SampleSet struct {
	GrayPath   string
	RGBPath    string
	ParamsPath string
	TruthPath  string
}

// GenerateSamples 按固定种子确定性地生成地形图像、阶段参数与解析真值。
// 重复运行产出逐字节一致的内容。
func GenerateSamples(dir string) (*SampleSet, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	params := DefaultStageParams()
	stages := BuildStages(params)

	gray := genTerrainGray(SampleSeed)
	rgb := genTerrainRGB(SampleSeed + 1)
	truth := genTruth(SampleSeed+2, stages, params)

	set := &SampleSet{
		GrayPath:   filepath.Join(dir, "terrain_gray.png"),
		RGBPath:    filepath.Join(dir, "terrain_rgb.png"),
		ParamsPath: filepath.Join(dir, "stages.json"),
		TruthPath:  filepath.Join(dir, "truth.json"),
	}
	if err := writePNG(set.GrayPath, gray); err != nil {
		return nil, err
	}
	if err := writePNG(set.RGBPath, rgb); err != nil {
		return nil, err
	}
	pb, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(set.ParamsPath, pb, 0o644); err != nil {
		return nil, err
	}
	if err := SaveTruth(set.TruthPath, truth); err != nil {
		return nil, err
	}
	return set, nil
}

// LoadStageParams 从 JSON 读取阶段参数。
func LoadStageParams(path string) (*StageParams, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p StageParams
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func writePNG(path string, img image.Image) error {
	b, err := EncodePNG(img)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// genTerrainGray 生成单通道地形表面：多频正弦叠加 + 确定性噪声。
func genTerrainGray(seed int64) *image.Gray {
	rng := rand.New(rand.NewSource(seed))
	out := image.NewGray(image.Rect(0, 0, SampleWidth, SampleHeight))
	p1, p2, p3 := rng.Float64()*math.Pi*2, rng.Float64()*math.Pi*2, rng.Float64()*math.Pi*2
	for y := 0; y < SampleHeight; y++ {
		for x := 0; x < SampleWidth; x++ {
			fx, fy := float64(x), float64(y)
			v := 128.0 +
				52.0*math.Sin(fx*0.055+p1)*math.Cos(fy*0.043+p2) +
				28.0*math.Sin((fx+fy)*0.021+p3) +
				14.0*math.Sin(fx*0.13)*math.Sin(fy*0.11) +
				(rng.Float64()-0.5)*18.0
			out.Pix[y*out.Stride+x] = grayClamp(v)
		}
	}
	return out
}

// genTerrainRGB 生成三通道地形表面：各通道独立的高度/坡度/湿度分量。
func genTerrainRGB(seed int64) *image.RGBA {
	rng := rand.New(rand.NewSource(seed))
	out := image.NewRGBA(image.Rect(0, 0, SampleWidth, SampleHeight))
	p1, p2 := rng.Float64()*math.Pi*2, rng.Float64()*math.Pi*2
	for y := 0; y < SampleHeight; y++ {
		for x := 0; x < SampleWidth; x++ {
			fx, fy := float64(x), float64(y)
			r := 128.0 + 60.0*math.Sin(fx*0.05+p1)*math.Cos(fy*0.06) + (rng.Float64()-0.5)*14
			g := 128.0 + 55.0*math.Sin((fx-fy)*0.033+p2) + (rng.Float64()-0.5)*14
			b := 128.0 + 48.0*math.Cos(fx*0.021)*math.Sin(fy*0.027+p1) + (rng.Float64()-0.5)*14
			i := out.PixOffset(x, y)
			out.Pix[i+0] = grayClamp(r)
			out.Pix[i+1] = grayClamp(g)
			out.Pix[i+2] = grayClamp(b)
			out.Pix[i+3] = 255
		}
	}
	return out
}

func grayClamp(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// genTruth 生成控制点并计算逐阶段解析真值坐标。
// 控制点确定性选取，且保证各阶段坐标均落在对应图像范围内。
func genTruth(seed int64, stages []Stage, params *StageParams) *Truth {
	rng := rand.New(rand.NewSource(seed))
	const n = 24
	const margin = 36.0
	t := &Truth{
		Seed:             seed,
		DistortionCenter: [2]float64{params.Radial.Cx, params.Radial.Cy},
		DistortionRadius: params.Radial.Radius,
		Stages:           make(map[string][]ControlPoint),
	}
	w, h := float64(SampleWidth), float64(SampleHeight)
	accepted := 0
	for accepted < n {
		x := margin + rng.Float64()*(w-2*margin)
		y := margin + rng.Float64()*(h-2*margin)
		pts := []ControlPoint{{ID: accepted, X: x, Y: y}}
		cur := []ControlPoint{pts[0]}
		perStage := make(map[string]ControlPoint)
		ok := true
		cw, chh := w, h
		for _, st := range stages {
			nx, ny := st.Forward(cur[0].X, cur[0].Y)
			nw, nh := st.OutSize(int(cw), int(chh))
			if nx < 1 || ny < 1 || nx > float64(nw)-2 || ny > float64(nh)-2 {
				ok = false
				break
			}
			cur = []ControlPoint{{ID: accepted, X: nx, Y: ny}}
			perStage[st.ID()] = cur[0]
			cw, chh = float64(nw), float64(nh)
		}
		if !ok {
			continue
		}
		t.Source = append(t.Source, pts[0])
		for _, st := range stages {
			t.StagePointsAppend(st.ID(), perStage[st.ID()])
		}
		accepted++
	}
	return t
}

// StagePointsAppend 向真值追加某阶段的一个控制点。
func (t *Truth) StagePointsAppend(stageID string, pt ControlPoint) {
	t.Stages[stageID] = append(t.Stages[stageID], pt)
}
