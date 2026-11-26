package sample

import (
	"testing"
)

func TestSmokeGenerate(t *testing.T) {
	b, err := Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decisions=%d ticks=%d critical=%d", len(b.Truth.Decisions), len(b.Truth.Ticks), len(b.CriticalCases))
	for _, d := range b.Truth.Decisions {
		t.Logf("tick=%d %s tenant=%s ask=%d admit=%d queue=%d verdict=%s pre=%d corr=%d reason=%s constraint=%s encroacher=%s retry=%d", d.Tick, d.RequestID, d.TenantID, d.AskedBytes/MiB, d.AdmittedBytes/MiB, d.QueuedBytes/MiB, d.Verdict, d.PreheldBytes/MiB, d.SettlementCorrection/MiB, d.ReasonCode, d.Constraint, d.EncroacherTenantID, d.RetryTick)
	}
}
