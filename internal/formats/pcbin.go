package formats

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"strata/internal/model"
)

const pcbinMagic = "PCZ1"

type pcbinQuantization struct {
	Kind string    `json:"kind"`
	Min  []float64 `json:"min"`
	Max  []float64 `json:"max"`
}

type pcbinHeader struct {
	Version            int               `json:"version"`
	PointCount         int               `json:"pointCount"`
	FieldOrder         []string          `json:"fieldOrder"`
	OptionalAttributes []string          `json:"optionalAttributes"`
	Quantization       pcbinQuantization `json:"quantization"`
	Bounds             []float64         `json:"bounds"`
	PayloadBytes       int               `json:"payloadBytes"`
	HasFaces           bool              `json:"hasFaces"`
	FaceCount          int               `json:"faceCount,omitempty"`
	Checksum           string            `json:"checksum"`
	Generator          string            `json:"generator"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

func loadPCBin(path string) (*model.Geometry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	if len(data) < 8 {
		return nil, model.NewError(model.EDECODE, "compact binary file is shorter than its header", "header", 0, nil)
	}
	if string(data[:4]) != pcbinMagic {
		return nil, model.NewError(model.EDECODE, "invalid PCBin magic", "header.magic", 0, nil)
	}
	if data[4] != 1 {
		return nil, model.NewError(model.EVER, fmt.Sprintf("unsupported PCBin version %d", data[4]), "header.version", 4, nil)
	}
	headerLen := int(binary.LittleEndian.Uint16(data[6:8]))
	if headerLen <= 0 || 8+headerLen > len(data) {
		return nil, model.NewError(model.EVER, "invalid compact header length", "header.headerLength", 6, map[string]any{"headerLength": headerLen})
	}
	var h pcbinHeader
	if err := json.Unmarshal(data[8:8+headerLen], &h); err != nil {
		return nil, model.NewError(model.EDECODE, "invalid compact JSON header: "+err.Error(), "header.json", 8, nil)
	}
	if h.Version != 1 {
		return nil, model.NewError(model.EVER, fmt.Sprintf("unsupported header version %d", h.Version), "header.version", 8, nil)
	}
	if h.PointCount < 0 {
		return nil, model.NewError(model.EVER, "negative point count", "header.pointCount", 8, nil)
	}
	for _, required := range []string{"x", "y", "z"} {
		found := false
		for _, f := range h.FieldOrder {
			if f == required {
				found = true
			}
		}
		if !found {
			return nil, model.NewError(model.EVER, "compact header is missing required field", "vertex."+required, 8, map[string]any{"fieldOrder": h.FieldOrder})
		}
	}
	bodyOffset := 8 + headerLen
	payload := data[bodyOffset:]
	if h.PayloadBytes != len(payload) {
		return nil, model.NewError(model.EVER, "compact payload length does not match header", "header.payloadBytes", 6, map[string]any{
			"declared": h.PayloadBytes, "actual": len(payload),
		})
	}
	if h.Checksum != "" {
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != h.Checksum {
			return nil, model.NewError(model.EDECODE, "compact payload checksum mismatch", "payload.checksum", int64(bodyOffset), map[string]any{
				"declared": h.Checksum, "actual": hex.EncodeToString(sum[:]),
			})
		}
	}
	recordSize := 0
	for _, f := range h.FieldOrder {
		switch f {
		case "x", "y", "z":
			recordSize += 2
		case "intensity", "red", "green", "blue":
			recordSize++
		default:
			return nil, model.NewError(model.EVER, "unsupported compact field "+f, "vertex."+f, 8, nil)
		}
	}
	if recordSize <= 0 || h.PointCount*recordSize > len(payload) {
		return nil, model.NewError(model.EVER, "compact binary vertex word length mismatch", "vertex.record", int64(bodyOffset), map[string]any{
			"pointCount": h.PointCount, "recordBytes": recordSize, "payloadBytes": len(payload),
		})
	}
	if !h.Quantization.KindValid() {
		return nil, model.NewError(model.EVER, "unsupported compact quantization", "header.quantization.kind", 8, nil)
	}
	if len(h.Quantization.Min) != 3 || len(h.Quantization.Max) != 3 {
		return nil, model.NewError(model.EVER, "compact quantization requires 3 min/max values", "header.quantization", 8, nil)
	}
	points := make([]model.Point, h.PointCount)
	for i := 0; i < h.PointCount; i++ {
		rec := payload[i*recordSize : (i+1)*recordSize]
		cursor := 0
		values := map[string]float64{}
		for _, f := range h.FieldOrder {
			switch f {
			case "x", "y", "z":
				q := int(binary.LittleEndian.Uint16(rec[cursor : cursor+2]))
				cursor += 2
				axis := map[string]int{"x": 0, "y": 1, "z": 2}[f]
				lo, hi := h.Quantization.Min[axis], h.Quantization.Max[axis]
				values[f] = lo + (hi-lo)*float64(q)/65535
			case "intensity":
				values[f] = float64(rec[cursor]) / 255
				cursor++
			case "red", "green", "blue":
				values[f] = float64(rec[cursor])
				cursor++
			}
		}
		points[i] = makePoint(values)
	}
	faceOffset := h.PointCount * recordSize
	var faces []model.Face
	if h.HasFaces {
		if h.FaceCount*12 > len(payload)-faceOffset {
			return nil, model.NewError(model.EVER, "compact face payload length mismatch", "face.record", int64(bodyOffset+faceOffset), nil)
		}
		faces = make([]model.Face, h.FaceCount)
		for i := 0; i < h.FaceCount; i++ {
			base := faceOffset + i*12
			a := int(binary.LittleEndian.Uint32(payload[base : base+4]))
			b := int(binary.LittleEndian.Uint32(payload[base+4 : base+8]))
			c := int(binary.LittleEndian.Uint32(payload[base+8 : base+12]))
			if !validIndex(a, len(points)) || !validIndex(b, len(points)) || !validIndex(c, len(points)) {
				return nil, model.NewError(model.EDECODE, "compact face index out of range", "face.vertex_indices", int64(bodyOffset+base), nil)
			}
			faces[i] = model.Face{A: a, B: b, C: c}
		}
	}
	model.CanonicalizePoints(points)
	info, _ := os.Stat(path)
	var size int64
	if info != nil {
		size = info.Size()
	}
	fields := make([]model.FieldSpec, 0, len(h.FieldOrder))
	for i, f := range h.FieldOrder {
		typ := "uint16"
		if f == "intensity" || f == "red" || f == "green" || f == "blue" {
			typ = "uint8"
		}
		fields = append(fields, model.FieldSpec{
			Index: i, Name: f, Type: typ, Path: "vertex." + f,
			Offset: int64(8 + headerLen), Optional: f != "x" && f != "y" && f != "z",
		})
	}
	meta := model.SourceMeta{
		VersionID: model.VersionPCBin, VersionNumber: 1, DisplayName: "PCBin compact v1",
		Format: "PCZ1 custom compact binary", Encoding: "custom-compact",
		File: path, FileBytes: size, PointCount: len(points), FaceCount: len(faces),
		Bounds: pointsBounds(points), FieldOrder: h.FieldOrder,
		OptionalAttributes: h.OptionalAttributes, Fields: fields,
		Quantization: model.Quantization{Kind: h.Quantization.Kind, Min: h.Quantization.Min, Max: h.Quantization.Max},
		HeaderMeta: model.HeaderMeta{
			HeaderBytes: int(headerLen + 8), RecordBytes: recordSize, PayloadBytes: len(payload), Checksum: h.Checksum,
			Generator: h.Generator, Custom: h.Metadata,
		},
		HasIntensity: contains(h.FieldOrder, "intensity"),
		HasColors:    contains(h.FieldOrder, "red"),
		HasNormals:   false,
	}
	if seed, ok := h.Metadata["seed"]; ok {
		fmt.Sscanf(seed, "%d", &meta.HeaderMeta.Seed)
	}
	return &model.Geometry{Points: points, Faces: faces, Meta: meta}, nil
}

func (q pcbinQuantization) KindValid() bool {
	return q.Kind == "bbox-u16"
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func writePCBin(path string, geometry *model.Geometry, mesh bool) (*LossyReport, error) {
	report := &LossyReport{}
	hasNormals := geometryHasNormals(geometry.Points)
	hasColors := false
	hasIntensity := false
	for _, p := range geometry.Points {
		if p.R != 0 || p.G != 0 || p.B != 0 {
			hasColors = true
		}
		if p.Intensity != 0 {
			hasIntensity = true
		}
	}
	b := pointsBounds(geometry.Points)
	if hasNormals {
		report.Warnings = append(report.Warnings, model.Warning{
			Code: model.ELOSSY, Severity: "warning",
			Message: "PCBin compact v1 has no normal slot; normals were omitted",
			Path:    "vertex.normals", Transform: "normals:downgraded:omitted",
		})
		report.DowngradeTrace = append(report.DowngradeTrace, "normals:downgraded:omitted")
	}
	if hasColors {
		report.Warnings = append(report.Warnings, model.Warning{
			Code: model.ELOSSY, Severity: "warning",
			Message: "PCBin compact v1 stores grayscale intensity rather than RGB",
			Path:    "vertex.colors", Transform: "colors:downgraded:grayscale-intensity",
		})
		report.DowngradeTrace = append(report.DowngradeTrace, "colors:downgraded:grayscale-intensity")
	}
	if len(geometry.Points) > 0 {
		report.Warnings = append(report.Warnings, model.Warning{
			Code: model.ELOSSY, Severity: "warning",
			Message: "positions are quantized to the source bounding box using unsigned 16-bit coordinates",
			Path:    "vertex.x", Transform: "position:float64->bbox-u16",
		})
		report.DowngradeTrace = append(report.DowngradeTrace, "position:float64->bbox-u16")
	}
	recordSize := 6
	fieldOrder := []string{"x", "y", "z"}
	optional := []string{}
	if hasIntensity {
		recordSize++
		fieldOrder = append(fieldOrder, "intensity")
		optional = append(optional, "intensity")
	}
	payloadLen := len(geometry.Points) * recordSize
	if mesh && len(geometry.Faces) > 0 {
		payloadLen += len(geometry.Faces) * 12
	}
	payload := make([]byte, 0, payloadLen)
	extent := [3]float64{b.Max.X - b.Min.X, b.Max.Y - b.Min.Y, b.Max.Z - b.Min.Z}
	axisValue := func(v float64, axis int) uint16 {
		if extent[axis] <= 1e-15 {
			return 0
		}
		lo := []float64{b.Min.X, b.Min.Y, b.Min.Z}[axis]
		q := (v - lo) / extent[axis] * 65535
		if q < 0 {
			q = 0
		}
		if q > 65535 {
			q = 65535
		}
		return uint16(math.Round(q))
	}
	for _, p := range geometry.Points {
		var rec [8]byte
		binary.LittleEndian.PutUint16(rec[0:2], axisValue(p.X, 0))
		binary.LittleEndian.PutUint16(rec[2:4], axisValue(p.Y, 1))
		binary.LittleEndian.PutUint16(rec[4:6], axisValue(p.Z, 2))
		cursor := 6
		if hasIntensity {
			rec[cursor] = uint8(math.Round(clamp(p.Intensity, 0, 1) * 255))
			cursor++
		}
		payload = append(payload, rec[:cursor]...)
	}
	if mesh && len(geometry.Faces) > 0 {
		for _, f := range geometry.Faces {
			var rec [12]byte
			binary.LittleEndian.PutUint32(rec[0:4], uint32(f.A))
			binary.LittleEndian.PutUint32(rec[4:8], uint32(f.B))
			binary.LittleEndian.PutUint32(rec[8:12], uint32(f.C))
			payload = append(payload, rec[:]...)
		}
	}
	sum := sha256.Sum256(payload)
	h := pcbinHeader{
		Version: 1, PointCount: len(geometry.Points), FieldOrder: fieldOrder,
		OptionalAttributes: optional,
		Quantization: pcbinQuantization{
			Kind: "bbox-u16",
			Min:  []float64{b.Min.X, b.Min.Y, b.Min.Z},
			Max:  []float64{b.Max.X, b.Max.Y, b.Max.Z},
		},
		Bounds:       []float64{b.Min.X, b.Min.Y, b.Min.Z, b.Max.X, b.Max.Y, b.Max.Z},
		PayloadBytes: len(payload), HasFaces: mesh && len(geometry.Faces) > 0, FaceCount: len(geometry.Faces),
		Checksum:  hex.EncodeToString(sum[:]),
		Generator: "pointcloud-studio/deterministic-encoder-v1",
		Metadata:  map[string]string{"sourceVersion": geometry.Meta.VersionID},
	}
	if geometry.Meta.HeaderMeta.Seed > 0 {
		h.Metadata["seed"] = fmt.Sprintf("%d", geometry.Meta.HeaderMeta.Seed)
	}
	if geometry.Meta.HeaderMeta.Generator != "" {
		h.Metadata["generator"] = geometry.Meta.HeaderMeta.Generator
	}
	h.Metadata["fieldLayout"] = strings.Join(fieldOrder, ",")
	if mesh {
		h.Generator = "pointcloud-studio/deterministic-mesh-encoder-v1"
		h.Metadata["kind"] = "triangle-mesh"
	}
	headerJSON, err := json.Marshal(h)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "header.json", 0, nil)
	}
	if len(headerJSON) > 65535 {
		return nil, model.NewError(model.EVER, "compact header too large", "header", 0, nil)
	}
	var out []byte
	out = append(out, pcbinMagic...)
	out = append(out, 1, 0)
	var hlen [2]byte
	binary.LittleEndian.PutUint16(hlen[:], uint16(len(headerJSON)))
	out = append(out, hlen[:]...)
	out = append(out, headerJSON...)
	out = append(out, payload...)
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	readback, err := loadPCBin(path)
	if err != nil {
		return nil, err
	}
	report.ReadbackPoints = len(readback.Points)
	report.ReadbackFaces = len(readback.Faces)
	return report, nil
}
