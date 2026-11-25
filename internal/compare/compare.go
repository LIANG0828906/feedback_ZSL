package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"terrainpipeline/internal/model"
)

func BakeOutputs(wantBlocks, gotBlocks map[string]*model.BlockResult, wantSeams, gotSeams []*model.SeamReport) *model.Error {
	ids := make([]string, 0, len(wantBlocks))
	for id := range wantBlocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	gotIDs := make([]string, 0, len(gotBlocks))
	for id := range gotBlocks {
		gotIDs = append(gotIDs, id)
	}
	sort.Strings(gotIDs)
	if fmt.Sprint(ids) != fmt.Sprint(gotIDs) {
		return model.NewError(model.CodeDiff, "", "blocks", "块集合不一致: want=%v got=%v", ids, gotIDs)
	}

	for _, id := range ids {
		w := wantBlocks[id]
		g := gotBlocks[id]
		if w.NormalDigest != g.NormalDigest {
			if pixel := firstPixelMismatch(w, g); pixel != "" {
				return model.NewError(model.CodeDiff, id, "normal_map", "法线图首个不一致位置 %s; want_digest=%s got_digest=%s", pixel, w.NormalDigest[:12], g.NormalDigest[:12])
			}
			return model.NewError(model.CodeDiff, id, "normal_map", "法线图摘要不一致 want=%s got=%s", w.NormalDigest, g.NormalDigest)
		}
		if w.SlopeDigest != g.SlopeDigest {
			if idx := firstSlopeMismatch(w, g); idx >= 0 {
				wp := w.Pixels[idx]
				gp := g.Pixels[idx]
				return model.NewError(model.CodeDiff, id, "slope_map", "坡度图首个不一致像素 (%d,%d): want=%.12f got=%.12f", wp.I, wp.J, wp.SlopeDeg, gp.SlopeDeg)
			}
			return model.NewError(model.CodeDiff, id, "slope_map", "坡度图摘要不一致 want=%s got=%s", w.SlopeDigest, g.SlopeDigest)
		}
		if w.WeightDigest != g.WeightDigest {
			return model.NewError(model.CodeDiff, id, "weights", "混合权重摘要不一致 want=%s got=%s", w.WeightDigest, g.WeightDigest)
		}
		if w.ResultHash != g.ResultHash {
			return model.NewError(model.CodeDiff, id, "result_hash", "块结果哈希不一致 want=%s got=%s", w.ResultHash, g.ResultHash)
		}
	}

	if len(wantSeams) != len(gotSeams) {
		return model.NewError(model.CodeDiff, "", "seams", "接缝统计数量不一致 want=%d got=%d", len(wantSeams), len(gotSeams))
	}
	for i := range wantSeams {
		w := wantSeams[i]
		g := gotSeams[i]
		if w.SeamID != g.SeamID {
			return model.NewError(model.CodeDiff, w.SeamID, "seam_id", "接缝顺序/ID不一致 want=%s got=%s", w.SeamID, g.SeamID)
		}
		wantJSON, _ := json.Marshal(w)
		gotJSON, _ := json.Marshal(g)
		if string(wantJSON) != string(gotJSON) {
			index := firstSeamPointMismatch(w, g)
			if index >= 0 {
				return model.NewError(model.CodeDiff, w.BlockA+"/"+w.BlockB, "seam_stats",
					"接缝 %s 首个不一致点 index=%d; want_raw=%.9f got_raw=%.9f want_weld=%.9f got_weld=%.9f",
					w.SeamID, index, w.Points[index].RawAngleDeg, g.Points[index].RawAngleDeg,
					w.Points[index].WeldedAngleDeg, g.Points[index].WeldedAngleDeg)
			}
			return model.NewError(model.CodeDiff, w.SeamID, "seam_stats", "接缝统计 JSON 不一致 want=%s got=%s", shortHash(wantJSON), shortHash(gotJSON))
		}
	}
	return nil
}

func firstPixelMismatch(w, g *model.BlockResult) string {
	if len(w.Pixels) != len(g.Pixels) {
		return fmt.Sprintf("length %d!=%d", len(w.Pixels), len(g.Pixels))
	}
	for i := range w.Pixels {
		wp, gp := w.Pixels[i], g.Pixels[i]
		if wp.Normal != gp.Normal || wp.RGB != gp.RGB {
			return fmt.Sprintf("(%d,%d)", wp.I, wp.J)
		}
	}
	return ""
}

func firstSlopeMismatch(w, g *model.BlockResult) int {
	if len(w.Pixels) != len(g.Pixels) {
		return 0
	}
	for i := range w.Pixels {
		if w.Pixels[i].SlopeDeg != g.Pixels[i].SlopeDeg {
			return i
		}
	}
	return -1
}

func firstSeamPointMismatch(w, g *model.SeamReport) int {
	if len(w.Points) != len(g.Points) {
		return 0
	}
	for i := range w.Points {
		wj, _ := json.Marshal(w.Points[i])
		gj, _ := json.Marshal(g.Points[i])
		if string(wj) != string(gj) {
			return i
		}
	}
	return -1
}

func SummarizeBlocks(blocks map[string]*model.BlockResult) map[string]string {
	out := map[string]string{}
	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out[id] = blocks[id].ResultHash
	}
	return out
}

func DirectoryHashes(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		h.Write([]byte(filepath.ToSlash(rel)))
		h.Write([]byte{0})
		h.Write(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}

func EqualHashMaps(a, b map[string]string) (string, bool) {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return k, false
		}
	}
	bKeys := make([]string, 0, len(b))
	for k := range b {
		bKeys = append(bKeys, k)
	}
	sort.Strings(bKeys)
	if len(keys) != len(bKeys) {
		return "file-set", false
	}
	return "", true
}

func shortHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}
