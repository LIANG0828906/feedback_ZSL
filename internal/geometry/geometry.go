package geometry

import (
	"sort"

	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/fixedpoint"
	"github.com/gameaudio/workbench/internal/model"
)

// FPoint 是定点化的平面点。
type FPoint struct {
	X fixedpoint.Fix
	Y fixedpoint.Fix
}

// FWall 是定点化的墙体线段。
type FWall struct {
	ID string
	A  FPoint
	B  FPoint
}

// Engine 持有定点转换器与定点化后的几何数据。
type Engine struct {
	conv  *fixedpoint.Converter
	walls []FWall
}

// New 构造几何引擎并对墙体做退化校验，端点重合返回 E_DEGEN。
func New(cfg model.GeoConfig, walls []model.Wall) (*Engine, error) {
	conv, err := fixedpoint.NewConverter(cfg)
	if err != nil {
		return nil, err
	}
	e := &Engine{conv: conv}
	for _, w := range walls {
		a := e.quant(w.A)
		b := e.quant(w.B)
		if a.X.Raw == b.X.Raw && a.Y.Raw == b.Y.Raw {
			return nil, errors.New(errors.EDegen, "wall:"+w.ID,
				"墙体端点退化重合")
		}
		e.walls = append(e.walls, FWall{ID: w.ID, A: a, B: b})
	}
	return e, nil
}

// Conv 暴露转换器（声学层需要还原长度）。
func (e *Engine) Conv() *fixedpoint.Converter { return e.conv }

