package terrainbake

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// AlgorithmVersion is stored in every checkpoint. Change it when any operation
// that can affect pixel output changes.
const AlgorithmVersion = "terrain-normal-v1"

// RawBlock contains unencoded tangent-space normals before seam continuity is
// applied. Slope is intentionally derived only after seam resolution.
type RawBlock struct {
	BlockID string
	Width   int
	Height  int
	Normals []Vec3
}

func bakeRawBlock(p *Project, blockID string, params BakeParams) (*RawBlock, error) {
	if err := ValidateBakeParams(params); err != nil {
		return nil, err
	}
	b, ok := FindBlock(p, blockID)
	if !ok {
		return nil, &Error{Code: CodeSchema, Object: "block", Field: "id", BlockID: blockID, Message: "block does not exist"}
	}
	materials := make([]Material, len(b.Materials))
	for i, id := range b.Materials {
		m, ok := FindMaterial(p, id)
		if !ok {
			return nil, &Error{
				Code: CodeSchema, Object: "block:" + blockID, Field: "material_ids",
				Message: fmt.Sprintf("material %q does not exist", id), BlockID: blockID,
			}
		}
		materials[i] = *m
	}
	pixels := b.Width * b.Height
	raw := &RawBlock{BlockID: blockID, Width: b.Width, Height: b.Height, Normals: make([]Vec3, pixels)}
	nLayers := len(b.Materials)
	for y := 0; y < b.Height; y++ {
		for x := 0; x < b.Width; x++ {
			dx, dy := differential(b, x, y, params.Operator)
			var sum Vec3
			for layer, m := range materials {
				weight := float64(b.Weights[(y*b.Width+x)*nLayers+layer]) / WeightQuantum
				if weight == 0 {
					continue
				}
				ln := Vec3{
					X: -dx * params.Scale * m.NormalScale,
					Y: -dy * params.Scale * m.NormalScale,
					Z: 1,
				}
				var ok bool
				ln, ok = normalize(ln)
				if !ok {
					return nil, &Error{
						Code: CodeBake, Object: "block:" + blockID, Field: "heights",
						BlockID: blockID, X: x, Y: y,
						Message: "derived a zero-length layer normal",
					}
				}
				ln = rotateZ(ln, m.RotationDeg)
				sum.X += weight * ln.X
				sum.Y += weight * ln.Y
				sum.Z += weight * ln.Z
			}
			n, ok := normalize(sum)
			if !ok {
				return nil, &Error{
					Code: CodeBake, Object: "block:" + blockID, Field: "material_ids",
					BlockID: blockID, X: x, Y: y,
					Message: "material-weighted normal is zero-length",
				}
			}
			// Tangent-space normals point away from the surface. Keep +Z
			// non-negative for a stable, conventional RGB encoding.
			if n.Z < 0 {
				n = Vec3{-n.X, -n.Y, -n.Z}
			}
			raw.Normals[y*b.Width+x] = n
		}
	}
	return raw, nil
}

func differential(b *Block, x, y int, operator string) (float64, float64) {
	at := func(px, py int) float64 {
		if px < 0 {
			px = 0
		} else if px >= b.Width {
			px = b.Width - 1
		}
		if py < 0 {
			py = 0
		} else if py >= b.Height {
			py = b.Height - 1
		}
		return b.Elevation[py*b.Width+px]
	}
	switch operator {
	case "forward":
		return at(x+1, y) - at(x, y), at(x, y+1) - at(x, y)
	case "central":
		return (at(x+1, y) - at(x-1, y)) * 0.5, (at(x, y+1) - at(x, y-1)) * 0.5
	default: // sobel
		dx := (at(x+1, y-1) + 2*at(x+1, y) + at(x+1, y+1) -
			at(x-1, y-1) - 2*at(x-1, y) - at(x-1, y+1)) / 8
		dy := (at(x-1, y+1) + 2*at(x, y+1) + at(x+1, y+1) -
			at(x-1, y-1) - 2*at(x, y-1) - at(x+1, y-1)) / 8
		return dx, dy
	}
}

func rotateZ(v Vec3, deg float64) Vec3 {
	if deg == 0 {
		return v
	}
	r := deg * math.Pi / 180
	c, s := math.Cos(r), math.Sin(r)
	return Vec3{X: v.X*c - v.Y*s, Y: v.X*s + v.Y*c, Z: v.Z}
}

func slopeDegrees(n Vec3) float64 {
	horizontal := math.Hypot(n.X, n.Y)
	a := math.Atan2(horizontal, math.Abs(n.Z)) * 180 / math.Pi
	if a < 0 {
		a = 0
	}
	if a > 90 {
		a = 90
	}
	return a
}

