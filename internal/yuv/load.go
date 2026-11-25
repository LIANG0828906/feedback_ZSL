package yuv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"uvenhance/internal/apperr"
)

func LoadSample(dir string) (*Sample, error) {
	metaPath := filepath.Join(dir, "metadata.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, apperr.Meta("metadata.json", "cannot read sample metadata: "+err.Error())
	}
	if err := validateRequiredFields(raw); err != nil {
		return nil, err
	}
	var md Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return nil, apperr.Meta("metadata.json", "invalid JSON: "+err.Error())
	}
	if err := validateMetadata(&md); err != nil {
		return nil, err
	}

	planes := make(map[string]*Plane, 3)
	for _, name := range []string{"Y", "U", "V"} {
		pm := md.Planes[name]
		p, err := readPlane(dir, name, pm, md.QuantMin, md.QuantMax)
		if err != nil {
			return nil, err
		}
		if want := md.InputHashes[name]; want != "" && p.ContentHash() != want {
			return nil, apperr.Plane(name, want, p.ContentHash(), "input content hash mismatch")
		}
		planes[name] = p
	}
	return &Sample{Metadata: md, Y: planes["Y"], U: planes["U"], V: planes["V"]}, nil
}

func validateRequiredFields(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return apperr.Meta("metadata.json", "invalid JSON: "+err.Error())
	}
	for _, key := range []string{
		"format_version", "id", "width", "height", "bit_depth",
		"quant_min", "quant_max", "chroma_alignment", "upsample", "planes",
	} {
		if !hasJSONField(root, key) {
			return apperr.Meta(key, "required metadata field is missing")
		}
	}
	var planes map[string]json.RawMessage
	if err := json.Unmarshal(root["planes"], &planes); err != nil {
		return apperr.Meta("planes", "plane metadata must be an object")
	}
	for _, name := range []string{"Y", "U", "V"} {
		rawPlane, ok := planes[name]
		if !ok || isJSONNull(rawPlane) {
			return apperr.Meta("planes."+name, "required plane metadata is missing")
		}
		var plane map[string]json.RawMessage
		if err := json.Unmarshal(rawPlane, &plane); err != nil {
			return apperr.Meta("planes."+name, "plane metadata must be an object")
		}
		for _, key := range []string{"file", "width", "height", "bit_depth"} {
			if !hasJSONField(plane, key) {
				return apperr.Meta("planes."+name+"."+key, "required plane metadata field is missing")
			}
		}
	}
	return nil
}

