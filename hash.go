package lipsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CanonicalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("lipsync: canonical marshal failed: %v", err))
	}
	return data
}

func HashCanonical(prefix string, v any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prefix))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(CanonicalJSON(v))
	return hex.EncodeToString(h.Sum(nil))
}

func ComputeDialogueHash(d *Dialogue) string {
	if d == nil {
		return ""
	}
	copyValue := *d
	copyValue.ContentHash = ""
	return HashCanonical("dialogue-v1", &copyValue)
}

func EnsureDialogueHashes(p *Project) {
	if p == nil {
		return
	}
	for _, d := range p.Dialogues {
		if d != nil {
			d.ContentHash = ComputeDialogueHash(d)
		}
	}
}

func PackSummaryHash(pack *Pack) string {
	if pack == nil {
		return ""
	}
	light := struct {
		DialogueID string   `json:"dialogueId"`
		Index      int      `json:"index"`
		PhonemeIDs []string `json:"phonemeIds"`
		StartFrame int      `json:"startFrame"`
		EndFrame   int      `json:"endFrame"`
	}{pack.DialogueID, pack.Index, pack.PhonemeIDs, pack.StartFrame, pack.EndFrame}
	return HashCanonical("pack-summary-v1", light)
}

func CacheKey(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0x1f})
		}
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
