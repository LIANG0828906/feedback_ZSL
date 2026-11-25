package audiofp

import (
	"context"
	"math"
	"reflect"
	"testing"
)

func testRecipe() Recipe {
	return Recipe{
		FrameLen: 128, FrameShift: 64, Bands: 8, QuantBits: 4,
		SimilarityThreshold: 0.5, CandidateLimit: 10,
	}
}

func testAsset(id string, total int, silentTail int) PCMAsset {
	samples := make([]int16, total)
	silentFrom := total - silentTail
	for i := 0; i < total; i++ {
		if silentTail > 0 && i >= silentFrom {
			continue
		}
		v := 0.32*math.Sin(2*math.Pi*float64(i)*7/128) +
			0.21*math.Sin(2*math.Pi*float64(i)*29/128) +
			0.11*math.Sin(2*math.Pi*float64(i)*53/128)
		samples[i] = int16(math.Round(v * 32767))
	}
	return PCMAsset{ID: id, SampleRate: 8000, Samples: samples}
}

func TestExtractionIsDeterministicAcrossWorkersAndChunks(t *testing.T) {
	asset := testAsset("a0", 700, 0)
	r := testRecipe()
	first, _, err := ExtractAsset(context.Background(), asset, r, ExtractionOptions{
		Workers: 1, FrameChunk: 3, SamplingBufferPerWorker: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ExtractAsset(context.Background(), asset, r, ExtractionOptions{
		Workers: 8, FrameChunk: 17, SamplingBufferPerWorker: 4096,
		MaxSamplingBufferSamples: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("worker count, chunk size, or buffer pressure changed fingerprints")
	}
	if len(first) != frameCount(len(asset.Samples), r) {
		t.Fatalf("got %d frames, want %d", len(first), frameCount(len(asset.Samples), r))
	}
	for i := 1; i < len(first); i++ {
		if first[i].StartSample <= first[i-1].StartSample {
			t.Fatal("frames are not in ascending sample order")
		}
	}
}

func TestSampleLimiterBlocksAndRecordsReason(t *testing.T) {
	limiter := newSampleLimiter(64)
	ctx := context.Background()
	if err := limiter.acquire(ctx, 64, "a", 1, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- limiter.acquire(ctx, 32, "a", 1, 1)
	}()
	for {
		limiter.mu.Lock()
		events := len(limiter.events)
		limiter.mu.Unlock()
		if events > 0 {
			break
		}
	}
	limiter.release(64)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, events := limiter.snapshot()
	if len(events) == 0 {
		t.Fatal("buffer exhaustion was not recorded")
	}
}

func TestSilentFrameIsRetainedAndMarkedZero(t *testing.T) {
	asset := testAsset("silent", 256, 128)
	frames, _, err := ExtractAsset(context.Background(), asset, testRecipe(), ExtractionOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	last := frames[len(frames)-1]
	if !last.Zero {
		t.Fatal("silent frame was not marked zero")
	}
	for _, level := range last.Levels {
		if level != 0 {
			t.Fatal("zero frame contains a non-zero band level")
		}
	}
}

func TestExtractionParameterAndOrderErrors(t *testing.T) {
	asset := testAsset("a0", 300, 0)
	bad := testRecipe()
	bad.FrameShift = bad.FrameLen + 1
	if _, _, err := ExtractAsset(context.Background(), asset, bad, ExtractionOptions{}); CodeOf(err) != CodeParameter {
		t.Fatalf("got %v, want E_PARA", err)
	}
	_, _, err := ExtractAsset(context.Background(), asset, testRecipe(), ExtractionOptions{
		Inject: &Injection{Kind: "frame_order", AssetID: "a0", Shard: 2, Frame: 1},
	})
	if CodeOf(err) != CodeOrder {
		t.Fatalf("got %v, want E_ORDER", err)
	}
	typed, _ := AsError(err)
	if typed.AssetID != "a0" || typed.Shard != 2 || typed.Frame != 1 {
		t.Fatalf("E_ORDER detail missing: %+v", typed)
	}
}

func TestIncrementalSessionDoesNotMutateCallerSamples(t *testing.T) {
	assets := []PCMAsset{testAsset("a0", 400, 0), testAsset("a1", 400, 0)}
	beforeHash := assets[1].ContentHash()
	session, err := NewIncrementalSession(context.Background(), assets, checkpointRecipe(), ExtractionOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	replacement := PCMAsset{ID: "a1", Samples: testAsset("repl", 60, 0).Samples}
	_, err = session.Apply(context.Background(), EditOp{
		Seq: 0, Kind: "replace", AssetID: "a1", StartSample: 100, EndSample: 160,
	}, map[int]PCMAsset{0: replacement})
	if err != nil {
		t.Fatal(err)
	}
	if assets[1].ContentHash() != beforeHash {
		t.Fatal("incremental session mutated caller-owned PCM")
	}
}