func hasJSONField(obj map[string]json.RawMessage, key string) bool {
	raw, ok := obj[key]
	return ok && !isJSONNull(raw)
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func validateMetadata(md *Metadata) error {
	if md.FormatVersion != 1 {
		return apperr.Meta("format_version", fmt.Sprintf("unsupported format version %d", md.FormatVersion))
	}
	if md.ID == "" {
		return apperr.Meta("id", "sample id is required")
	}
	if md.Width <= 0 || md.Height <= 0 {
		return apperr.Meta("width/height", "positive frame dimensions are required")
	}
	if md.Width%2 != 0 || md.Height%2 != 0 {
		return apperr.Plane("Y", "even luma dimensions",
			fmt.Sprintf("%dx%d", md.Width, md.Height), "luma width and height must be even")
	}
	if err := validBitDepth(md.BitDepth); err != nil {
		return apperr.Meta("bit_depth", err.Error())
	}
	if md.QuantMin >= md.QuantMax {
		return apperr.Meta("quant_min/quant_max", fmt.Sprintf("invalid range [%d,%d]", md.QuantMin, md.QuantMax))
	}
	maxCode := uint32(1)<<uint(md.BitDepth) - 1
	if uint32(md.QuantMax) > maxCode {
		return apperr.Meta("quant_max", fmt.Sprintf("value %d exceeds %d-bit range", md.QuantMax, md.BitDepth))
	}
	if md.ChromaAlignment != AlignCenter && md.ChromaAlignment != AlignTopLeft {
		return apperr.Meta("chroma_alignment", "must be center or top_left")
	}
	if md.Upsample != UpsampleBilin && md.Upsample != UpsampleNear {
		return apperr.Meta("upsample", "must be bilinear or nearest")
	}
	if md.Planes == nil {
		return apperr.Meta("planes", "plane metadata is required")
	}
	expected := map[string][2]int{
		"Y": {md.Width, md.Height},
		"U": {md.Width / 2, md.Height / 2},
		"V": {md.Width / 2, md.Height / 2},
	}
	for _, name := range []string{"Y", "U", "V"} {
		pm, ok := md.Planes[name]
		if !ok {
			return apperr.Meta("planes."+name, "plane metadata is required")
		}
		if strings.TrimSpace(pm.File) == "" {
			return apperr.Meta("planes."+name+".file", "plane file is required")
		}
		want := expected[name]
		if pm.Width != want[0] || pm.Height != want[1] {
			return apperr.Plane(name,
				fmt.Sprintf("%dx%d", want[0], want[1]),
				fmt.Sprintf("%dx%d", pm.Width, pm.Height),
				"declared plane dimensions are inconsistent with 4:2:0")
		}
		if pm.BitDepth != md.BitDepth {
			return apperr.Plane(name,
				fmt.Sprintf("bit_depth=%d", md.BitDepth),
				fmt.Sprintf("bit_depth=%d", pm.BitDepth),
				"plane bit depths differ")
		}
	}
	for _, b := range md.Blocks {
		if b.Rect.W <= 0 || b.Rect.H <= 0 || b.Rect.X < 0 || b.Rect.Y < 0 ||
			b.Rect.X+b.Rect.W > md.Width || b.Rect.Y+b.Rect.H > md.Height {
			return apperr.Meta("blocks", "block rectangle outside image: "+b.Name)
		}
	}
	return nil
}

func validBitDepth(d int) error {
	switch d {
	case 8, 10, 12, 16:
		return nil
	default:
		return fmt.Errorf("unsupported bit depth %d", d)
	}
}

func readPlane(dir, name string, pm PlaneMeta, qMin, qMax uint16) (*Plane, error) {
	path := filepath.Clean(filepath.Join(dir, pm.File))
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, apperr.Plane(name, "path inside sample directory", pm.File, "unsafe plane path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Plane(name, fmt.Sprintf("readable file %s", pm.File), err.Error(), "cannot read plane")
	}
	bytesPerSample := 1
	if pm.BitDepth > 8 {
		bytesPerSample = 2
	}
	expectedBytes := pm.Width * pm.Height * bytesPerSample
	if len(raw) != expectedBytes {
		return nil, apperr.Plane(name,
			fmt.Sprintf("%d bytes for %dx%d depth %d", expectedBytes, pm.Width, pm.Height, pm.BitDepth),
			fmt.Sprintf("%d bytes", len(raw)),
			"plane byte size mismatch")
	}
	data := make([]uint16, pm.Width*pm.Height)
	if bytesPerSample == 1 {
		for i, b := range raw {
			data[i] = uint16(b)
		}
	} else {
		for i := range data {
			data[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
		}
	}
	maxCode := uint32(1)<<uint(pm.BitDepth) - 1
	for i, v := range data {
		if uint32(v) > maxCode {
			return nil, apperr.Plane(name,
				fmt.Sprintf("sample <= %d", maxCode),
				fmt.Sprintf("sample[%d]=%d", i, v),
				"sample exceeds declared bit depth")
		}
	}
	return &Plane{
		Name: name, Width: pm.Width, Height: pm.Height, BitDepth: pm.BitDepth,
		QuantMin: qMin, QuantMax: qMax, Data: data,
	}, nil
}

func WritePlane(path string, p *Plane) error {
	bytesPerSample := 1
	if p.BitDepth > 8 {
		bytesPerSample = 2
	}
	raw := make([]byte, len(p.Data)*bytesPerSample)
	if bytesPerSample == 1 {
		for i, v := range p.Data {
			raw[i] = byte(v)
		}
	} else {
		for i, v := range p.Data {
			binary.LittleEndian.PutUint16(raw[i*2:i*2+2], v)
		}
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return apperr.IO("write plane "+path, err)
	}
	return nil
}

func HashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
