package colortrace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CanonicalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var canonical any = canonicalValue(raw)
	return json.Marshal(canonical)
}

func canonicalValue(v any) any {
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(value))
		for _, key := range keys {
			out[key] = canonicalValue(value[key])
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = canonicalValue(value[i])
		}
		return out
	default:
		return v
	}
}

func HashCanonical(prefix string, values ...any) string {
	h := sha256.New()
	fmt.Fprint(h, prefix)
	for _, value := range values {
		data, err := CanonicalJSON(value)
		if err != nil {
			panic(err)
		}
		h.Write([]byte{0})
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ContentHashValues(values []int64) string {
	return HashCanonical("region-content-v1", values)
}

// HashChunkContent returns the stable chunk content hash excluding the
// self-referential ContentHash field.
func HashChunkContent(result *ChunkResult) string {
	if result == nil {
		return ""
	}
	copyValue := *result
	copyValue.ContentHash = ""
	return HashCanonical("chunk-result-v1", copyValue)
}

func SequenceFingerprint(stages []StageConfig) string {
	type slim struct {
		Index        int    `json:"index"`
		ID           string `json:"id"`
		FactorX      int    `json:"factor_x"`
		FactorY      int    `json:"factor_y"`
		Bits         int    `json:"bits"`
		ParamVersion string `json:"param_version"`
	}
	items := make([]slim, 0, len(stages))
	for _, stage := range stages {
		items = append(items, slim{
			Index: stage.Index, ID: stage.ID, FactorX: stage.FactorX,
			FactorY: stage.FactorY, Bits: stage.Bits, ParamVersion: stage.ParamVersion,
		})
	}
	return HashCanonical("stage-sequence-v1", items)
}

func ParamVersionFingerprint(stages []StageConfig) string {
	return HashCanonical("stage-params-v1", stages)
}

func ChunkID(c ChunkCoord) string {
	return fmt.Sprintf("c%02dx%02d", c.X, c.Y)
}

func UnitKey(stage int, chunkID string) string {
	return fmt.Sprintf("s%02d/%s", stage, chunkID)
}
