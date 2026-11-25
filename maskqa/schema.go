package maskqa

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	schemaV1Name = "maskqa.icon_project/v1"
	schemaV2Name = "maskqa.icon_project/v2"
	schemaV3Name = "maskqa.icon_project/v3"
)

type ExportResult struct {
	Version        string       `json:"version"`
	Schema         string       `json:"schema"`
	Data           []byte       `json:"-"`
	Residuals      []Truncation `json:"residuals"`
	ResidualDigest string       `json:"residual_digest"`
}

type wireV1 struct {
	Schema           string         `json:"schema"`
	IconID           *string        `json:"icon_id"`
	Seed             *int64         `json:"seed"`
	WidthPx          *int           `json:"width_px"`
	HeightPx         *int           `json:"height_px"`
	BrushWidthPx     *int           `json:"brush_width_px"`
	Rounding         *string        `json:"rounding_mode"`
	ImagePNGBase64   *string        `json:"image_png_base64"`
	CandidateMaskB64 *string        `json:"candidate_mask_png_base64"`
	DirectionTable   *string        `json:"direction_table"`
	ResidualDigest   string         `json:"conversion_residual_digest,omitempty"`
	Strokes          []wireStrokeV1 `json:"strokes"`
}

type wireStrokeV1 struct {
	StrokeNo        *int `json:"stroke_no"`
	ProcessPriority *int `json:"process_priority"`
	StartX          *int `json:"start_x_px"`
	StartY          *int `json:"start_y_px"`
	EndX            *int `json:"end_x_px"`
	EndY            *int `json:"end_y_px"`
	WeightPermille  *int `json:"weight_permille"`
	DirectionCode   any  `json:"direction_code"`
}

type wireV2 struct {
	Schema         string         `json:"schema"`
	ProjectID      *string        `json:"projectId"`
	Seed           *int64         `json:"seed"`
	WidthPx        *int           `json:"widthPx"`
	HeightPx       *int           `json:"heightPx"`
	BrushWidthPx   *int           `json:"brushWidthPx"`
	Rounding       *string        `json:"roundingMode"`
	ImageB64       *string        `json:"imagePngBase64"`
	CandidateB64   *string        `json:"candidateMaskPngBase64"`
	CoordEncoding  *wireCoordV2   `json:"coordinateEncoding"`
	WeightScale    *string        `json:"weightScale"`
	DirectionTable *string        `json:"directionTable"`
	ResidualDigest string         `json:"conversionResidualDigest,omitempty"`
	Strokes        []wireStrokeV2 `json:"strokes"`
}

type wireCoordV2 struct {
	Mode          string `json:"mode"`
	IndexStepX    int    `json:"indexStepX"`
	IndexStepY    int    `json:"indexStepY"`
	OffsetQuantum int    `json:"offsetQuantum"`
}

type wireStrokeV2 struct {
	StrokeIndex      *int   `json:"strokeIndex"`
	Priority         *int   `json:"priority"`
	StartOffsetX     *int   `json:"startOffsetX"`
	StartOffsetY     *int   `json:"startOffsetY"`
	EndOffsetX       *int   `json:"endOffsetX"`
	EndOffsetY       *int   `json:"endOffsetY"`
	WeightPermille   *int   `json:"weightPermille"`
	HardnessPermille *int   `json:"hardnessPermille"`
	DirectionCode    string `json:"directionCode"`
}

type wireV3 struct {
	Schema         string         `json:"schema"`
	ProjectID      *string        `json:"project_id"`
	Seed           *int64         `json:"seed"`
	WidthPx        *int           `json:"width_px"`
	HeightPx       *int           `json:"height_px"`
	BrushWidthPx   *int           `json:"brush_width_px"`
	CutAlphaBPS    *int           `json:"cut_alpha_bps"`
	Rounding       *string        `json:"rounding_mode"`
	ImageB64       *string        `json:"image_png_base64"`
	CandidateB64   *string        `json:"candidate_mask_png_base64"`
	WeightScale    *string        `json:"weight_scale"`
	DirectionTable *string        `json:"direction_table"`
	ResidualDigest string         `json:"conversion_residual_digest,omitempty"`
	Strokes        []wireStrokeV3 `json:"strokes"`
}

type wirePointV3 struct {
	X *int `json:"x"`
	Y *int `json:"y"`
}