func finalizeBlock(raw *RawBlock, overrides map[pixelKey]Vec3, contentHash string, seam SeamStats) (*BlockOutput, error) {
	if raw == nil || len(raw.Normals) != raw.Width*raw.Height {
		return nil, &Error{Code: CodeBake, Object: "block", Field: "normals", Message: "invalid raw normal buffer"}
	}
	normals := make([]Vec3, len(raw.Normals))
	copy(normals, raw.Normals)
	for k, v := range overrides {
		if k.blockID != raw.BlockID {
			continue
		}
		if k.x < 0 || k.x >= raw.Width || k.y < 0 || k.y >= raw.Height {
			return nil, &Error{Code: CodeBake, Object: "seam", Field: "pixel", BlockID: raw.BlockID, X: k.x, Y: k.y, Message: "seam override outside block"}
		}
		normals[k.y*raw.Width+k.x] = v
	}
	slopes := make([]float64, len(normals))
	nrgba := image.NewNRGBA(image.Rect(0, 0, raw.Width, raw.Height))
	gray := image.NewGray16(image.Rect(0, 0, raw.Width, raw.Height))
	for i, n := range normals {
		slopes[i] = slopeDegrees(n)
		r, g, b := encodeNormal(n)
		nrgba.SetNRGBA(i%raw.Width, i/raw.Width, color.NRGBA{R: r, G: g, B: b, A: 255})
		sv := uint16(math.Round(slopes[i] / 90 * 65535))
		gray.SetGray16(i%raw.Width, i/raw.Width, color.Gray16{Y: sv})
	}
	normalPNG, err := encodePNG(nrgba)
	if err != nil {
		return nil, &Error{Code: CodeBake, Object: "block:" + raw.BlockID, Field: "normal_png", BlockID: raw.BlockID, Message: err.Error()}
	}
	slopePNG, err := encodePNG(gray)
	if err != nil {
		return nil, &Error{Code: CodeBake, Object: "block:" + raw.BlockID, Field: "slope_png", BlockID: raw.BlockID, Message: err.Error()}
	}
	data, err := encodeDataBinary(raw.Width, raw.Height, normals, slopes)
	if err != nil {
		return nil, err
	}
	seam.BlockID = raw.BlockID
	return &BlockOutput{
		BlockID: raw.BlockID, Width: raw.Width, Height: raw.Height,
		Normals: normals, SlopeDeg: slopes,
		NormalPNG: normalPNG, SlopePNG: slopePNG,
		NormalDigest: digestBytes(normalPNG),
		SlopeDigest:  digestBytes(slopePNG),
		DataDigest:   digestBytes(data),
		ContentHash:  contentHash,
		Seam:         seam,
	}, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const rawMagic = "TBR1"

func encodeRawBinary(raw *RawBlock) ([]byte, error) {
	if raw == nil || len(raw.Normals) != raw.Width*raw.Height {
		return nil, &Error{Code: CodeBake, Object: "block", Field: "normals", Message: "invalid raw buffer"}
	}
	var buf bytes.Buffer
	buf.WriteString(rawMagic)
	_ = binary.Write(&buf, binary.LittleEndian, int32(raw.Width))
	_ = binary.Write(&buf, binary.LittleEndian, int32(raw.Height))
	for _, n := range raw.Normals {
		_ = binary.Write(&buf, binary.LittleEndian, n.X)
		_ = binary.Write(&buf, binary.LittleEndian, n.Y)
		_ = binary.Write(&buf, binary.LittleEndian, n.Z)
	}
	return buf.Bytes(), nil
}

func decodeRawBinary(data []byte) (*RawBlock, error) {
	if len(data) < 12 || string(data[:4]) != rawMagic {
		return nil, &Error{Code: CodeIO, Object: "raw_block", Field: "magic", Message: "invalid raw block file"}
	}
	w := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	h := int(int32(binary.LittleEndian.Uint32(data[8:12])))
	if w <= 0 || h <= 0 || w*h > (len(data)-12)/24 || len(data) != 12+w*h*24 {
		return nil, &Error{Code: CodeIO, Object: "raw_block", Field: "size", Message: "raw block dimensions or size mismatch"}
	}
	raw := &RawBlock{Width: w, Height: h, Normals: make([]Vec3, w*h)}
	off := 12
	for i := range raw.Normals {
		raw.Normals[i].X = math.Float64frombits(binary.LittleEndian.Uint64(data[off:]))
		raw.Normals[i].Y = math.Float64frombits(binary.LittleEndian.Uint64(data[off+8:]))
		raw.Normals[i].Z = math.Float64frombits(binary.LittleEndian.Uint64(data[off+16:]))
		off += 24
	}
	return raw, nil
}

const dataMagic = "TBD1"

func encodeDataBinary(w, h int, normals []Vec3, slopes []float64) ([]byte, error) {
	if w <= 0 || h <= 0 || len(normals) != w*h || len(slopes) != w*h {
		return nil, &Error{Code: CodeIO, Object: "block_data", Field: "size", Message: "invalid final block buffers"}
	}
	var buf bytes.Buffer
	buf.WriteString(dataMagic)
	_ = binary.Write(&buf, binary.LittleEndian, int32(w))
	_ = binary.Write(&buf, binary.LittleEndian, int32(h))
	for i, n := range normals {
		_ = binary.Write(&buf, binary.LittleEndian, n.X)
		_ = binary.Write(&buf, binary.LittleEndian, n.Y)
		_ = binary.Write(&buf, binary.LittleEndian, n.Z)
		_ = binary.Write(&buf, binary.LittleEndian, slopes[i])
	}
	return buf.Bytes(), nil
}

func decodeDataBinary(data []byte) (int, int, []Vec3, []float64, error) {
	if len(data) < 12 || string(data[:4]) != dataMagic {
		return 0, 0, nil, nil, &Error{Code: CodeIO, Object: "block_data", Field: "magic", Message: "invalid block data file"}
	}
	w := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	h := int(int32(binary.LittleEndian.Uint32(data[8:12])))
	if w <= 0 || h <= 0 || len(data) != 12+w*h*32 {
		return 0, 0, nil, nil, &Error{Code: CodeIO, Object: "block_data", Field: "size", Message: "block data dimensions or size mismatch"}
	}
	normals := make([]Vec3, w*h)
	slopes := make([]float64, w*h)
	off := 12
	for i := 0; i < w*h; i++ {
		normals[i].X = math.Float64frombits(binary.LittleEndian.Uint64(data[off:]))
		normals[i].Y = math.Float64frombits(binary.LittleEndian.Uint64(data[off+8:]))
		normals[i].Z = math.Float64frombits(binary.LittleEndian.Uint64(data[off+16:]))
		slopes[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[off+24:]))
		off += 32
	}
	return w, h, normals, slopes, nil
}
