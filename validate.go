package terrainbake

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// LoadProject reads a self-contained project JSON file and performs complete
// structural validation before any baking begins.
func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodeIO, Object: "project", Field: "path", Message: err.Error()}
	}
	return DecodeProject(data)
}

// SaveProject writes a project using the canonical JSON representation.
func SaveProject(path string, p *Project) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return &Error{Code: CodeIO, Object: "project", Field: "json", Message: err.Error()}
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return &Error{Code: CodeIO, Object: "project", Field: "path", Message: err.Error()}
	}
	return nil
}

// DecodeProject validates a project JSON document, including the presence of
// required fields. Missing fields are reported as E_SCHEMA with object/field.
func DecodeProject(data []byte) (*Project, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, &Error{Code: CodeSchema, Object: "project", Field: "json", Message: err.Error()}
	}
	requiredTop := []string{"schema_version", "name", "seed", "blocks", "materials", "seams", "truth", "seam_truth"}
	for _, f := range requiredTop {
		if _, ok := top[f]; !ok {
			return nil, &Error{Code: CodeSchema, Object: "project", Field: f, Message: "required field is missing"}
		}
	}
	if err := requireObjectFields(top["blocks"], "block", []string{
		"id", "width", "height", "origin_x", "origin_y", "heights", "material_ids", "weights",
	}); err != nil {
		return nil, err
	}
	if err := validateQuantizedWeightJSON(top["blocks"]); err != nil {
		return nil, err
	}
	if err := requireObjectFields(top["materials"], "material", []string{
		"id", "normal_scale", "rotation_deg",
	}); err != nil {
		return nil, err
	}
	if err := requireObjectFields(top["seams"], "seam", []string{
		"id", "block_a", "block_b", "edge_a", "edge_b", "max_angle_deg", "max_iterations",
	}); err != nil {
		return nil, err
	}
	if err := requireObjectFields(top["truth"], "truth", []string{
		"block_id", "x", "y", "normal", "slope_deg",
	}); err != nil {
		return nil, err
	}
	if err := requireObjectFields(top["seam_truth"], "seam_truth", []string{
		"seam_id", "index", "angle_deg",
	}); err != nil {
		return nil, err
	}

	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, &Error{Code: CodeSchema, Object: "project", Field: "json", Message: err.Error()}
	}
	if err := ValidateProject(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func validateQuantizedWeightJSON(raw json.RawMessage) error {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return &Error{Code: CodeSchema, Object: "block", Field: "list", Message: err.Error()}
	}
	for i, obj := range blocks {
		objName := fmt.Sprintf("block[%d]", i)
		var id string
		if idRaw, ok := obj["id"]; ok && json.Unmarshal(idRaw, &id) == nil && id != "" {
			objName = "block:" + id
		}
		var values []json.RawMessage
		if err := json.Unmarshal(obj["weights"], &values); err != nil {
			return &Error{Code: CodeSchema, Object: objName, Field: "weights", Message: "weights must be a JSON integer array"}
		}
		for index, value := range values {
			var num json.Number
			if err := json.Unmarshal(value, &num); err != nil {
				return &Error{
					Code: CodeSchema, Object: objName, Field: "weights",
					Message: "quantized weights must be JSON integers",
					Details: map[string]any{"index": index, "value": string(value)},
				}
			}
			text := string(num)
			if strings.ContainsAny(text, ".eE") {
				return &Error{
					Code: CodeSchema, Object: objName, Field: "weights",
					Message: "quantized weights must be JSON integers",
					Details: map[string]any{"index": index, "value": text},
				}
			}
			if _, err := strconv.ParseInt(text, 10, 64); err != nil {
				return &Error{
					Code: CodeSchema, Object: objName, Field: "weights",
					Message: "quantized weights must be JSON integers",
					Details: map[string]any{"index": index, "value": text},
				}
			}
		}
	}
	return nil
}

