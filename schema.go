package maskqc

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

const (
	SchemaV1 = "qc.strokes.v1"
	SchemaV2 = "qc.strokes.v2"
	SchemaV3 = "qc.project.v3"
)

var schemaAliases = map[string]string{
	"qc.v1": SchemaV1, "qc.strokes.v1": SchemaV1, "v1": SchemaV1, "1.0": SchemaV1, "1.0.0": SchemaV1,
	"qc.v2": SchemaV2, "qc.strokes.v2": SchemaV2, "v2": SchemaV2, "2.0": SchemaV2, "2.0.0": SchemaV2,
	"qc.v3": SchemaV3, "qc.project.v3": SchemaV3, "v3": SchemaV3, "3.0": SchemaV3, "3.0.0": SchemaV3,
}

func MarshalProjectVersion(p *Project, version string) ([]byte, error) {
	canonical, ok := schemaAliases[version]
	if !ok {
		return nil, schemaError("$.schema", "unknown schema version %q", version)
	}
	var doc map[string]any
	switch canonical {
	case SchemaV1:
		doc = marshalV1(p)
	case SchemaV2:
		doc = marshalV2(p)
	default:
		doc = marshalV3(p)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func marshalV1(p *Project) map[string]any {
	strokes := make([]map[string]any, 0, len(p.Strokes))
	for _, s := range p.Strokes {
		strokes = append(strokes, map[string]any{
			"id":                s.ID,
			"ord":               s.Seq,
			"process":           s.Priority,
			"brush_mode":        opToV1(s.Op),
			"from_px":           []int{s.Start.X, s.Start.Y},
			"to_px":             []int{s.End.X, s.End.Y},
			"coverage_permille": s.WeightBP / 10,
			"hardness_percent":  s.HardnessPermille / 10,
			"radius_px":         s.Radius,
			"direction_code":    directionToV1(s.DeclaredDirection),
		})
	}
	return map[string]any{
		"schema":  SchemaV1,
		"icon_id": p.ID,
		"seed":    p.Seed,
		"canvas":  map[string]int{"w": p.Width, "h": p.Height},
		"assets": map[string]string{
			"image": "image.png", "candidate": "candidate.png", "reference": "reference.png",
		},
		"strokes": strokes,
		"truth":   marshalTruthV1(p.Truth),
	}
}

func marshalV2(p *Project) map[string]any {
	strokes := make([]map[string]any, 0, len(p.Strokes))
	for _, s := range p.Strokes {
		startOff, _ := OffsetFromAbsolute(s.Start, s.Seq, fmt.Sprintf("strokes.%s.start_offset", s.ID), nil)
		endOff, _ := OffsetFromAbsolute(s.End, s.Seq, fmt.Sprintf("strokes.%s.end_offset", s.ID), nil)
		strokes = append(strokes, map[string]any{
			"stroke_id":   s.ID,
			"stroke_no":   s.Seq,
			"priority":    s.Priority,
			"mode":        opToV2(s.Op),
			"start_off":   []int{startOff.X, startOff.Y},
			"end_off":     []int{endOff.X, endOff.Y},
			"coverage_bp": s.WeightBP,
			"hardness_pm": s.HardnessPermille,
			"radius":      s.Radius,
			"direction":   directionToV2(s.DeclaredDirection),
		})
	}
	return map[string]any{
		"schema": SchemaV2,
		"meta":   map[string]any{"icon": p.ID, "seed": p.Seed},
		"width":  p.Width,
		"height": p.Height,
		"coordinate_rule": map[string]string{
			"expression": "seq_derived_offset",
			"anchor":     "anchor(seq)=(3*seq,2*seq)",
		},
		"layers":  map[string]string{"photo": "image.png", "candidate_mask": "candidate.png", "truth_mask": "reference.png"},
		"strokes": strokes,
		"truth":   marshalTruthV2(p.Truth),
	}
}

func marshalV3(p *Project) map[string]any {
	strokes := make([]map[string]any, 0, len(p.Strokes))
	for _, s := range p.Strokes {
		strokes = append(strokes, map[string]any{
			"id":        s.ID,
			"sequence":  s.Seq,
			"priority":  s.Priority,
			"composite": map[string]string{"operator": defaultOp(s.Op), "role": defaultRole(s.Role)},
			"geometry": map[string]any{
				"expression": "absolute_pixels",
				"start":      map[string]int{"x": s.Start.X, "y": s.Start.Y},
				"end":        map[string]int{"x": s.End.X, "y": s.End.Y},
			},
			"coverage":  map[string]int{"basis": 10000, "value": s.WeightBP},
			"hardness":  map[string]int{"basis": 1000, "value": s.HardnessPermille},
			"radius_px": s.Radius,
			"direction": directionToV3(s.DeclaredDirection),
		})
	}
	return map[string]any{
		"schema":  SchemaV3,
		"project": map[string]any{"id": p.ID, "seed": p.Seed},
		"canvas":  map[string]int{"width": p.Width, "height": p.Height},
		"assets": map[string]string{
			"image": "image.png", "candidate": "candidate.png", "reference": "reference.png",
		},
		"strokes": strokes,
		"truth":   marshalTruthV3(p.Truth),
	}
}

func marshalTruthV1(t Truth) map[string]any {
	domains := make([]map[string]any, 0, len(t.Domains))
	for _, d := range t.Domains {
		domains = append(domains, map[string]any{
			"index": d.DomainID, "cx1000": d.CentroidX1000, "cy1000": d.CentroidY1000,
			"area": d.Area, "bbox": bboxV1(d.BBox), "holes": d.Holes,
			"band_px": d.BandWidthPixels, "hole_tol": d.HoleTolerance, "band_tol_pm": d.BandTolerancePM,
		})
	}
	segments := make([]map[string]any, 0, len(t.Segments))
	for _, s := range t.Segments {
		segments = append(segments, map[string]any{
			"id": s.SegmentID, "domain_index": s.DomainID, "stroke": s.StrokeID,
			"dir_code": directionToV1(s.Class), "nx": s.NormalX, "ny": s.NormalY,
			"points": s.PointCount, "bbox": bboxV1(s.BBox),
		})
	}
	return map[string]any{"domains": domains, "boundary_segments": segments}
}

func bboxV1(b BBox) []int { return []int{b.MinX, b.MinY, b.MaxX, b.MaxY} }

func marshalTruthV2(t Truth) map[string]any {
	b, _ := json.Marshal(marshalTruthV3(t))
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func marshalTruthV3(t Truth) map[string]any {
	return map[string]any{"domains": t.Domains, "segments": t.Segments}
}

func defaultOp(op string) string {
	if op == "" {
		return StrokeOpOver
	}
	return op
}

func defaultRole(role string) string {
	if role == "" {
		return RolePaint
	}
	return role
}

func opToV1(op string) int {
	if op == StrokeOpErase {
		return 1
	}
	return 0
}

func opToV2(op string) string {
	if op == StrokeOpErase {
		return "DST_OUT"
	}
	return "SRC_OVER"
}

func directionToV1(c DirectionClass) int {
	switch c {
	case DirForward:
		return 1
	case DirReverse:
		return 2
	case DirLeft:
		return 3
	case DirRight:
		return 4
	default:
		return 0
	}
}

func directionFromV1(code int) DirectionClass {
	switch code {
	case 1:
		return DirForward
	case 2:
		return DirReverse
	case 3:
		return DirLeft
	case 4:
		return DirRight
	default:
		return DirUnknown
	}
}

func directionToV2(c DirectionClass) string {
	switch c {
	case DirForward:
		return "FWD"
	case DirReverse:
		return "REV"
	case DirLeft:
		return "LFT"
	case DirRight:
		return "RGT"
	default:
		return "UNK"
	}
}

func directionFromV2(code string) DirectionClass {
	switch code {
	case "FWD", "FORWARD":
		return DirForward
	case "REV", "REVERSE":
		return DirReverse
	case "LFT", "LEFT":
		return DirLeft
	case "RGT", "RIGHT":
		return DirRight
	default:
		return DirUnknown
	}
}

func directionToV3(c DirectionClass) string {
	if c == "" {
		return string(DirUnknown)
	}
	return string(c)
}

func directionFromV3(code string) DirectionClass {
	switch DirectionClass(code) {
	case DirForward, DirReverse, DirLeft, DirRight, DirUnknown:
		return DirectionClass(code)
	default:
		return DirUnknown
	}
}

// LoadProjectFile loads one versioned project JSON and resolves PNG assets
// relative to its directory.
func LoadProjectFile(path string) (*Project, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return LoadProjectJSON(data, filepath.Dir(path))
}

func LoadProjectJSON(data []byte, assetDir string) (*Project, error) {
	var head struct {
		Schema  string `json:"schema"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, schemaError("$", "invalid JSON: %v", err)
	}
	schemaKey := head.Schema
	if schemaKey == "" {
		schemaKey = head.Version
	}
	if schemaKey == "" {
		return nil, schemaError("$.schema", "missing required schema/version field")
	}
	canonical, ok := schemaAliases[schemaKey]
	if !ok {
		return nil, schemaError("$.schema", "unknown schema version %q", schemaKey)
	}
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	report := &ConversionReport{}
	var p *Project
	switch canonical {
	case SchemaV1:
		p, err = loadV1(root, report)
	case SchemaV2:
		p, err = loadV2(root, report)
	default:
		p, err = loadV3(root, report)
	}
	if err != nil {
		return nil, err
	}
	p.SchemaVersion = canonical
	p.AssetDir = assetDir
	p.ConvertReport = report
	if err := loadAssets(p, root, canonical, assetDir); err != nil {
		return nil, err
	}
	if err := validateLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func validateLoadedProject(p *Project) error {
	if p.ID == "" {
		return schemaError("$.id", "project id is required")
	}
	if p.Width <= 0 || p.Height <= 0 || p.Width > 4096 || p.Height > 4096 {
		return schemaError("$.canvas", "canvas dimensions must be between 1 and 4096")
	}
	if p.Candidate == nil {
		return schemaError("$.assets.candidate", "candidate mask asset is required")
	}
	if p.Truth.Reference == nil {
		return schemaError("$.assets.reference", "reference mask asset is required")
	}
	if p.Candidate.Width != p.Width || p.Candidate.Height != p.Height {
		return schemaError("$.assets.candidate", "candidate dimensions %dx%d do not match canvas %dx%d", p.Candidate.Width, p.Candidate.Height, p.Width, p.Height)
	}
	if p.Truth.Reference.Width != p.Width || p.Truth.Reference.Height != p.Height {
		return schemaError("$.assets.reference", "reference dimensions %dx%d do not match canvas %dx%d", p.Truth.Reference.Width, p.Truth.Reference.Height, p.Width, p.Height)
	}
	for i := range p.Strokes {
		if err := ValidateStroke(p.Strokes[i]); err != nil {
			return err
		}
	}
	return nil
}

func loadAssets(p *Project, root rawObject, schema, dir string) error {
	assetPath, err := assetRelPath(root, schema)
	if err != nil {
		return err
	}
	if imageRel := assetPath["image"]; imageRel != "" {
		img, err := ReadImagePNG(filepath.Join(dir, imageRel))
		if err != nil {
			return schemaError("$.assets.image", "read image asset: %v", err)
		}
		p.Image = img
	}
	cand, err := ReadMaskPNG(filepath.Join(dir, assetPath["candidate"]))
	if err != nil {
		return schemaError("$.assets.candidate", "read candidate asset: %v", err)
	}
	p.Candidate = cand
	ref, err := ReadMaskPNG(filepath.Join(dir, assetPath["reference"]))
	if err != nil {
		return schemaError("$.assets.reference", "read reference asset: %v", err)
	}
	p.Truth.Reference = ref
	return nil
}

func assetRelPath(root rawObject, schema string) (map[string]string, error) {
	key := "assets"
	path := "$.assets"
	if schema == SchemaV2 {
		key = "layers"
		path = "$.layers"
	}
	obj, err := root.object("$", key)
	if err != nil {
		return nil, err
	}
	image, _ := obj.string(path, "photo")
	if image == "" {
		image, _ = obj.string(path, "image")
	}
	candidate, err := obj.string(path, "candidate")
	if err != nil {
		candidate, err = obj.string("$.layers", "candidate_mask")
		if err != nil {
			return nil, err
		}
	}
	reference, err := obj.string(path, "reference")
	if err != nil {
		reference, err = obj.string("$.layers", "truth_mask")
		if err != nil {
			return nil, err
		}
	}
	if image == "" {
		image = "image.png"
	}
	return map[string]string{"image": image, "candidate": candidate, "reference": reference}, nil
}
