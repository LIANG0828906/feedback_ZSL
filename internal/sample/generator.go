package sample

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// SchemaVersion and AlgorithmVersion pin every on-disk and computed contract.
// A bump invalidates old checkpoints, forcing a full recompute.
const (
	SchemaVersion    = "vqa-schema-1"
	AlgorithmVersion = "vqa-algo-1"
	FrameMagic       = "VQAFRAME"
)

// Geometry describes the analytic primitives embedded in every frame.
type Geometry struct {
	RectX0, RectY0, RectX1, RectY1 int
	CircleCX, CircleCY, CircleR    int
}

// DefaultGeometry places a rectangle and circle inside the default 64x64 frame.
func DefaultGeometry() Geometry {
	return Geometry{
		RectX0: 6, RectY0: 8, RectX1: 30, RectY1: 26,
		CircleCX: 44, CircleCY: 42, CircleR: 12,
	}
}

// edgePixels analytically enumerates boundary positions of the primitives. The
// result is deterministic and order-stable; it never comes from pixel scanning.
func (g Geometry) edgePixels(max int) []EdgePixel {
	var edges []EdgePixel
	add := func(x, y int, desc string) {
		if len(edges) >= max {
			return
		}
		edges = append(edges, EdgePixel{X: x, Y: y, Channel: 0, Description: desc})
	}
	for x := g.RectX0; x <= g.RectX1 && len(edges) < max; x++ {
		add(x, g.RectY0, "rect-top")
		add(x, g.RectY1, "rect-bottom")
	}
	for y := g.RectY0 + 1; y < g.RectY1 && len(edges) < max; y++ {
		add(g.RectX0, y, "rect-left")
		add(g.RectX1, y, "rect-right")
	}
	for deg := 0; deg < 360 && len(edges) < max; deg += 30 {
		rad := float64(deg) * math.Pi / 180
		x := g.CircleCX + int(math.Round(float64(g.CircleR)*math.Cos(rad)))
		y := g.CircleCY + int(math.Round(float64(g.CircleR)*math.Sin(rad)))
		add(x, y, "circle-perimeter")
	}
	return edges
}

// GenConfig holds the deterministic generation parameters.
type GenConfig struct {
	Seed         int64
	Width        int
	Height       int
	Channels     int
	Frames       int
	BlockSize    int
	Qualities    []int
	SampleFrames []int
	GeneratedAt  string
}

// Generated bundles the sequences and manifest produced by one generation.
type Generated struct {
	Manifest *Manifest
	Orig     []*Frame
	Recon    map[int][]*Frame
}

// Generate builds the original sequence, every lossy reconstruction and the
// truth fingerprint manifest deterministically from cfg.
func Generate(cfg GenConfig) (*Generated, error) {
	geo := DefaultGeometry()
	orig := make([]*Frame, cfg.Frames)
	for f := 0; f < cfg.Frames; f++ {
		orig[f] = generateFrame(cfg, f, geo)
	}
	recon := map[int][]*Frame{}
	fp := map[string]FrameFingerprint{}
	for _, qp := range cfg.Qualities {
		codec := NewCodec(cfg.BlockSize, qp)
		frames := make([]*Frame, cfg.Frames)
		var blockReports [][]BlockReport // frame -> blocks in raster order
		for f := 0; f < cfg.Frames; f++ {
			rec, reports := reconstruct(cfg, codec, orig[f])
			frames[f] = rec
			blockReports = append(blockReports, reports)
		}
		recon[qp] = frames
		for _, sf := range cfg.SampleFrames {
			fp[fingerprintKey(qp, sf)] = buildFingerprint(cfg, sf, orig[sf], frames[sf], blockReports[sf], geo)
		}
	}
	m := &Manifest{
		SchemaVersion:  SchemaVersion,
		Seed:           cfg.Seed,
		Width:          cfg.Width,
		Height:         cfg.Height,
		Channels:       cfg.Channels,
		Frames:         cfg.Frames,
		BlockSize:      cfg.BlockSize,
		QualityLevels:  append([]int(nil), cfg.Qualities...),
		SampleFrames:   append([]int(nil), cfg.SampleFrames...),
		Fingerprints:   fp,
		GeneratedAtUTC: cfg.GeneratedAt,
	}
	return &Generated{Manifest: m, Orig: orig, Recon: recon}, nil
}

func generateFrame(cfg GenConfig, frameIndex int, geo Geometry) *Frame {
	w, h, ch := cfg.Width, cfg.Height, cfg.Channels
	fr := &Frame{Width: w, Height: h, Channels: ch, Pix: make([]uint8, w*h*ch)}
	r := newRNG(cfg.Seed ^ (int64(frameIndex+1) * 2654435761))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			grad := (x*128/w + y*128/w) // smooth two-axis gradient
			texture := r.intn(2) * 22   // high-frequency deterministic texture
			base := grad/2 + texture
			inRect := x >= geo.RectX0 && x <= geo.RectX1 && y >= geo.RectY0 && y <= geo.RectY1
			dx := x - geo.CircleCX
			dy := y - geo.CircleCY
			inCircle := dx*dx+dy*dy <= geo.CircleR*geo.CircleR
			for c := 0; c < ch; c++ {
				v := base + c*9 + frameIndex*3
				if inRect {
					v = 210 + c*5
				}
				if inCircle {
					v = 60 + c*7
				}
				fr.Set(x, y, c, v)
			}
		}
	}
	return fr
}

