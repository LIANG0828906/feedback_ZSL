package seams

import (
	"errors"
	"testing"

	"terrainpipeline/internal/model"
)

func TestClassifyKnownCausesAndAmbiguousCode(t *testing.T) {
	tests := []struct {
		name    string
		signals Signals
		want    model.Attribution
		code    model.ErrorCode
	}{
		{
			name:    "geometric",
			signals: Signals{ThresholdDeg: 6, RawAngleDeg: 18, GeometryAngleDeg: 18, CorrectedAngleDeg: 16.5, AlternativeAngleDeg: 0},
			want:    model.AttributionGeometric,
		},
		{
			name:    "material",
			signals: Signals{ThresholdDeg: 6, RawAngleDeg: 22, GeometryAngleDeg: 6.6, CorrectedAngleDeg: 0, AlternativeAngleDeg: 0},
			want:    model.AttributionMaterial,
		},
		{
			name:    "differential",
			signals: Signals{ThresholdDeg: 6, RawAngleDeg: 6.4, GeometryAngleDeg: 6.4, CorrectedAngleDeg: 0, AlternativeAngleDeg: 0},
			want:    model.AttributionDifferential,
		},
		{
			name:    "ambiguous",
			signals: Signals{ThresholdDeg: 6, RawAngleDeg: 9.2, GeometryAngleDeg: 6.1, CorrectedAngleDeg: 5.7, AlternativeAngleDeg: 7.4},
			want:    model.AttributionAmbiguous,
			code:    model.CodeSeam,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.signals)
			if got != tt.want {
				t.Fatalf("attribution=%s want %s err=%v", got, tt.want, err)
			}
			if tt.code != "" {
				var te *model.Error
				if !errors.As(err, &te) || te.Code != tt.code {
					t.Fatalf("expected %s, got %v", tt.code, err)
				}
			}
		})
	}
}
