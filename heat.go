package layercache

import (
	"fmt"
	"sort"
)

const canonicalHeatScale int64 = 10000

type accessRecord struct {
	atMS  int64
	count int64
}

type heatPoint struct {
	score            int64
	level            HeatLevel
	lastAccessMS     int64
	countFromMS      int64
	countToMS        int64
	countInWindow    int64
	overflowDetected bool
	saturated        bool
}

func collectAccessRecords(p *Project) map[string][]accessRecord {
	records := make(map[string][]accessRecord, len(p.Objects))
	for _, object := range p.Objects {
		for _, sample := range object.Samples {
			if sample.Count > 0 {
				records[object.ID] = append(records[object.ID], accessRecord{atMS: sample.AtMS, count: sample.Count})
			}
		}
	}
	for _, event := range p.Events {
		if event.Kind == EventAccess && event.Count > 0 {
			records[event.ObjectID] = append(records[event.ObjectID], accessRecord{atMS: event.AtMS, count: event.Count})
		}
	}
	for id := range records {
		sort.SliceStable(records[id], func(i, j int) bool { return records[id][i].atMS < records[id][j].atMS })
	}
	return records
}

func sampleWatermark(object Object, atMS int64) *int64 {
	for i := range object.Samples {
		if object.Samples[i].AtMS == atMS {
			return object.Samples[i].Watermark
		}
		if object.Samples[i].AtMS > atMS {
			break
		}
	}
	return nil
}

func heatAt(p *Project, records []accessRecord, nowMS int64, watermark *int64) (heatPoint, error) {
	cfg := p.Config
	limit := int64(1)<<cfg.FixedBits - 1
	lowerBound := nowMS - cfg.HeatWindowMS
	from := lowerBound
	if from < 0 {
		from = 0
	}
	lastAccess := int64(-1)
	windowCount := int64(0)
	overflow := false
	saturated := false
	for _, rec := range records {
		if rec.atMS > nowMS {
			break
		}
		if rec.count > 0 {
			lastAccess = rec.atMS
			if rec.atMS > lowerBound {
				var didSat bool
				windowCount, didSat = satAdd(windowCount, rec.count, limit)
				overflow = overflow || didSat
				saturated = saturated || didSat
			}
		}
	}
	raw := int64(0)
	if windowCount > 0 {
		var didSat bool
		raw, didSat = satMul(windowCount, canonicalHeatScale, limit)
		overflow = overflow || didSat
		saturated = saturated || didSat
		if didSat {
			raw = canonicalHeatScale
		} else {
			raw /= cfg.HeatMaxCount
		}
		if raw > canonicalHeatScale {
			raw = canonicalHeatScale
			saturated = true
		}
	}
	factor := int64(0)
	if lastAccess >= 0 {
		elapsed := nowMS - lastAccess
		if elapsed < 0 {
			elapsed = 0
		}
		if elapsed < cfg.HeatDecayMS {
			remaining := cfg.HeatDecayMS - elapsed
			var didSat bool
			factor, didSat = satMul(remaining, canonicalHeatScale, limit)
			overflow = overflow || didSat
			saturated = saturated || didSat
			if didSat {
				factor = canonicalHeatScale
			} else {
				factor /= cfg.HeatDecayMS
			}
		}
	}
	score := int64(0)
	if raw > 0 && factor > 0 {
		var didSat bool
		score, didSat = satMul(raw, factor, limit)
		overflow = overflow || didSat
		saturated = saturated || didSat
		if didSat {
			score = canonicalHeatScale
		} else {
			score /= canonicalHeatScale
		}
	}
	// All supplied schemas have at least permille precision. Quantizing the
	// canonical fixed point score keeps exact cross-version equality.
	score = (score / 10) * 10
	if score > canonicalHeatScale {
		score = canonicalHeatScale
		saturated = true
	}
	level := heatLevel(score, cfg)
	point := heatPoint{
		score:            score,
		level:            level,
		lastAccessMS:     lastAccess,
		countFromMS:      from,
		countToMS:        nowMS,
		countInWindow:    windowCount,
		overflowDetected: overflow,
		saturated:        saturated,
	}
	if watermark != nil {
		if *watermark != score {
			return heatPoint{}, bizError(
				CodeWatermarkMismatch,
				p.SchemaVersion,
				fmt.Sprintf("object %q at %dms", "", nowMS),
				"declared watermark %d does not match computed score %d", *watermark, score,
			)
		}
		// The watermark is authoritative in versions that provide it. It has
		// already been proven equivalent to the deterministic calculation.
		point.score = *watermark
		point.level = heatLevel(*watermark, cfg)
	}
	return point, nil
}

func heatLevel(score int64, cfg Config) HeatLevel {
	if score >= cfg.HotThreshold {
		return HeatHot
	}
	if score <= cfg.ColdThreshold {
		return HeatCold
	}
	return HeatWarm
}

func satAdd(a, b, limit int64) (int64, bool) {
	if a > limit-b {
		return limit, true
	}
	return a + b, false
}

func satMul(a, b, limit int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if a > limit/b {
		return limit, true
	}
	return a * b, false
}