func reconstruct(cfg GenConfig, codec *Codec, src *Frame) (*Frame, []BlockReport) {
	w, h, ch, n := cfg.Width, cfg.Height, cfg.Channels, cfg.BlockSize
	rec := &Frame{Width: w, Height: h, Channels: ch, Pix: make([]uint8, w*h*ch)}
	var reports []BlockReport
	for by := 0; by < h/n; by++ {
		for bx := 0; bx < w/n; bx++ {
			tile := make([]uint8, n*n*ch)
			for y := 0; y < n; y++ {
				for x := 0; x < n; x++ {
					for c := 0; c < ch; c++ {
						tile[(y*n+x)*ch+c] = src.Pix[((by*n+y)*w+(bx*n+x))*ch+c]
					}
				}
			}
			rep := codec.EncodeBlock(tile, ch)
			reports = append(reports, rep)
			for y := 0; y < n; y++ {
				for x := 0; x < n; x++ {
					for c := 0; c < ch; c++ {
						rec.Pix[((by*n+y)*w+(bx*n+x))*ch+c] = rep.Recon[(y*n+x)*ch+c]
					}
				}
			}
		}
	}
	return rec, reports
}

func buildFingerprint(cfg GenConfig, frameIndex int, orig, rec *Frame, reports []BlockReport, geo Geometry) FrameFingerprint {
	mse := meanSquaredError(orig, rec)
	psnr := 20 * math.Log10(255.0/math.Sqrt(mse))
	margin := 1.0
	fp := FrameFingerprint{
		Frame:      frameIndex,
		PSNRMin:    math.Max(0, psnr-margin),
		PSNRMax:    psnr + margin,
		EdgePixels: geo.edgePixels(16),
	}
	for i, rep := range reports {
		errs := make([]int, 0, len(rep.PixelErr))
		for _, e := range rep.PixelErr {
			errs = append(errs, absInt(e))
		}
		sort.Ints(errs)
		bx := i % (cfg.Width / cfg.BlockSize)
		by := i / (cfg.Width / cfg.BlockSize)
		fp.BlockQuantiles = append(fp.BlockQuantiles, BlockQuantile{
			BX: bx, BY: by,
			Q10: nearestRank(errs, 0.10),
			Q50: nearestRank(errs, 0.50),
			Q90: nearestRank(errs, 0.90),
		})
	}
	return fp
}

func nearestRank(sorted []int, p float64) int {
	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func meanSquaredError(a, b *Frame) float64 {
	sum := 0.0
	for i := range a.Pix {
		d := float64(a.Pix[i]) - float64(b.Pix[i])
		sum += d * d
	}
	return sum / float64(len(a.Pix))
}

func fingerprintKey(quality, frame int) string {
	return itoa(quality) + ":" + itoa(frame)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// EncodeFrame serializes a frame with magic, header, payload and trailing CRC32.
func EncodeFrame(f *Frame) []byte {
	buf := make([]byte, 0, len(f.Pix)+32)
	buf = append(buf, FrameMagic...)
	hdr := make([]byte, 11)
	hdr[0] = 1 // frame format version
	binary.BigEndian.PutUint16(hdr[1:3], uint16(f.Width))
	binary.BigEndian.PutUint16(hdr[3:5], uint16(f.Height))
	hdr[5] = byte(f.Channels)
	binary.BigEndian.PutUint32(hdr[6:10], uint32(len(f.Pix)))
	hdr[10] = 0 // reserved
	buf = append(buf, hdr...)
	buf = append(buf, f.Pix...)
	crc := crc32.ChecksumIEEE(buf)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint32(tail, crc)
	buf = append(buf, tail...)
	return buf
}

// WritePersisted writes the manifest and all frame files under dataDir.
func WritePersisted(dataDir string, g *Generated) error {
	if err := os.MkdirAll(filepath.Join(dataDir, "orig"), 0o755); err != nil {
		return err
	}
	for f, fr := range g.Orig {
		p := filepath.Join(dataDir, "orig", frameName(f))
		if err := os.WriteFile(p, EncodeFrame(fr), 0o644); err != nil {
			return err
		}
	}
	for qp, frames := range g.Recon {
		dir := filepath.Join(dataDir, "recon", "q"+itoa(qp))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for f, fr := range frames {
			if err := os.WriteFile(filepath.Join(dir, frameName(f)), EncodeFrame(fr), 0o644); err != nil {
				return err
			}
		}
	}
	return WriteManifest(dataDir, g.Manifest)
}

func frameName(f int) string {
	return "frame_" + pad4(f) + ".bin"
}

func pad4(v int) string {
	s := itoa(v)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
