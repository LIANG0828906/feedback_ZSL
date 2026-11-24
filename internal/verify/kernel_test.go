package verify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"screenverify/internal/dataset"
	"screenverify/internal/model"
	"screenverify/internal/rverr"
	"screenverify/internal/sample"
)

func TestParameterValidationFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Params)
		field  string
	}{
		{"grid", func(p *Params) { p.GridX = 0 }, "grid_x"},
		{"threshold", func(p *Params) { p.TamperThreshold = 1.2 }, "tamper_threshold"},
		{"block", func(p *Params) { p.BlockSize = 7 }, "block_size"},
		{"window", func(p *Params) { p.ResidualWindow = 99 }, "residual_window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultParams()
			tc.mutate(&p)
			err := p.Validate(96, 64, 16)
			if rverr.CodeOf(err) != rverr.EPara {
				t.Fatalf("code=%v err=%v", rverr.CodeOf(err), err)
			}
			if e := rverr.As(err); e.Field != tc.field {
				t.Fatalf("field=%q want %q", e.Field, tc.field)
			}
		})
	}
	rt := RuntimeConfig{Parallelism: 2, BufferPixelsPerThread: 10}
	if err := rt.Validate(96, 64); rverr.CodeOf(err) != rverr.EPara {
		t.Fatalf("expected buffer E_PARA, got %v", err)
	}
}

func TestConfidenceMonotonicWithThreshold(t *testing.T) {
	frame := &model.Frame{No: 8}
	wm := WatermarkEvidence{MinConfidence: 1}
	tl := TimelineEvidence{Anomaly: true, Jump: true, Score: 0.9}
	res := ResidualEvidence{Score: 0.2}
	noise := NoiseEvidence{Score: 0.4}
	edge := EdgeEvidence{Score: 0.1}
	prev := 2.0
	for i := 0; i <= 10; i++ {
		threshold := float64(i) / 10.0
		p := DefaultParams()
		p.TamperThreshold = threshold
		d := decide(frame, p, wm, tl, res, noise, edge)
		if d.Confidence < 0 || d.Confidence > 1 {
			t.Fatalf("confidence out of range: %v", d.Confidence)
		}
		if d.Confidence > prev+1e-12 {
			t.Fatalf("confidence increased at threshold %.1f: prev %.6f now %.6f", threshold, prev, d.Confidence)
		}
		prev = d.Confidence
	}
}

func TestBufferLimiterQueuesAndRecordsWait(t *testing.T) {
	l := NewBufferLimiter(1, 100)
	first, err := l.Acquire(context.Background(), 100, 1, EvNoise, "test")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *Lease, 1)
	go func() {
		lease, err := l.Acquire(context.Background(), 10, 2, EvNoise, "pixel_buffer_exhausted")
		if err != nil {
			t.Error(err)
			done <- nil
			return
		}
		done <- lease
	}()
	time.Sleep(40 * time.Millisecond)
	first.Release()
	select {
	case lease := <-done:
		if lease == nil {
			t.Fatal("queued acquisition failed")
		}
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("buffered acquisition was not unblocked")
	}
	waits := l.Waits()
	if len(waits) == 0 || waits[0].Reason != "pixel_buffer_exhausted" {
		t.Fatalf("missing queue wait record: %+v", waits)
	}
}

func TestCompareLocatesBlockField(t *testing.T) {
	a := Snapshot{Noises: []NoiseEvidence{{
		Frame: 8,
		Blocks: []NoiseBlock{
			{BlockX: 0, BlockY: 0, Variance: 1},
			{BlockX: 3, BlockY: 0, Variance: 2},
		},
	}}}
	b := Snapshot{Noises: []NoiseEvidence{{
		Frame: 8,
		Blocks: []NoiseBlock{
			{BlockX: 0, BlockY: 0, Variance: 1},
			{BlockX: 3, BlockY: 0, Variance: 2.5},
		},
	}}}
	d := CompareSnapshots(a, b)
	if d == nil || d.Code != "E_DIFF" || d.Frame != 8 || d.BlockX != 3 || d.Field != "variance" {
		t.Fatalf("unexpected diff: %+v", d)
	}
}

func TestWatermarkDecodeDeterministic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watermark")
	if _, err := sample.GenerateClip(dir, "watermark", "watermark_break", 99, 16); err != nil {
		t.Fatal(err)
	}
	set, err := dataset.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultParams()
	f := set.Frames[3]
	region := set.Truth.Frames[3].WatermarkRegion
	first := decodeWatermark(f, region, p)
	b1, _ := json.Marshal(first)
	for i := 0; i < 32; i++ {
		got := decodeWatermark(f, region, p)
		b2, _ := json.Marshal(got)
		if string(b1) != string(b2) {
			t.Fatalf("non-deterministic decode on iteration %d", i)
		}
	}

	results := make(chan string, 16)
	for i := 0; i < 16; i++ {
		go func() {
			got := decodeWatermark(f, region, p)
			b, _ := json.Marshal(got)
			results <- string(b)
		}()
	}
	for i := 0; i < 16; i++ {
		if got := <-results; got != string(b1) {
			t.Fatal("decode varies by goroutine")
		}
	}
}
