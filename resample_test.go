package ortho

import (
	"context"
	"testing"
)

func testCfg() ResampleConfig {
	return ResampleConfig{InterpOrder: 1, TileSize: 64, Band: 2, Workers: 4, BufferPixels: 1 << 20}
}

func TestTiledEqualsWholeImage(t *testing.T) {
	src := genTerrainGray(SampleSeed)
	st := NewRadial(DefaultStageParams().Radial)

	tiled, err := Resample(context.Background(), src, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	whole := testCfg()
	whole.TileSize = 1 << 15 // 单块即整图
	full, err := Resample(context.Background(), src, st, whole, nil)
	if err != nil {
		t.Fatal(err)
	}
	if x, y, ok := CompareImages(tiled.Img, full.Img); !ok {
		t.Fatalf("tiled != whole at (%d,%d)", x, y)
	}
}

func TestParallelEqualsSerial(t *testing.T) {
	src := genTerrainRGB(SampleSeed + 1)
	st := NewHomography(DefaultStageParams().Homography)
	par, err := Resample(context.Background(), src, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ser := testCfg()
	ser.Workers = 1
	seq, err := Resample(context.Background(), src, st, ser, nil)
	if err != nil {
		t.Fatal(err)
	}
	if x, y, ok := CompareImages(par.Img, seq.Img); !ok {
		t.Fatalf("parallel != serial at (%d,%d)", x, y)
	}
}

func TestChannelsPreserved(t *testing.T) {
	st := NewAffine(DefaultStageParams().Affine)
	gray := genTerrainGray(SampleSeed)
	g, err := Resample(context.Background(), gray, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if Channels(g.Img) != 1 {
		t.Fatalf("gray output channels = %d, want 1", Channels(g.Img))
	}
	rgb := genTerrainRGB(SampleSeed + 1)
	r, err := Resample(context.Background(), rgb, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if Channels(r.Img) != 3 {
		t.Fatalf("rgb output channels = %d, want 3", Channels(r.Img))
	}
	w, h := st.OutSize(SampleWidth, SampleHeight)
	if r.Img.Bounds().Dx() != w || r.Img.Bounds().Dy() != h {
		t.Fatalf("output size not derived from params")
	}
}

func TestBandInsufficient(t *testing.T) {
	src := genTerrainGray(SampleSeed)
	st := NewAffine(DefaultStageParams().Affine)
	cfg := testCfg()
	cfg.Band = 0
	_, err := Resample(context.Background(), src, st, cfg, nil)
	if err == nil || err.Code != EBand {
		t.Fatalf("expected E_BAND, got %v", err)
	}
	if err.NeedBand != RequiredBand(1) {
		t.Fatalf("NeedBand = %d, want %d", err.NeedBand, RequiredBand(1))
	}
}

func TestBufferExhaustionRecorded(t *testing.T) {
	src := genTerrainRGB(SampleSeed + 1)
	st := NewAffine(DefaultStageParams().Affine)
	cfg := testCfg()
	cfg.BufferPixels = cfg.TileSize * cfg.TileSize * 3 * 2 // 仅 2 个块缓冲
	log := &WaitLog{}
	res, err := Resample(context.Background(), src, st, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if log.Len() == 0 {
		t.Fatal("expected buffer exhaustion wait events")
	}
	// 缓冲受限时结果仍须与充足缓冲一致。
	full, err := Resample(context.Background(), src, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if x, y, ok := CompareImages(res.Img, full.Img); !ok {
		t.Fatalf("buffer-limited != unlimited at (%d,%d)", x, y)
	}
	for _, ev := range log.Events {
		if ev.Reason != "pixel_buffer_exhausted" {
			t.Fatalf("unexpected wait reason %q", ev.Reason)
		}
	}
}

func TestTileFetchWindowsBanded(t *testing.T) {
	src := genTerrainGray(SampleSeed)
	st := NewAffine(DefaultStageParams().Affine)
	res, err := Resample(context.Background(), src, st, testCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tiles) == 0 {
		t.Fatal("no tiles recorded")
	}
	for _, m := range res.Tiles {
		if m.SX1 < m.SX0 || m.SY1 < m.SY0 {
			t.Fatalf("invalid fetch window %+v", m)
		}
	}
}