func requireObjectFields(raw json.RawMessage, object string, fields []string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return &Error{Code: CodeSchema, Object: object, Field: "list", Message: "required list is missing or null"}
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return &Error{Code: CodeSchema, Object: object, Field: "list", Message: err.Error()}
	}
	for i, obj := range list {
		for _, f := range fields {
			if _, ok := obj[f]; !ok {
				objName := object
				if idRaw, ok := obj["id"]; ok {
					var id string
					if json.Unmarshal(idRaw, &id) == nil && id != "" {
						objName = object + ":" + id
					}
				} else if bidRaw, ok := obj["block_id"]; ok {
					var id string
					if json.Unmarshal(bidRaw, &id) == nil && id != "" {
						objName = object + ":" + id
					}
				}
				if objName == object {
					objName = fmt.Sprintf("%s[%d]", object, i)
				}
				return &Error{Code: CodeSchema, Object: objName, Field: f, Message: "required field is missing"}
			}
		}
	}
	return nil
}

// ValidateProject checks schema invariants, dimensions, quantized weights,
// seam references/adjacency and truth bounds.
func ValidateProject(p *Project) error {
	if p == nil {
		return &Error{Code: CodeSchema, Object: "project", Field: "project", Message: "project is nil"}
	}
	if p.SchemaVersion != SchemaVersion {
		return &Error{
			Code: CodeSchema, Object: "project", Field: "schema_version",
			Message: fmt.Sprintf("unsupported schema version %d (want %d)", p.SchemaVersion, SchemaVersion),
		}
	}
	if p.Name == "" {
		return &Error{Code: CodeSchema, Object: "project", Field: "name", Message: "name must not be empty"}
	}
	if len(p.Blocks) == 0 {
		return &Error{Code: CodeSchema, Object: "project", Field: "blocks", Message: "at least one block is required"}
	}

	blockByID := make(map[string]*Block, len(p.Blocks))
	for i := range p.Blocks {
		b := &p.Blocks[i]
		obj := "block:" + b.ID
		if b.ID == "" {
			return &Error{Code: CodeSchema, Object: fmt.Sprintf("block[%d]", i), Field: "id", Message: "block id must not be empty"}
		}
		if _, dup := blockByID[b.ID]; dup {
			return &Error{Code: CodeSchema, Object: obj, Field: "id", Message: "duplicate block id"}
		}
		blockByID[b.ID] = b
		if b.Width <= 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "width", Message: "width must be positive"}
		}
		if b.Height <= 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "height", Message: "height must be positive"}
		}
		if b.OriginX < 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "origin_x", Message: "origin_x must be non-negative"}
		}
		if b.OriginY < 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "origin_y", Message: "origin_y must be non-negative"}
		}
		pixels := b.Width * b.Height
		if len(b.Elevation) != pixels {
			return &Error{
				Code: CodeSchema, Object: obj, Field: "heights",
				Message: fmt.Sprintf("height sample count %d does not match %dx%d=%d", len(b.Elevation), b.Width, b.Height, pixels),
				Details: map[string]any{"want": pixels, "got": len(b.Elevation)},
			}
		}
		for j, h := range b.Elevation {
			if !validFinite(h) {
				return &Error{Code: CodeSchema, Object: obj, Field: "heights", Message: "elevation contains a non-finite value", Details: map[string]any{"index": j}}
			}
		}
		if len(b.Materials) == 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "material_ids", Message: "at least one material layer is required"}
		}
		seenMat := map[string]bool{}
		for _, id := range b.Materials {
			if id == "" {
				return &Error{Code: CodeSchema, Object: obj, Field: "material_ids", Message: "material id must not be empty"}
			}
			if seenMat[id] {
				return &Error{Code: CodeSchema, Object: obj, Field: "material_ids", Message: "duplicate material id"}
			}
			seenMat[id] = true
		}
		wantWeights := pixels * len(b.Materials)
		if len(b.Weights) != wantWeights {
			return &Error{
				Code: CodeSchema, Object: obj, Field: "weights",
				Message: fmt.Sprintf("weight count %d does not match %d pixels x %d layers", len(b.Weights), pixels, len(b.Materials)),
				Details: map[string]any{"want": wantWeights, "got": len(b.Weights)},
			}
		}
		nLayers := len(b.Materials)
		for px := 0; px < pixels; px++ {
			sum := 0
			for layer := 0; layer < nLayers; layer++ {
				w := b.Weights[px*nLayers+layer]
				if w < 0 || w > WeightQuantum {
					return &Error{
						Code: CodeSchema, Object: obj, Field: "weights",
						Message: fmt.Sprintf("quantized weight %d is outside [0,%d]", w, WeightQuantum),
						Details: map[string]any{"pixel": px, "layer": layer, "value": w, "min": 0, "max": WeightQuantum},
					}
				}
				sum += w
			}
			if sum != WeightQuantum {
				return &Error{
					Code: CodeSchema, Object: obj, Field: "weights",
					Message: fmt.Sprintf("weights for pixel %d sum to %d, want %d", px, sum, WeightQuantum),
					Details: map[string]any{"pixel": px, "sum": sum, "want": WeightQuantum},
				}
			}
		}
	}

	if len(p.Materials) == 0 {
		return &Error{Code: CodeSchema, Object: "project", Field: "materials", Message: "at least one material is required"}
	}
	materialByID := make(map[string]*Material, len(p.Materials))
	for i := range p.Materials {
		m := &p.Materials[i]
		obj := "material:" + m.ID
		if m.ID == "" {
			return &Error{Code: CodeSchema, Object: fmt.Sprintf("material[%d]", i), Field: "id", Message: "material id must not be empty"}
		}
		if _, dup := materialByID[m.ID]; dup {
			return &Error{Code: CodeSchema, Object: obj, Field: "id", Message: "duplicate material id"}
		}
		materialByID[m.ID] = m
		if !validFinite(m.NormalScale) || m.NormalScale <= 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "normal_scale", Message: "normal_scale must be finite and positive"}
		}
		if !validFinite(m.RotationDeg) {
			return &Error{Code: CodeSchema, Object: obj, Field: "rotation_deg", Message: "rotation_deg must be finite"}
		}
	}
	for i := range p.Blocks {
		b := &p.Blocks[i]
		for _, id := range b.Materials {
			if _, ok := materialByID[id]; !ok {
				return &Error{
					Code: CodeSchema, Object: "block:" + b.ID, Field: "material_ids",
					Message: fmt.Sprintf("referenced material %q does not exist", id),
					Details: map[string]any{"material_id": id},
				}
			}
		}
	}

	seamIDs := map[string]bool{}
	for i := range p.Seams {
		s := &p.Seams[i]
		obj := "seam:" + s.ID
		if s.ID == "" {
			return &Error{Code: CodeSchema, Object: fmt.Sprintf("seam[%d]", i), Field: "id", Message: "seam id must not be empty"}
		}
		if seamIDs[s.ID] {
			return &Error{Code: CodeSchema, Object: obj, Field: "id", Message: "duplicate seam id"}
		}
		seamIDs[s.ID] = true
		ba, okA := blockByID[s.BlockA]
		bb, okB := blockByID[s.BlockB]
		if !okA || !okB {
			field := "block_a"
			missing := s.BlockA
			if okA {
				field, missing = "block_b", s.BlockB
			}
			return &Error{
				Code: CodeSchema, Object: obj, Field: field,
				Message: fmt.Sprintf("referenced adjacent block %q does not exist", missing),
				Details: map[string]any{"block_id": missing},
			}
		}
		if s.BlockA == s.BlockB {
			return &Error{Code: CodeSchema, Object: obj, Field: "block_b", Message: "a seam must connect two different blocks"}
		}
		if !validEdge(s.EdgeA) {
			return &Error{Code: CodeSchema, Object: obj, Field: "edge_a", Message: "invalid edge"}
		}
		if !validEdge(s.EdgeB) {
			return &Error{Code: CodeSchema, Object: obj, Field: "edge_b", Message: "invalid edge"}
		}
		la, lb := edgeLength(ba, s.EdgeA), edgeLength(bb, s.EdgeB)
		if la != lb {
			return &Error{
				Code: CodeSchema, Object: obj, Field: "edge_b",
				Message: fmt.Sprintf("edge lengths differ: %d versus %d", la, lb),
			}
		}
		if !blocksAdjacent(ba, s.EdgeA, bb, s.EdgeB) {
			return &Error{
				Code: CodeSchema, Object: obj, Field: "block_b",
				Message: fmt.Sprintf("blocks %q and %q are not adjacent on the declared edges", ba.ID, bb.ID),
			}
		}
		if !validFinite(s.MaxAngleDeg) || s.MaxAngleDeg <= 0 || s.MaxAngleDeg > 180 {
			return &Error{Code: CodeSchema, Object: obj, Field: "max_angle_deg", Message: "max_angle_deg must be in (0,180]"}
		}
		if s.MaxIterations <= 0 {
			return &Error{Code: CodeSchema, Object: obj, Field: "max_iterations", Message: "max_iterations must be positive"}
		}
	}

	for i := range p.Truth {
		t := &p.Truth[i]
		obj := "truth:" + t.BlockID
		b, ok := blockByID[t.BlockID]
		if !ok {
			return &Error{Code: CodeSchema, Object: obj, Field: "block_id", Message: "truth references a missing block"}
		}
		if t.X < 0 || t.X >= b.Width || t.Y < 0 || t.Y >= b.Height {
			return &Error{Code: CodeSchema, Object: obj, Field: "x", Message: "truth pixel is outside the block"}
		}
		n, ok := normalize(t.Normal)
		if !ok || !validFinite(t.Normal.X) || !validFinite(t.Normal.Y) || !validFinite(t.Normal.Z) {
			return &Error{Code: CodeSchema, Object: obj, Field: "normal", Message: "truth normal must be a finite non-zero vector"}
		}
		t.Normal = n
		if !validFinite(t.SlopeDeg) || t.SlopeDeg < 0 || t.SlopeDeg > 90.0000001 {
			return &Error{Code: CodeSchema, Object: obj, Field: "slope_deg", Message: "slope_deg must be in [0,90]"}
		}
	}
	for i := range p.SeamTruth {
		st := &p.SeamTruth[i]
		obj := "seam_truth:" + st.SeamID
		s, ok := findSeam(p, st.SeamID)
		if !ok {
			return &Error{Code: CodeSchema, Object: obj, Field: "seam_id", Message: "seam truth references a missing seam"}
		}
		if st.Index < 0 || st.Index >= edgeLength(mustBlock(blockByID, s.BlockA), s.EdgeA) {
			return &Error{Code: CodeSchema, Object: obj, Field: "index", Message: "seam truth index is outside the shared edge"}
		}
		if !validFinite(st.AngleDeg) || st.AngleDeg < 0 || st.AngleDeg > 180 {
			return &Error{Code: CodeSchema, Object: obj, Field: "angle_deg", Message: "angle_deg must be in [0,180]"}
		}
	}
	return nil
}

