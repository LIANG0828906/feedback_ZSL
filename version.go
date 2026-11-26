package strokelab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const (
	Version1 = "mask-work/v1"
	Version2 = "mask-work/v2"
	Version3 = "mask-work/v3"
)

type RLEPair struct {
	Value uint8 `json:"value"`
	Count int   `json:"count"`
}

func (p RLEPair) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int{int(p.Value), p.Count})
}

func (p *RLEPair) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '[' {
		var pair [2]int
		if err := json.Unmarshal(data, &pair); err != nil {
			return err
		}
		if pair[0] < 0 || pair[0] > 255 {
			return fmt.Errorf("RLE value %d outside uint8", pair[0])
		}
		p.Value, p.Count = uint8(pair[0]), pair[1]
		return nil
	}
	type pairAlias RLEPair
	var alias pairAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*p = RLEPair(alias)
	return nil
}

func encodeRLE(mask []uint8) []RLEPair {
	if len(mask) == 0 {
		return nil
	}
	out := make([]RLEPair, 0, 16)
	value := mask[0]
	count := 1
	for _, v := range mask[1:] {
		if v == value {
			count++
			continue
		}
		out = append(out, RLEPair{Value: value, Count: count})
		value, count = v, 1
	}
	out = append(out, RLEPair{Value: value, Count: count})
	return out
}

func decodeRLE(field string, runs []RLEPair, want int) ([]uint8, error) {
	total := 0
	for _, run := range runs {
		if run.Count <= 0 {
			return nil, coded(CodeVersion, "%s has non-positive run count %d", field, run.Count)
		}
		total += run.Count
		if total > want {
			return nil, coded(CodeVersion, "%s expands beyond expected length %d", field, want)
		}
	}
	if total != want {
		return nil, coded(CodeVersion, "%s expands to %d pixels, want %d", field, total, want)
	}
	out := make([]uint8, 0, want)
	for _, run := range runs {
		for i := 0; i < run.Count; i++ {
			out = append(out, run.Value)
		}
	}
	return out, nil
}

type rawMeta struct {
	ProjectID         *string           `json:"project_id"`
	Name              string            `json:"name"`
	Seed              int64             `json:"seed"`
	BlockSize         *int              `json:"block_size"`
	Rounding          string            `json:"rounding"`
	ConversionTrace   []ConversionTrace `json:"conversion_trace"`
	CoordinateQuantum *int              `json:"coordinate_quantum,omitempty"`
}

type rawTruth struct {
	ReferenceRuns []RLEPair `json:"reference_runs"`
	Domains       []struct {
		ID             int  `json:"id"`
		Area           int  `json:"area"`
		CentroidXMilli int  `json:"centroid_x_milli"`
		CentroidYMilli int  `json:"centroid_y_milli"`
		BBox           Rect `json:"bbox"`
		Holes          int  `json:"holes"`
		RingWidthMilli int  `json:"ring_width_milli"`
		BoundaryPixels int  `json:"boundary_pixels"`
	} `json:"domains"`
	Contributions []struct {
		StrokeID  string    `json:"stroke_id"`
		StrokeSeq int       `json:"stroke_seq"`
		Runs      []RLEPair `json:"runs"`
	} `json:"contributions"`
}

type rawMask struct {
	Runs []RLEPair `json:"runs"`
}

type rawV1 struct {
	Version    string    `json:"version"`
	Meta       *rawMeta  `json:"meta"`
	Width      *int      `json:"width"`
	Height     *int      `json:"height"`
	Candidate  *rawMask  `json:"candidate_mask"`
	Operations []rawV1Op `json:"operations"`
	Truth      *rawTruth `json:"truth"`
}

type rawV1Op struct {
	ID             *string `json:"operation_id"`
	From           []int   `json:"from_px"`
	To             []int   `json:"to_px"`
	Priority       *int    `json:"priority"`
	Seq            *int    `json:"step_seq"`
	WeightPermille *int    `json:"weight_permille"`
	Radius         *int    `json:"radius_px"`
}

type rawV2 struct {
	Version string   `json:"version"`
	Meta    *rawMeta `json:"meta"`
	Canvas  *struct {
		Width  *int `json:"w"`
		Height *int `json:"h"`
	} `json:"canvas"`
	Candidate *rawMask  `json:"candidate"`
	BrushOps  []rawV2Op `json:"brush_ops"`
	Truth     *rawTruth `json:"truth"`
}

type rawV2Op struct {
	Stroke *string `json:"stroke"`
	Group  *string `json:"group"`
	Offset *struct {
		FromDX *int `json:"from_dx"`
		FromDY *int `json:"from_dy"`
		ToDX   *int `json:"to_dx"`
		ToDY   *int `json:"to_dy"`
	} `json:"offset"`
	PhaseRank      *int `json:"phase_rank"`
	OrderNo        *int `json:"order_no"`
	WeightPermille *int `json:"weight_permille"`
	Radius         *int `json:"radius_px"`
}

type rawV3 struct {
	Version string   `json:"version"`
	Meta    *rawMeta `json:"meta"`
	Canvas  *struct {
		Width  *int `json:"width"`
		Height *int `json:"height"`
	} `json:"canvas"`
	Candidate       *rawMask     `json:"candidate_mask"`
	ConstructGroups []rawV3Group `json:"construct_groups"`
	Truth           *rawTruth    `json:"truth"`
}

type rawV3Group struct {
	Name       *string       `json:"name"`
	Stage      *int          `json:"stage"`
	Constructs []rawV3Stroke `json:"constructs"`
}

