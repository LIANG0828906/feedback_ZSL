package sample

import (
	"fmt"

	"sre-timeline/internal/model"
)

const DefaultSeed int64 = 42

// Generate builds the canonical incident. The seed is accepted so callers can
// inject reproducible variants; the default fixture is intentionally stable and
// does not depend on wall clock or random global state.
func Generate(seed int64) *model.Project {
	const second = int64(1000)
	origin := int64(1_700_000_000_000)
	start := origin
	at := func(sec int64) int64 { return start + sec*second }

	p := &model.Project{
		ID:       "INC-20231114-42",
		Revision: fmt.Sprintf("rev-seed-%d", seed),
		Seed:     seed,
		Window: model.Window{
			StartMS: start,
			EndMS:   at(480),
			TickMS:  5 * second,
		},
		Policy: model.Policy{
			DedupToleranceMS: 10 * second,
			MergeGapMS:       60 * second,
			LookbackMS:       600 * second,
			PropagationGen:   3,
			FixedScale:       1_000_000,
			FixedWidthBits:   64,
			ScoreTolerance:   1_000,
		},
		Services: []model.Service{
			{ID: "gateway", Name: "Edge Gateway", ErrorThresholdBP: 500, SaturationThresholdBP: 9000, BaselineErrorBP: 40, BaselineSaturationBP: 3500},
			{ID: "order", Name: "Order API", ErrorThresholdBP: 500, SaturationThresholdBP: 8500, BaselineErrorBP: 50, BaselineSaturationBP: 4000},
			{ID: "payment", Name: "Payment Core", ErrorThresholdBP: 450, SaturationThresholdBP: 8000, BaselineErrorBP: 60, BaselineSaturationBP: 4500},
			{ID: "inventory", Name: "Inventory DB", ErrorThresholdBP: 400, SaturationThresholdBP: 8000, BaselineErrorBP: 30, BaselineSaturationBP: 5000},
		},
		Edges: []model.Edge{
			{Caller: "gateway", Callee: "order"},
			{Caller: "order", Callee: "payment"},
			{Caller: "order", Callee: "inventory"},
		},
	}

	seq := 0
	add := func(ev model.Event) model.Event {
		seq++
		ev.Seq = seq
		p.Events = append(p.Events, ev)
		return ev
	}
	intPtr := func(v int) *int { return &v }

	// Baseline observations.
	add(model.Event{ID: "obs-gw-01", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_error_rate", Fingerprint: "health-gateway", OccurredMS: at(30), AliveMS: 90_000, ErrorRateBP: intPtr(40), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-order-01", Source: model.SourceHealth, ServiceID: "order", Kind: "http_error_rate", Fingerprint: "health-order", OccurredMS: at(30), AliveMS: 90_000, ErrorRateBP: intPtr(50), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-pay-01", Source: model.SourceHealth, ServiceID: "payment", Kind: "http_error_rate", Fingerprint: "health-payment", OccurredMS: at(30), AliveMS: 90_000, ErrorRateBP: intPtr(60), TypeWeightBP: 3000})
	add(model.Event{ID: "cap-inv-01", Source: model.SourceCapacity, ServiceID: "inventory", Kind: "saturation", Fingerprint: "capacity-inventory", OccurredMS: at(30), AliveMS: 90_000, SaturationBP: intPtr(5200), TypeWeightBP: 4000})

	// Causal change: payment deploy precedes all downstream-caller symptoms.
	add(model.Event{ID: "chg-pay-deploy", Source: model.SourceChange, ServiceID: "payment", Kind: "deployment", Fingerprint: "deploy-pay-v842", OccurredMS: at(60), IngestLagMS: 2_000, AliveMS: 300_000, ChangeType: "deploy", TypeWeightBP: 10000})

	// Inventory flap creates two atomic intervals merged across a short gap.
	add(model.Event{ID: "obs-inv-02", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(60), AliveMS: 90_000, ErrorRateBP: intPtr(650), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-pay-02", Source: model.SourceHealth, ServiceID: "payment", Kind: "http_error_rate", Fingerprint: "health-payment", OccurredMS: at(90), AliveMS: 90_000, ErrorRateBP: intPtr(820), TypeWeightBP: 3000})
	add(model.Event{ID: "cap-pay-02", Source: model.SourceCapacity, ServiceID: "payment", Kind: "saturation", Fingerprint: "capacity-payment", OccurredMS: at(90), AliveMS: 90_000, SaturationBP: intPtr(9100), TypeWeightBP: 4000})
	add(model.Event{ID: "obs-inv-03", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(90), AliveMS: 90_000, ErrorRateBP: intPtr(700), TypeWeightBP: 3000})

	// Repeated alert reports are duplicate raw events and must collapse.
	add(model.Event{ID: "alert-pay-01", Source: model.SourceAlert, ServiceID: "payment", Kind: "high_error_rate", Fingerprint: "alert-payment-errors", OccurredMS: at(100), IngestLagMS: 1_000, AliveMS: 120_000, TypeWeightBP: 4500})
	add(model.Event{ID: "alert-pay-01-retry", Source: model.SourceAlert, ServiceID: "payment", Kind: "high_error_rate", Fingerprint: "alert-payment-errors", OccurredMS: at(105), IngestLagMS: 6_000, AliveMS: 120_000, TypeWeightBP: 4500})

	add(model.Event{ID: "obs-order-02", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(120), AliveMS: 90_000, ErrorRateBP: intPtr(620), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-inv-04", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(120), AliveMS: 90_000, ErrorRateBP: intPtr(120), TypeWeightBP: 3000})

	add(model.Event{ID: "alert-order-01", Source: model.SourceAlert, ServiceID: "order", Kind: "dependency_failures", Fingerprint: "alert-order-dep", OccurredMS: at(130), IngestLagMS: 2_000, AliveMS: 120_000, TypeWeightBP: 4500})
	add(model.Event{ID: "alert-order-01-retry", Source: model.SourceAlert, ServiceID: "order", Kind: "dependency_failures", Fingerprint: "alert-order-dep", OccurredMS: at(135), IngestLagMS: 6_000, AliveMS: 120_000, TypeWeightBP: 4500})

	add(model.Event{ID: "obs-order-03", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(150), AliveMS: 90_000, ErrorRateBP: intPtr(700), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-inv-05", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(150), AliveMS: 90_000, ErrorRateBP: intPtr(650), TypeWeightBP: 3000})

	add(model.Event{ID: "obs-gw-02", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_5xx", Fingerprint: "health-gateway", OccurredMS: at(180), AliveMS: 90_000, ErrorRateBP: intPtr(720), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-order-04", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(180), AliveMS: 90_000, ErrorRateBP: intPtr(680), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-inv-06", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(180), AliveMS: 90_000, ErrorRateBP: intPtr(720), TypeWeightBP: 3000})
	add(model.Event{ID: "alert-gw-01", Source: model.SourceAlert, ServiceID: "gateway", Kind: "edge_5xx", Fingerprint: "alert-gateway-5xx", OccurredMS: at(190), IngestLagMS: 1_000, AliveMS: 120_000, TypeWeightBP: 4500})
	add(model.Event{ID: "alert-gw-01-retry", Source: model.SourceAlert, ServiceID: "gateway", Kind: "edge_5xx", Fingerprint: "alert-gateway-5xx", OccurredMS: at(195), IngestLagMS: 6_000, AliveMS: 120_000, TypeWeightBP: 4500})

	add(model.Event{ID: "obs-inv-07", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(210), AliveMS: 90_000, ErrorRateBP: intPtr(100), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-pay-03", Source: model.SourceHealth, ServiceID: "payment", Kind: "http_error_rate", Fingerprint: "health-payment", OccurredMS: at(210), AliveMS: 90_000, ErrorRateBP: intPtr(870), TypeWeightBP: 3000})
	add(model.Event{ID: "cap-pay-03", Source: model.SourceCapacity, ServiceID: "payment", Kind: "saturation", Fingerprint: "capacity-payment", OccurredMS: at(210), AliveMS: 90_000, SaturationBP: intPtr(8800), TypeWeightBP: 4000})
	add(model.Event{ID: "obs-order-05", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(210), AliveMS: 90_000, ErrorRateBP: intPtr(590), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-gw-03", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_5xx", Fingerprint: "health-gateway", OccurredMS: at(210), AliveMS: 90_000, ErrorRateBP: intPtr(680), TypeWeightBP: 3000})

	add(model.Event{ID: "obs-gw-04", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_5xx", Fingerprint: "health-gateway", OccurredMS: at(240), AliveMS: 90_000, ErrorRateBP: intPtr(610), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-pay-04", Source: model.SourceHealth, ServiceID: "payment", Kind: "http_error_rate", Fingerprint: "health-payment", OccurredMS: at(270), AliveMS: 90_000, ErrorRateBP: intPtr(760), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-order-06", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(270), AliveMS: 90_000, ErrorRateBP: intPtr(540), TypeWeightBP: 3000})

	// Recovery boundary at 300s.
	add(model.Event{ID: "rec-pay-01", Source: model.SourceRecovery, ServiceID: "payment", Kind: "service_recovered", Fingerprint: "recovery-payment", OccurredMS: at(300), AliveMS: 30_000, RecoveryFor: "chg-pay-deploy", TypeWeightBP: 0})
	add(model.Event{ID: "obs-pay-05", Source: model.SourceHealth, ServiceID: "payment", Kind: "http_error_rate", Fingerprint: "health-payment", OccurredMS: at(300), AliveMS: 90_000, ErrorRateBP: intPtr(80), TypeWeightBP: 3000})
	add(model.Event{ID: "cap-pay-05", Source: model.SourceCapacity, ServiceID: "payment", Kind: "saturation", Fingerprint: "capacity-payment", OccurredMS: at(300), AliveMS: 90_000, SaturationBP: intPtr(5000), TypeWeightBP: 4000})
	add(model.Event{ID: "rec-order-01", Source: model.SourceRecovery, ServiceID: "order", Kind: "dependency_recovered", Fingerprint: "recovery-order", OccurredMS: at(300), AliveMS: 30_000, TypeWeightBP: 0})
	add(model.Event{ID: "obs-order-07", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(300), AliveMS: 90_000, ErrorRateBP: intPtr(90), TypeWeightBP: 3000})
	add(model.Event{ID: "rec-gw-01", Source: model.SourceRecovery, ServiceID: "gateway", Kind: "edge_recovered", Fingerprint: "recovery-gateway", OccurredMS: at(300), AliveMS: 30_000, TypeWeightBP: 0})
	add(model.Event{ID: "obs-gw-05", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_5xx", Fingerprint: "health-gateway", OccurredMS: at(300), AliveMS: 90_000, ErrorRateBP: intPtr(70), TypeWeightBP: 3000})

	// A later, sufficiently separated inventory interval must not be merged.
	add(model.Event{ID: "obs-inv-08", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(300), AliveMS: 90_000, ErrorRateBP: intPtr(650), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-inv-09", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(330), AliveMS: 90_000, ErrorRateBP: intPtr(680), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-inv-10", Source: model.SourceHealth, ServiceID: "inventory", Kind: "db_error_rate", Fingerprint: "health-inventory", OccurredMS: at(360), AliveMS: 90_000, ErrorRateBP: intPtr(110), TypeWeightBP: 3000})
	add(model.Event{ID: "cap-inv-11", Source: model.SourceCapacity, ServiceID: "inventory", Kind: "saturation", Fingerprint: "capacity-inventory", OccurredMS: at(390), AliveMS: 90_000, SaturationBP: intPtr(5100), TypeWeightBP: 4000})
	add(model.Event{ID: "obs-gw-06", Source: model.SourceHealth, ServiceID: "gateway", Kind: "http_5xx", Fingerprint: "health-gateway", OccurredMS: at(420), AliveMS: 90_000, ErrorRateBP: intPtr(40), TypeWeightBP: 3000})
	add(model.Event{ID: "obs-order-08", Source: model.SourceHealth, ServiceID: "order", Kind: "dependency_error_rate", Fingerprint: "health-order", OccurredMS: at(420), AliveMS: 90_000, ErrorRateBP: intPtr(50), TypeWeightBP: 3000})

	for i := range p.Events {
		ev := &p.Events[i]
		svc, ok := p.Service(ev.ServiceID)
		if !ok {
			continue
		}
		switch ev.Source {
		case model.SourceHealth:
			if ev.ErrorRateBP != nil && *ev.ErrorRateBP < svc.ErrorThresholdBP {
				ev.TypeWeightBP = 0
			}
		case model.SourceCapacity:
			if ev.SaturationBP != nil && *ev.SaturationBP < svc.SaturationThresholdBP {
				ev.TypeWeightBP = 0
			}
		}
	}
	return p
}
