package grid

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type ExportArtifact struct {
	Version    int              `json:"version"`
	Data       json.RawMessage  `json:"data"`
	Report     ConversionReport `json:"report"`
	Normalized *Level           `json:"-"`
}

// ExportVersion applies the declared degradation rules, records every lossy
// representation, and emits the requested schema JSON.
func ExportVersion(src *Level, version int) (*ExportArtifact, error) {
	n := cloneLevel(src)
	rep := ConversionReport{SourceSchema: 3, TargetSchema: version}

	if err := normalizeCommonIntegers(n, &rep); err != nil {
		return nil, err
	}

	switch version {
	case 1:
		normalizeV1(n, &rep)
		data, err := json.MarshalIndent(buildV1(n), "", "  ")
		if err != nil {
			return nil, err
		}
		return &ExportArtifact{Version: 1, Data: append(data, '\n'), Report: rep, Normalized: n}, nil
	case 2:
		normalizeV2(n, &rep)
		data, err := json.MarshalIndent(buildV2(n), "", "  ")
		if err != nil {
			return nil, err
		}
		return &ExportArtifact{Version: 2, Data: append(data, '\n'), Report: rep, Normalized: n}, nil
	case 3:
		doc, err := buildV3(n)
		if err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, err
		}
		return &ExportArtifact{Version: 3, Data: append(data, '\n'), Report: rep, Normalized: n}, nil
	default:
		return nil, decodeError("", "schema", "unsupported export version %d", version)
	}
}

func cloneLevel(src *Level) *Level {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Bounds = src.Bounds
	cp.Obstacles = append([]Obstacle(nil), src.Obstacles...)
	cp.Launches = append([]Launch(nil), src.Launches...)
	cp.Requests = append([]FrameRequest(nil), src.Requests...)
	cp.Truths = append([]Truth(nil), src.Truths...)
	return &cp
}

func normalizeCommonIntegers(n *Level, rep *ConversionReport) error {
	var err error
	n.Gravity, err = normalizeVecMM(n.Gravity, "", "gravity", rep)
	if err != nil {
		return err
	}
	for i := range n.Bounds {
		n.Bounds[i], err = normalizeVecMM(n.Bounds[i], "", fmt.Sprintf("sceneBounds[%d]", i), rep)
		if err != nil {
			return err
		}
	}
	for i := range n.Obstacles {
		o := &n.Obstacles[i]
		o.A, err = normalizeVecMM(o.A, o.ID, fmt.Sprintf("obstacles[%d].a", i), rep)
		if err != nil {
			return err
		}
		o.B, err = normalizeVecMM(o.B, o.ID, fmt.Sprintf("obstacles[%d].b", i), rep)
		if err != nil {
			return err
		}
	}
	for i := range n.Launches {
		l := &n.Launches[i]
		l.Origin, err = normalizeVecMM(l.Origin, l.ID, fmt.Sprintf("launches[%d].origin", i), rep)
		if err != nil {
			return err
		}
		l.Velocity, err = normalizeVecMM(l.Velocity, l.ID, fmt.Sprintf("launches[%d].velocity", i), rep)
		if err != nil {
			return err
		}
	}
	return nil
}

func normalizeVecMM(v Vec2, objectID, field string, rep *ConversionReport) (Vec2, error) {
	xmm := UnitsToMM(objectID, field+".x", v.X, &rep.Truncations)
	ymm := UnitsToMM(objectID, field+".y", v.Y, &rep.Truncations)
	x, err := MMToUnits(objectID, field+".x", xmm)
	if err != nil {
		return Vec2{}, err
	}
	y, err := MMToUnits(objectID, field+".y", ymm)
	if err != nil {
		return Vec2{}, err
	}
	return Vec2{X: x, Y: y}, nil
}

func normalizeV1(n *Level, rep *ConversionReport) {
	ticks := MSToTicks("", "time_step_tick", n.StepMS, &rep.Truncations)
	n.StepMS = ticks * TickMilliseconds
	for i := range n.Obstacles {
		o := &n.Obstacles[i]
		pm := BPSToPermille(o.ID, fmt.Sprintf("obstacles[%d].restitution_pm", i), o.Restitution, &rep.Truncations)
		o.Restitution = pm * 10
		normalField := fmt.Sprintf("obstacles[%d].normal", i)
		defaultNormal := RightNormal(o.A, o.B)
		semantic := o.Normal != defaultNormal
		rep.Losses = append(rep.Losses, FieldTrace{
			Kind: "E_LOSSY", ObjectID: o.ID, Field: normalField,
			OldValue:     fmt.Sprintf("(%d,%d)", o.Normal.X, o.Normal.Y),
			NewValue:     fmt.Sprintf("(%d,%d)", defaultNormal.X, defaultNormal.Y),
			Disposition:  "omit-and-fill-declared-default",
			Source:       "declared-default:right-normal-q1000",
			SemanticLoss: semantic,
		})
		o.Normal = defaultNormal
		rep.Losses = append(rep.Losses, penetrationLoss(o.ID, i, o.Penetrable, "obstacles[%d].allowsPenetration"))
		o.Penetrable = false
		appendDeltaRepresentationLoss(rep, o.ID, i, "obstacles[%d].anchorDelta/endpointDelta")
	}
}