type rawV3Stroke struct {
	ID   *string `json:"id"`
	From *struct {
		X *int `json:"x"`
		Y *int `json:"y"`
	} `json:"from"`
	To *struct {
		X *int `json:"x"`
		Y *int `json:"y"`
	} `json:"to"`
	Radius   *int `json:"radius_px"`
	Hardness *int `json:"hardness_permille"`
	Weight   *int `json:"weight_pp10000"`
	Seq      *int `json:"seq"`
}

// LoadProjectJSON dispatches by the explicit top-level version field. It never
// guesses a schema and rejects unknown versions with E_VER.
func LoadProjectJSON(data []byte) (*Project, error) {
	var env struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, withDetails(CodeVersion, "invalid project JSON: %v", nil, err)
	}
	if env.Version == "" {
		return nil, coded(CodeVersion, "required field version is missing")
	}
	var p *Project
	var err error
	switch env.Version {
	case Version1:
		p, err = loadV1(data)
	case Version2:
		p, err = loadV2(data)
	case Version3:
		p, err = loadV3(data)
	default:
		return nil, coded(CodeVersion, "unknown project version %q", env.Version)
	}
	if err != nil {
		return nil, err
	}
	p.SourceVersion = env.Version
	h := sha256.Sum256(data)
	p.SourceDocument = hex.EncodeToString(h[:8])
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1(data []byte) (*Project, error) {
	var raw rawV1
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, withDetails(CodeVersion, "v1 JSON shape error: %v", nil, err)
	}
	width, height, err := requireCanvasV1(&raw)
	if err != nil {
		return nil, err
	}
	meta, err := requireMeta(raw.Meta)
	if err != nil {
		return nil, err
	}
	if raw.Candidate == nil {
		return nil, coded(CodeVersion, "required field candidate_mask is missing")
	}
	candidate, err := decodeRLE("candidate_mask.runs", raw.Candidate.Runs, width*height)
	if err != nil {
		return nil, err
	}
	if len(raw.Operations) == 0 {
		return nil, coded(CodeVersion, "required field operations is missing or empty")
	}
	truth, err := decodeTruth("truth", raw.Truth, width, height)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: meta.ID, Seed: meta.Seed, Name: meta.Name,
		Width: width, Height: height, BlockSize: meta.BlockSize,
		Rounding: meta.Rounding, CandidateMask: candidate, Truth: truth,
		ConversionTrace:   append([]ConversionTrace(nil), meta.Trace...),
		ConversionVersion: ConversionVersion,
	}
	p.ConversionTrace = append(p.ConversionTrace, ConversionTrace{
		Version: Version1, Path: "groups", OldValue: "<missing>", NewValue: "stage-derived",
		Rule: "v1.schema.synthesize_group_from_priority", SourceDefault: "stage-<priority>",
		Location: "operations[].group", Residual: 0,
	})
	groupStages := make(map[string]int)
	for i, op := range raw.Operations {
		path := fmt.Sprintf("operations[%d]", i)
		if op.ID == nil || *op.ID == "" {
			return nil, coded(CodeVersion, "%s.operation_id is required", path)
		}
		if op.Priority == nil {
			return nil, coded(CodeVersion, "%s.priority is required", path)
		}
		if op.Radius == nil {
			return nil, coded(CodeVersion, "%s.radius_px is required", path)
		}
		if len(op.From) != 2 || len(op.To) != 2 {
			return nil, coded(CodeVersion, "%s.from_px/to_px must each have two integers", path)
		}
		weightPermille := 1000
		sourceDefault := "v1.schema.default_weight_permille=1000"
		weightOld := any("<missing>")
		if op.WeightPermille != nil {
			weightPermille = *op.WeightPermille
			sourceDefault = ""
			weightOld = weightPermille
		}
		weight, err := PermilleToWeight(int64(weightPermille))
		if err != nil {
			return nil, err
		}
		groupName := fmt.Sprintf("stage-%d", *op.Priority)
		groupStages[groupName] = *op.Priority
		p.ConversionTrace = append(p.ConversionTrace,
			fieldTrace(Version1, path+".from_px.x", op.From[0], op.From[0], "v1.absolute_pixel_identity", ""),
			fieldTrace(Version1, path+".from_px.y", op.From[1], op.From[1], "v1.absolute_pixel_identity", ""),
			fieldTrace(Version1, path+".to_px.x", op.To[0], op.To[0], "v1.absolute_pixel_identity", ""),
			fieldTrace(Version1, path+".to_px.y", op.To[1], op.To[1], "v1.absolute_pixel_identity", ""),
			fieldTrace(Version1, path+".priority", *op.Priority, *op.Priority, "v1.stage_identity", ""),
			fieldTrace(Version1, path+".radius_px", *op.Radius, *op.Radius, "v1.radius_identity", ""),
			fieldTrace(Version1, path+".weight_permille", weightOld, weight, "v1.weight_permille_to_pp10000", sourceDefault),
			fieldTrace(Version1, path+".hardness_permille", "<missing>", 1000, "v1.schema.default_hardness", "1000"),
			fieldTrace(Version1, path+".group", "<missing>", groupName, "v1.schema.synthesize_group_from_priority", groupName),
		)
		seq := i + 1
		if op.Seq != nil {
			seq = *op.Seq + 1
			p.ConversionTrace = append(p.ConversionTrace,
				fieldTrace(Version1, path+".step_seq", *op.Seq, seq, "v1.zero_based_seq_to_one_based", ""))
		} else {
			p.ConversionTrace = append(p.ConversionTrace,
				fieldTrace(Version1, path+".step_seq", "<missing>", seq, "v1.schema.default_seq_from_declaration", fmt.Sprintf("%d", seq)))
		}
		p.Strokes = append(p.Strokes, Stroke{
			ID: *op.ID, Group: groupName, DeclOrder: i,
			Start:  Point{X: op.From[0], Y: op.From[1]},
			End:    Point{X: op.To[0], Y: op.To[1]},
			Radius: *op.Radius, Hardness: 1000, Weight: int(weight),
			Stage: *op.Priority, Seq: seq, SourcePath: path,
		})
	}
	for name, stage := range groupStages {
		p.Groups = append(p.Groups, Group{Name: name, Stage: stage})
	}
	sort.Slice(p.Groups, func(i, j int) bool { return p.Groups[i].Name < p.Groups[j].Name })
	return p, nil
}