type wireStrokeV3 struct {
	ID            *string      `json:"id"`
	Index         *int         `json:"index"`
	Priority      *int         `json:"order_priority"`
	From          *wirePointV3 `json:"from"`
	To            *wirePointV3 `json:"to"`
	WeightBPS     *int         `json:"weight_basis_points"`
	Hardness      *int         `json:"hardness_permille"`
	DirectionCode string       `json:"direction_code"`
}

// LoadProject detects a schema version and maps all source fields to the canonical model.
func LoadProject(data []byte) (*Project, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, schemaError("", "$", "invalid JSON: %v", err)
	}
	switch head.Schema {
	case schemaV1Name:
		return loadV1(data)
	case schemaV2Name:
		return loadV2(data)
	case schemaV3Name:
		return loadV3(data)
	default:
		ver := "unknown"
		if strings.Contains(head.Schema, "/v") {
			ver = head.Schema[strings.LastIndex(head.Schema, "/v")+1:]
		}
		return nil, schemaError(ver, "schema", "unknown schema %q; supported versions are v1,v2,v3", head.Schema)
	}
}

func requiredPtr[T any](v *T, version, path string) (T, error) {
	if v == nil {
		var zero T
		return zero, schemaError(version, path, "missing required field")
	}
	return *v, nil
}

