package slikernel

import (
	"fmt"
	"math/rand"
	"sort"
)

type Fixture struct {
	Seed    int64   `json:"seed"`
	Project Project `json:"project"`
	Truth   Report  `json:"truth"`
}

type serviceBlueprint struct {
	name  string
	route string
	base  string
}

var defaultBlueprints = []serviceBlueprint{
	{name: "checkout", route: "/checkout", base: "checkout"},
	{name: "search", route: "/search", base: "search"},
	{name: "payments", route: "/pay", base: "payments"},
}

// GenerateFixture produces a deterministic, local-only observation project
// and its independently calculated truth report.
func GenerateFixture(seed int64) (Fixture, error) {
	project := generateProject(seed)
	if err := ValidateProject(project); err != nil {
		return Fixture{}, err
	}
	truth, err := ComputeTruth(project)
	if err != nil {
		return Fixture{}, err
	}
	return Fixture{Seed: seed, Project: project, Truth: truth}, nil
}

func generateProject(seed int64) Project {
	random := rand.New(rand.NewSource(seed))
	const originMs int64 = 1_700_000_000_000
	const windowMs int64 = 60_000
	const baseWindows int64 = 12
	project := Project{
		ProjectID:         fmt.Sprintf("sre-fixture-%d", seed),
		TimeOriginMs:      originMs,
		EvaluationStartMs: originMs,
		EvaluationEndMs:   originMs + baseWindows*windowMs,
	}
	for _, blueprint := range defaultBlueprints {
		service := Service{Name: blueprint.name}
		commonThresholds := []Threshold{
			{ID: "WARN", BurnPPM: 500_000, ConsecutiveWindows: 2},
			{ID: "CRIT", BurnPPM: 1_000_000, ConsecutiveWindows: 1},
		}
		service.SLIs = []SLI{
			{
				ID: blueprint.base + "-availability", Kind: KindAvailability, Route: blueprint.route,
				StatusCodes: []int{200, 201, 500, 503}, WindowMs: windowMs,
				TargetPPM: 997_000, RollingWindows: 3, Thresholds: commonThresholds,
				GapPolicy: GapIncludeEmpty,
			},
			{
				ID: blueprint.base + "-latency", Kind: KindLatency, Route: blueprint.route,
				StatusCodes: []int{200, 201, 500, 503}, WindowMs: windowMs,
				TargetPPM: 980_000, RollingWindows: 3, QuantilePPM: 950_000,
				LatencyThresholdUs: 60_000, Thresholds: commonThresholds,
				GapPolicy: GapIncludeEmpty,
			},
			{
				ID: blueprint.base + "-throughput", Kind: KindThroughput, Route: blueprint.route,
				StatusCodes: []int{200, 201, 500, 503}, WindowMs: windowMs,
				TargetPPM: 990_000, RollingWindows: 3, ExpectedEventsPerWindow: 1_000,
				Thresholds: commonThresholds, GapPolicy: GapIncludeEmpty,
			},
		}
		project.Services = append(project.Services, service)
	}

	for windowIndex := int64(0); windowIndex < baseWindows; windowIndex++ {
		for serviceIndex, service := range project.Services {
			// A deliberate interior observation gap proves that rolling windows
			// keep the empty base window instead of shifting samples.
			if service.Name == "search" && windowIndex == 4 {
				continue
			}
			baseTimestamp := originMs + windowIndex*windowMs
			events := int64(800 + random.Intn(601))
			errorEvents := int64(0)
			if events > 100 {
				errorEvents = int64(random.Intn(int(events/55) + 1))
			}
			goodEvents := events - errorEvents
			errorStatus := 500
			if (windowIndex+int64(serviceIndex))%2 == 0 {
				errorStatus = 503
			}
			project.Observations = append(project.Observations, Observation{
				Service: service.Name, Route: defaultBlueprints[serviceIndex].route,
				StatusCode: 200, TimestampMs: baseTimestamp + 1,
				Events: goodEvents, Errors: 0,
				LatencySamplesUs: generateLatencies(random, goodEvents),
				ThroughputEvents: goodEvents,
			})
			if errorEvents > 0 {
				project.Observations = append(project.Observations, Observation{
					Service: service.Name, Route: defaultBlueprints[serviceIndex].route,
					StatusCode: errorStatus, TimestampMs: baseTimestamp + 2,
					Events: errorEvents, Errors: errorEvents,
					LatencySamplesUs: generateLatencies(random, errorEvents),
					ThroughputEvents: errorEvents,
				})
			}
			// Noise on another route must never enter a declared SLI.
			noiseEvents := int64(900 + random.Intn(201))
			project.Observations = append(project.Observations, Observation{
				Service: service.Name, Route: "/health",
				StatusCode: 200, TimestampMs: baseTimestamp + 5,
				Events: noiseEvents, Errors: 0,
				LatencySamplesUs: []int64{500, 700, 900},
				ThroughputEvents: noiseEvents,
			})
		}
	}
	sort.SliceStable(project.Observations, func(i, j int) bool {
		left := project.Observations[i]
		right := project.Observations[j]
		if left.TimestampMs != right.TimestampMs {
			return left.TimestampMs < right.TimestampMs
		}
		if left.Service != right.Service {
			return left.Service < right.Service
		}
		return left.StatusCode < right.StatusCode
	})
	for index := range project.Observations {
		project.Observations[index].Seq = int64(index + 1)
	}
	return project
}

func generateLatencies(random *rand.Rand, count int64) []int64 {
	if count <= 0 {
		return nil
	}
	samples := make([]int64, count)
	for index := range samples {
		if random.Float64() < 0.021 {
			samples[index] = 61_000 + random.Int63n(120_000)
		} else {
			samples[index] = 12_000 + random.Int63n(42_000)
		}
	}
	return samples
}
