package pipeline

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestSchemaAnomalyCodes(t *testing.T) {
	sample, err := GenerateDefault()
	if err != nil {
		t.Fatal(err)
	}
	base, err := sample.VersionDocument("v2")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutated []byte
		code    string
		path    string
	}{
		{
			name: "negative width",
			mutated: bytes.Replace(base, []byte(`"windowWidth": 20000`),
				[]byte(`"windowWidth": -20000`), 1),
			code: CodeFeatureWindowWidth,
			path: ".windowWidth",
		},
		{
			name: "zero step",
			mutated: bytes.Replace(base, []byte(`"windowStep": 5000`),
				[]byte(`"windowStep": 0`), 1),
			code: CodeFeatureStepZero,
			path: ".windowStep",
		},
		{
			name: "weight out of range",
			mutated: bytes.Replace(base, []byte(`"weightPerMyriad": 10000`),
				[]byte(`"weightPerMyriad": 20000`), 1),
			code: CodeAggWeightOutOfRange,
			path: ".weightPerMyriad",
		},
		{
			name: "conflicting aliases",
			mutated: bytes.Replace(base, []byte(`"windowWidth": 20000`),
				[]byte(`"windowWidth": 20000, "window_width_ms": 12345`), 1),
			code: CodeSchemaAmbiguous,
			path: "$.features[0]",
		},
		{
			name: "relative time basis missing",
			mutated: bytes.Replace(base, []byte(`"timeOriginMs": 1704067200000`),
				[]byte(`"timeOriginMs": null`), 1),
			code: CodeTimeBasisMissing,
			path: ".timeOriginMs",
		},
		{
			name: "version and alignment combination",
			mutated: bytes.Replace(base, []byte(`"alignment": "first_data"`),
				[]byte(`"alignment": "epoch"`), 1),
			code: CodeSchemaAmbiguous,
			path: "$.alignment",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadJSON(tc.mutated)
			be, ok := AsBusinessError(err)
			if !ok || be.Code != tc.code {
				t.Fatalf("got %v want %s", err, tc.code)
			}
			found := false
			for _, value := range be.Context {
				if value == tc.path || len(tc.path) > 0 && contains(value, tc.path) {
					found = true
				}
			}
			if !found {
				t.Fatalf("error lacks path %q: %+v", tc.path, be.Context)
			}
		})
	}
}

func TestRunAnomalyCodesAndSourceImmutability(t *testing.T) {
	base := loadVersion(t, "v2")
	snapshot := base.Clone()
	tests := []struct {
		name string
		run  func() (*RunResult, error)
		code string
	}{
		{
			name: "negative width",
			run: func() (*RunResult, error) {
				bad := base.Clone()
				bad.Features[0].WindowWidthMs = -1
				return Run(context.Background(), bad, DefaultRunOptions())
			},
			code: CodeFeatureWindowWidth,
		},
		{
			name: "zero step",
			run: func() (*RunResult, error) {
				bad := base.Clone()
				bad.Features[0].WindowStepMs = 0
				return Run(context.Background(), bad, DefaultRunOptions())
			},
			code: CodeFeatureStepZero,
		},
		{
			name: "weight out of range",
			run: func() (*RunResult, error) {
				bad := base.Clone()
				bad.Features[0].Aggregations[0].Weight = -1
				return Run(context.Background(), bad, DefaultRunOptions())
			},
			code: CodeAggWeightOutOfRange,
		},
		{
			name: "watermark earlier than committed window",
			run: func() (*RunResult, error) {
				bad := base.Clone()
				v := bad.TimeOriginMs
				bad.Events[len(bad.Events)-1].WatermarkMs = &v
				return Run(context.Background(), bad, DefaultRunOptions())
			},
			code: CodeWatermarkBeforeCommitted,
		},
		{
			name: "thread budget exceeded",
			run: func() (*RunResult, error) {
				opts := DefaultRunOptions()
				opts.Workers = 65
				opts.MaxWorkers = 64
				return Run(context.Background(), base, opts)
			},
			code: CodeThreadBudgetExceeded,
		},
		{
			name: "buffer budget exceeded",
			run: func() (*RunResult, error) {
				opts := DefaultRunOptions()
				opts.WindowBufferPerFeature = 1025
				opts.MaxWindowBufferPerFeature = 1024
				return Run(context.Background(), base, opts)
			},
			code: CodeBufferBudgetExceeded,
		},
		{
			name: "streaming backpressure",
			run: func() (*RunResult, error) {
				opts := DefaultRunOptions()
				opts.Workers = 2
				opts.WindowBufferPerFeature = 1
				opts.MaxWindowBufferPerFeature = 2
				return Run(context.Background(), base, opts)
			},
			code: CodeBackpressureWindowBuffer,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.run()
			if got := ErrorCode(err); got != tc.code {
				t.Fatalf("got code %s (%v) want %s", got, err, tc.code)
			}
			be, _ := AsBusinessError(err)
			if len(be.Context) == 0 {
				t.Fatalf("error has no locating context")
			}
			_ = result
			if !reflect.DeepEqual(base, snapshot) {
				t.Fatalf("anomaly path modified source project")
			}
		})
	}
}

func contains(value, part string) bool {
	if len(part) == 0 {
		return false
	}
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
