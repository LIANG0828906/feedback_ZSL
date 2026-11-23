package ortho

import (
	"fmt"
	"strings"
)

// Code 为组件对外暴露的稳定错误码。
type Code string

const (
	EParam  Code = "E_PARA"   // 阶段参数越界或非法
	EDecode Code = "E_DECODE" // 输入图像不可解码
	EGeom   Code = "E_GEOM"   // 真值重投影误差超差
	EBand   Code = "E_BAND"   // 分块重叠边带不足
	EDiff   Code = "E_DIFF"   // 增量与全量结果不一致
	EStage  Code = "E_STAGE"  // 阶段内部执行异常
	EState  Code = "E_STATE"  // 缓存提交违反不变式
)

// GeomOutlier 描述一个超差控制点。
type GeomOutlier struct {
	PointID   int
	Measured  float64
	Tolerance float64
}

// Error 为组件统一错误类型，携带阶段标识、字段名与定位信息。
type Error struct {
	Code     Code
	Stage    string
	Field    string
	Msg      string
	X, Y     int // E_DIFF 首个不一致像素坐标
	NeedBand int // E_BAND 所需最小边带宽度
	Outliers []GeomOutlier
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", e.Code)
	if e.Stage != "" {
		fmt.Fprintf(&b, " stage=%s", e.Stage)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.Msg != "" {
		fmt.Fprintf(&b, ": %s", e.Msg)
	}
	if e.Code == EBand {
		fmt.Fprintf(&b, " (need band >= %d)", e.NeedBand)
	}
	if e.Code == EDiff {
		fmt.Fprintf(&b, " first mismatch at (%d,%d)", e.X, e.Y)
	}
	if e.Code == EGeom && len(e.Outliers) > 0 {
		ids := make([]string, 0, len(e.Outliers))
		for _, o := range e.Outliers {
			ids = append(ids, fmt.Sprintf("#%d=%.4f", o.PointID, o.Measured))
		}
		fmt.Fprintf(&b, " outliers: %s", strings.Join(ids, ","))
	}
	return b.String()
}
