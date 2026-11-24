package formats

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"strata/internal/model"
)

type plyProperty struct {
	Name     string
	Type     string
	IsList   bool
	CountTyp string
	ItemTyp  string
	Offset   int
	Size     int
	Index    int
}

type plyElement struct {
	Name       string
	Count      int
	Properties []plyProperty
	RecordSize int
	HeaderOff  int64
}

type plyHeader struct {
	Format      string
	Version     string
	FormatOff   int64
	EndOff      int64
	HeaderBytes int64
	Elements    []plyElement
	Comments    []string
}

var plyTypeSizes = map[string]int{
	"char": 1, "int8": 1, "uchar": 1, "uint8": 1,
	"short": 2, "int16": 2, "ushort": 2, "uint16": 2,
	"int": 4, "int32": 4, "uint": 4, "uint32": 4,
	"float": 4, "float32": 4, "double": 8, "float64": 8,
}

func parsePLYHeader(path string) (*plyHeader, []byte, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	h := &plyHeader{}
	var offset int64
	var current *plyElement
	first := true
	for {
		line, err := reader.ReadString('\n')
		lineOffset := offset
		offset += int64(len(line))
		if err != nil && len(line) == 0 {
			return nil, nil, 0, model.NewError(model.EDECODE, "PLY header is incomplete", "header", offset, nil)
		}
		text := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if first {
			if text != "ply" {
				return nil, nil, 0, model.NewError(model.EDECODE, "missing PLY magic", "header.magic", 0, nil)
			}
			first = false
		} else if text == "end_header" {
			h.EndOff = offset
			h.HeaderBytes = offset
			break
		} else if strings.HasPrefix(text, "format ") {
			parts := strings.Fields(text)
			if len(parts) < 3 {
				return nil, nil, 0, model.NewError(model.EVER, "malformed format declaration", "header.format", lineOffset, nil)
			}
			h.Format = parts[1]
			h.Version = parts[2]
			h.FormatOff = lineOffset
		} else if strings.HasPrefix(text, "comment ") {
			h.Comments = append(h.Comments, strings.TrimPrefix(text, "comment "))
		} else if strings.HasPrefix(text, "element ") {
			parts := strings.Fields(text)
			if len(parts) != 3 {
				return nil, nil, 0, model.NewError(model.EDECODE, "malformed element declaration", "header.element", lineOffset, nil)
			}
			count, err := strconv.Atoi(parts[2])
			if err != nil || count < 0 {
				return nil, nil, 0, model.NewError(model.EVER, "invalid element count", "header.element.count", lineOffset, nil)
			}
			h.Elements = append(h.Elements, plyElement{Name: parts[1], Count: count, HeaderOff: lineOffset})
			current = &h.Elements[len(h.Elements)-1]
		} else if strings.HasPrefix(text, "property ") {
			if current == nil {
				return nil, nil, 0, model.NewError(model.EDECODE, "property outside element", "header.property", lineOffset, nil)
			}
			parts := strings.Fields(text)
			p := plyProperty{Index: len(current.Properties)}
			if len(parts) == 5 && parts[1] == "list" {
				p.IsList = true
				p.CountTyp = parts[2]
				p.ItemTyp = parts[3]
				p.Name = parts[4]
				if _, ok := plyTypeSizes[p.CountTyp]; !ok {
					return nil, nil, 0, model.NewError(model.EVER, "unknown list count type", "header.property."+p.Name, lineOffset, nil)
				}
				if _, ok := plyTypeSizes[p.ItemTyp]; !ok {
					return nil, nil, 0, model.NewError(model.EVER, "unknown list item type", "header.property."+p.Name, lineOffset, nil)
				}
			} else if len(parts) == 3 {
				p.Type = parts[1]
				p.Name = parts[2]
				size, ok := plyTypeSizes[p.Type]
				if !ok {
					return nil, nil, 0, model.NewError(model.EVER, "unknown property type "+p.Type, "header.property."+p.Name, lineOffset, nil)
				}
				p.Offset = current.RecordSize
				p.Size = size
				current.RecordSize += size
			} else {
				return nil, nil, 0, model.NewError(model.EDECODE, "malformed property declaration", "header.property", lineOffset, nil)
			}
			current.Properties = append(current.Properties, p)
		}
		if err != nil {
			return nil, nil, 0, model.NewError(model.EDECODE, "PLY header is incomplete", "header", offset, nil)
		}
	}
	if h.Format == "" {
		return nil, nil, 0, model.NewError(model.EVER, "missing format declaration", "header.format", 0, nil)
	}
	if h.Version != "1.0" {
		return nil, nil, 0, model.NewError(model.EVER, "unsupported PLY version "+h.Version, "header.format.version", h.FormatOff, nil)
	}
	if h.Format != "ascii" && h.Format != "binary_little_endian" {
		return nil, nil, 0, model.NewError(model.EVER, "unsupported PLY format "+h.Format, "header.format", h.FormatOff, nil)
	}
	return h, data, offset, nil
}

