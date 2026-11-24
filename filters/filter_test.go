package filters

import (
	"context"
	"sync/atomic"
	"testing"

	"yuvench/errs"
	"yuvench/yuv"
)

func testPlane(width, height int) *yuv.Plane {
	data := make([]uint16, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			data[y*width+x] = uint16((x*17 + y*29 + (x*y)%31) % 256)
		}
	}
	return &yuv.Plane{Name: "Y", Width: width, Height: height, BitDepth: 8, MinCode: 0, MaxCode: 255, Data: data}
}

func TestStagesAreChunkAndWorkerIndependent(t *testing.T) {
	plane := testPlane(73, 29)
	params := DefaultParams()
	nodes := []string{NodeYDenoise, NodeYSharpen, NodeYContrast, NodeUSmooth, NodeUGain, NodeVSmooth, NodeVGain}
	for _, node := range nodes {
		t.Run(node, func(t *testing.T) {
			small := DefaultLimits()
			small.MaxParallel = 1
			small.ChunkRows = 1
			first, err := Apply(context.Background(), plane, node, params, Runtime{Limits: small})
			if err != nil {
				t.Fatal(err)
			}
			large := DefaultLimits()
			large.MaxParallel = 4
			large.ChunkRows = 29
			second, err := Apply(context.Background(), plane, node, params, Runtime{Limits: large})
			if err != nil {
				t.Fatal(err)
			}
			if !first.SamePixels(second) {
				t.Fatalf("%s output changed with chunking/parallelism", node)
			}
			if first.Width != plane.Width || first.Height != plane.Height || first.BitDepth != plane.BitDepth {
				t.Fatalf("%s changed plane shape", node)
			}
		})
	}
}

func TestParameterValidation(t *testing.T) {
	params := DefaultParams()
	params.YSharpen.Amount = 3
	_, err := Apply(context.Background(), testPlane(9, 9), NodeYSharpen, params, Runtime{Limits: DefaultLimits()})
	business, ok := errs.As(err)
	if !ok || business.Code != errs.CodeParameter || business.Field != "ySharpen.amount" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBufferBudgetBlocksAndRecordsReason(t *testing.T) {
	plane := testPlane(4096, 16)
	params := DefaultParams()
	params.YDenoise.Radius = 8
	limits := Limits{MaxParallel: 3, MaxBufferBytes: 64 << 10, PerWorkerBufferBytes: 64 << 10, ChunkRows: 4}
	var waits atomic.Int32
	_, err := Apply(context.Background(), plane, NodeYDenoise, params, Runtime{
		Limits: limits,
		OnBufferWait: func(reason string) {
			if reason == "" {
				t.Errorf("empty buffer wait reason")
			}
			waits.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if waits.Load() == 0 {
		t.Fatal("expected at least one blocked buffer request")
	}
}
