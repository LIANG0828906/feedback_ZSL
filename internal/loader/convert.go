package loader

import (
	"encoding/json"
	"strconv"

	"replaybench/internal/eerr"
	"replaybench/internal/model"
	"replaybench/internal/timeconv"
)

func ConvertDocument(raw []byte, target model.FormatID) ([]byte, error) {
	var w WireReplay
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, eerr.Schema("$", "invalid JSON while converting: %v", err)
	}
	if w.Format == nil {
		return nil, eerr.Schema("$.format", "format is required")
	}
	source := model.FormatID(*w.Format)
	if source < 1 || source > 3 {
		return nil, eerr.Schema("$.format", "unknown format number %d", *w.Format)
	}
	if target < 1 || target > 3 {
		return nil, eerr.Schema("$.target_format", "unknown target format number %d", target)
	}
	basis := model.TimeBasis{EpochMs: w.EpochMs, Rounding: timeconv.Floor}
	if w.MsPerRound != nil {
		basis.MsPerRound = *w.MsPerRound
	}
	if w.Rounding != "" {
		rounding, err := timeconv.NormalizeRounding(w.Rounding)
		if err != nil {
			return nil, err
		}
		basis.Rounding = rounding
	}

	if source == model.FormatCooldownQueue {
		swapSemanticSlots(&w)
	}
	if source == model.FormatTimestamp {
		for i := range w.Frames {
			path := framePath(i) + ".timestamp_ms"
			if w.Frames[i].TimestampMs == nil {
				return nil, eerr.Schema(path, "timestamp_ms is required for format 3")
			}
			round, err := timeconv.TimestampToRound(*w.Frames[i].TimestampMs, basis, path)
			if err != nil {
				return nil, err
			}
			r := int64(round)
			w.Frames[i].Round = &r
			w.Frames[i].TimestampMs = nil
		}
	}

	f := int(target)
	w.Format = &f
	w.FormatVersion = map[model.FormatID]string{
		model.FormatWeightCast:    "1.0.0",
		model.FormatCooldownQueue: "2.0.0",
		model.FormatTimestamp:     "3.0.0",
	}[target]

	if target == model.FormatCooldownQueue {
		swapSemanticSlots(&w)
	}
	if target == model.FormatTimestamp {
		for i := range w.Frames {
			path := framePath(i) + ".timestamp_ms"
			if w.Frames[i].Round == nil {
				return nil, eerr.Schema(framePath(i)+".round", "round is required before timestamp conversion")
			}
			ts, err := timeconv.RoundToTimestamp(int(*w.Frames[i].Round), basis, path)
			if err != nil {
				return nil, err
			}
			w.Frames[i].TimestampMs = &ts
			w.Frames[i].Round = nil
		}
	}
	return json.Marshal(w)
}

func swapSemanticSlots(w *WireReplay) {
	for i := range w.Units {
		w.Units[i].PrimaryInt, w.Units[i].SecondaryInt = w.Units[i].SecondaryInt, w.Units[i].PrimaryInt
	}
	for i := range w.Frames {
		w.Frames[i].PrimaryList, w.Frames[i].SecondaryList = w.Frames[i].SecondaryList, w.Frames[i].PrimaryList
	}
}

func framePath(i int) string {
	return "$.frames[" + strconv.Itoa(i) + "]"
}
