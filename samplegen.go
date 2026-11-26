package downsample

import (
	"math/rand"
	"time"
)

const DeterministicSeed int64 = 20261126

type WeightCase struct {
	Name         string                   `json:"name"`
	Description  string                   `json:"description"`
	Report       WeightConversionReport   `json:"report"`
	WantCode     string                   `json:"want_code"`
	RoundTrip    []WeightConversionReport `json:"round_trip,omitempty"`
	RoundTripEnd int64                    `json:"round_trip_end,omitempty"`
}

type SampleBundle struct {
	Seed        int64                      `json:"seed"`
	BaseTimeMS  int64                      `json:"base_time_ms"`
	Project     *Project                   `json:"project"`
	Documents   map[SchemaVersion][]byte   `json:"-"`
	Truth       map[WindowKey]WindowResult `json:"truth"`
	WeightCases []WeightCase               `json:"weight_cases"`
}

func GenerateBundle() (*SampleBundle, *Error) {
	return GenerateBundleWithSeed(DeterministicSeed)
}

func GenerateBundleWithSeed(seed int64) (*SampleBundle, *Error) {
	rng := rand.New(rand.NewSource(seed))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	minute := int64(60_000)
	end := base + 8*minute

	project := &Project{
		ProjectID:     "demo-downsample-project",
		ConfigVersion: "cfg-v1",
		BaseTimeMS:    base,
		RangeStartMS:  base,
		RangeEndMS:    end,
		Tolerance:     1e-6,
		Series:        map[string][]Point{},
		Functions:     DefaultFunctionTable(),
		Degradation: DegradationPolicy{
			MissingFillField: "use_declared_default",
			FillLookback:     2,
		},
	}

	seq := int64(0)
	nextSeq := func() int64 { seq++; return seq }
	mk := func(metric string, minuteOffset, secondOffset int64, value float64, weight int64) Point {
		return Point{
			Seq: nextSeq(), MetricID: metric,
			TimestampMS: base + minuteOffset*minute + secondOffset*1000,
			Value:       value, Weight: weight,
			SourceWeightRaw: weight, SourceWeightScale: int64(ScalePermyriad),
		}
	}

	cpuID := "cpu.usage"
	cpuPoints := []Point{
		mk(cpuID, 0, 0, 10.0, 1000),
		mk(cpuID, 0, 10, 12.0, 2000),
		mk(cpuID, 0, 20, 14.0, 3000),
		mk(cpuID, 1, 0, 20.0, 1000), // exact right boundary of window 0: belongs to window 1
		mk(cpuID, 1, 15, 22.0, 2000),
		mk(cpuID, 1, 45, 24.0, 3000),
		mk(cpuID, 3, 0, 30.0, 2000),
		mk(cpuID, 3, 30, 32.0, 4000),
		mk(cpuID, 7, 0, 40.0, 1000),
	}

	memID := "mem.usage"
	memPoints := []Point{
		mk(memID, 0, 0, 60+rng.Float64(), 1500),
		mk(memID, 0, 35, 62+rng.Float64(), 2500),
		mk(memID, 1, 10, 64+rng.Float64(), 2000),
		mk(memID, 2, 25, 70+rng.Float64(), 3000),
		mk(memID, 4, 20, 80+rng.Float64(), 2500),
	}

	diskID := "disk.io"
	diskPoints := []Point{
		mk(diskID, 0, 0, 100+rng.Float64()*10, 1000),
		mk(diskID, 1, 30, 110+rng.Float64()*10, 2000),
		mk(diskID, 3, 30, 120+rng.Float64()*10, 3000),
		mk(diskID, 7, 0, 130+rng.Float64()*10, 1000),
	}

	project.Metrics = []Metric{
		{
			ID: cpuID, Ordinal: 0, Kind: "raw", WindowMS: minute, AnchorMS: base,
			SourceAlignment: SourceAlignEpoch, WeightScale: ScalePermyriad, WeightBits: 32,
			Functions: []Func{FuncAvg, FuncSum, FuncMin, FuncMax, FuncWeightedAvg},
			Fill:      FillPolicy{Enabled: true, Strategy: "last_valid_window", MaxLookbackWindows: 2, FieldPresent: true},
		},
		{
			ID: memID, Ordinal: 1, Kind: "raw", WindowMS: minute, AnchorMS: base,
			SourceAlignment: SourceAlignFirstPoint, WeightScale: ScalePermyriad, WeightBits: 32,
			Functions: []Func{FuncAvg, FuncSum, FuncMin, FuncMax, FuncWeightedAvg},
			Fill:      FillPolicy{Enabled: true, Strategy: "last_valid_window", MaxLookbackWindows: 2, FieldPresent: true},
		},
		{
			ID: diskID, Ordinal: 2, Kind: "raw", WindowMS: 2 * minute, AnchorMS: base,
			SourceAlignment: SourceAlignEpoch, WeightScale: ScalePermyriad, WeightBits: 32,
			Functions: []Func{FuncAvg, FuncSum, FuncMax, FuncWeightedAvg},
			Fill:      FillPolicy{Enabled: true, Strategy: "last_valid_window", MaxLookbackWindows: 1, FieldPresent: true},
		},
		{
			ID: "cpu.scaled", Ordinal: 3, Kind: "derived", WindowMS: minute, AnchorMS: base,
			SourceAlignment: SourceAlignEpoch, WeightScale: ScalePermyriad, WeightBits: 32,
			Functions: []Func{FuncDerived},
			Fill:      FillPolicy{Enabled: true, Strategy: "last_valid_window", MaxLookbackWindows: 2, FieldPresent: true},
			Derivation: &Derivation{
				Op: "scale", Factor: 2,
				Inputs: []DerivedInput{{MetricID: cpuID, Function: FuncAvg, Lag: 0}},
			},
		},
	}
	project.Series[cpuID] = cpuPoints
	project.Series[memID] = memPoints
	project.Series[diskID] = diskPoints

	docs := map[SchemaVersion][]byte{}
	var rerr *Error
	if docs[SchemaV1], rerr = RenderDocument(project, SchemaV1); rerr != nil {
		return nil, rerr
	}
	if docs[SchemaV2], rerr = RenderDocument(project, SchemaV2); rerr != nil {
		return nil, rerr
	}
	if docs[SchemaV3], rerr = RenderDocument(project, SchemaV3); rerr != nil {
		return nil, rerr
	}

	loaded, lerr := LoadProject(docs[SchemaV1])
	if lerr != nil {
		return nil, lerr
	}
	truthEngine := NewEngine(loaded, Budget{MaxWorkers: 4, MaxWindowsPerMetric: 64})
	truth, terr := truthEngine.RecomputeAll(InvalidationSource{Kind: "generator_truth"})
	if terr != nil {
		return nil, terr
	}

	return &SampleBundle{
		Seed: seed, BaseTimeMS: base, Project: loaded, Documents: docs,
		Truth: truth.Results, WeightCases: BuildWeightCases(),
	}, nil
}