func requireCanvasV1(raw *rawV1) (int, int, error) {
	if raw.Width == nil {
		return 0, 0, coded(CodeVersion, "required field width is missing")
	}
	if raw.Height == nil {
		return 0, 0, coded(CodeVersion, "required field height is missing")
	}
	if *raw.Width <= 0 || *raw.Height <= 0 {
		return 0, 0, coded(CodeVersion, "invalid canvas %dx%d", *raw.Width, *raw.Height)
	}
	return *raw.Width, *raw.Height, nil
}

func loadV2(data []byte) (*Project, error) {
	var raw rawV2
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, withDetails(CodeVersion, "v2 JSON shape error: %v", nil, err)
	}
	if raw.Canvas == nil || raw.Canvas.Width == nil || raw.Canvas.Height == nil {
		return nil, coded(CodeVersion, "required v2 canvas.w/canvas.h is missing")
	}
	width, height := *raw.Canvas.Width, *raw.Canvas.Height
	if width <= 0 || height <= 0 {
		return nil, coded(CodeVersion, "invalid canvas %dx%d", width, height)
	}
	meta, err := requireMeta(raw.Meta)
	if err != nil {
		return nil, err
	}
	if raw.Candidate == nil {
		return nil, coded(CodeVersion, "required field candidate is missing")
	}
	candidate, err := decodeRLE("candidate.runs", raw.Candidate.Runs, width*height)
	if err != nil {
		return nil, err
	}
	truth, err := decodeTruth("truth", raw.Truth, width, height)
	if err != nil {
		return nil, err
	}
	if len(raw.BrushOps) == 0 {
		return nil, coded(CodeVersion, "required field brush_ops is missing or empty")
	}
	quantum := 2
	if meta.Quantum != nil {
		quantum = *meta.Quantum
	}
	if quantum <= 0 || quantum > 1024 {
		return nil, coded(CodeVersion, "invalid coordinate quantum %d", quantum)
	}
	p := &Project{
		ID: meta.ID, Seed: meta.Seed, Name: meta.Name,
		Width: width, Height: height, BlockSize: meta.BlockSize,
		Rounding: meta.Rounding, CandidateMask: candidate, Truth: truth,
		ConversionTrace:   append([]ConversionTrace(nil), meta.Trace...),
		ConversionVersion: ConversionVersion,
	}
	groupStage := make(map[string]int)
	anchor := Point{X: 0, Y: 0}
	for i, op := range raw.BrushOps {
		path := fmt.Sprintf("brush_ops[%d]", i)
		if op.Stroke == nil || *op.Stroke == "" {
			return nil, coded(CodeVersion, "%s.stroke is required", path)
		}
		if op.Offset == nil {
			return nil, coded(CodeVersion, "%s.offset is required", path)
		}
		if op.Offset.FromDX == nil || op.Offset.FromDY == nil ||
			op.Offset.ToDX == nil || op.Offset.ToDY == nil {
			return nil, coded(CodeVersion, "%s.offset needs from_dx/from_dy/to_dx/to_dy", path)
		}
		if op.PhaseRank == nil {
			return nil, coded(CodeVersion, "%s.phase_rank is required", path)
		}
		if op.Radius == nil {
			return nil, coded(CodeVersion, "%s.radius_px is required", path)
		}
		group := "default"
		groupDefault := "v2.schema.default_group=default"
		groupOld := any("<missing>")
		if op.Group != nil && *op.Group != "" {
			group = *op.Group
			groupDefault = ""
			groupOld = group
		}
		fromX, err := StrokedOffsetToAbs(path+".offset.from_dx", int64(*op.Offset.FromDX), int64(anchor.X), int64(quantum))
		if err != nil {
			return nil, err
		}
		fromY, err := StrokedOffsetToAbs(path+".offset.from_dy", int64(*op.Offset.FromDY), int64(anchor.Y), int64(quantum))
		if err != nil {
			return nil, err
		}
		toX, err := StrokedOffsetToAbs(path+".offset.to_dx", int64(*op.Offset.ToDX), int64(anchor.X), int64(quantum))
		if err != nil {
			return nil, err
		}
		toY, err := StrokedOffsetToAbs(path+".offset.to_dy", int64(*op.Offset.ToDY), int64(anchor.Y), int64(quantum))
		if err != nil {
			return nil, err
		}
		weightPermille := 1000
		weightDefault := "v2.schema.default_weight_permille=1000"
		weightOld := any("<missing>")
		if op.WeightPermille != nil {
			weightPermille = *op.WeightPermille
			weightDefault = ""
			weightOld = weightPermille
		}
		weight, err := PermilleToWeight(int64(weightPermille))
		if err != nil {
			return nil, err
		}
		seq := i + 1
		seqRule := "v2.schema.default_order_from_declaration"
		seqOld := "<missing>"
		if op.OrderNo != nil {
			seq = *op.OrderNo
			seqRule = "v2.order_no_identity"
			seqOld = fmt.Sprintf("%d", seq)
		}
		p.ConversionTrace = append(p.ConversionTrace,
			fieldTrace(Version2, path+".offset.from_dx", *op.Offset.FromDX,
				fmt.Sprintf("%d", fromX), fmt.Sprintf("v2.relative_quantum=%d", quantum), ""),
			fieldTrace(Version2, path+".offset.from_dy", *op.Offset.FromDY,
				fmt.Sprintf("%d", fromY), fmt.Sprintf("v2.relative_quantum=%d", quantum), ""),
			fieldTrace(Version2, path+".offset.to_dx", *op.Offset.ToDX,
				fmt.Sprintf("%d", toX), fmt.Sprintf("v2.relative_quantum=%d", quantum), ""),
			fieldTrace(Version2, path+".offset.to_dy", *op.Offset.ToDY,
				fmt.Sprintf("%d", toY), fmt.Sprintf("v2.relative_quantum=%d", quantum), ""),
			fieldTrace(Version2, path+".phase_rank", *op.PhaseRank, *op.PhaseRank, "v2.stage_identity", ""),
			fieldTrace(Version2, path+".radius_px", *op.Radius, *op.Radius, "v2.radius_identity", ""),
			fieldTrace(Version2, path+".weight_permille", weightOld, weight,
				"v2.weight_permille_to_pp10000", weightDefault),
			fieldTrace(Version2, path+".hardness_permille", "<missing>", 1000,
				"v2.schema.default_hardness", "1000"),
			fieldTrace(Version2, path+".group", groupOld, group, "v2.schema.group", groupDefault),
			fieldTrace(Version2, path+".order_no", seqOld, seq, seqRule, ""),
		)
		if _, ok := groupStage[group]; !ok {
			groupStage[group] = *op.PhaseRank
		}
		p.Strokes = append(p.Strokes, Stroke{
			ID: *op.Stroke, Group: group, DeclOrder: i,
			Start:  Point{X: int(fromX), Y: int(fromY)},
			End:    Point{X: int(toX), Y: int(toY)},
			Radius: *op.Radius, Hardness: 1000, Weight: int(weight),
			Stage: *op.PhaseRank, Seq: seq, SourcePath: path,
		})
		anchor = Point{X: int(toX), Y: int(toY)}
	}
	for name, stage := range groupStage {
		p.Groups = append(p.Groups, Group{Name: name, Stage: stage})
	}
	sort.Slice(p.Groups, func(i, j int) bool { return p.Groups[i].Name < p.Groups[j].Name })
	return p, nil
}