func loadPLY(path, versionID string) (*model.Geometry, error) {
	h, data, dataStart, err := parsePLYHeader(path)
	if err != nil {
		return nil, err
	}
	wantFormat := "ascii"
	if versionID == model.VersionPLYBinary {
		wantFormat = "binary_little_endian"
	}
	if h.Format != wantFormat {
		return nil, model.NewError(model.EVER,
			fmt.Sprintf("version %s requires %s, file declares %s", versionID, wantFormat, h.Format),
			"header.format", h.FormatOff, nil)
	}
	var vertexEl *plyElement
	var faceEl *plyElement
	for i := range h.Elements {
		switch h.Elements[i].Name {
		case "vertex":
			vertexEl = &h.Elements[i]
		case "face":
			faceEl = &h.Elements[i]
		}
	}
	if vertexEl == nil {
		return nil, model.NewError(model.EVER, "missing vertex element", "vertex", dataStart, nil)
	}
	fieldMap := map[string]plyProperty{}
	required := []string{"x", "y", "z"}
	for _, name := range required {
		found := false
		for _, p := range vertexEl.Properties {
			if p.Name == name && !p.IsList {
				found = true
			}
		}
		if !found {
			return nil, model.NewError(model.EVER, "missing required vertex field", "vertex."+name, dataStart, map[string]any{
				"required": required,
			})
		}
	}
	for _, p := range vertexEl.Properties {
		fieldMap[p.Name] = p
	}
	points := make([]PointList, 0)
	_ = points
	cloudPoints := make([]model.Point, 0, vertexEl.Count)
	var faces []model.Face
	if h.Format == "ascii" {
		lines := strings.Split(string(data[dataStart:]), "\n")
		lineCursor := dataStart
		tokenIndex := 0
		for tokenIndex < len(lines) && len(cloudPoints) < vertexEl.Count {
			line := strings.TrimSpace(lines[tokenIndex])
			lineCursor += int64(len(lines[tokenIndex]) + 1)
			tokenIndex++
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < len(vertexEl.Properties) {
				return nil, model.NewError(model.EDECODE, "vertex row has too few fields", "vertex.record", lineCursor-int64(len(line))-1, map[string]any{
					"expected": len(vertexEl.Properties), "actual": len(fields),
				})
			}
			values := map[string]float64{}
			for i, p := range vertexEl.Properties {
				v, err := strconv.ParseFloat(fields[i], 64)
				if err != nil {
					return nil, model.NewError(model.EDECODE, "invalid numeric vertex token", "vertex."+p.Name, lineCursor-int64(len(line))-1, nil)
				}
				values[p.Name] = v
			}
			cloudPoints = append(cloudPoints, makePoint(values))
		}
		if len(cloudPoints) != vertexEl.Count {
			return nil, model.NewError(model.EDECODE, "vertex element is truncated", "vertex", int64(len(data)), map[string]any{
				"expected": vertexEl.Count, "actual": len(cloudPoints),
			})
		}
		if faceEl != nil {
			for tokenIndex < len(lines) && len(faces) < faceEl.Count {
				line := strings.TrimSpace(lines[tokenIndex])
				lineCursor += int64(len(lines[tokenIndex]) + 1)
				tokenIndex++
				if line == "" {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) < 4 {
					return nil, model.NewError(model.EDECODE, "invalid face row", "face.record", lineCursor-int64(len(line))-1, nil)
				}
				n, _ := strconv.Atoi(fields[0])
				if n != 3 || len(fields) < 4 {
					return nil, model.NewError(model.EVER, "only triangle faces are supported", "face.vertex_indices", lineCursor-int64(len(line))-1, nil)
				}
				a, _ := strconv.Atoi(fields[1])
				b, _ := strconv.Atoi(fields[2])
				c, _ := strconv.Atoi(fields[3])
				if !validIndex(a, len(cloudPoints)) || !validIndex(b, len(cloudPoints)) || !validIndex(c, len(cloudPoints)) {
					return nil, model.NewError(model.EDECODE, "face index out of range", "face.vertex_indices", lineCursor-int64(len(line))-1, nil)
				}
				faces = append(faces, model.Face{A: a, B: b, C: c})
			}
			if len(faces) != faceEl.Count {
				return nil, model.NewError(model.EDECODE, "face element is truncated", "face", int64(len(data)), nil)
			}
		}
	} else {
		recordSize := vertexEl.RecordSize
		if recordSize <= 0 {
			return nil, model.NewError(model.EVER, "binary vertex record has zero width", "vertex.record", dataStart, nil)
		}
		need := int64(vertexEl.Count * recordSize)
		if int64(len(data))-dataStart < need {
			return nil, model.NewError(model.EVER, "binary vertex payload is shorter than declared", "vertex.record", dataStart+int64(len(data))-dataStart, map[string]any{
				"expectedBytes": need, "actualBytes": int64(len(data)) - dataStart,
			})
		}
		if faceEl == nil && int64(len(data))-dataStart != need {
			return nil, model.NewError(model.EVER, "binary vertex payload length does not match declared word layout", "vertex.record", dataStart+need, map[string]any{
				"expectedBytes": need, "actualBytes": int64(len(data)) - dataStart,
			})
		}
		body := data[dataStart : dataStart+need]
		if faceEl == nil && len(body)%recordSize != 0 {
			return nil, model.NewError(model.EVER, "binary record length does not divide payload", "vertex.record", dataStart+int64(len(body)-(len(body)%recordSize)), map[string]any{
				"recordBytes": recordSize,
			})
		}
		for i := 0; i < vertexEl.Count; i++ {
			rec := body[i*recordSize : (i+1)*recordSize]
			values := map[string]float64{}
			for _, p := range vertexEl.Properties {
				if p.IsList {
					return nil, model.NewError(model.EVER, "list property is not valid in vertex record", "vertex."+p.Name, dataStart, nil)
				}
				v, err := decodePLYScalar(p.Type, rec[p.Offset:p.Offset+p.Size])
				if err != nil {
					return nil, model.NewError(model.EDECODE, err.Error(), "vertex."+p.Name, dataStart+int64(i*recordSize+p.Offset), nil)
				}
				values[p.Name] = v
			}
			cloudPoints = append(cloudPoints, makePoint(values))
		}
		if faceEl != nil {
			cursor := dataStart + need
			for i := 0; i < faceEl.Count; i++ {
				if cursor >= int64(len(data)) {
					return nil, model.NewError(model.EDECODE, "face element is truncated", "face.record", cursor, nil)
				}
				countSize := 1
				countType := "uchar"
				itemType := "int"
				for _, p := range faceEl.Properties {
					if p.IsList {
						countSize = plyTypeSizes[p.CountTyp]
						countType = p.CountTyp
						itemType = p.ItemTyp
						break
					}
				}
				if cursor+int64(countSize) > int64(len(data)) {
					return nil, model.NewError(model.EDECODE, "face list count is truncated", "face.record.count", cursor, nil)
				}
				count, _ := decodePLYScalar(countType, data[cursor:cursor+int64(countSize)])
				cursor += int64(countSize)
				if int(count) != 3 {
					return nil, model.NewError(model.EVER, "only triangle lists are supported", "face.vertex_indices", cursor, nil)
				}
				itemSize := plyTypeSizes[itemType]
				if cursor+int64(3*itemSize) > int64(len(data)) {
					return nil, model.NewError(model.EVER, "binary face record length is invalid", "face.record", cursor, nil)
				}
				idx := make([]int, 3)
				for j := 0; j < 3; j++ {
					v, _ := decodePLYScalar(itemType, data[cursor:cursor+int64(itemSize)])
					idx[j] = int(v)
					cursor += int64(itemSize)
					if !validIndex(idx[j], len(cloudPoints)) {
						return nil, model.NewError(model.EDECODE, "face index out of range", "face.vertex_indices", cursor, nil)
					}
				}
				faces = append(faces, model.Face{A: idx[0], B: idx[1], C: idx[2]})
			}
			if cursor != int64(len(data)) {
				return nil, model.NewError(model.EVER, "binary payload has trailing bytes outside the declared elements", "payload", cursor, map[string]any{
					"trailingBytes": int64(len(data)) - cursor,
				})
			}
		}
	}
	model.CanonicalizePoints(cloudPoints)
	meta, err := plyMetadata(path, h, vertexEl, len(cloudPoints), len(faces))
	if err != nil {
		return nil, err
	}
	meta.Bounds = pointsBounds(cloudPoints)
	return &model.Geometry{Points: cloudPoints, Faces: faces, Meta: meta}, nil
}

