package iconcut

import (
	"bytes"
	"encoding/gob"
)

type StageArtifact struct {
	ImageID    string        `json:"image_id"`
	TileIndex  int           `json:"tile_index"`
	TX         int           `json:"tx"`
	TY         int           `json:"ty"`
	Stage      string        `json:"stage"`
	Core       Rect          `json:"core"`
	Band       Rect          `json:"band,omitempty"`
	SourceRect Rect          `json:"source_rect,omitempty"`
	RGB        []byte        `json:"rgb,omitempty"`
	Labels     []byte        `json:"labels,omitempty"`
	LabelW     int           `json:"label_w,omitempty"`
	LabelH     int           `json:"label_h,omitempty"`
	MapEntries []MapEntry    `json:"map_entries,omitempty"`
	TileStats  []RegionStats `json:"tile_stats,omitempty"`
	Trace      *GrowTrace    `json:"trace,omitempty"`
}

func EncodeArtifact(a *StageArtifact) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(a); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func DecodeArtifact(b []byte) (*StageArtifact, error) {
	var a StageArtifact
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&a); err != nil {
		return nil, err
	}
	return &a, nil
}

type StageSummary struct {
	TileIndex   int           `json:"tile_index"`
	TX          int           `json:"tx"`
	TY          int           `json:"ty"`
	Stage       string        `json:"stage"`
	Core        Rect          `json:"core"`
	Band        Rect          `json:"band,omitempty"`
	SourceRect  Rect          `json:"source_rect,omitempty"`
	PixelCount  int           `json:"pixel_count"`
	LabelCount  int           `json:"label_count,omitempty"`
	TileStats   []RegionStats `json:"tile_stats,omitempty"`
	MaxResidual float64       `json:"max_residual,omitempty"`
}

func SummaryFor(a *StageArtifact) StageSummary {
	s := StageSummary{
		TileIndex: a.TileIndex, TX: a.TX, TY: a.TY, Stage: a.Stage,
		Core: a.Core, Band: a.Band, SourceRect: a.SourceRect,
		PixelCount: a.Core.W * a.Core.H, TileStats: a.TileStats,
	}
	if len(a.Labels) > 0 {
		n := 0
		for _, v := range a.Labels {
			if v != 0 {
				n++
			}
		}
		s.LabelCount = n
	}
	for _, e := range a.MapEntries {
		if e.ResidualPX > s.MaxResidual {
			s.MaxResidual = e.ResidualPX
		}
	}
	return s
}

func labelsToCore(m *LabelMask, r Rect) []byte {
	out := make([]byte, r.W*r.H)
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			out[y*r.W+x] = m.At(r.X+x, r.Y+y)
		}
	}
	return out
}

func copyCoreLabels(dst *LabelMask, labels []byte, r Rect) {
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			dst.Set(r.X+x, r.Y+y, labels[y*r.W+x])
		}
	}
}

func rgbToCore(im *Image, r Rect) []byte {
	out := make([]byte, r.W*r.H*3)
	for y := 0; y < r.H; y++ {
		srcStart := ((r.Y+y)*im.W + r.X) * 3
		dstStart := y * r.W * 3
		copy(out[dstStart:dstStart+r.W*3], im.Pix[srcStart:srcStart+r.W*3])
	}
	return out
}

func copyCoreRGB(dst *Image, rgb []byte, r Rect) {
	for y := 0; y < r.H; y++ {
		dstStart := ((r.Y+y)*dst.W + r.X) * 3
		srcStart := y * r.W * 3
		copy(dst.Pix[dstStart:dstStart+r.W*3], rgb[srcStart:srcStart+r.W*3])
	}
}
