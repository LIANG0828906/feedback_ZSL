package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/gameaudio/workbench/internal/model"
)

// Content 对任意可 JSON 序列化结构计算稳定 SHA-256。
func Content(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		// 工程结构全部为基础类型，理论上不会失败。
		panic("hash marshal: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RegionFingerprint 是区域几何与参数的完整指纹。
func RegionFingerprint(r model.Region) string {
	return Content(struct {
		ID           string        `json:"id"`
		Vertices     []model.Point `json:"vertices"`
		ReverbTime   float64       `json:"reverbTime"`
		ERDensity    float64       `json:"erDensity"`
		Gain         float64       `json:"gain"`
		ParamVersion int           `json:"paramVersion"`
		GeomVersion  int           `json:"geomVersion"`
	}{
		ID: r.ID, Vertices: r.Vertices, ReverbTime: r.ReverbTime,
		ERDensity: r.ERDensity, Gain: r.Gain,
		ParamVersion: r.ParamVersion, GeomVersion: r.GeomVersion,
	})
}

// WallFingerprint 是墙体集合的指纹（排序后哈希，顺序无关）。
func WallFingerprint(walls []model.Wall) string {
	cp := make([]model.Wall, len(walls))
	copy(cp, walls)
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	return Content(cp)
}

// PairInput 是单对计算的完整输入快照。
type PairInput struct {
	Source      model.Source   `json:"source"`
	Listener    model.Listener `json:"listener"`
	Region      model.Region   `json:"region"`
	Walls       []model.Wall   `json:"walls"`
	ScaleBits   int            `json:"scaleBits"`
	AlgoVersion string         `json:"algoVersion"`
}

// PairHash 计算单对输入哈希，作为缓存键与快照内容校验依据。
func PairHash(in PairInput) string {
	return Content(in)
}
