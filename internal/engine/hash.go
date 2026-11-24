package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"replaykernel/internal/model"
)

// HashReplay 计算回放原始载荷的内容哈希，作为恢复时的整体作废依据。
func HashReplay(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// HashTruth 计算真值指纹（规范化 JSON 后 SHA-256）。
func HashTruth(t *model.MatchTruth) string {
	b, err := json.Marshal(t)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashWindowReport 计算窗口报告内容哈希（不含哈希字段自身）。
func hashWindowReport(r *model.WindowReport) string {
	cp := *r
	cp.ContentHash = ""
	b, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