// PointList is kept as a local alias to make the ascii/binary parser's
// allocation behavior explicit without exposing parser internals.
type PointList = model.Point

func makePoint(v map[string]float64) model.Point {
	return model.Point{
		X: v["x"], Y: v["y"], Z: v["z"],
		NX: v["nx"], NY: v["ny"], NZ: v["nz"],
		R: uint8(clamp(v["red"], 0, 255)), G: uint8(clamp(v["green"], 0, 255)), B: uint8(clamp(v["blue"], 0, 255)),
		Intensity: v["intensity"],
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func validIndex(i, n int) bool {
	return i >= 0 && i < n
}

func decodePLYScalar(typ string, data []byte) (float64, error) {
	switch typ {
	case "char", "int8":
		return float64(int8(data[0])), nil
	case "uchar", "uint8":
		return float64(data[0]), nil
	case "short", "int16":
		return float64(int16(binary.LittleEndian.Uint16(data))), nil
	case "ushort", "uint16":
		return float64(binary.LittleEndian.Uint16(data)), nil
	case "int", "int32":
		return float64(int32(binary.LittleEndian.Uint32(data))), nil
	case "uint", "uint32":
		return float64(binary.LittleEndian.Uint32(data)), nil
	case "float", "float32":
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(data))), nil
	case "double", "float64":
		return math.Float64frombits(binary.LittleEndian.Uint64(data)), nil
	default:
		return 0, fmt.Errorf("unknown scalar type %s", typ)
	}
}