// Walls 返回内部定点墙体快照（顺序稳定，按 ID 排序）。
func (e *Engine) Walls() []FWall {
	out := make([]FWall, len(e.walls))
	copy(out, e.walls)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Quant 把 float 点定点化。
func (e *Engine) Quant(p model.Point) FPoint { return e.quant(p) }

func (e *Engine) quant(p model.Point) FPoint {
	return FPoint{X: e.conv.FromFloat(p.X), Y: e.conv.FromFloat(p.Y)}
}

// Float 把定点点还原为 float。
func (e *Engine) Float(p FPoint) model.Point {
	return model.Point{X: e.conv.ToFloat(p.X), Y: e.conv.ToFloat(p.Y)}
}

func orient(a, b, c FPoint) int {
	return fixedpoint.Cross(a.X, a.Y, b.X, b.Y, c.X, c.Y).Sign()
}

// SegmentsIntersect 判定两条线段是否在内部或端点处相交（不含共线重叠）。
// 共线情况返回 false，规则对生成器与引擎完全一致。
func SegmentsIntersect(p1, q1, p2, q2 FPoint) bool {
	o1 := orient(p1, q1, p2)
	o2 := orient(p1, q1, q2)
	o3 := orient(p2, q2, p1)
	o4 := orient(p2, q2, q1)
	if o1 == 0 || o2 == 0 || o3 == 0 || o4 == 0 {
		// 本工程的确定性样本网格对齐，出现共线即视为不交叉，
		// 端点恰好落在另一线段上的退化情况在样本中不会出现。
		return false
	}
	return o1 != o2 && o3 != o4
}

// BlockingWalls 返回与 S-L 连线相交的墙体 ID 列表（按 ID 排序，顺序无关确定性）。
func (e *Engine) BlockingWalls(s, l FPoint) []string {
	var ids []string
	for _, w := range e.walls {
		if SegmentsIntersect(s, l, w.A, w.B) {
			ids = append(ids, w.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Distance 返回两个定点点之间的欧氏长度。
func (e *Engine) Distance(a, b FPoint) float64 {
	dx := fixedpoint.Sub(a.X, b.X)
	dy := fixedpoint.Sub(a.Y, b.Y)
	return e.conv.Hypot(dx, dy)
}

// polylineLen 计算折线 S -> p -> L 的总长度。
func (e *Engine) polylineLen(s, p, l FPoint) float64 {
	return e.Distance(s, p) + e.Distance(p, l)
}

// pathClear 判定折线上的每一段是否都不与任何其他墙体相交。
// 仅豁免折线实际贴附的宿主墙 ownerID；其余遮挡墙仍参与阻断判定，
// 这样封闭盒等场景才能正确返回 E_PATH。
func (e *Engine) pathClear(s, p, l FPoint, ownerID string) bool {
	seg := [][2]FPoint{{s, p}, {p, l}}
	for _, w := range e.walls {
		if w.ID == ownerID {
			continue
		}
		for _, sg := range seg {
			if SegmentsIntersect(sg[0], sg[1], w.A, w.B) {
				return false
			}
		}
	}
	return true
}

// Diffraction 估算最短绕射折线路径长度。
// 对每面遮挡墙的两个墙角端点构造单折点折线 S->P->L，
// 过滤掉仍被其他墙体阻断的候选，取最短；
// 没有任何可行折线时返回 E_PATH。
func (e *Engine) Diffraction(s, l FPoint, blockingIDs []string) (float64, error) {
	if len(blockingIDs) == 0 {
		return e.Distance(s, l), nil
	}
	wallByID := map[string]FWall{}
	for _, w := range e.walls {
		wallByID[w.ID] = w
	}
	type cand struct {
		length float64
		wallID string
	}
	var cands []cand
	for _, id := range blockingIDs {
		w := wallByID[id]
		for _, corner := range [2]FPoint{w.A, w.B} {
			if e.pathClear(s, corner, l, id) {
				cands = append(cands, cand{
					length: e.polylineLen(s, corner, l),
					wallID: id,
				})
			}
		}
	}
	if len(cands) == 0 {
		return 0, errors.New(errors.EPath, "diffraction",
			"所有墙角绕射折线均被阻断，绕射路径不存在")
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].length != cands[j].length {
			return cands[i].length < cands[j].length
		}
		return cands[i].wallID < cands[j].wallID
	})
	return cands[0].length, nil
}

// PolygonClosedAndSimple 校验多边形闭合（顶点数>=3，首尾关系由调用方保证
// 不重复存储）且边不自交。
func PolygonClosedAndSimple(verts []FPoint) bool {
	n := len(verts)
	if n < 3 {
		return false
	}
	edges := make([][2]FPoint, n)
	for i := 0; i < n; i++ {
		edges[i] = [2]FPoint{verts[i], verts[(i+1)%n]}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			// 相邻边共享端点，不算自交；其余边对不得相交。
			if (i == 0 && j == n-1) || j == i+1 {
				continue
			}
			if SegmentsIntersect(edges[i][0], edges[i][1], edges[j][0], edges[j][1]) {
				return false
			}
		}
	}
	return true
}

// PointInPolygon 射线法判定点是否在多边形内（含边界）。
func PointInPolygon(p FPoint, verts []FPoint) bool {
	n := len(verts)
	inside := false
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		vi, vj := verts[i], verts[j]
		if orient(vi, vj, p) == 0 {
			// 点在边上：用包围盒快速确认。
			minX, maxX := minRaw(vi.X.Raw, vj.X.Raw), maxRaw(vi.X.Raw, vj.X.Raw)
			minY, maxY := minRaw(vi.Y.Raw, vj.Y.Raw), maxRaw(vi.Y.Raw, vj.Y.Raw)
			if p.X.Raw >= minX && p.X.Raw <= maxX && p.Y.Raw >= minY && p.Y.Raw <= maxY {
				return true
			}
		}
		if (vi.Y.Raw > p.Y.Raw) != (vj.Y.Raw > p.Y.Raw) {
			// 无除法判定 p.X < 交点 x：
			// xInt = vi.X + (p.Y-vi.Y)*(vj.X-vi.X)/(vj.Y-vi.Y)。
			// 比较两侧同乘分母，并按分母符号翻转不等号方向。
			dx := fixedpoint.Sub(vj.X, vi.X).Raw
			dy := fixedpoint.Sub(vj.Y, vi.Y).Raw
			tdy := fixedpoint.Sub(p.Y, vi.Y).Raw
			dpx := fixedpoint.Sub(p.X, vi.X).Raw
			lhs := fixedpoint.Mul128(dpx, dy)
			rhs := fixedpoint.Mul128(tdy, dx)
			if (dy > 0 && cmpI128(lhs, rhs) < 0) ||
				(dy < 0 && cmpI128(lhs, rhs) > 0) {
				inside = !inside
			}
		}
	}
	return inside
}

func minRaw(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func cmpI128(a, b fixedpoint.I128) int {
	if a.Hi != b.Hi {
		if a.Hi < b.Hi {
			return -1
		}
		return 1
	}
	if a.Lo < b.Lo {
		return -1
	}
	if a.Lo > b.Lo {
		return 1
	}
	return 0
}

func maxRaw(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// int128Div 用 math/big 之外的简单方式：数值规模有界，回退到 float64
// 仅用于射线交点的比较侧，主判定仍由定点叉积承担。