func requiredPoint(p *wirePointV3, version, path string) (Point, error) {
	if p == nil {
		return Point{}, schemaError(version, path, "missing required field")
	}
	x, err := requiredPtr(p.X, version, path+".x")
	if err != nil {
		return Point{}, err
	}
	y, err := requiredPtr(p.Y, version, path+".y")
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func decodeProjectAssets(version string, imageB64 *string, maskB64 *string, width, height int) ([]byte, *Mask, error) {
	imgStr, err := requiredPtr(imageB64, version, "image_png_base64")
	if err != nil {
		// v2 uses camel path; caller can tolerate by trying alternate pointer externally? Here path generic.
		return nil, nil, err
	}
	if _, raw, err := DecodePNGBase64(imgStr); err != nil {
		if qe, ok := AsError(err); ok && qe.Code == CodeDecode {
			qe.SourceVersion = version
			qe.Path = "image_png_base64"
			qe.Field = qe.Path
		}
		return nil, nil, err
	} else {
		_ = raw
	}
	maskStr, err := requiredPtr(maskB64, version, "candidate_mask_png_base64")
	if err != nil {
		return nil, nil, err
	}
	mask, err := DecodeMaskBase64(maskStr)
	if err != nil {
		if qe, ok := AsError(err); ok {
			qe.SourceVersion = version
		}
		return nil, nil, err
	}
	if mask.Width != width || mask.Height != height {
		return nil, nil, paramError("candidate_mask_png_base64.size", fmt.Sprintf("%dx%d", width, height), "mask size %dx%d contradicts project size", mask.Width, mask.Height)
	}
	rawImg, _ := base64.StdEncoding.DecodeString(imgStr)
	return rawImg, mask, nil
}

func loadV1(data []byte) (*Project, error) {
	ver := SchemaV1
	var w wireV1
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, schemaError(ver, "$", "invalid v1 JSON: %v", err)
	}
	id, err := requiredPtr(w.IconID, ver, "icon_id")
	if err != nil {
		return nil, err
	}
	seed, err := requiredPtr(w.Seed, ver, "seed")
	if err != nil {
		return nil, err
	}
	width, err := requiredPtr(w.WidthPx, ver, "width_px")
	if err != nil {
		return nil, err
	}
	height, err := requiredPtr(w.HeightPx, ver, "height_px")
	if err != nil {
		return nil, err
	}
	brush, err := requiredPtr(w.BrushWidthPx, ver, "brush_width_px")
	if err != nil {
		return nil, err
	}
	rounding := RoundingFloor
	if w.Rounding != nil {
		rounding = *w.Rounding
	}
	table := "arrow8_legacy_v1"
	if w.DirectionTable != nil {
		table = *w.DirectionTable
	}

	imgRaw, mask, err := decodeProjectAssets(ver, w.ImagePNGBase64, w.CandidateMaskB64, width, height)
	if err != nil {
		if qe, ok := AsError(err); ok && qe.Path == "image_png_base64" {
			qe.Path = "image_png_base64"
			qe.Field = qe.Path
		}
		return nil, err
	}
	if len(w.Strokes) == 0 {
		return nil, schemaError(ver, "strokes", "at least one stroke is required")
	}

	p := &Project{
		SourceVersion: ver, ProjectID: id, Seed: seed, Width: width, Height: height,
		BrushWidthPx: brush, CutAlphaBPS: DefaultCutAlphaBPS, RoundingMode: rounding,
		CoordEncoding: CoordinateEncoding{Mode: "absolute_px", OffsetQuantum: 1},
		WeightScale:   WeightPermilleScaleName(), DirectionTable: table,
		ImagePNG: imgRaw, Candidate: mask, ResidualDigest: w.ResidualDigest,
	}
	for i, sw := range w.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		idx, err := requiredPtr(sw.StrokeNo, ver, path+".stroke_no")
		if err != nil {
			return nil, err
		}
		priority, err := requiredPtr(sw.ProcessPriority, ver, path+".process_priority")
		if err != nil {
			return nil, err
		}
		sx, err := requiredPtr(sw.StartX, ver, path+".start_x_px")
		if err != nil {
			return nil, err
		}
		sy, err := requiredPtr(sw.StartY, ver, path+".start_y_px")
		if err != nil {
			return nil, err
		}
		ex, err := requiredPtr(sw.EndX, ver, path+".end_x_px")
		if err != nil {
			return nil, err
		}
		ey, err := requiredPtr(sw.EndY, ver, path+".end_y_px")
		if err != nil {
			return nil, err
		}
		wp, err := requiredPtr(sw.WeightPermille, ver, path+".weight_permille")
		if err != nil {
			return nil, err
		}
		if sw.DirectionCode == nil {
			return nil, schemaError(ver, path+".direction_code", "missing required field")
		}
		code := normalizeAnyCode(sw.DirectionCode)
		cat, err := DecodeDirection(ver, table, code, path+".direction_code")
		if err != nil {
			return nil, err
		}
		weightBPS, err := WeightPermilleToBPS(wp, path+".weight_permille")
		if err != nil {
			return nil, err
		}
		st := Stroke{
			Index: idx, ID: fmt.Sprintf("stroke-%d", idx), Priority: priority,
			Start: Point{X: sx, Y: sy}, End: Point{X: ex, Y: ey},
			WeightBPS: weightBPS, HardnessPermille: DefaultHardnessPermille, HardnessPresent: false,
			DirectionCategory: cat,
		}
		st.DeclaredAngleDeg = CategoryAngle(cat)
		p.Strokes = append(p.Strokes, st)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(data []byte) (*Project, error) {
	ver := SchemaV2
	var w wireV2
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, schemaError(ver, "$", "invalid v2 JSON: %v", err)
	}
	id, err := requiredPtr(w.ProjectID, ver, "projectId")
	if err != nil {
		return nil, err
	}
	seed, err := requiredPtr(w.Seed, ver, "seed")
	if err != nil {
		return nil, err
	}
	width, err := requiredPtr(w.WidthPx, ver, "widthPx")
	if err != nil {
		return nil, err
	}
	height, err := requiredPtr(w.HeightPx, ver, "heightPx")
	if err != nil {
		return nil, err
	}
	brush, err := requiredPtr(w.BrushWidthPx, ver, "brushWidthPx")
	if err != nil {
		return nil, err
	}
	rounding := RoundingFloor
	if w.Rounding != nil {
		rounding = *w.Rounding
	}
	if w.CoordEncoding == nil {
		return nil, schemaError(ver, "coordinateEncoding", "missing required field")
	}
	ce := *w.CoordEncoding
	if ce.Mode != "stroke_index_offset" {
		return nil, schemaError(ver, "coordinateEncoding.mode", "v2 requires stroke_index_offset")
	}
	if ce.OffsetQuantum == 0 {
		ce.OffsetQuantum = 1
	}
	table := "compass_clock_v2"
	if w.DirectionTable != nil {
		table = *w.DirectionTable
	}
	scale := "permille"
	if w.WeightScale != nil {
		scale = *w.WeightScale
	}

	imgRaw, mask, err := decodeProjectAssets(ver, w.ImageB64, w.CandidateB64, width, height)
	if err != nil {
		if qe, ok := AsError(err); ok && qe.Path == "image_png_base64" {
			qe.Path = "imagePngBase64"
			qe.Field = qe.Path
		}
		if qe, ok := AsError(err); ok && qe.Path == "candidate_mask_png_base64" {
			qe.Path = "candidateMaskPngBase64"
			qe.Field = qe.Path
		}
		return nil, err
	}
	p := &Project{
		SourceVersion: ver, ProjectID: id, Seed: seed, Width: width, Height: height,
		BrushWidthPx: brush, CutAlphaBPS: DefaultCutAlphaBPS, RoundingMode: rounding,
		CoordEncoding: CoordinateEncoding{Mode: ce.Mode, IndexStepX: ce.IndexStepX, IndexStepY: ce.IndexStepY, OffsetQuantum: ce.OffsetQuantum},
		WeightScale:   scale, DirectionTable: table, ImagePNG: imgRaw, Candidate: mask,
		ResidualDigest: w.ResidualDigest,
	}
	for i, sw := range w.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		idx, err := requiredPtr(sw.StrokeIndex, ver, path+".strokeIndex")
		if err != nil {
			return nil, err
		}
		priority, err := requiredPtr(sw.Priority, ver, path+".priority")
		if err != nil {
			return nil, err
		}
		sx, err := requiredPtr(sw.StartOffsetX, ver, path+".startOffsetX")
		if err != nil {
			return nil, err
		}
		sy, err := requiredPtr(sw.StartOffsetY, ver, path+".startOffsetY")
		if err != nil {
			return nil, err
		}
		ex, err := requiredPtr(sw.EndOffsetX, ver, path+".endOffsetX")
		if err != nil {
			return nil, err
		}
		ey, err := requiredPtr(sw.EndOffsetY, ver, path+".endOffsetY")
		if err != nil {
			return nil, err
		}
		wp, err := requiredPtr(sw.WeightPermille, ver, path+".weightPermille")
		if err != nil {
			return nil, err
		}
		hp, err := requiredPtr(sw.HardnessPermille, ver, path+".hardnessPermille")
		if err != nil {
			return nil, err
		}
		if sw.DirectionCode == "" {
			return nil, schemaError(ver, path+".directionCode", "missing required field")
		}
		cat, err := DecodeDirection(ver, table, sw.DirectionCode, path+".directionCode")
		if err != nil {
			return nil, err
		}
		start, err := IndexOffsetToAbsolute(Point{X: sx, Y: sy}, idx, p.CoordEncoding, path+".startOffset")
		if err != nil {
			return nil, err
		}
		end, err := IndexOffsetToAbsolute(Point{X: ex, Y: ey}, idx, p.CoordEncoding, path+".endOffset")
		if err != nil {
			return nil, err
		}
		weightBPS, err := WeightPermilleToBPS(wp, path+".weightPermille")
		if err != nil {
			return nil, err
		}
		p.Strokes = append(p.Strokes, Stroke{
			Index: idx, ID: fmt.Sprintf("stroke-%d", idx), Priority: priority,
			Start: start, End: end, WeightBPS: weightBPS,
			HardnessPermille: hp, HardnessPresent: true,
			DirectionCategory: cat, DeclaredAngleDeg: CategoryAngle(cat),
		})
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(data []byte) (*Project, error) {
	ver := SchemaV3
	var w wireV3
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, schemaError(ver, "$", "invalid v3 JSON: %v", err)
	}
	id, err := requiredPtr(w.ProjectID, ver, "project_id")
	if err != nil {
		return nil, err
	}
	seed, err := requiredPtr(w.Seed, ver, "seed")
	if err != nil {
		return nil, err
	}
	width, err := requiredPtr(w.WidthPx, ver, "width_px")
	if err != nil {
		return nil, err
	}
	height, err := requiredPtr(w.HeightPx, ver, "height_px")
	if err != nil {
		return nil, err
	}
	brush, err := requiredPtr(w.BrushWidthPx, ver, "brush_width_px")
	if err != nil {
		return nil, err
	}
	rounding := RoundingFloor
	if w.Rounding != nil {
		rounding = *w.Rounding
	}
	cut := DefaultCutAlphaBPS
	if w.CutAlphaBPS != nil {
		cut = *w.CutAlphaBPS
	}
	table := "canonical_snake_v3"
	if w.DirectionTable != nil {
		table = *w.DirectionTable
	}
	scale := "basis_points"
	if w.WeightScale != nil {
		scale = *w.WeightScale
	}
	imgRaw, mask, err := decodeProjectAssets(ver, w.ImageB64, w.CandidateB64, width, height)
	if err != nil {
		return nil, err
	}
	p := &Project{
		SourceVersion: ver, ProjectID: id, Seed: seed, Width: width, Height: height,
		BrushWidthPx: brush, CutAlphaBPS: cut, RoundingMode: rounding,
		CoordEncoding: CoordinateEncoding{Mode: "absolute_px", OffsetQuantum: 1},
		WeightScale:   scale, DirectionTable: table, ImagePNG: imgRaw, Candidate: mask,
		ResidualDigest: w.ResidualDigest,
	}
	for i, sw := range w.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		sid, err := requiredPtr(sw.ID, ver, path+".id")
		if err != nil {
			return nil, err
		}
		idx, err := requiredPtr(sw.Index, ver, path+".index")
		if err != nil {
			return nil, err
		}
		priority, err := requiredPtr(sw.Priority, ver, path+".order_priority")
		if err != nil {
			return nil, err
		}
		start, err := requiredPoint(sw.From, ver, path+".from")
		if err != nil {
			return nil, err
		}
		end, err := requiredPoint(sw.To, ver, path+".to")
		if err != nil {
			return nil, err
		}
		weight, err := requiredPtr(sw.WeightBPS, ver, path+".weight_basis_points")
		if err != nil {
			return nil, err
		}
		hardness, err := requiredPtr(sw.Hardness, ver, path+".hardness_permille")
		if err != nil {
			return nil, err
		}
		if sw.DirectionCode == "" {
			return nil, schemaError(ver, path+".direction_code", "missing required field")
		}
		cat, err := DecodeDirection(ver, table, sw.DirectionCode, path+".direction_code")
		if err != nil {
			return nil, err
		}
		p.Strokes = append(p.Strokes, Stroke{
			Index: idx, ID: sid, Priority: priority, Start: start, End: end,
			WeightBPS: weight, HardnessPermille: hardness, HardnessPresent: true,
			DirectionCategory: cat, DeclaredAngleDeg: CategoryAngle(cat),
		})
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func normalizeAnyCode(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

// ExportProject serializes the canonical project into one of the three source schemas.
func ExportProject(p *Project, version string) (*ExportResult, error) {
	switch version {
	case SchemaV1:
		return exportV1(p)
	case SchemaV2:
		return exportV2(p)
	case SchemaV3:
		return exportV3(p)
	default:
		return nil, schemaError(version, "schema", "unknown export version")
	}
}

func exportV1(p *Project) (*ExportResult, error) {
	imgB64 := base64.StdEncoding.EncodeToString(p.ImagePNG)
	maskB64, err := EncodeMaskBase64(p.Candidate)
	if err != nil {
		return nil, err
	}
	table := "arrow8_legacy_v1"
	out := wireV1{
		Schema: schemaV1Name, IconID: &p.ProjectID, Seed: &p.Seed,
		WidthPx: &p.Width, HeightPx: &p.Height, BrushWidthPx: &p.BrushWidthPx,
		Rounding: &p.RoundingMode, ImagePNGBase64: &imgB64, CandidateMaskB64: &maskB64,
		DirectionTable: &table,
	}
	res := []Truncation{}
	for _, s := range p.Strokes {
		wp, tr, err := WeightBPSToPermille(s.WeightBPS, p.RoundingMode, fmt.Sprintf("strokes[%d].weight_permille", s.Index-1), s.Index)
		if err != nil {
			return nil, err
		}
		res = append(res, tr)
		code, err := EncodeDirection(table, s.DirectionCategory)
		if err != nil {
			return nil, err
		}
		out.Strokes = append(out.Strokes, wireStrokeV1{
			StrokeNo: &s.Index, ProcessPriority: &s.Priority,
			StartX: &s.Start.X, StartY: &s.Start.Y, EndX: &s.End.X, EndY: &s.End.Y,
			WeightPermille: &wp, DirectionCode: json.RawMessage(codeAsJSON(code)),
		})
	}
	digest := DigestResiduals(res)
	out.ResidualDigest = digest
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: SchemaV1, Schema: schemaV1Name, Data: data, Residuals: res, ResidualDigest: digest}, nil
}

func exportV2(p *Project) (*ExportResult, error) {
	imgB64 := base64.StdEncoding.EncodeToString(p.ImagePNG)
	maskB64, err := EncodeMaskBase64(p.Candidate)
	if err != nil {
		return nil, err
	}
	table := "compass_clock_v2"
	enc := wireCoordV2{Mode: "stroke_index_offset", IndexStepX: 3, IndexStepY: 5, OffsetQuantum: 1}
	scale := "permille"
	out := wireV2{
		Schema: schemaV2Name, ProjectID: &p.ProjectID, Seed: &p.Seed,
		WidthPx: &p.Width, HeightPx: &p.Height, BrushWidthPx: &p.BrushWidthPx,
		Rounding: &p.RoundingMode, ImageB64: &imgB64, CandidateB64: &maskB64,
		CoordEncoding: &enc, WeightScale: &scale, DirectionTable: &table,
	}
	res := []Truncation{}
	for _, s := range p.Strokes {
		wp, tr, err := WeightBPSToPermille(s.WeightBPS, p.RoundingMode, fmt.Sprintf("strokes[%d].weightPermille", s.Index-1), s.Index)
		if err != nil {
			return nil, err
		}
		res = append(res, tr)
		start, trs, err := AbsoluteToIndexOffset(s.Start, s.Index, CoordinateEncoding{Mode: enc.Mode, IndexStepX: enc.IndexStepX, IndexStepY: enc.IndexStepY, OffsetQuantum: enc.OffsetQuantum}, fmt.Sprintf("strokes[%d].startOffset", s.Index-1))
		if err != nil {
			return nil, err
		}
		res = append(res, trs...)
		end, trs, err := AbsoluteToIndexOffset(s.End, s.Index, CoordinateEncoding{Mode: enc.Mode, IndexStepX: enc.IndexStepX, IndexStepY: enc.IndexStepY, OffsetQuantum: enc.OffsetQuantum}, fmt.Sprintf("strokes[%d].endOffset", s.Index-1))
		if err != nil {
			return nil, err
		}
		res = append(res, trs...)
		code, err := EncodeDirection(table, s.DirectionCategory)
		if err != nil {
			return nil, err
		}
		hp := s.HardnessPermille
		out.Strokes = append(out.Strokes, wireStrokeV2{
			StrokeIndex: &s.Index, Priority: &s.Priority,
			StartOffsetX: &start.X, StartOffsetY: &start.Y, EndOffsetX: &end.X, EndOffsetY: &end.Y,
			WeightPermille: &wp, HardnessPermille: &hp, DirectionCode: code,
		})
	}
	digest := DigestResiduals(res)
	out.ResidualDigest = digest
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: SchemaV2, Schema: schemaV2Name, Data: data, Residuals: res, ResidualDigest: digest}, nil
}