func plyMetadata(path string, h *plyHeader, vertexEl *plyElement, points, faces int) (model.SourceMeta, error) {
	versionID := model.VersionPLYASCII
	display := "PLY ASCII v1"
	encoding := "ascii"
	if h.Format == "binary_little_endian" {
		versionID = model.VersionPLYBinary
		display = "PLY binary LE v1"
		encoding = "binary_little_endian"
	}
	info, _ := os.Stat(path)
	fields := make([]model.FieldSpec, 0, len(vertexEl.Properties))
	order := make([]string, 0, len(vertexEl.Properties))
	hasNormals, hasColors, hasIntensity := false, false, false
	for _, p := range vertexEl.Properties {
		fields = append(fields, model.FieldSpec{
			Index: p.Index, Name: p.Name, Type: p.Type,
			Path: "vertex." + p.Name, Offset: h.HeaderBytes + int64(p.Offset),
			Optional: p.Name != "x" && p.Name != "y" && p.Name != "z",
		})
		order = append(order, p.Name)
		switch p.Name {
		case "nx", "ny", "nz":
			hasNormals = true
		case "red", "green", "blue":
			hasColors = true
		case "intensity":
			hasIntensity = true
		}
	}
	optional := []string{}
	if hasNormals {
		optional = append(optional, "normals")
	}
	if hasColors {
		optional = append(optional, "colors")
	}
	if hasIntensity {
		optional = append(optional, "intensity")
	}
	seed := 0
	for _, c := range h.Comments {
		if strings.HasPrefix(c, "seed ") {
			seed, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(c, "seed ")))
		}
	}
	var size int64
	if info != nil {
		size = info.Size()
	}
	quantization := model.Quantization{Kind: "text-float"}
	if h.Format == "binary_little_endian" {
		quantization = model.Quantization{Kind: "float32"}
	}
	return model.SourceMeta{
		VersionID: versionID, VersionNumber: 1, DisplayName: display,
		Format: "PLY " + h.Format + " " + h.Version, Encoding: encoding,
		File: path, FileBytes: size, PointCount: points, FaceCount: faces,
		FieldOrder: order, OptionalAttributes: optional, Fields: fields,
		Quantization: quantization,
		HeaderMeta: model.HeaderMeta{
			Seed: seed, Generator: strings.Join(h.Comments, " | "),
			HeaderBytes: int(h.HeaderBytes), RecordBytes: vertexEl.RecordSize,
			PayloadBytes: int(size) - int(h.HeaderBytes),
		},
		HasNormals: hasNormals, HasColors: hasColors, HasIntensity: hasIntensity,
	}, nil
}