func BuildWeightCases() []WeightCase {
	cases := []WeightCase{}

	out, rep, err := ConvertWeight(123, ScalePermille, 16, ScalePermyriad, 16)
	rep.MetricID = "exact-case"
	cases = append(cases, WeightCase{
		Name: "exact_permille_to_permyriad", Description: "123‰ must become exact 1230/10000",
		Report: rep, WantCode: CodeOK,
	})
	_ = out

	out, rep, err = ConvertWeight(12345, ScalePermyriad, 16, ScalePermille, 16)
	rep.MetricID = "rounding-case"
	cases = append(cases, WeightCase{
		Name: "rounding_permyriad_to_permille", Description: "12345/10000 rounds to 1235‰ within 0.5 target unit",
		Report: rep, WantCode: CodeOK,
	})
	_ = out

	out, rep, err = ConvertWeight(30000, ScalePermille, 16, ScalePermyriad, 16)
	rep.MetricID = "saturation-case"
	want := CodeOK
	if err != nil {
		want = err.Code
	}
	cases = append(cases, WeightCase{
		Name: "saturation_16bit", Description: "30000‰ converts past int16 and must saturate with a large truth delta",
		Report: rep, WantCode: want,
	})
	_ = out

	roundReports, end, _ := StableRoundTrip(12345, ScalePermyriad, 32, 12)
	last := roundReports[len(roundReports)-1]
	cases = append(cases, WeightCase{
		Name: "repeated_round_trip", Description: "repeated scale conversion reaches a fixed point and cannot amplify",
		Report: last, RoundTrip: roundReports, RoundTripEnd: end, WantCode: CodeOK,
	})
	return cases
}
