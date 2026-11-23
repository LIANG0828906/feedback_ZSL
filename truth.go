package ortho

import (
	"encoding/json"
	"math"
	"os"
	"sort"
)

// ControlPoint 为真值控制点。
type ControlPoint struct {
	ID int     `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

// Truth 为解析真值：源图控制点、逐阶段校正坐标、畸变中心与半径。
type Truth struct {
	Seed             int64                     `json:"seed"`
	DistortionCenter [2]float64                `json:"distortion_center"`
	DistortionRadius float64                   `json:"distortion_radius"`
	Source           []ControlPoint            `json:"source"`
	Stages           map[string][]ControlPoint `json:"stages"`
}

// SaveTruth 将真值写入 JSON 文件。
func SaveTruth(path string, t *Truth) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadTruth 从 JSON 文件读取真值。
func LoadTruth(path string) (*Truth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Truth
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// StageGeomReport 为单阶段重投影误差报告。
type StageGeomReport struct {
	Stage  string
	Count  int
	Median float64
	Max    float64
}

// CheckGeometry 将源图控制点经各阶段正向变换并与真值逐阶段比对。
// 任一阶段最大误差超过 tol 时返回 E_GEOM 并列出超差控制点编号与实测值。
func CheckGeometry(stages []Stage, truth *Truth, tol float64) ([]StageGeomReport, *Error) {
	reports := make([]StageGeomReport, 0, len(stages))
	prev := truth.Source
	for _, st := range stages {
		want, ok := truth.Stages[st.ID()]
		if !ok {
			return reports, &Error{Code: EGeom, Stage: st.ID(), Msg: "truth missing for stage"}
		}
		if len(want) != len(prev) {
			return reports, &Error{Code: EGeom, Stage: st.ID(), Msg: "control point count mismatch"}
		}
		errs := make([]float64, len(prev))
		var outliers []GeomOutlier
		for i, pt := range prev {
			gx, gy := st.Forward(pt.X, pt.Y)
			d := dist(gx, gy, want[i].X, want[i].Y)
			errs[i] = d
			if d > tol {
				outliers = append(outliers, GeomOutlier{PointID: want[i].ID, Measured: d, Tolerance: tol})
			}
		}
		sorted := make([]float64, len(errs))
		copy(sorted, errs)
		sort.Float64s(sorted)
		median := sorted[len(sorted)/2]
		if len(sorted)%2 == 0 && len(sorted) > 1 {
			median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
		}
		reports = append(reports, StageGeomReport{
			Stage:  st.ID(),
			Count:  len(errs),
			Median: median,
			Max:    sorted[len(sorted)-1],
		})
		if len(outliers) > 0 {
			return reports, &Error{Code: EGeom, Stage: st.ID(), Msg: "reprojection error exceeds declared tolerance", Outliers: outliers}
		}
		prev = want
	}
	return reports, nil
}

func dist(x1, y1, x2, y2 float64) float64 {
	dx, dy := x1-x2, y1-y2
	return math.Sqrt(dx*dx + dy*dy)
}
