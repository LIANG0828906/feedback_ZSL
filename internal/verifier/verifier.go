package verifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/layers"
)

type Report struct {
	SnapshotID              string `json:"snapshot_id"`
	TruthFingerprint        string `json:"truth_fingerprint"`
	MaskHash                string `json:"mask_hash"`
	RecomputedMaskHash      string `json:"recomputed_mask_hash"`
	StatsHash               string `json:"stats_hash"`
	RecomputedStatsHash     string `json:"recomputed_stats_hash"`
	RegionCount             int    `json:"region_count"`
	LayerCount              int    `json:"layer_count"`
	StatsMaskMismatchPixels int    `json:"stats_mask_mismatch_pixels"`
	CommitOrderStable       bool   `json:"commit_order_stable"`
	Valid                   bool   `json:"valid"`
}

// Verify reconstructs the foreground mask using only committed block layers,
// region ownership metadata, and the expected truth fingerprint. It does not
// import or invoke the pipeline package.
func Verify(store *checkpoint.Store, snapshotID, expectedTruthFingerprint string) (Report, error) {
	report := Report{SnapshotID: snapshotID}
	snapshot, err := store.GetSnapshot(snapshotID)
	if err != nil {
		return report, err
	}
	if snapshot.Status != "complete" {
		return report, core.StateError("snapshot", "all-layers", snapshot.Status, string(checkpoint.Committed))
	}
	if expectedTruthFingerprint == "" || snapshot.TruthFingerprint != expectedTruthFingerprint {
		return report, core.VersionError(snapshotID, expectedTruthFingerprint, snapshot.TruthFingerprint)
	}
	report.TruthFingerprint = snapshot.TruthFingerprint
	rows, err := store.LoadRows(snapshotID)
	if err != nil {
		return report, err
	}
	if len(rows) == 0 {
		return report, core.StateError("snapshot", "all-layers", "empty", string(checkpoint.Committed))
	}
	rowByKey := make(map[string]checkpoint.Row, len(rows))
	width, height := 0, 0
	for _, row := range rows {
		if row.State != checkpoint.Committed {
			return report, core.StateError(strconv.Itoa(row.BlockIndex), row.Layer, string(row.State), string(checkpoint.Committed))
		}
		if row.ParamVersion != snapshot.ParamVersion || row.AlgorithmVersion != snapshot.AlgorithmVersion {
			_ = store.InvalidateSnapshot(snapshotID, "verifier found layer version mismatch")
			return report, core.VersionError(snapshotID,
				snapshot.ParamVersion+"|"+snapshot.AlgorithmVersion,
				row.ParamVersion+"|"+row.AlgorithmVersion)
		}
		if core.BytesHash(row.Content) != row.ContentHash {
			_ = store.InvalidateSnapshot(snapshotID, "verifier found content hash mismatch")
			return report, core.VersionError(snapshotID, row.ContentHash, core.BytesHash(row.Content))
		}
		rowByKey[strconv.Itoa(row.BlockIndex)+":"+row.Layer] = row
		if row.BlockX+row.BlockW > width {
			width = row.BlockX + row.BlockW
		}
		if row.BlockY+row.BlockH > height {
			height = row.BlockY + row.BlockH
		}
	}
	report.LayerCount = len(rows)
	history, err := store.CommitHistory(snapshotID)
	if err != nil {
		return report, err
	}
	report.CommitOrderStable = commitOrderStable(rows, history)
	if !report.CommitOrderStable {
		_ = store.InvalidateSnapshot(snapshotID, "commit sequence is not stable")
		return report, core.NewError(core.CodeVersion, "persisted commit sequence is not block/layer stable")
	}

	labels := make([]int32, width*height)
	statsMask := make([]byte, width*height)
	coveredLabels := make([]bool, width*height)
	coveredStats := make([]bool, width*height)
	for _, row := range rows {
		if row.Layer != string(layers.Flood) {
			continue
		}
		key := strconv.Itoa(row.BlockIndex) + ":"
		floodRow, hasFlood := rowByKey[key+string(layers.Flood)]
		statsRow, hasStats := rowByKey[key+string(layers.Stats)]
		if !hasFlood || !hasStats {
			return report, core.StateError(strconv.Itoa(row.BlockIndex), "flood/stats", "missing", "committed")
		}
		decoded, err := layers.DecodeInt32(floodRow.Content)
		if err != nil {
			_ = store.InvalidateSnapshot(snapshotID, "verifier could not decode flood labels")
			return report, err
		}
		if len(decoded) != floodRow.BlockW*floodRow.BlockH {
			_ = store.InvalidateSnapshot(snapshotID, "verifier found flood layer length mismatch")
			return report, core.DecodeError("flood layer length does not match block dimensions")
		}
		layers.CopyIntoInt32(labels, width, layers.Block{
			Index: floodRow.BlockIndex, X: floodRow.BlockX, Y: floodRow.BlockY,
			W: floodRow.BlockW, H: floodRow.BlockH,
		}, decoded)
		for yy := 0; yy < floodRow.BlockH; yy++ {
			for xx := 0; xx < floodRow.BlockW; xx++ {
				coveredLabels[(floodRow.BlockY+yy)*width+floodRow.BlockX+xx] = true
			}
		}
		if len(statsRow.Content) != statsRow.BlockW*statsRow.BlockH {
			_ = store.InvalidateSnapshot(snapshotID, "verifier found stats layer length mismatch")
			return report, core.DecodeError("stats layer length does not match block dimensions")
		}
		layers.CopyIntoUint8(statsMask, width, layers.Block{
			Index: statsRow.BlockIndex, X: statsRow.BlockX, Y: statsRow.BlockY,
			W: statsRow.BlockW, H: statsRow.BlockH,
		}, statsRow.Content)
		for yy := 0; yy < statsRow.BlockH; yy++ {
			for xx := 0; xx < statsRow.BlockW; xx++ {
				coveredStats[(statsRow.BlockY+yy)*width+statsRow.BlockX+xx] = true
			}
		}
	}
	for i := 0; i < width*height; i++ {
		if !coveredLabels[i] || !coveredStats[i] {
			return report, core.StateError("snapshot", "coverage", "gap", "complete")
		}
	}
	owners := make(map[int32]core.RegionRecord, len(snapshot.RegionTable))
	for _, region := range snapshot.RegionTable {
		owners[region.ID] = region
	}
	rawMask := make([]byte, width*height)
	for i, label := range labels {
		if label == 0 {
			continue
		}
		region, ok := owners[label]
		if !ok {
			_ = store.InvalidateSnapshot(snapshotID, "verifier found label without region ownership")
			return report, core.VersionError(snapshotID, "region table entry", strconv.Itoa(int(label)))
		}
		if region.Kind == core.SeedForeground {
			rawMask[i] = 1
		}
	}
	recomputed := independentClean(rawMask, width, height, snapshot.Params.CleanupRadius)
	for i := range recomputed {
		if recomputed[i] != statsMask[i] {
			report.StatsMaskMismatchPixels++
		}
	}
	report.RecomputedMaskHash = core.MaskHash(recomputed)
	report.MaskHash = snapshot.MaskHash
	if report.StatsMaskMismatchPixels != 0 || report.RecomputedMaskHash != snapshot.MaskHash {
		_ = store.InvalidateSnapshot(snapshotID, "verifier-reconstructed mask disagrees with committed stats layers")
		return report, core.VersionError(snapshotID, snapshot.MaskHash, report.RecomputedMaskHash)
	}
	components := independentComponents(recomputed, width, height)
	report.RegionCount = len(components)
	report.RecomputedStatsHash = independentStatsHash(recomputed, components)
	report.StatsHash = snapshot.StatsHash
	if report.RecomputedStatsHash != snapshot.StatsHash {
		_ = store.InvalidateSnapshot(snapshotID, "verifier-recomputed statistics hash mismatch")
		return report, core.VersionError(snapshotID, snapshot.StatsHash, report.RecomputedStatsHash)
	}
	report.Valid = true
	return report, nil
}

