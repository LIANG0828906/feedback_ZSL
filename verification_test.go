package fogkernel

import (
	"testing"
)

func TestGeneratedVerification(t *testing.T) {
	bundle, err := GenerateDefaultBundle()
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyGeneratedBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ok=%v rays=%d hits=%d exactT=%.3e tau=%.3e energy=%.3e preview=%.6f bound=%.6f diag=%v terminals=%d",
		report.OK, report.RayCount, report.HitCount, report.ExactMaxTError, report.ExactMaxTauError,
		report.ExactMaxEnergyError, report.PreviewMaxError, report.PreviewBound,
		report.DiagnosticReplays, report.TerminalMatches)
	for _, v := range report.Violations {
		t.Logf("violation: %+v", v)
	}
	if !report.OK {
		t.Fatalf("verification failed: %+v", report.Violations)
	}
}