func loadV3(data []byte) (*Project, error) {
	var raw rawV3
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, withDetails(CodeVersion, "v3 JSON shape error: %v", nil, err)
	}
	if raw.Canvas == nil || raw.Canvas.Width == nil || raw.Canvas.Height == nil {
		return nil, coded(CodeVersion, "required v3 canvas.width/canvas.height is missing")
	}
	width, height := *raw.Canvas.Width, *raw.Canvas.Height
	if width <= 0 || height <= 0 {
		return nil, coded(CodeVersion, "invalid canvas %dx%d", width, height)
	}
	meta, err := requireMeta(raw.Meta)
	if err != nil {
		return nil, err
	}
	if raw.Candidate == nil {
		return nil, coded(CodeVersion, "required field candidate_mask is missing")
	}
	candidate, err := decodeRLE("candidate_mask.runs", raw.Candidate.Runs, width*height)
	if err != nil {
		return nil, err
	}
	truth, err := decodeTruth("truth", raw.Truth, width, height)
	if err != nil {
		return nil, err
	}
	if len(raw.ConstructGroups) == 0 {
		return nil, coded(CodeVersion, "required field construct_groups is missing or empty")
	}
	p := &Project{
		ID: meta.ID, Seed: meta.Seed, Name: meta.Name,
		Width: width, Height: height, BlockSize: meta.BlockSize,
		Rounding: meta.Rounding, CandidateMask: candidate, Truth: truth,
		ConversionTrace:   append([]ConversionTrace(nil), meta.Trace...),
		ConversionVersion: ConversionVersion,
	}
	decl := 0
	for gi, g := range raw.ConstructGroups {
		gpath := fmt.Sprintf("construct_groups[%d]", gi)
		if g.Name == nil || *g.Name == "" {
			return nil, coded(CodeVersion, "%s.name is required", gpath)
		}
		if g.Stage == nil {
			return nil, coded(CodeVersion, "%s.stage is required", gpath)
		}
		if len(g.Constructs) == 0 {
			return nil, coded(CodeVersion, "%s.constructs is required or empty", gpath)
		}
		p.Groups = append(p.Groups, Group{Name: *g.Name, Stage: *g.Stage})
		for si, s := range g.Constructs {
			path := fmt.Sprintf("%s.constructs[%d]", gpath, si)
			if s.ID == nil || *s.ID == "" {
				return nil, coded(CodeVersion, "%s.id is required", path)
			}
			if s.From == nil || s.From.X == nil || s.From.Y == nil ||
				s.To == nil || s.To.X == nil || s.To.Y == nil {
				return nil, coded(CodeVersion, "%s.from/to coordinates are required", path)
			}
			if s.Radius == nil || s.Hardness == nil || s.Weight == nil || s.Seq == nil {
				return nil, coded(CodeVersion, "%s radius_px/hardness_permille/weight_pp10000/seq are required", path)
			}
			p.Strokes = append(p.Strokes, Stroke{
				ID: *s.ID, Group: *g.Name, DeclOrder: decl,
				Start:  Point{X: *s.From.X, Y: *s.From.Y},
				End:    Point{X: *s.To.X, Y: *s.To.Y},
				Radius: *s.Radius, Hardness: *s.Hardness, Weight: *s.Weight,
				Stage: *g.Stage, Seq: *s.Seq, SourcePath: path,
			})
			p.ConversionTrace = append(p.ConversionTrace,
				fieldTrace(Version3, path+".from", fmt.Sprintf("%d,%d", *s.From.X, *s.From.Y),
					fmt.Sprintf("%d,%d", *s.From.X, *s.From.Y), "v3.absolute_pixel_identity", ""),
				fieldTrace(Version3, path+".to", fmt.Sprintf("%d,%d", *s.To.X, *s.To.Y),
					fmt.Sprintf("%d,%d", *s.To.X, *s.To.Y), "v3.absolute_pixel_identity", ""),
				fieldTrace(Version3, path+".radius_px", *s.Radius, *s.Radius, "v3.radius_identity", ""),
				fieldTrace(Version3, path+".hardness_permille", *s.Hardness, *s.Hardness, "v3.hardness_identity", ""),
				fieldTrace(Version3, path+".weight_pp10000", *s.Weight, *s.Weight, "v3.weight_identity", ""),
				fieldTrace(Version3, path+".seq", *s.Seq, *s.Seq, "v3.sequence_identity", ""),
				fieldTrace(Version3, path+".stage", *g.Stage, *g.Stage, "v3.group_stage_identity", ""),
			)
			decl++
		}
	}
	return p, nil
}