func mustBlock(m map[string]*Block, id string) *Block {
	if b, ok := m[id]; ok {
		return b
	}
	return &Block{}
}

func validEdge(e Edge) bool {
	switch e {
	case EdgeLeft, EdgeRight, EdgeTop, EdgeBottom:
		return true
	default:
		return false
	}
}

func edgeLength(b *Block, e Edge) int {
	if e == EdgeLeft || e == EdgeRight {
		return b.Height
	}
	return b.Width
}

func blocksAdjacent(a *Block, ea Edge, b *Block, eb Edge) bool {
	opposite := func(x, y Edge) bool {
		return (x == EdgeLeft && y == EdgeRight) || (x == EdgeRight && y == EdgeLeft) ||
			(x == EdgeTop && y == EdgeBottom) || (x == EdgeBottom && y == EdgeTop)
	}
	if !opposite(ea, eb) {
		return false
	}
	switch ea {
	case EdgeRight:
		return a.OriginX+a.Width == b.OriginX && a.OriginY == b.OriginY
	case EdgeLeft:
		return b.OriginX+b.Width == a.OriginX && a.OriginY == b.OriginY
	case EdgeBottom:
		return a.OriginY+a.Height == b.OriginY && a.OriginX == b.OriginX
	case EdgeTop:
		return b.OriginY+b.Height == a.OriginY && a.OriginX == b.OriginX
	}
	return false
}