func commitOrderStable(rows, history []checkpoint.Row) bool {
	if len(rows) != len(history) || len(rows) == 0 {
		return false
	}
	expected := make([]string, 0, len(rows))
	sorted := append([]checkpoint.Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].BlockIndex != sorted[j].BlockIndex {
			return sorted[i].BlockIndex < sorted[j].BlockIndex
		}
		return layerRank(sorted[i].Layer) < layerRank(sorted[j].Layer)
	})
	for _, row := range sorted {
		expected = append(expected, strconv.Itoa(row.BlockIndex)+":"+row.Layer)
	}
	for i, row := range history {
		if expected[i] != strconv.Itoa(row.BlockIndex)+":"+row.Layer {
			return false
		}
	}
	return true
}

func layerRank(layer string) int {
	for i, item := range layers.Order {
		if string(item) == layer {
			return i
		}
	}
	return 99
}

func independentStatsHash(mask []byte, components []core.MaskComponent) string {
	body, _ := json.Marshal(struct {
		Mask       string               `json:"mask"`
		Components []core.MaskComponent `json:"components"`
	}{core.MaskHash(mask), components})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func independentClean(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		return append([]byte(nil), mask...)
	}
	opened := localMax(localMin(mask, w, h, radius), w, h, radius)
	return localMin(localMax(opened, w, h, radius), w, h, radius)
}

func localMin(mask []byte, w, h, radius int) []byte {
	tmp := make([]byte, len(mask))
	out := make([]byte, len(mask))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(1)
			for k := -radius; k <= radius; k++ {
				xx := x + k
				if xx < 0 || xx >= w || mask[y*w+xx] == 0 {
					value = 0
					break
				}
			}
			tmp[y*w+x] = value
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(1)
			for k := -radius; k <= radius; k++ {
				yy := y + k
				if yy < 0 || yy >= h || tmp[yy*w+x] == 0 {
					value = 0
					break
				}
			}
			out[y*w+x] = value
		}
	}
	return out
}