type requiredMeta struct {
	ID        string
	Name      string
	Seed      int64
	BlockSize int
	Rounding  RoundingMode
	Trace     []ConversionTrace
	Quantum   *int
}

func requireMeta(meta *rawMeta) (requiredMeta, error) {
	if meta == nil {
		return requiredMeta{}, coded(CodeVersion, "required field meta is missing")
	}
	if meta.ProjectID == nil || *meta.ProjectID == "" {
		return requiredMeta{}, coded(CodeVersion, "required field meta.project_id is missing")
	}
	blockSize := DefaultBlockSize
	if meta.BlockSize != nil {
		blockSize = *meta.BlockSize
	}
	if blockSize <= 0 {
		return requiredMeta{}, coded(CodeVersion, "meta.block_size must be positive")
	}
	rounding := RoundHalfAwayFromZero
	if meta.Rounding != "" {
		rounding = RoundingMode(meta.Rounding)
	}
	if !rounding.valid() {
		return requiredMeta{}, coded(CodeVersion, "unknown meta.rounding %q", meta.Rounding)
	}
	return requiredMeta{
		ID: *meta.ProjectID, Name: meta.Name, Seed: meta.Seed,
		BlockSize: blockSize, Rounding: rounding, Trace: meta.ConversionTrace,
		Quantum: meta.CoordinateQuantum,
	}, nil
}

func fieldTrace(version, path string, oldValue, newValue any, rule, sourceDefault string) ConversionTrace {
	return ConversionTrace{
		Version: version, Path: path,
		OldValue: fmt.Sprint(oldValue), NewValue: fmt.Sprint(newValue),
		Rule: rule, SourceDefault: sourceDefault,
		Residual: 0,
	}
}

func decodeTruth(field string, raw *rawTruth, width, height int) (*Truth, error) {
	if raw == nil {
		return nil, coded(CodeVersion, "required field %s is missing", field)
	}
	if len(raw.ReferenceRuns) == 0 {
		return nil, coded(CodeVersion, "required field %s.reference_runs is missing", field)
	}
	if len(raw.Domains) == 0 {
		return nil, coded(CodeVersion, "required field %s.domains is missing", field)
	}
	if raw.Contributions == nil {
		return nil, coded(CodeVersion, "required field %s.contributions is missing", field)
	}
	mask, err := decodeRLE(field+".reference_runs", raw.ReferenceRuns, width*height)
	if err != nil {
		return nil, err
	}
	t := &Truth{ReferenceMask: mask}
	for _, d := range raw.Domains {
		if d.ID <= 0 || d.Area <= 0 {
			return nil, coded(CodeVersion, "%s.domains has invalid id/area", field)
		}
		t.Domains = append(t.Domains, TruthDomain{
			ID: d.ID, Area: d.Area, CentroidXMilli: d.CentroidXMilli,
			CentroidYMilli: d.CentroidYMilli, BBox: d.BBox, Holes: d.Holes,
			RingWidthMilli: d.RingWidthMilli, BoundaryPixels: d.BoundaryPixels,
		})
	}
	for _, c := range raw.Contributions {
		if c.StrokeID == "" {
			return nil, coded(CodeVersion, "%s.contributions[].stroke_id is required", field)
		}
		cm, err := decodeRLE(field+".contributions["+c.StrokeID+"].runs", c.Runs, width*height)
		if err != nil {
			return nil, err
		}
		t.Contributions = append(t.Contributions, TruthContribution{
			StrokeID: c.StrokeID, StrokeSeq: c.StrokeSeq, Mask: cm,
		})
	}
	return t, nil
}

type ExportOptions struct {
	AllowLossy      bool
	VerifyRoundTrip bool
}