func findSeam(p *Project, id string) (*Seam, bool) {
	for i := range p.Seams {
		if p.Seams[i].ID == id {
			return &p.Seams[i], true
		}
	}
	return nil, false
}

// InvalidateProjectParams reports parameter errors using E_PARA, while
// structural project errors remain E_SCHEMA.
func ValidateBakeParams(p BakeParams) error {
	switch p.Operator {
	case "central", "forward", "sobel":
	default:
		return &Error{
			Code: CodeParam, Object: "bake_params", Field: "operator",
			Message: fmt.Sprintf("unsupported differential operator %q (want central, forward or sobel)", p.Operator),
		}
	}
	if !validFinite(p.Scale) || p.Scale <= 0 || p.Scale > 1000 {
		return &Error{Code: CodeParam, Object: "bake_params", Field: "scale", Message: "scale must be finite and in (0,1000]"}
	}
	return nil
}

// VerifyTruth compares every analytic truth sample against final outputs.
func VerifyTruth(p *Project, outputs map[string]*BlockOutput) (int, error) {
	checked := 0
	for _, t := range p.Truth {
		o, ok := outputs[t.BlockID]
		if !ok {
			return checked, &Error{Code: CodeBake, Object: "truth", Field: "block_id", BlockID: t.BlockID, Message: "missing output for truth block"}
		}
		if t.X < 0 || t.X >= o.Width || t.Y < 0 || t.Y >= o.Height {
			return checked, &Error{Code: CodeBake, Object: "truth", Field: "x", BlockID: t.BlockID, X: t.X, Y: t.Y, Message: "truth pixel outside output"}
		}
		n := o.Normals[t.Y*o.Width+t.X]
		if angleDeg(n, t.Normal) > 0.01 {
			return checked, &Error{
				Code: CodeDiff, Object: "truth", Field: "normal", BlockID: t.BlockID, X: t.X, Y: t.Y,
				Message: fmt.Sprintf("truth normal mismatch: got (%.12f,%.12f,%.12f), want (%.12f,%.12f,%.12f)",
					n.X, n.Y, n.Z, t.Normal.X, t.Normal.Y, t.Normal.Z),
				Angle: angleDeg(n, t.Normal),
			}
		}
		slope := o.SlopeDeg[t.Y*o.Width+t.X]
		if math.Abs(slope-t.SlopeDeg) > 0.01 {
			return checked, &Error{
				Code: CodeDiff, Object: "truth", Field: "slope_deg", BlockID: t.BlockID, X: t.X, Y: t.Y,
				Message: fmt.Sprintf("truth slope mismatch: got %.9f, want %.9f", slope, t.SlopeDeg),
			}
		}
		checked++
	}
	for _, st := range p.SeamTruth {
		s, ok := findSeam(p, st.SeamID)
		if !ok {
			return checked, &Error{Code: CodeBake, Object: "seam_truth", Field: "seam_id", SeamID: st.SeamID, Message: "missing seam"}
		}
		a, _ := FindBlock(p, s.BlockA)
		b, _ := FindBlock(p, s.BlockB)
		pa := edgePoint(a, s.EdgeA, st.Index)
		pb := edgePoint(b, s.EdgeB, edgeIndexB(p, s, st.Index))
		oa, okA := outputs[s.BlockA]
		ob, okB := outputs[s.BlockB]
		if !okA || !okB {
			return checked, &Error{Code: CodeBake, Object: "seam_truth", Field: "block_id", SeamID: st.SeamID, Message: "missing seam block output"}
		}
		na := oa.Normals[pa.Y*a.Width+pa.X]
		nb := ob.Normals[pb.Y*b.Width+pb.X]
		got := angleDeg(na, nb)
		if math.Abs(got-st.AngleDeg) > 0.01 {
			return checked, &Error{
				Code: CodeDiff, Object: "seam_truth", Field: "angle_deg", SeamID: st.SeamID,
				BlockA: s.BlockA, BlockB: s.BlockB, Angle: got,
				Message: fmt.Sprintf("seam truth angle mismatch: got %.9f, want %.9f", got, st.AngleDeg),
			}
		}
		checked++
	}
	return checked, nil
}
