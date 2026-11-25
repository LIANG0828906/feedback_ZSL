package forward

import "testing"

func TestWidthOverflowAndSaturation(t *testing.T) {
	w, err := NewWidthCalc(16)
	if err != nil {
		t.Fatal(err)
	}
	if _, biz := w.Fit(70000); biz == nil || biz.Code != CodeFragmentOverflow {
		t.Fatalf("expected overflow, got %v", biz)
	}
	value, saturated := w.SaturatingAdd(65530, 100)
	if !saturated || value != 65535 {
		t.Fatalf("unexpected saturation: value=%d saturated=%v", value, saturated)
	}

	plan := PlanFragmentation(FragmentRequest{
		PacketID: "overflow", TunnelID: "T", PayloadBytes: 1_000_000,
		MTUBytes: 1500, PathMTUBytes: 1500, OuterProtocol: "udp", CipherSuite: "none",
		IntegerBits: 16, MaxFragments: 128,
	})
	if plan.RejectCode != CodeFragmentOverflow {
		t.Fatalf("expected E_FRAG_OVERFLOW, got %q", plan.RejectCode)
	}
}

func TestTooManyFragmentsDistinguishable(t *testing.T) {
	plan := PlanFragmentation(FragmentRequest{
		PacketID: "too-many", TunnelID: "T", PayloadBytes: 20_000,
		MTUBytes: 1480, PathMTUBytes: 1500, OuterProtocol: "udp", CipherSuite: "aes-256-gcm",
		IntegerBits: 32, MaxFragments: 2,
	})
	if plan.RejectCode != CodeFragmentTooMany {
		t.Fatalf("expected %s, got %q", CodeFragmentTooMany, plan.RejectCode)
	}
}

func TestStateCannotSkipHandshake(t *testing.T) {
	_, _, biz := NextTunnelState(StateIdle, "handshake_complete")
	if biz == nil || biz.Code != CodeStateSkip {
		t.Fatalf("expected state skip error, got %v", biz)
	}
	next, reason, biz := NextTunnelState(StateIdle, "connect")
	if biz != nil || next != StateHandshaking || reason == "" {
		t.Fatalf("connect should enter handshaking: %s %s %v", next, reason, biz)
	}
}

func TestParallelReportsAreDeterministic(t *testing.T) {
	sample := GenerateSample(DefaultSeed)
	eng, err := LoadEngineering(SchemaV2, sample.Documents[SchemaV2])
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := RunReport(eng.Project, eng.Events)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []Budget{{Workers: 1, PerTunnelBuffer: 1}, {Workers: 4, PerTunnelBuffer: 2}} {
		for i := 0; i < 3; i++ {
			parallel, err := RunParallelReport(eng.Project, eng.Events, budget)
			if err != nil {
				t.Fatal(err)
			}
			if !SignaturesEqual(ReportSignature(sequential), ReportSignature(parallel)) {
				t.Fatalf("non-deterministic or mismatched report budget=%+v iteration=%d", budget, i)
			}
		}
	}
}