type ExportReport struct {
	TargetVersion string            `json:"target_version"`
	Losses        []LossItem        `json:"losses,omitempty"`
	Traces        []ConversionTrace `json:"traces"`
	RoundTripOK   bool              `json:"round_trip_ok"`
}

// ExportProject serializes to the requested version. Strict mode (the zero
// value) returns E_LOSSY when any target field cannot preserve the internal
// value. Successful exports are read back and compared before being returned.
func ExportProject(p *Project, target string, opts ExportOptions) ([]byte, *ExportReport, error) {
	if p == nil {
		return nil, nil, coded(CodeVersion, "project is nil")
	}
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	if target != Version1 && target != Version2 && target != Version3 {
		return nil, nil, coded(CodeVersion, "unknown export version %q", target)
	}
	cloned := p.Clone()
	report := &ExportReport{TargetVersion: target}
	var data []byte
	var err error
	switch target {
	case Version1:
		data, err = exportV1(cloned, report)
	case Version2:
		data, err = exportV2(cloned, report)
	case Version3:
		data, err = exportV3(cloned, report)
	}
	if err != nil {
		return nil, report, err
	}
	if len(report.Losses) > 0 && !opts.AllowLossy {
		return nil, report, withDetails(CodeLossy,
			"target %s cannot represent %d internal fields", report, target, len(report.Losses))
	}
	if opts.VerifyRoundTrip && len(report.Losses) == 0 {
		loaded, loadErr := LoadProjectJSON(data)
		if loadErr != nil {
			return nil, report, &CodedError{
				Code: CodeRoundTrip, Message: "exported document cannot be loaded back", Err: loadErr,
			}
		}
		if mismatch := equivalentForTarget(p, loaded, target); mismatch != "" {
			return nil, report, coded(CodeRoundTrip, "round-trip mismatch at %s", mismatch)
		}
		report.RoundTripOK = true
	}
	return data, report, nil
}

func exportV1(p *Project, report *ExportReport) ([]byte, error) {
	for _, s := range p.Strokes {
		canonicalGroup := fmt.Sprintf("stage-%d", s.Stage)
		if s.Group != "" && s.Group != "default" && s.Group != canonicalGroup {
			report.Losses = append(report.Losses, LossItem{
				Code: "group_structure_not_expressible", Path: "strokes[" + s.ID + "].group",
				Reason: "v1 has no group field", Value: s.Group,
				Disposition: "export v2/v3 or normalize all strokes to group=default",
			})
		}
		if s.Hardness != 1000 {
			report.Losses = append(report.Losses, LossItem{
				Code: "hardness_not_expressible", Path: "strokes[" + s.ID + "].hardness_permille",
				Reason:      "v1 has no hardness field and declares default 1000",
				Value:       fmt.Sprintf("%d", s.Hardness),
				Disposition: "set hardness=1000 or export v3",
			})
		}
	}
	type outOp struct {
		ID             string `json:"operation_id"`
		From           []int  `json:"from_px"`
		To             []int  `json:"to_px"`
		Priority       int    `json:"priority"`
		Seq            int    `json:"step_seq"`
		WeightPermille int    `json:"weight_permille"`
		Radius         int    `json:"radius_px"`
	}
	var ops []outOp
	for i, s := range p.Strokes {
		weight, residual, err := WeightToPermille(int64(s.Weight), p.Rounding)
		if err != nil {
			return nil, err
		}
		if residual != 0 {
			report.Losses = append(report.Losses, LossItem{
				Code: "weight_scale_truncation", Path: fmt.Sprintf("strokes[%d].weight_pp10000", i),
				Reason: "per-mille target cannot represent internal per-ten-thousand weight",
				Value:  fmt.Sprintf("%d", s.Weight), Residual: residual,
				Disposition: "round weight to a multiple of 10 or export v3",
			})
		}
		report.Traces = append(report.Traces,
			fieldTrace(Version1, fmt.Sprintf("strokes[%d].from_px", i), s.Start,
				[]int{s.Start.X, s.Start.Y}, "internal_absolute_to_v1_absolute", ""),
			fieldTrace(Version1, fmt.Sprintf("strokes[%d].to_px", i), s.End,
				[]int{s.End.X, s.End.Y}, "internal_absolute_to_v1_absolute", ""),
			ConversionTrace{
				Version: Version1, Path: fmt.Sprintf("strokes[%d].weight_permille", i),
				OldValue: fmt.Sprintf("%d", s.Weight), NewValue: fmt.Sprintf("%d", weight),
				Rule: "weight_pp10000_to_permille", Residual: residual,
			},
			ConversionTrace{
				Version: Version1, Path: fmt.Sprintf("strokes[%d].hardness_permille", i),
				OldValue: fmt.Sprintf("%d", s.Hardness), NewValue: "1000",
				Rule: "v1.schema.default_hardness", SourceDefault: "1000",
				Residual: int64(s.Hardness - 1000),
			},
			ConversionTrace{
				Version: Version1, Path: fmt.Sprintf("strokes[%d].step_seq", i),
				OldValue: fmt.Sprintf("%d", s.Seq), NewValue: fmt.Sprintf("%d", s.Seq-1),
				Rule: "one_based_to_v1_zero_based_seq",
			},
		)
		ops = append(ops, outOp{
			ID: s.ID, From: []int{s.Start.X, s.Start.Y}, To: []int{s.End.X, s.End.Y},
			Priority: s.Stage, Seq: s.Seq - 1, WeightPermille: int(weight), Radius: s.Radius,
		})
	}
	doc := map[string]any{
		"version":        Version1,
		"meta":           exportMeta(p, report),
		"width":          p.Width,
		"height":         p.Height,
		"candidate_mask": map[string]any{"runs": encodeRLE(p.CandidateMask)},
		"operations":     ops,
		"truth":          exportTruth(p.Truth),
	}
	return marshalJSON(doc)
}