func normalizeV2(n *Level, rep *ConversionReport) {
	for i := range n.Obstacles {
		o := &n.Obstacles[i]
		pm := BPSToPermille(o.ID, fmt.Sprintf("obstacleList[%d].restitutionPermille", i), o.Restitution, &rep.Truncations)
		o.Restitution = pm * 10
		rep.Losses = append(rep.Losses, penetrationLoss(o.ID, i, o.Penetrable, "obstacleList[%d].allowsPenetration"))
		o.Penetrable = false
		appendDeltaRepresentationLoss(rep, o.ID, i, "obstacleList[%d].anchorDelta/endpointDelta")
	}
}

func penetrationLoss(id string, i int, value bool, pattern string) FieldTrace {
	field := fmt.Sprintf(pattern, i)
	return FieldTrace{
		Kind: "E_LOSSY", ObjectID: id, Field: field, OldValue: strconv.FormatBool(value),
		NewValue: "false", Disposition: "omit-and-fill-declared-default",
		Source: "declared-default:penetrable=false", SemanticLoss: value,
	}
}

func appendDeltaRepresentationLoss(rep *ConversionReport, id string, i int, pattern string) {
	rep.Losses = append(rep.Losses, FieldTrace{
		Kind: "E_LOSSY", ObjectID: id, Field: fmt.Sprintf(pattern, i),
		OldValue: "relative-anchor+endpoint-delta", NewValue: "absolute start_mm/end_mm or from/to",
		Disposition: "collapse-deltas-to-absolute-endpoints", SemanticLoss: false,
	})
}

// ---------- v1 JSON ----------

type dtoV1 struct {
	SchemaVersion int             `json:"schema_version"`
	LevelCode     string          `json:"level_code"`
	Gravity       dtoV1MMVec      `json:"gravity_mm_s2"`
	TimeStepTick  int64           `json:"time_step_tick"`
	WorldBounds   []dtoV1MMPoint  `json:"world_bounds_mm"`
	Obstacles     []dtoV1Obstacle `json:"obstacles"`
	Launches      []dtoV1Launch   `json:"launches"`
}
type dtoV1MMPoint struct {
	XMM int64 `json:"x_mm"`
	YMM int64 `json:"y_mm"`
}
type dtoV1MMVec = dtoV1MMPoint
type dtoV1Obstacle struct {
	ObstacleID    string       `json:"obstacle_id"`
	StartMM       dtoV1MMPoint `json:"start_mm"`
	EndMM         dtoV1MMPoint `json:"end_mm"`
	RestitutionPM int64        `json:"restitution_pm"`
	MaterialVer   int          `json:"material_ver"`
	SeqNo         int          `json:"seq_no"`
}
type dtoV1Launch struct {
	LaunchID    string       `json:"launch_id"`
	OriginMM    dtoV1MMPoint `json:"origin_mm"`
	SpeedMMS    dtoV1MMPoint `json:"speed_mm_s"`
	BounceLimit int          `json:"bounce_limit"`
	FrameLimit  int          `json:"frame_limit"`
}

func mmPoint(p Vec2) dtoV1MMPoint {
	return dtoV1MMPoint{XMM: p.X / PositionScale, YMM: p.Y / PositionScale}
}

func buildV1(l *Level) dtoV1 {
	doc := dtoV1{
		SchemaVersion: 1, LevelCode: l.Code,
		Gravity: mmPoint(l.Gravity), TimeStepTick: l.StepMS / TickMilliseconds,
		WorldBounds: []dtoV1MMPoint{mmPoint(l.Bounds[0]), mmPoint(l.Bounds[1])},
	}
	for _, o := range l.Obstacles {
		doc.Obstacles = append(doc.Obstacles, dtoV1Obstacle{
			ObstacleID: o.ID, StartMM: mmPoint(o.A), EndMM: mmPoint(o.B),
			RestitutionPM: o.Restitution / 10, MaterialVer: o.MaterialVer, SeqNo: o.Seq,
		})
	}
	for _, x := range l.Launches {
		doc.Launches = append(doc.Launches, dtoV1Launch{
			LaunchID: x.ID, OriginMM: mmPoint(x.Origin), SpeedMMS: mmPoint(x.Velocity),
			BounceLimit: x.BounceLimit, FrameLimit: x.FrameLimit,
		})
	}
	return doc
}