func exportV3(p *Project) (*ExportResult, error) {
	imgB64 := base64.StdEncoding.EncodeToString(p.ImagePNG)
	maskB64, err := EncodeMaskBase64(p.Candidate)
	if err != nil {
		return nil, err
	}
	table := "canonical_snake_v3"
	scale := "basis_points"
	out := wireV3{
		Schema: schemaV3Name, ProjectID: &p.ProjectID, Seed: &p.Seed,
		WidthPx: &p.Width, HeightPx: &p.Height, BrushWidthPx: &p.BrushWidthPx,
		CutAlphaBPS: &p.CutAlphaBPS, Rounding: &p.RoundingMode,
		ImageB64: &imgB64, CandidateB64: &maskB64, WeightScale: &scale, DirectionTable: &table,
		ResidualDigest: DigestResiduals(nil),
	}
	for _, s := range p.Strokes {
		code, err := EncodeDirection(table, s.DirectionCategory)
		if err != nil {
			return nil, err
		}
		id, idx, pr := s.ID, s.Index, s.Priority
		w, h := s.WeightBPS, s.HardnessPermille
		out.Strokes = append(out.Strokes, wireStrokeV3{
			ID: &id, Index: &idx, Priority: &pr,
			From:      &wirePointV3{X: &s.Start.X, Y: &s.Start.Y},
			To:        &wirePointV3{X: &s.End.X, Y: &s.End.Y},
			WeightBPS: &w, Hardness: &h, DirectionCode: code,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	digest := DigestResiduals(nil)
	return &ExportResult{Version: SchemaV3, Schema: schemaV3Name, Data: data, ResidualDigest: digest}, nil
}

func codeAsJSON(code string) string {
	if n, err := strconv.Atoi(code); err == nil {
		return strconv.Itoa(n)
	}
	return strconv.Quote(code)
}

func WeightPermilleScaleName() string { return "permille" }
