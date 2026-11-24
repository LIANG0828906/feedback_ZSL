package generator

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/gameaudio/workbench/internal/acoustics"
	"github.com/gameaudio/workbench/internal/geometry"
	"github.com/gameaudio/workbench/internal/model"
)

// Seed 是固定随机种子，保证每次生成的工程逐字节一致。
const Seed int64 = 20261124

// DefaultScaleBits 是样本声明的定点小数位。
const DefaultScaleBits = 4

// SampleCurveCount 与声学层采样数保持一致。
const SampleCurveCount = acoustics.SampleCount

// Generate 按固定种子产出完整工程与解析真值。
func Generate() *model.Project {
	rng := rand.New(rand.NewSource(Seed))

	level := model.Level{ID: "L1", X0: 0, Y0: 0, X1: 40, Y1: 30}

	// 两个互不重叠的矩形混响区域，左右分栏。
	regions := []model.Region{
		{
			ID: "R1",
			Vertices: []model.Point{
				{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 30}, {X: 0, Y: 30},
			},
			ReverbTime:   1.2,
			ERDensity:    24,
			Gain:         1.0,
			ParamVersion: 1,
			GeomVersion:  1,
		},
		{
			ID: "R2",
			Vertices: []model.Point{
				{X: 20, Y: 0}, {X: 40, Y: 0}, {X: 40, Y: 30}, {X: 20, Y: 30},
			},
			ReverbTime:   0.7,
			ERDensity:    40,
			Gain:         0.8,
			ParamVersion: 1,
			GeomVersion:  1,
		},
	}

	// 声源：每个区域 3 个，坐标来自确定性 RNG 并量化到 0.5 网格。
	sources := make([]model.Source, 0, 6)
	for ri, r := range regions {
		baseX := 2 + ri*20
		for k := 0; k < 3; k++ {
			sources = append(sources, model.Source{
				ID:       sourceID(ri, k),
				RegionID: r.ID,
				Pos: model.Point{
					X: float64(baseX + rng.Intn(14)),
					Y: float64(2 + rng.Intn(26)),
				},
			})
		}
	}

	// 听者路径点：每个区域 4 个，按顺序分窗口（每 2 个听者一个窗口）。
	listeners := make([]model.Listener, 0, 8)
	for ri, r := range regions {
		baseX := 4 + ri*20
		for k := 0; k < 4; k++ {
			listeners = append(listeners, model.Listener{
				ID:       listenerID(ri, k),
				RegionID: r.ID,
				WindowID: (ri*4 + k) / 2,
				Pos: model.Point{
					X: float64(baseX + rng.Intn(12)),
					Y: float64(2 + rng.Intn(26)),
				},
			})
		}
	}

	// 墙体：确定性生成竖墙、横墙与斜墙，拒绝退化与重复。
	walls := generateWalls(rng)

	p := &model.Project{
		SchemaVersion: "1.0",
		Level:         level,
		Regions:       regions,
		Sources:       sources,
		Listeners:     listeners,
		Walls:         walls,
	}

	// 解析真值：使用与工作台完全相同的定点几何引擎与声学合成器构造，
	// 真值只来自这里的几何与参数，不读取任何工作台缓存。
	p.Truths = buildTruths(p)
	return p
}

func generateWalls(rng *rand.Rand) []model.Wall {
	walls := make([]model.Wall, 0, 10)
	seen := map[[4]int]bool{}
	add := func(id string, ax, ay, bx, by int) {
		if ax == bx && ay == by {
			return
		}
		key := [4]int{ax, ay, bx, by}
		if seen[key] {
			return
		}
		seen[key] = true
		walls = append(walls, model.Wall{
			ID: id,
			A:  model.Point{X: float64(ax), Y: float64(ay)},
			B:  model.Point{X: float64(bx), Y: float64(by)},
		})
	}

	// 墙体全部布置在 R1（x∈[2,18]）内部，长度 2~3，位置来自固定 RNG。
	// 稀疏布局保证任何被遮挡对都至少有一条墙角绕射折线可达；
	// E_PATH 异常链路由 geometry 包的专门单元测试覆盖。
	// 4 面竖墙：x 取 5/8/11/14 附近，y 区间互不重叠。
	for i := 0; i < 4; i++ {
		x := 5 + i*3 + rng.Intn(2)
		y0 := 3 + rng.Intn(4)
		add(wallID(len(walls)), x, y0, x, y0+2)
	}
	// 2 面横墙：放在竖墙之间的空隙。
	for i := 0; i < 2; i++ {
		y := 18 + i*6 + rng.Intn(3)
		x0 := 4 + rng.Intn(4)
		add(wallID(len(walls)), x0, y, x0+3, y)
	}
	// 1 面斜墙，位于角落且很短。
	{
		x := 15 + rng.Intn(2)
		y := 22 + rng.Intn(3)
		add(wallID(len(walls)), x, y, x+2, y+2)
	}
	return walls
}

func buildTruths(p *model.Project) []model.Truth {
	eng, err := geometry.New(model.GeoConfig{ScaleBits: DefaultScaleBits}, p.Walls)
	if err != nil {
		panic("generator geometry: " + err.Error())
	}
	regionByID := map[string]model.Region{}
	for _, r := range p.Regions {
		regionByID[r.ID] = r
	}
	truths := make([]model.Truth, 0, len(p.Sources)*len(p.Listeners))
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			fS := eng.Quant(s.Pos)
			fL := eng.Quant(l.Pos)
			blocking := eng.BlockingWalls(fS, fL)
			occluded := len(blocking) > 0
			direct := eng.Distance(fS, fL)
			eff := direct
			diff := 0.0
			if occluded {
				diff, err = eng.Diffraction(fS, fL, blocking)
				if err != nil {
					panic("generator diffraction: " + err.Error())
				}
				eff = diff
			}
			region := regionByID[s.RegionID]
			out := acoustics.Synthesize(acoustics.Input{
				Region:         region,
				EffectiveLen:   eff,
				Occluded:       occluded,
				DiffractionLen: diff,
			})
			truths = append(truths, model.Truth{
				SourceID:       s.ID,
				ListenerID:     l.ID,
				Occluded:       occluded,
				DiffractionLen: diff,
				Curve:          out.Curve,
				SPL:            out.SPL,
			})
		}
	}
	return truths
}

// WriteFile 把工程以确定性 JSON 落盘。
func WriteFile(p *model.Project, dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func sourceID(ri, k int) string {
	return "S" + string(rune('1'+ri)) + "-" + string(rune('1'+k))
}

func listenerID(ri, k int) string {
	return "L" + string(rune('1'+ri)) + "-" + string(rune('1'+k))
}

func wallID(i int) string {
	return "W" + itoa(i+1)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
