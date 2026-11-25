package gen

import (
	"fmt"
	"math/rand"

	"featurekernel/internal/kernel"
)

const (
	DefaultSeed       int64 = 20261126
	DefaultBatchCount       = 3
	TickMs            int64 = 60_000
	HourMs            int64 = 3_600_000
)

type builder struct {
	records []kernel.Record
}

func (b *builder) event(feature string, timeMs int64, channel string, value float64) kernel.Event {
	seq := int64(len(b.records) + 1)
	event := kernel.Event{ArrivalSeq: seq, FeatureID: feature, TimeMs: timeMs, Channel: channel, Value: value}
	b.records = append(b.records, kernel.Record{Seq: seq, Kind: kernel.RecordEvent, Event: &event})
	return event
}

func (b *builder) watermark(timeMs int64) {
	seq := int64(len(b.records) + 1)
	b.records = append(b.records, kernel.Record{
		Seq: seq, Kind: kernel.RecordWatermark,
		Watermark: &kernel.Watermark{ArrivalSeq: seq, TimeMs: timeMs},
	})
}

func BuildProject(rootSeed int64, batchIndex int) (*kernel.Project, error) {
	if batchIndex < 0 {
		return nil, kernel.NewError(kernel.CodeInvalidArgument, "batch index must be non-negative", map[string]any{"batch_index": batchIndex})
	}
	seed := rootSeed + int64(batchIndex)*7919
	rng := rand.New(rand.NewSource(seed))
	batchID := fmt.Sprintf("batch-%03d", batchIndex+1)
	projectID := fmt.Sprintf("feature-project-%03d", batchIndex+1)
	base := int64(1704067200000)
	channelNames := []string{"web", "ios", "android"}
	weights := map[string]int64{"web": 500_000, "ios": 300_000, "android": 200_000}

	features := []kernel.FeatureDef{
		{
			ID: "latency_ms", WidthMs: 180_000, SlideMs: 60_000, AnchorMs: base,
			Aggregations:      []kernel.AggFunc{kernel.AggCount, kernel.AggSum, kernel.AggAvg, kernel.AggMin, kernel.AggMax, kernel.AggWeightedAvg},
			AllowedLatenessMs: 120_000, ChannelWeightsPPM: weights,
		},
		{
			ID: "amount_cent", WidthMs: 180_000, SlideMs: 60_000, AnchorMs: base,
			Aggregations:      []kernel.AggFunc{kernel.AggCount, kernel.AggSum, kernel.AggAvg, kernel.AggMax, kernel.AggWeightedAvg},
			AllowedLatenessMs: 120_000, ChannelWeightsPPM: weights,
		},
		{
			ID: "jitter_ms", WidthMs: 150_000, SlideMs: 60_000, AnchorMs: base,
			Aggregations:      []kernel.AggFunc{kernel.AggCount, kernel.AggSum, kernel.AggAvg, kernel.AggMin, kernel.AggMax},
			AllowedLatenessMs: 120_000, ChannelWeightsPPM: weights,
		},
		{
			ID: "event_volume", WidthMs: 300_000, SlideMs: 60_000, AnchorMs: base,
			Aggregations:      []kernel.AggFunc{kernel.AggCount, kernel.AggSum, kernel.AggAvg, kernel.AggMin, kernel.AggMax},
			AllowedLatenessMs: 120_000, ChannelWeightsPPM: weights,
		},
	}

	b := &builder{}
	at := func(tick int64) int64 { return base + tick*TickMs }
	randomValue := func(feature string) float64 {
		switch feature {
		case "latency_ms":
			return float64(20 + rng.Intn(101))
		case "amount_cent":
			return float64(100 + rng.Intn(900))
		case "jitter_ms":
			return float64(1 + rng.Intn(40))
		default:
			return float64(1 + rng.Intn(5))
		}
	}

	featureIDs := []string{"latency_ms", "amount_cent", "jitter_ms", "event_volume"}
	for tick := int64(0); tick <= 9; tick++ {
		for _, featureID := range featureIDs {
			channel := channelNames[rng.Intn(len(channelNames))]
			b.event(featureID, at(tick), channel, randomValue(featureID))
		}
	}
	b.watermark(at(8))

	for _, featureID := range featureIDs {
		b.event(featureID, at(7), "web", lateValue(featureID, rng, 700))
	}
	for _, featureID := range featureIDs {
		b.event(featureID, at(4), "ios", lateValue(featureID, rng, 900))
	}

	for tick := int64(10); tick <= 14; tick++ {
		for _, featureID := range featureIDs {
			channel := channelNames[rng.Intn(len(channelNames))]
			b.event(featureID, at(tick), channel, randomValue(featureID))
		}
	}
	b.watermark(at(13))

	for _, featureID := range featureIDs {
		b.event(featureID, at(12), "android", lateValue(featureID, rng, 1200))
	}
	for _, featureID := range featureIDs {
		b.event(featureID, at(9), "web", lateValue(featureID, rng, 1500))
	}

	project := &kernel.Project{
		ProjectID:      projectID,
		BatchID:        batchID,
		Seed:           seed,
		TimeBaseMs:     base,
		TickMs:         TickMs,
		FixedTolerance: 1e-6,
		Channels:       channelNames,
		Features:       features,
		Derived: []kernel.DerivedSpec{{
			ID: "amount_per_latency_sum_ratio", BaseFeatureID: "latency_ms",
			NumeratorFeature: "amount_cent", DenominatorFeature: "latency_ms", Expression: "sum_ratio",
		}},
		Records: b.records,
		WatermarkPolicy: kernel.WatermarkPolicy{
			ExplicitFieldEnabled: true,
			Rule:                 kernel.WatermarkDegradeMaxSeenLag,
			LagMs:                TickMs,
			Status:               "explicit_generated",
		},
		PathPolicy: kernel.PathPolicy{IncrementalRatioThreshold: 3, Boundary: kernel.PathBoundaryRule},
		Budget:     kernel.Budget{Workers: 4, MaxWindowsPerFeature: 256},
	}
	return project, nil
}

func lateValue(featureID string, rng *rand.Rand, salt int) float64 {
	switch featureID {
	case "latency_ms":
		return float64(salt%100 + rng.Intn(20))
	case "amount_cent":
		return float64(salt%500 + rng.Intn(100))
	case "jitter_ms":
		return float64(salt%20 + rng.Intn(10))
	default:
		return float64(1 + rng.Intn(3))
	}
}