func pointsBounds(points []model.Point) model.Bounds {
	b := model.EmptyBounds()
	for _, p := range points {
		b.Include(p.Position())
	}
	return b
}

func writePLY(path, versionID string, geometry *model.Geometry, mesh bool) (*LossyReport, error) {
	report := &LossyReport{}
	hasNormals := false
	hasColors := false
	hasIntensity := false
	for _, p := range geometry.Points {
		if p.HasNormal() {
			hasNormals = true
		}
		if p.R != 0 || p.G != 0 || p.B != 0 {
			hasColors = true
		}
		if p.Intensity != 0 {
			hasIntensity = true
		}
	}
	ascii := versionID == model.VersionPLYASCII
	var buf bytes.Buffer
	buf.WriteString("ply\n")
	if ascii {
		buf.WriteString("format ascii 1.0\n")
	} else {
		buf.WriteString("format binary_little_endian 1.0\n")
	}
	buf.WriteString("comment PointCloud Studio deterministic export\n")
	buf.WriteString("comment target_version " + versionID + "\n")
	if geometry.Meta.HeaderMeta.Seed > 0 {
		fmt.Fprintf(&buf, "comment seed %d\n", geometry.Meta.HeaderMeta.Seed)
	}
	if geometry.Meta.HeaderMeta.Generator != "" {
		buf.WriteString("comment generator " + strings.ReplaceAll(geometry.Meta.HeaderMeta.Generator, "\n", " ") + "\n")
	}
	if ascii {
		buf.WriteString("comment record_layout text-float64\n")
	} else {
		buf.WriteString("comment record_layout z,x,y:float32;rgb:uint8;intensity:float32\n")
	}
	fmt.Fprintf(&buf, "element vertex %d\n", len(geometry.Points))
	if ascii {
		buf.WriteString("property float x\nproperty float y\nproperty float z\n")
		if hasNormals {
			buf.WriteString("property float nx\nproperty float ny\nproperty float nz\n")
		}
	} else {
		buf.WriteString("property float z\nproperty float x\nproperty float y\n")
		if hasNormals {
			// The compact v1 binary mapping intentionally has no normal slots.
			report.Warnings = append(report.Warnings, model.Warning{
				Code: model.ELOSSY, Severity: "warning",
				Message: "target version has no normal attributes; normals were omitted",
				Path:    "vertex.normals", Transform: "normals:downgraded:omitted",
			})
			report.DowngradeTrace = append(report.DowngradeTrace, "normals:downgraded:omitted")
			hasNormals = false
		}
	}
	if hasColors {
		buf.WriteString("property uchar red\nproperty uchar green\nproperty uchar blue\n")
	}
	if hasIntensity {
		buf.WriteString("property float intensity\n")
	}
	if report.DowngradeTrace == nil && !ascii {
		if geometryHasNormals(geometry.Points) {
			// covered above
		}
	}
	if mesh && len(geometry.Faces) > 0 {
		fmt.Fprintf(&buf, "element face %d\n", len(geometry.Faces))
		buf.WriteString("property list uchar int vertex_indices\n")
	}
	buf.WriteString("end_header\n")
	if ascii {
		for _, p := range geometry.Points {
			fmt.Fprintf(&buf, "%s %s %s", floatText(p.X), floatText(p.Y), floatText(p.Z))
			if hasNormals {
				fmt.Fprintf(&buf, " %s %s %s", floatText(p.NX), floatText(p.NY), floatText(p.NZ))
			}
			if hasColors {
				fmt.Fprintf(&buf, " %d %d %d", p.R, p.G, p.B)
			}
			if hasIntensity {
				fmt.Fprintf(&buf, " %s", floatText(p.Intensity))
			}
			buf.WriteByte('\n')
		}
		for _, f := range geometry.Faces {
			fmt.Fprintf(&buf, "3 %d %d %d\n", f.A, f.B, f.C)
		}
	} else {
		if versionID == model.VersionPLYBinary {
			report.DowngradeTrace = append(report.DowngradeTrace, "position:float64->float32")
			report.Warnings = append(report.Warnings, model.Warning{
				Code: model.ELOSSY, Severity: "warning",
				Message: "binary v1 stores positions as float32",
				Path:    "vertex.x", Transform: "position:float64->float32",
			})
		}
		for _, p := range geometry.Points {
			writeF32(&buf, p.Z)
			writeF32(&buf, p.X)
			writeF32(&buf, p.Y)
			if hasColors {
				buf.Write([]byte{p.R, p.G, p.B})
			}
			if hasIntensity {
				writeF32(&buf, p.Intensity)
			}
		}
		if mesh && len(geometry.Faces) > 0 {
			for _, f := range geometry.Faces {
				buf.WriteByte(3)
				_ = binary.Write(&buf, binary.LittleEndian, int32(f.A))
				_ = binary.Write(&buf, binary.LittleEndian, int32(f.B))
				_ = binary.Write(&buf, binary.LittleEndian, int32(f.C))
			}
		}
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
	}
	readback, err := loadPLY(path, versionID)
	if err != nil {
		return nil, err
	}
	report.ReadbackPoints = len(readback.Points)
	report.ReadbackFaces = len(readback.Faces)
	return report, nil
}

func geometryHasNormals(points []model.Point) bool {
	for _, p := range points {
		if p.HasNormal() {
			return true
		}
	}
	return false
}

func writeF32(w io.Writer, v float64) {
	_ = binary.Write(w, binary.LittleEndian, float32(v))
}

func floatText(v float64) string {
	return strconv.FormatFloat(v, 'g', 17, 64)
}

func dirOf(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return "."
	}
	return path[:i]
}