func exportV2(p *Project, report *ExportReport) ([]byte, error) {
	const quantum = 2
	type outOffset struct {
		FromDX int `json:"from_dx"`
		FromDY int `json:"from_dy"`
		ToDX   int `json:"to_dx"`
		ToDY   int `json:"to_dy"`
	}
	type outOp struct {
		Stroke         string    `json:"stroke"`
		Group          string    `json:"group"`
		Offset         outOffset `json:"offset"`
		PhaseRank      int       `json:"phase_rank"`
		OrderNo        int       `json:"order_no"`
		WeightPermille int       `json:"weight_permille"`
		Radius         int       `json:"radius_px"`
	}
	var ops []outOp
	anchor := Point{}
	for i, s := range p.Strokes {
		fields := []struct {
			name string
			abs  int
			anc  int
		}{
			{"offset.from_dx", s.Start.X, anchor.X},
			{"offset.from_dy", s.Start.Y, anchor.Y},
			{"offset.to_dx", s.End.X, anchor.X},
			{"offset.to_dy", s.End.Y, anchor.Y},
		}
		encoded := make([]int, 4)
		for j, f := range fields {
			v, _, residual, err := AbsToStrokedOffset(fmt.Sprintf("strokes[%d].%s", i, f.name),
				int64(f.abs), int64(f.anc), quantum, p.Rounding)
			if err != nil {
				return nil, err
			}
			if residual != 0 {
				report.Losses = append(report.Losses, LossItem{
					Code:   "relative_coordinate_quantization",
					Path:   fmt.Sprintf("strokes[%d].%s", i, f.name),
					Reason: fmt.Sprintf("v2 relative coordinate quantum is %d", quantum),
					Value:  fmt.Sprintf("%d", f.abs), Residual: residual,
					Disposition: "align coordinates to the quantum or export v3",
				})
			}
			report.Traces = append(report.Traces, ConversionTrace{
				Version: Version2, Path: fmt.Sprintf("strokes[%d].%s", i, f.name),
				OldValue: fmt.Sprintf("%d", f.abs), NewValue: fmt.Sprintf("%d", v),
				Rule:     fmt.Sprintf("absolute_to_previous_end_quantum_%d", quantum),
				Residual: residual,
			})
			encoded[j] = int(v)
		}
		weight, residual, err := WeightToPermille(int64(s.Weight), p.Rounding)
		if err != nil {
			return nil, err
		}
		if residual != 0 {
			report.Losses = append(report.Losses, LossItem{
				Code: "weight_scale_truncation", Path: fmt.Sprintf("strokes[%d].weight_pp10000", i),
				Reason: "per-mille target cannot represent internal per-ten-thousand weight",
				Value:  fmt.Sprintf("%d", s.Weight), Residual: residual,
				Disposition: "round weight to a multiple of 10 or export v3",
			})
		}
		if s.Hardness != 1000 {
			report.Losses = append(report.Losses, LossItem{
				Code: "hardness_not_expressible", Path: fmt.Sprintf("strokes[%d].hardness_permille", i),
				Reason:      "v2 omits hardness and declares default 1000",
				Value:       fmt.Sprintf("%d", s.Hardness),
				Disposition: "set hardness=1000 or export v3",
			})
		}
		ops = append(ops, outOp{
			Stroke: s.ID, Group: s.Group, PhaseRank: s.Stage, OrderNo: s.Seq,
			WeightPermille: int(weight), Radius: s.Radius,
			Offset: outOffset{FromDX: encoded[0], FromDY: encoded[1], ToDX: encoded[2], ToDY: encoded[3]},
		})
		anchor = s.End
	}
	meta := exportMeta(p, report)
	meta["coordinate_quantum"] = quantum
	doc := map[string]any{
		"version":   Version2,
		"meta":      meta,
		"canvas":    map[string]any{"w": p.Width, "h": p.Height},
		"candidate": map[string]any{"runs": encodeRLE(p.CandidateMask)},
		"brush_ops": ops,
		"truth":     exportTruth(p.Truth),
	}
	return marshalJSON(doc)
}