// ---------- v2 JSON ----------

type dtoPoint struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}
type dtoV2 struct {
	Schema       int             `json:"schema"`
	LevelID      string          `json:"levelId"`
	Gravity      dtoPoint        `json:"gravity"`
	TimeStepMS   int64           `json:"timeStepMs"`
	Bounds       []dtoPoint      `json:"bounds"`
	ObstacleList []dtoV2Obstacle `json:"obstacleList"`
	LaunchList   []dtoV2Launch   `json:"launchList"`
}
type dtoV2Obstacle struct {
	ID                  string   `json:"id"`
	From                dtoPoint `json:"from"`
	To                  dtoPoint `json:"to"`
	Normal              dtoPoint `json:"normal"`
	RestitutionPermille int64    `json:"restitutionPermille"`
	MaterialVersion     int      `json:"materialVersion"`
	Seq                 int      `json:"seq"`
}
type dtoV2Launch struct {
	ID          string   `json:"id"`
	Origin      dtoPoint `json:"origin"`
	Velocity    dtoPoint `json:"velocity"`
	BounceLimit int      `json:"bounceLimit"`
	FrameLimit  int      `json:"frameLimit"`
}

func schemaMMPoint(p Vec2) dtoPoint { return dtoPoint{X: p.X / PositionScale, Y: p.Y / PositionScale} }
func normalPoint(p Vec2) dtoPoint   { return dtoPoint{X: p.X, Y: p.Y} }

func buildV2(l *Level) dtoV2 {
	doc := dtoV2{
		Schema: 2, LevelID: l.Code, Gravity: schemaMMPoint(l.Gravity), TimeStepMS: l.StepMS,
		Bounds: []dtoPoint{schemaMMPoint(l.Bounds[0]), schemaMMPoint(l.Bounds[1])},
	}
	for _, o := range l.Obstacles {
		doc.ObstacleList = append(doc.ObstacleList, dtoV2Obstacle{
			ID: o.ID, From: schemaMMPoint(o.A), To: schemaMMPoint(o.B), Normal: normalPoint(o.Normal),
			RestitutionPermille: o.Restitution / 10, MaterialVersion: o.MaterialVer, Seq: o.Seq,
		})
	}
	for _, x := range l.Launches {
		doc.LaunchList = append(doc.LaunchList, dtoV2Launch{
			ID: x.ID, Origin: schemaMMPoint(x.Origin), Velocity: schemaMMPoint(x.Velocity),
			BounceLimit: x.BounceLimit, FrameLimit: x.FrameLimit,
		})
	}
	return doc
}

// ---------- v3 JSON ----------

type dtoV3 struct {
	SchemaLevel  int            `json:"schemaLevel"`
	SceneID      string         `json:"sceneId"`
	Acceleration dtoPoint       `json:"acceleration"`
	StepMillis   int64          `json:"stepMillis"`
	SceneBounds  []dtoPoint     `json:"sceneBounds"`
	Barriers     []dtoV3Barrier `json:"barriers"`
	Emitters     []dtoV3Emitter `json:"emitters"`
}
type dtoV3Barrier struct {
	Key               string   `json:"key"`
	AnchorDelta       dtoPoint `json:"anchorDelta"`
	EndpointDelta     dtoPoint `json:"endpointDelta"`
	Face              dtoPoint `json:"face"`
	BounceBasisPoints int64    `json:"bounceBasisPoints"`
	MaterialRevision  int      `json:"materialRevision"`
	Order             int      `json:"order"`
	AllowsPenetration bool     `json:"allowsPenetration"`
}
type dtoV3Emitter struct {
	Key           string   `json:"key"`
	Start         dtoPoint `json:"start"`
	InitialVector dtoPoint `json:"initialVector"`
	BounceCeiling int      `json:"bounceCeiling"`
	FrameCeiling  int      `json:"frameCeiling"`
}

func buildV3(l *Level) (dtoV3, error) {
	doc := dtoV3{
		SchemaLevel: 3, SceneID: l.Code, Acceleration: schemaMMPoint(l.Gravity), StepMillis: l.StepMS,
		SceneBounds: []dtoPoint{schemaMMPoint(l.Bounds[0]), schemaMMPoint(l.Bounds[1])},
	}
	var prevA *Point
	for i, o := range l.Obstacles {
		d, _, err := AbsoluteToDeltas(o, prevA, fmt.Sprintf("barriers[%d]", i))
		if err != nil {
			return dtoV3{}, err
		}
		doc.Barriers = append(doc.Barriers, dtoV3Barrier{
			Key: o.ID, AnchorDelta: dtoPoint{X: d.Anchor.X / PositionScale, Y: d.Anchor.Y / PositionScale},
			EndpointDelta: dtoPoint{X: d.Endpoint.X / PositionScale, Y: d.Endpoint.Y / PositionScale},
			Face:          normalPoint(o.Normal), BounceBasisPoints: o.Restitution,
			MaterialRevision: o.MaterialVer, Order: o.Seq, AllowsPenetration: o.Penetrable,
		})
		a := o.A
		prevA = &a
	}
	for _, x := range l.Launches {
		doc.Emitters = append(doc.Emitters, dtoV3Emitter{
			Key: x.ID, Start: schemaMMPoint(x.Origin), InitialVector: schemaMMPoint(x.Velocity),
			BounceCeiling: x.BounceLimit, FrameCeiling: x.FrameLimit,
		})
	}
	return doc, nil
}

