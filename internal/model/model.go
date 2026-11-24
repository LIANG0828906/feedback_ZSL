package model

// 数据模型：确定性样本工程的全部结构均使用显式的 JSON 标签，
// 字段缺失由反序列化后的零值校验捕获并返回 E_SCHEMA。

// Project 是一个平面关卡的完整声学工程。
type Project struct {
	SchemaVersion string     `json:"schemaVersion"`
	Level         Level      `json:"level"`
	Regions       []Region   `json:"regions"`
	Sources       []Source   `json:"sources"`
	Listeners     []Listener `json:"listeners"`
	Walls         []Wall     `json:"walls"`
	Truths        []Truth    `json:"truths"`
}

// Level 描述关卡矩形范围。
type Level struct {
	ID string  `json:"id"`
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

// Region 是一个混响区域（简单多边形，顶点不含重复闭合点）。
type Region struct {
	ID           string  `json:"id"`
	Vertices     []Point `json:"vertices"`
	ReverbTime   float64 `json:"reverbTime"`   // 混响时间 RT60，秒
	ERDensity    float64 `json:"erDensity"`    // 早期反射密度
	Gain         float64 `json:"gain"`         // 区域增益（线性）
	ParamVersion int     `json:"paramVersion"` // 参数版本，编辑时递增
	GeomVersion  int     `json:"geomVersion"`  // 几何版本，边界拖动时递增
}

// Point 是平面坐标点。
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Source 是声源，归属一个混响区域。
type Source struct {
	ID       string `json:"id"`
	RegionID string `json:"regionId"`
	Pos      Point  `json:"pos"`
}

// Listener 是听者路径点，归属一个混响区域。
type Listener struct {
	ID       string `json:"id"`
	RegionID string `json:"regionId"`
	Pos      Point  `json:"pos"`
	WindowID int    `json:"windowId"` // 检查点窗口分组
}

// Wall 是遮挡墙体线段。
type Wall struct {
	ID string `json:"id"`
	A  Point  `json:"a"`
	B  Point  `json:"b"`
}

// Truth 是生成器给出的解析真值。
type Truth struct {
	SourceID       string    `json:"sourceId"`
	ListenerID     string    `json:"listenerId"`
	Occluded       bool      `json:"occluded"`
	DiffractionLen float64   `json:"diffractionLen"`
	Curve          []float64 `json:"curve"`
	SPL            float64   `json:"spl"`
}

// Result 是工作台对单个声源-听者对的完整计算结果。
type Result struct {
	SourceID       string    `json:"sourceId"`
	ListenerID     string    `json:"listenerId"`
	RegionID       string    `json:"regionId"`
	Occluded       bool      `json:"occluded"`
	WallCount      int       `json:"wallCount"`
	DirectLen      float64   `json:"directLen"`
	DiffractionLen float64   `json:"diffractionLen"`
	EffectiveLen   float64   `json:"effectiveLen"`
	Curve          []float64 `json:"curve"`
	SPL            float64   `json:"spl"`
	Failed         bool      `json:"failed"`
	ErrorCode      string    `json:"errorCode,omitempty"`
	ErrorDetail    string    `json:"errorDetail,omitempty"`
	CacheHit       bool      `json:"cacheHit"`
}

// Profile 是逐听者路径点的声学剖面（聚合该听者全部声源）。
type Profile struct {
	ListenerID    string    `json:"listenerId"`
	WindowID      int       `json:"windowId"`
	MeanSPL       float64   `json:"meanSpl"`
	MaxSPL        float64   `json:"maxSpl"`
	OccludedPairs int       `json:"occludedPairs"`
	TotalPairs    int       `json:"totalPairs"`
	FailedPairs   int       `json:"failedPairs"`
	CurveMean     []float64 `json:"curveMean"`
}

// Invalidation 记录一次编辑导致的失效对象与原因。
type Invalidation struct {
	ObjectID string `json:"objectId"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
}

// GeoConfig 声明定点几何运算的精度参数。
type GeoConfig struct {
	ScaleBits int `json:"scaleBits"` // 定点小数位数，合法区间 [1, 8]
}