func exportV3(p *Project, report *ExportReport) ([]byte, error) {
	type outPoint struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	type outStroke struct {
		ID       string   `json:"id"`
		From     outPoint `json:"from"`
		To       outPoint `json:"to"`
		Radius   int      `json:"radius_px"`
		Hardness int      `json:"hardness_permille"`
		Weight   int      `json:"weight_pp10000"`
		Seq      int      `json:"seq"`
	}
	type outGroup struct {
		Name       string      `json:"name"`
		Stage      int         `json:"stage"`
		Constructs []outStroke `json:"constructs"`
	}
	groupIndex := make(map[string]int)
	var groups []outGroup
	for _, g := range p.Groups {
		groupIndex[g.Name] = len(groups)
		groups = append(groups, outGroup{Name: g.Name, Stage: g.Stage})
	}
	for i, s := range p.Strokes {
		gi, ok := groupIndex[s.Group]
		if !ok {
			groups = append(groups, outGroup{Name: s.Group, Stage: s.Stage})
			gi = len(groups) - 1
			groupIndex[s.Group] = gi
		}
		if groups[gi].Stage != s.Stage {
			report.Losses = append(report.Losses, LossItem{
				Code: "group_stage_conflict", Path: fmt.Sprintf("strokes[%d].stage", i),
				Reason:      "v3 stores one stage per group",
				Value:       fmt.Sprintf("group=%d stroke=%d", groups[gi].Stage, s.Stage),
				Disposition: "normalize strokes in a group to the same stage",
			})
		}
		report.Traces = append(report.Traces,
			fieldTrace(Version3, fmt.Sprintf("strokes[%d].from", i), s.Start,
				fmt.Sprintf("%d,%d", s.Start.X, s.Start.Y), "internal_absolute_identity", ""),
			fieldTrace(Version3, fmt.Sprintf("strokes[%d].to", i), s.End,
				fmt.Sprintf("%d,%d", s.End.X, s.End.Y), "internal_absolute_identity", ""),
			fieldTrace(Version3, fmt.Sprintf("strokes[%d].weight_pp10000", i), s.Weight,
				s.Weight, "internal_weight_identity", ""),
		)
		groups[gi].Constructs = append(groups[gi].Constructs, outStroke{
			ID: s.ID, From: outPoint{X: s.Start.X, Y: s.Start.Y},
			To: outPoint{X: s.End.X, Y: s.End.Y}, Radius: s.Radius,
			Hardness: s.Hardness, Weight: s.Weight, Seq: s.Seq,
		})
	}
	doc := map[string]any{
		"version":          Version3,
		"meta":             exportMeta(p, report),
		"canvas":           map[string]any{"width": p.Width, "height": p.Height},
		"candidate_mask":   map[string]any{"runs": encodeRLE(p.CandidateMask)},
		"construct_groups": groups,
		"truth":            exportTruth(p.Truth),
	}
	return marshalJSON(doc)
}

func exportMeta(p *Project, report *ExportReport) map[string]any {
	traces := append([]ConversionTrace(nil), p.ConversionTrace...)
	traces = append(traces, report.Traces...)
	return map[string]any{
		"project_id": p.ID, "name": p.Name, "seed": p.Seed,
		"block_size": p.BlockSize, "rounding": string(p.Rounding),
		"conversion_trace": traces,
	}
}

func exportTruth(t *Truth) map[string]any {
	domains := make([]map[string]any, 0, len(t.Domains))
	for _, d := range t.Domains {
		domains = append(domains, map[string]any{
			"id": d.ID, "area": d.Area,
			"centroid_x_milli": d.CentroidXMilli, "centroid_y_milli": d.CentroidYMilli,
			"bbox": d.BBox, "holes": d.Holes, "ring_width_milli": d.RingWidthMilli,
			"boundary_pixels": d.BoundaryPixels,
		})
	}
	contrib := make([]map[string]any, 0, len(t.Contributions))
	for _, c := range t.Contributions {
		contrib = append(contrib, map[string]any{
			"stroke_id": c.StrokeID, "stroke_seq": c.StrokeSeq, "runs": encodeRLE(c.Mask),
		})
	}
	return map[string]any{
		"reference_runs": encodeRLE(t.ReferenceMask),
		"domains":        domains, "contributions": contrib,
	}
}

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal project: %w", err)
	}
	return append(data, '\n'), nil
}

func equivalentForTarget(a, b *Project, target string) string {
	if a.Width != b.Width || a.Height != b.Height {
		return "canvas"
	}
	if !reflect.DeepEqual(a.CandidateMask, b.CandidateMask) {
		return "candidate_mask"
	}
	if len(a.Strokes) != len(b.Strokes) {
		return "strokes.length"
	}
	for i := range a.Strokes {
		x, y := a.Strokes[i], b.Strokes[i]
		if x.ID != y.ID || x.Start != y.Start || x.End != y.End ||
			x.Radius != y.Radius || x.Weight != y.Weight ||
			x.Stage != y.Stage || x.Seq != y.Seq {
			return fmt.Sprintf("strokes[%d]", i)
		}
		if target != Version1 && x.Group != y.Group {
			return fmt.Sprintf("strokes[%d].group", i)
		}
		if target == Version3 && x.Hardness != y.Hardness {
			return fmt.Sprintf("strokes[%d].hardness", i)
		}
		if target != Version3 && y.Hardness != 1000 {
			return fmt.Sprintf("strokes[%d].hardness", i)
		}
	}
	if target == Version3 {
		am := make(map[string]int)
		bm := make(map[string]int)
		for _, g := range a.Groups {
			am[g.Name] = g.Stage
		}
		for _, g := range b.Groups {
			bm[g.Name] = g.Stage
		}
		if !reflect.DeepEqual(am, bm) {
			return "groups"
		}
	}
	if a.Truth != nil && b.Truth != nil {
		if !reflect.DeepEqual(a.Truth.Domains, b.Truth.Domains) ||
			!reflect.DeepEqual(a.Truth.ReferenceMask, b.Truth.ReferenceMask) {
			return "truth"
		}
		if len(a.Truth.Contributions) != len(b.Truth.Contributions) {
			return "truth.contributions"
		}
		for i := range a.Truth.Contributions {
			x, y := a.Truth.Contributions[i], b.Truth.Contributions[i]
			if x.StrokeID != y.StrokeID || x.StrokeSeq != y.StrokeSeq ||
				!reflect.DeepEqual(x.Mask, y.Mask) {
				return fmt.Sprintf("truth.contributions[%d]", i)
			}
		}
	}
	return ""
}

func VersionShort(v string) string {
	return strings.TrimPrefix(v, "mask-work/")
}