// VerifyExportRoundTrip decodes the exported artifact and compares it against
// the degradation-normalized content that the schema can represent.
func VerifyExportRoundTrip(art *ExportArtifact) error {
	decoded, _, err := DecodeVersion(art.Data)
	if err != nil {
		return err
	}
	if diff := FirstContentDifference(art.Normalized, decoded); diff != nil {
		return diff
	}
	return nil
}

// FirstContentDifference returns E_ROUND at the first schema-carried field
// that is not field-by-field equivalent.
func FirstContentDifference(a, b *Level) *Error {
	if a == nil || b == nil {
		return roundError("$", "nil level")
	}
	check := func(field string, av, bv any) bool {
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			return true
		}
		return false
	}
	if check("code", a.Code, b.Code) {
		return roundError("code", fmt.Sprintf("%q != %q", a.Code, b.Code))
	}
	if a.Gravity != b.Gravity {
		return roundError("gravity", fmt.Sprintf("%v != %v", a.Gravity, b.Gravity))
	}
	if a.StepMS != b.StepMS {
		return roundError("stepMillis", fmt.Sprintf("%d != %d", a.StepMS, b.StepMS))
	}
	if a.Bounds != b.Bounds {
		return roundError("sceneBounds", fmt.Sprintf("%v != %v", a.Bounds, b.Bounds))
	}
	if len(a.Obstacles) != len(b.Obstacles) {
		return roundError("barriers.length", fmt.Sprintf("%d != %d", len(a.Obstacles), len(b.Obstacles)))
	}
	for i := range a.Obstacles {
		ao, bo := a.Obstacles[i], b.Obstacles[i]
		p := fmt.Sprintf("barriers[%d]", i)
		if ao.ID != bo.ID {
			return roundError(p+".key", ao.ID+" != "+bo.ID)
		}
		fields := []struct {
			name string
			av   any
			bv   any
		}{
			{"order", ao.Seq, bo.Seq}, {"anchor", ao.A, bo.A}, {"endpoint", ao.B, bo.B},
			{"face", ao.Normal, bo.Normal}, {"bounceBasisPoints", ao.Restitution, bo.Restitution},
			{"materialRevision", ao.MaterialVer, bo.MaterialVer}, {"allowsPenetration", ao.Penetrable, bo.Penetrable},
		}
		for _, f := range fields {
			if fmt.Sprint(f.av) != fmt.Sprint(f.bv) {
				return roundError(p+"."+f.name, fmt.Sprintf("%v != %v", f.av, f.bv))
			}
		}
	}
	if len(a.Launches) != len(b.Launches) {
		return roundError("emitters.length", fmt.Sprintf("%d != %d", len(a.Launches), len(b.Launches)))
	}
	for i := range a.Launches {
		al, bl := a.Launches[i], b.Launches[i]
		p := fmt.Sprintf("emitters[%d]", i)
		if al.ID != bl.ID {
			return roundError(p+".key", al.ID+" != "+bl.ID)
		}
		if al.Origin != bl.Origin {
			return roundError(p+".start", fmt.Sprintf("%v != %v", al.Origin, bl.Origin))
		}
		if al.Velocity != bl.Velocity {
			return roundError(p+".initialVector", fmt.Sprintf("%v != %v", al.Velocity, bl.Velocity))
		}
		if al.BounceLimit != bl.BounceLimit {
			return roundError(p+".bounceCeiling", fmt.Sprintf("%d != %d", al.BounceLimit, bl.BounceLimit))
		}
		if al.FrameLimit != bl.FrameLimit {
			return roundError(p+".frameCeiling", fmt.Sprintf("%d != %d", al.FrameLimit, bl.FrameLimit))
		}
	}
	return nil
}

func roundError(field, msg string) *Error {
	return &Error{Code: CodeRound, Field: field, Message: "round-trip mismatch: " + msg}
}

func seedFromCode(code string) int64 {
	idx := strings.LastIndex(code, "-")
	if idx < 0 {
		return 0
	}
	n, err := strconv.ParseInt(code[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
