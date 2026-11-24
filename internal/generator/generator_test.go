package generator

import (
	"path/filepath"
	"testing"

	"spatialir/internal/model"
)

func TestGenerateDeterministic(t *testing.T) {
	dir := t.TempDir()
	spec := BuiltinSpecs()[0]
	m1, sig1, dry1, err := Generate(spec, filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	_, sig2, dry2, err := Generate(spec, filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if m1.SampleRate != spec.SampleRate || m1.Layout.Channels != 2 {
		t.Fatalf("unexpected manifest: %+v", m1)
	}
	for ch := range sig1 {
		if len(sig1[ch]) != spec.Length {
			t.Fatalf("length mismatch")
		}
		for i := range sig1[ch] {
			if sig1[ch][i] != sig2[ch][i] || dry1[ch][i] != dry2[ch][i] {
				t.Fatalf("non-deterministic output at ch=%d sample=%d", ch, i)
			}
		}
	}
	// 真值只来自解析构造：直达与反射到达点均为整数样本点且严格有序。
	for _, ct := range m1.Truth {
		prev := ct.DirectSample
		for _, rf := range ct.Reflections {
			if rf.ArrivalSample <= prev {
				t.Fatalf("truth arrivals not monotonic: %s", ct.Channel)
			}
			prev = rf.ArrivalSample
		}
	}
}

func TestMissingChannelFault(t *testing.T) {
	dir := t.TempDir()
	spec := BuiltinSpecs()[0]
	spec.Faults = model.FaultSpec{MissingChannel: "R"}
	m, _, _, err := Generate(spec, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSignalFile(filepath.Join(dir, m.SignalFiles[1])); err == nil {
		t.Fatal("expected missing channel file to be absent")
	}
}
