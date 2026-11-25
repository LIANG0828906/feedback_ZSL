package gateway

import (
	"encoding/json"
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 20261126

type SampleBundle struct {
	Canonical *Project          `json:"canonical"`
	Schemas   map[string][]byte `json:"schemas"`
	Truth     *RunReport        `json:"truth"`
}

func GenerateSample(seed int64) (*SampleBundle, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	origin := int64(1_700_000_000_000)
	boundaries := []int64{5_000, 10_000, 25_000, 50_000, 100_000, 250_000}

	services := []Service{
		{ID: "svc-auth", Name: "Authentication"},
		{ID: "svc-billing", Name: "Billing"},
		{ID: "svc-edge", Name: "Edge"},
	}
	routeSpecs := []struct {
		service int
		id      string
		path    string
		timeout int64
	}{
		{0, "route-auth-login", "/auth/login", 250},
		{0, "route-auth-session", "/auth/session", 500},
		{1, "route-billing-invoices", "/billing/invoices", 800},
		{1, "route-billing-charges", "/billing/charges", 1_200},
		{2, "route-edge-cdn", "/edge/cdn", 300},
		{2, "route-edge-waf", "/edge/waf", 450},
	}
	routes := make([]Route, 0, len(routeSpecs))
	for i, spec := range routeSpecs {
		routes = append(routes, Route{
			ID: spec.id, ServiceID: services[spec.service].ID, Path: spec.path, Prefix: spec.path,
			Order: i, TimeoutMS: spec.timeout,
			ErrorRules: []ErrorRule{
				{Class: "client_error", StatusMin: 400, StatusMax: 499},
				{Class: "server_error", StatusMin: 500, StatusMax: 599},
				{Class: "slow", TimeoutOnly: true},
			},
		})
	}

	statuses := []int{200, 200, 200, 201, 204, 301, 400, 404, 429, 500, 503}
	records := []Record{}
	addSpec := func(arrival int, routeIndex int, offsetMS int64, durationUS int64, critical bool) {
		route := routes[routeIndex]
		status := statuses[rng.Intn(len(statuses))]
		if critical {
			status = []int{200, 429, 500}[arrival%3]
		}
		reqBytes := int64(256 + rng.Intn(8192))
		respBytes := int64(512 + rng.Intn(16384))
		id := fmt.Sprintf("rec-%03d", arrival)
		if critical {
			id = fmt.Sprintf("crit-%02d-%03d", arrival, durationUS)
		}
		seq := (origin + offsetMS - origin) / 1000
		records = append(records, Record{
			ID: id, ArrivalSeq: arrival, Path: route.Path, RouteID: route.ID, ServiceID: route.ServiceID,
			EventTimeMS: origin + offsetMS, SourceEventSeq: &seq, DurationUS: durationUS,
			DurationFixed: FixedDuration(durationUS * DurationScale), DurationRaw: fmt.Sprintf("%d", durationUS),
			DurationUnit: "us", StatusCode: status, RequestBytes: reqBytes, ResponseBytes: respBytes,
		})
	}

	arrival := 0
	lateSpecs := []struct {
		offsetMS   int64
		durationUS int64
		route      int
	}{
		{6_000, 8_000, 0},   // initial WM 8000: lateness 2000, accepted at threshold.
		{4_000, 9_000, 1},   // initial WM 8000: lateness 4000, dropped.
		{14_000, 12_000, 2}, // WM 16000: lateness 2000, accepted at threshold.
		{13_000, 11_000, 3}, // WM 16000: lateness 3000, dropped.
		{22_000, 15_000, 4}, // WM 24000: lateness 2000, accepted at threshold.
		{20_000, 16_000, 5}, // WM 24000: lateness 4000, dropped.
	}
	for _, spec := range lateSpecs {
		addSpec(arrival, spec.route, spec.offsetMS, spec.durationUS, false)
		arrival++
	}

	// Deterministic broad traffic: five records per route.
	for routeIndex := range routes {
		for j := 0; j < 5; j++ {
			offset := int64(22_000 + ((routeIndex*3_700 + j*4_300 + rng.Intn(1_500)) % 12_000))
			offset = offset - offset%1000
			durationMS := int64(2 + rng.Intn(280))
			addSpec(arrival, routeIndex, offset, durationMS*1000, false)
			arrival++
		}
	}

	// Critical half-open boundary cases: boundary-1, boundary and boundary+1.
	for _, boundary := range boundaries[:3] {
		for _, delta := range []int64{-1, 0, 1} {
			duration := boundary + delta
			if duration < 0 {
				duration = 0
			}
			addSpec(arrival, arrival%len(routes), 24_000+int64(arrival%3)*1_000, duration, true)
			arrival++
		}
	}

	watermark8000 := origin + 8_000
	project := &Project{
		ProjectID: "gateway-proto-sample", ProjectVersion: fmt.Sprintf("seed-%d-v1", seed),
		Schema: SchemaV1, Seed: seed, TimeOriginMS: origin, SequenceStepMS: 1000,
		Window: WindowConfig{WidthMS: 10_000, SlideMS: 5_000}, AllowedLatenessMS: 2_000,
		HistogramBoundariesUS: boundaries, DurationToleranceUS: 1, CountTolerance: 0,
		Export: ExportConfig{MaxWindowsPerBatch: 6, MaxRecordsPerBatch: 24, PriorityRules: []PriorityRule{
			{Priority: 10, Level: "status", StatusCode: 500},
			{Priority: 20, Level: "status"},
			{Priority: 30, Level: "route"},
			{Priority: 40, Level: "service", ServiceID: "svc-billing"},
			{Priority: 50, Level: "service"},
		}},
		Budget:   Budget{Workers: 3, ServiceBuffer: 64, MaxWorkers: 16, MaxServiceBuffer: 4096},
		Services: services, Routes: routes, Records: records,
		WatermarkMS: &watermark8000,
		WatermarkMarks: []WatermarkMark{
			{BeforeArrivalSeq: 2, WatermarkMS: origin + 16_000},
			{BeforeArrivalSeq: 4, WatermarkMS: origin + 24_000},
			{BeforeArrivalSeq: arrival, WatermarkMS: origin + 50_000},
		},
		CommittedBoundaryMS:  &watermark8000,
		WatermarkSource:      "explicit",
		DeclaredFallbackRule: "when watermark field is absent, use committed_window_boundary_ms as presumed watermark",
		Metadata:             map[string]string{"generator": "gatewayproto.GenerateSample", "fixed_seed": fmt.Sprint(seed)},
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	normalizeRecordFixed(project)

	truth, err := BuildReferenceTruth(project)
	if err != nil {
		return nil, err
	}
	schemas := map[string][]byte{}
	for _, version := range []SchemaName{SchemaV1, SchemaV2, SchemaV3} {
		data, err := EncodeSchema(project, version)
		if err != nil {
			return nil, err
		}
		schemas[string(version)] = data
	}
	return &SampleBundle{Canonical: project, Schemas: schemas, Truth: truth}, nil
}

func normalizeRecordFixed(p *Project) {
	for i := range p.Records {
		if p.Records[i].DurationFixed == 0 && p.Records[i].DurationUS >= 0 {
			p.Records[i].DurationFixed = FixedDuration(p.Records[i].DurationUS * DurationScale)
		}
	}
}

func MarshalPretty(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