func localMax(mask []byte, w, h, radius int) []byte {
	tmp := make([]byte, len(mask))
	out := make([]byte, len(mask))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(0)
			for k := -radius; k <= radius; k++ {
				xx := x + k
				if xx >= 0 && xx < w && mask[y*w+xx] != 0 {
					value = 1
					break
				}
			}
			tmp[y*w+x] = value
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(0)
			for k := -radius; k <= radius; k++ {
				yy := y + k
				if yy >= 0 && yy < h && tmp[yy*w+x] != 0 {
					value = 1
					break
				}
			}
			out[y*w+x] = value
		}
	}
	return out
}

func independentComponents(mask []byte, w, h int) []core.MaskComponent {
	seen := make([]bool, len(mask))
	var components []core.MaskComponent
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			start := y*w + x
			if mask[start] == 0 || seen[start] {
				continue
			}
			stack := []int{start}
			seen[start] = true
			area := 0
			sumX, sumY := 0.0, 0.0
			minX, minY, maxX, maxY := x, y, x, y
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				px, py := p%w, p/w
				area++
				sumX += float64(px) + 0.5
				sumY += float64(py) + 0.5
				if px < minX {
					minX = px
				}
				if px > maxX {
					maxX = px
				}
				if py < minY {
					minY = py
				}
				if py > maxY {
					maxY = py
				}
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := px+d[0], py+d[1]
					if nx >= 0 && ny >= 0 && nx < w && ny < h {
						np := ny*w + nx
						if mask[np] != 0 && !seen[np] {
							seen[np] = true
							stack = append(stack, np)
						}
					}
				}
			}
			components = append(components, core.MaskComponent{
				CentroidX: sumX / float64(area), CentroidY: sumY / float64(area), Area: area,
				BBox:       core.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1},
				HoleCount:  independentHoles(mask, w, h, minX-1, minY-1, maxX+1, maxY+1),
				PixelCount: area,
			})
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].BBox.Y != components[j].BBox.Y {
			return components[i].BBox.Y < components[j].BBox.Y
		}
		if components[i].BBox.X != components[j].BBox.X {
			return components[i].BBox.X < components[j].BBox.X
		}
		if components[i].Area != components[j].Area {
			return components[i].Area < components[j].Area
		}
		return components[i].CentroidX < components[j].CentroidX
	})
	for i := range components {
		components[i].ID = i + 1
	}
	return components
}

func independentHoles(mask []byte, w, h, minX, minY, maxX, maxY int) int {
	minX = maxInt(minX, 0)
	minY = maxInt(minY, 0)
	if maxX >= w {
		maxX = w - 1
	}
	if maxY >= h {
		maxY = h - 1
	}
	ww, hh := maxX-minX+1, maxY-minY+1
	seen := make([]bool, ww*hh)
	queue := make([]int, 0, ww*hh)
	for x := 0; x < ww; x++ {
		queue = append(queue, x, (hh-1)*ww+x)
	}
	for y := 0; y < hh; y++ {
		queue = append(queue, y*ww, y*ww+ww-1)
	}
	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		x, y := p%ww, p/ww
		if mask[(minY+y)*w+minX+x] != 0 {
			continue
		}
		if x > 0 {
			queue = append(queue, p-1)
		}
		if x+1 < ww {
			queue = append(queue, p+1)
		}
		if y > 0 {
			queue = append(queue, p-ww)
		}
		if y+1 < hh {
			queue = append(queue, p+ww)
		}
	}
	holes := 0
	for y := 0; y < hh; y++ {
		for x := 0; x < ww; x++ {
			p := y*ww + x
			if seen[p] || mask[(minY+y)*w+minX+x] != 0 {
				continue
			}
			holes++
			stack := []int{p}
			seen[p] = true
			for len(stack) > 0 {
				q := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				qx, qy := q%ww, q/ww
				if qx > 0 && !seen[q-1] && mask[(minY+qy)*w+minX+qx-1] == 0 {
					seen[q-1] = true
					stack = append(stack, q-1)
				}
				if qx+1 < ww && !seen[q+1] && mask[(minY+qy)*w+minX+qx+1] == 0 {
					seen[q+1] = true
					stack = append(stack, q+1)
				}
				if qy > 0 && !seen[q-ww] && mask[(minY+qy-1)*w+minX+qx] == 0 {
					seen[q-ww] = true
					stack = append(stack, q-ww)
				}
				if qy+1 < hh && !seen[q+ww] && mask[(minY+qy+1)*w+minX+qx] == 0 {
					seen[q+ww] = true
					stack = append(stack, q+ww)
				}
			}
		}
	}
	return holes
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
