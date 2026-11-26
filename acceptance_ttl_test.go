package dnslab_test

import (
	"testing"

	"dnslab/fixedint"
)

func TestTTLRepresentationsProduceSameExpiryDecision(t *testing.T) {
	const base = int64(1_764_000_000_000)

	fromSeconds, saturated := fixedint.SecondsToMS(100)
	if saturated {
		t.Fatal("unexpected saturation")
	}
	absoluteFromSeconds, saturated := fixedint.RemainingToAbsolute(base, fromSeconds)
	if saturated {
		t.Fatal("unexpected absolute saturation")
	}
	absoluteFromMillis, _ := fixedint.RemainingToAbsolute(base, 100_000)
	absoluteDirect := base + 100_000

	if absoluteFromSeconds != absoluteFromMillis || absoluteFromMillis != absoluteDirect {
		t.Fatalf("absolute expiry mismatch: %d %d %d", absoluteFromSeconds, absoluteFromMillis, absoluteDirect)
	}

	freshAt := base + 50_000
	if r := fixedint.Remaining(absoluteDirect, freshAt); r.Value != 50_000 || r.Value <= 0 {
		t.Fatalf("expected fresh 50s remaining, got %+v", r)
	}
	expiredAt := base + 100_001
	if r := fixedint.Remaining(absoluteDirect, expiredAt); r.Value != -1 || r.Value > 0 {
		t.Fatalf("expected expired decision at -1ms, got %+v", r)
	}
}
