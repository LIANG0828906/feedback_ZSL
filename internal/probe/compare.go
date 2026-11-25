package probe

import (
	"math"

	"lightprobe/internal/diag"
)

type Snapshot struct {
	GridOrder       []string      `json:"grid_order"`
	Probes          []Record      `json:"probes"`
	Pixels          []PixelResult `json:"pixels"`
	RoomCommitOrder []string      `json:"room_commit_order"`
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := Snapshot{
		GridOrder:       append([]string{}, e.order...),
		Probes:          make([]Record, 0, len(e.order)),
		Pixels:          make([]PixelResult, 0, len(e.scene.Pixels)),
		RoomCommitOrder: append([]string{}, e.committedRoomOrder...),
	}
	for _, id := range e.order {
		snapshot.Probes = append(snapshot.Probes, *e.records[id])
	}
	for _, pixel := range e.scene.Pixels {
		if result, ok := e.pixelResults[pixel.ID]; ok {
			snapshot.Pixels = append(snapshot.Pixels, result)
		}
	}
	return snapshot
}

func CompareSnapshots(expected Snapshot, actual Snapshot) error {
	if len(expected.GridOrder) != len(actual.GridOrder) {
		return diffError("", "", "grid_order.length",
			"grid order length differs: expected %d actual %d", len(expected.GridOrder), len(actual.GridOrder))
	}
	actualProbes := make(map[string]Record, len(actual.Probes))
	for _, record := range actual.Probes {
		actualProbes[record.ID] = record
	}
	for index, id := range expected.GridOrder {
		if index >= len(actual.GridOrder) || actual.GridOrder[index] != id {
			actualID := ""
			if index < len(actual.GridOrder) {
				actualID = actual.GridOrder[index]
			}
			return diffError("", id, "submission_sequence",
				"first grid submission mismatch at index %d: expected %s actual %s", index, id, actualID)
		}
		wantRecord := expected.Probes[index]
		gotRecord, ok := actualProbes[id]
		if !ok {
			return diffError(wantRecord.RoomID, id, "record", "actual snapshot missing probe")
		}
		if wantRecord.ID != gotRecord.ID {
			return diffError(wantRecord.RoomID, id, "id", "probe id mismatch")
		}
		for channel := 0; channel < 3; channel++ {
			if math.Float64bits(wantRecord.Radiance[channel]) != math.Float64bits(gotRecord.Radiance[channel]) {
				err := diffError(wantRecord.RoomID, id, "radiance",
					"radiance channel %d differs bitwise: expected %.17g actual %.17g",
					channel, wantRecord.Radiance[channel], gotRecord.Radiance[channel])
				err.Details["channel"] = channel
				return err
			}
		}
		for band := 0; band < 9; band++ {
			for channel := 0; channel < 3; channel++ {
				if wantRecord.SH[band][channel] != gotRecord.SH[band][channel] {
					err := diffError(wantRecord.RoomID, id, "sh_fixed",
						"SH coefficient [%d][%d] differs: expected %d actual %d",
						band, channel, wantRecord.SH[band][channel], gotRecord.SH[band][channel])
					err.Details["band"] = band
					err.Details["channel"] = channel
					return err
				}
			}
		}
		if wantRecord.SHDigest != gotRecord.SHDigest {
			return diffError(wantRecord.RoomID, id, "sh_digest", "SH digest differs")
		}
	}

	actualPixels := make(map[string]PixelResult, len(actual.Pixels))
	for _, pixel := range actual.Pixels {
		actualPixels[pixel.PixelID] = pixel
	}
	for index, wantPixel := range expected.Pixels {
		gotPixel, ok := actualPixels[wantPixel.PixelID]
		if !ok {
			return diffError(wantPixel.RoomID, wantPixel.PixelID, "pixel", "actual snapshot missing pixel")
		}
		for channel := 0; channel < 3; channel++ {
			if math.Float64bits(wantPixel.Color[channel]) != math.Float64bits(gotPixel.Color[channel]) {
				err := diffError(wantPixel.RoomID, wantPixel.PixelID, "pixel_color",
					"pixel sequence index %d channel %d differs bitwise", index, channel)
				err.Details["channel"] = channel
				return err
			}
			if math.Float64bits(wantPixel.Radiance[channel]) != math.Float64bits(gotPixel.Radiance[channel]) {
				err := diffError(wantPixel.RoomID, wantPixel.PixelID, "pixel_radiance",
					"pixel radiance channel %d differs bitwise", channel)
				err.Details["channel"] = channel
				return err
			}
		}
	}

	if len(expected.RoomCommitOrder) != len(actual.RoomCommitOrder) {
		return diffError("", "", "room_commit_order.length", "room commit order length differs")
	}
	for index, expectedRoom := range expected.RoomCommitOrder {
		if expectedRoom != actual.RoomCommitOrder[index] {
			return diffError(expectedRoom, "", "room_commit_order",
				"room commit mismatch at index %d: expected %s actual %s",
				index, expectedRoom, actual.RoomCommitOrder[index])
		}
	}
	return nil
}

func diffError(roomID, id, field, format string, args ...any) *diag.Error {
	err := diag.New(diag.CodeDiff, format, args...)
	err.RoomID = roomID
	err.ID = id
	err.Field = field
	if err.Details == nil {
		err.Details = map[string]any{}
	}
	return err
}

func MaxVecAbsDiff(a, b [3]float64) float64 {
	maxDiff := 0.0
	for i := 0; i < 3; i++ {
		maxDiff = math.Max(maxDiff, math.Abs(a[i]-b[i]))
	}
	return maxDiff
}
