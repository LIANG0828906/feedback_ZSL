package loader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/fixedpoint"
	"github.com/gameaudio/workbench/internal/geometry"
	"github.com/gameaudio/workbench/internal/model"
)

// LoadFile 从磁盘读取并完整校验一个确定性工程。
func LoadFile(path string, cfg model.GeoConfig) (*model.Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New(errors.ESchema, "file", err.Error())
	}
	return Load(raw, cfg)
}

// Load 解析 JSON 并执行全部结构与引用校验。
func Load(raw []byte, cfg model.GeoConfig) (*model.Project, error) {
	var p model.Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New(errors.ESchema, "json", err.Error())
	}
	if err := Validate(&p, cfg); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate 执行字段完整性、几何结构与引用校验。
func Validate(p *model.Project, cfg model.GeoConfig) error {
	if p.SchemaVersion == "" {
		return errors.New(errors.ESchema, "schemaVersion", "字段缺失")
	}
	if p.Level.ID == "" {
		return errors.New(errors.ESchema, "level.id", "字段缺失")
	}
	if !finite(p.Level.X0) || !finite(p.Level.Y0) || !finite(p.Level.X1) || !finite(p.Level.Y1) {
		return errors.New(errors.ESchema, "level", "关卡范围存在非有限数值")
	}
	if p.Level.X1 <= p.Level.X0 || p.Level.Y1 <= p.Level.Y0 {
		return errors.New(errors.ESchema, "level", "关卡范围必须为正面积矩形")
	}
	if len(p.Regions) == 0 {
		return errors.New(errors.ESchema, "regions", "至少需要一个混响区域")
	}
	if len(p.Sources) == 0 || len(p.Listeners) == 0 {
		return errors.New(errors.ESchema, "sources/listeners", "声源与听者均不能为空")
	}

	conv, err := fixedpoint.NewConverter(cfg)
	if err != nil {
		return err
	}

	regionIDs := map[string]bool{}
	for i := range p.Regions {
		r := &p.Regions[i]
		if r.ID == "" {
			return errors.New(errors.ESchema, fmt.Sprintf("regions[%d].id", i), "区域标识缺失")
		}
		if regionIDs[r.ID] {
			return errors.New(errors.ESchema, "region:"+r.ID, "区域标识重复")
		}
		regionIDs[r.ID] = true
		if len(r.Vertices) < 3 {
			return errors.New(errors.ESchema, "region:"+r.ID, "多边形未闭合：顶点数少于 3")
		}
		for _, v := range r.Vertices {
			if !finite(v.X) || !finite(v.Y) {
				return errors.New(errors.ESchema, "region:"+r.ID, "多边形顶点存在非有限数值")
			}
		}
		fv := make([]geometry.FPoint, len(r.Vertices))
		for k, v := range r.Vertices {
			fv[k] = geometry.FPoint{X: conv.FromFloat(v.X), Y: conv.FromFloat(v.Y)}
		}
		if !geometry.PolygonClosedAndSimple(fv) {
			return errors.New(errors.ESchema, "region:"+r.ID, "多边形不闭合或存在自交")
		}
		if r.ReverbTime <= 0 || r.ERDensity <= 0 || r.Gain <= 0 {
			return errors.New(errors.ESchema, "region:"+r.ID, "混响时间/反射密度/增益必须为正数")
		}
	}

	// 墙体端点有限性与退化检查。
	wallIDs := map[string]bool{}
	for _, w := range p.Walls {
		if w.ID == "" {
			return errors.New(errors.ESchema, "wall.id", "墙体标识缺失")
		}
		if wallIDs[w.ID] {
			return errors.New(errors.ESchema, "wall:"+w.ID, "墙体标识重复")
		}
		wallIDs[w.ID] = true
		if !finite(w.A.X) || !finite(w.A.Y) || !finite(w.B.X) || !finite(w.B.Y) {
			return errors.New(errors.ESchema, "wall:"+w.ID, "墙体端点存在非有限数值")
		}
		if w.A.X == w.B.X && w.A.Y == w.B.Y {
			return errors.New(errors.EDegen, "wall:"+w.ID, "墙体端点退化重合")
		}
	}

	// 声源 / 听者的区域引用与关卡范围。
	for i, s := range p.Sources {
		if s.ID == "" {
			return errors.New(errors.ESchema, fmt.Sprintf("sources[%d].id", i), "声源标识缺失")
		}
		if !regionIDs[s.RegionID] {
			return errors.New(errors.ERef, "source:"+s.ID,
				"引用不存在的区域 "+s.RegionID)
		}
		if err := checkInLevel(p.Level, s.Pos, "source:"+s.ID); err != nil {
			return err
		}
	}
	for i, l := range p.Listeners {
		if l.ID == "" {
			return errors.New(errors.ESchema, fmt.Sprintf("listeners[%d].id", i), "听者标识缺失")
		}
		if !regionIDs[l.RegionID] {
			return errors.New(errors.ERef, "listener:"+l.ID,
				"引用不存在的区域 "+l.RegionID)
		}
		if err := checkInLevel(p.Level, l.Pos, "listener:"+l.ID); err != nil {
			return err
		}
	}

	// 每对声源-听者的真值声明必须存在。
	truthKey := map[string]bool{}
	for _, t := range p.Truths {
		truthKey[t.SourceID+"|"+t.ListenerID] = true
		if len(t.Curve) == 0 {
			return errors.New(errors.ESchema, "truth:"+t.SourceID+"->"+t.ListenerID,
				"真值衰减曲线采样缺失")
		}
	}
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			if !truthKey[s.ID+"|"+l.ID] {
				return errors.New(errors.ESchema,
					"truth:"+s.ID+"->"+l.ID, "缺少该声源听者对的真值声明")
			}
		}
	}
	return nil
}

func checkInLevel(lv model.Level, p model.Point, field string) error {
	if !finite(p.X) || !finite(p.Y) {
		return errors.New(errors.ESchema, field, "坐标存在非有限数值")
	}
	if p.X < lv.X0 || p.X > lv.X1 || p.Y < lv.Y0 || p.Y > lv.Y1 {
		return errors.New(errors.ESchema, field,
			fmt.Sprintf("坐标 (%v,%v) 超出关卡范围", p.X, p.Y))
	}
	return nil
}

func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
