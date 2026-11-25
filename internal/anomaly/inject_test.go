package anomaly_test

import (
	"testing"

	"credrot/internal/anomaly"
	"credrot/internal/bizerr"
	"credrot/internal/generator"
	"credrot/internal/hashutil"
	"credrot/internal/project"
)

func TestInjectedAnomaliesHaveStableCodesAndDoNotMutateSource(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	before, err := hashutil.JSONSHA256(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		kind anomaly.Kind
		code string
	}{
		{anomaly.DuplicateCredentialRef, bizerr.CodeDuplicateRef},
		{anomaly.NegativeTTL, bizerr.CodeNegativeTTL},
		{anomaly.BatchLimitZero, bizerr.CodeBatchLimit},
		{anomaly.SubscriptionCycle, bizerr.CodeSubCycle},
		{anomaly.MissingCredential, bizerr.CodeMissingCredential},
		{anomaly.DuplicateCredential, bizerr.CodeDuplicateCredential},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			mutated, err := anomaly.Inject(bundle.Project, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			err = project.Validate(mutated)
			if !bizerr.IsCode(err, tc.code) {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
		})
	}
	after, err := hashutil.JSONSHA256(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("anomaly injection mutated its source project")
	}
}
