package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	SchemaV1 = "stage.project.v1"
	SchemaV2 = "stage-project/2"
	SchemaV3 = "stage.project/3"
)

var SupportedSchemas = []string{SchemaV1, SchemaV2, SchemaV3}

// LoadProject is the single schema-detecting entry point. Parsed fixtures,
// cues and timeline values are always converted to the internal integer units.
func LoadProject(data []byte) (*Project, error) {
	var header struct {
		Schema *string `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, &Error{
			Code:    CodeSchema,
			Message: "invalid project JSON: " + err.Error(),
			Field:   "$",
			Version: "?",
		}
	}
	if header.Schema == nil || strings.TrimSpace(*header.Schema) == "" {
		return nil, &Error{
			Code:    CodeSchema,
			Message: "missing schema discriminator",
			Field:   "schema",
			Version: "?",
		}
	}
	var (
		p   *Project
		err error
	)
	switch *header.Schema {
	case SchemaV1:
		p, err = loadV1(data)
	case SchemaV2:
		p, err = loadV2(data)
	case SchemaV3:
		p, err = loadV3(data)
	default:
		return nil, &Error{
			Code:    CodeSchema,
			Message: "unknown schema version",
			Field:   "schema",
			Version: *header.Schema,
		}
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	p.SourceVersion = *header.Schema
	p.SourceHash = hex.EncodeToString(sum[:])
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func ValidateProject(p *Project) error {
	if p == nil {
		return domainError(CodeSchema, "nil project", "project")
	}
	version := p.SourceVersion
	if version == "" {
		version = InternalSchema
	}
	if strings.TrimSpace(p.ProjectID) == "" {
		return &Error{Code: CodeSchema, Message: "missing project id", Field: "project_id", Version: version}
	}
	if strings.TrimSpace(p.ParameterVersion) == "" {
		return &Error{Code: CodeSchema, Message: "missing parameter version", Field: "parameter_version", Version: version}
	}
	if len(p.Fixtures) == 0 {
		return &Error{Code: CodeSchema, Message: "fixture array is empty", Field: "fixtures", Version: version}
	}
	ids := make(map[string]struct{}, len(p.Fixtures))
	for i := range p.Fixtures {
		f := &p.Fixtures[i]
		path := fmt.Sprintf("fixtures[%d]", i)
		if strings.TrimSpace(f.ID) == "" {
			return &Error{Code: CodeSchema, Message: "missing fixture id", Field: path + ".id", Version: version}
		}
		if _, ok := ids[f.ID]; ok {
			return &Error{Code: CodeSchema, Message: "duplicate fixture id", Field: path + ".id", FixtureID: f.ID, Version: version}
		}
		ids[f.ID] = struct{}{}
	}
	cueIDs := make(map[string]struct{}, len(p.Cues))
	for i := range p.Cues {
		c := &p.Cues[i]
		path := fmt.Sprintf("cues[%d]", i)
		if strings.TrimSpace(c.ID) == "" {
			return &Error{Code: CodeSchema, Message: "missing cue id", Field: path + ".id", Version: version}
		}
		if _, ok := cueIDs[c.ID]; ok {
			return &Error{Code: CodeSchema, Message: "duplicate cue id", Field: path + ".id", CueID: c.ID, Version: version}
		}
		cueIDs[c.ID] = struct{}{}
		if _, ok := ids[c.FixtureID]; !ok {
			return &Error{Code: CodeSchema, Message: "cue references unknown fixture", Field: path + ".fixture_id", CueID: c.ID, Version: version}
		}
		switch c.Kind {
		case CueFade, CueSweep, CueColor:
		default:
			return &Error{Code: CodeSchema, Message: "unknown cue kind", Field: path + ".kind", CueID: c.ID, Version: version}
		}
		if c.EndMS < c.StartMS {
			return &Error{Code: CodeSchema, Message: "cue end precedes start", Field: path + ".end_ms", CueID: c.ID, Version: version}
		}
	}
	if p.Timeline.DurationMS <= 0 {
		return &Error{Code: CodeSchema, Message: "duration must be positive", Field: "timeline.duration_ms", Version: version}
	}
	if len(p.Timeline.Windows) == 0 {
		return &Error{Code: CodeSchema, Message: "timeline has no windows", Field: "timeline.windows", Version: version}
	}
	seenWindows := make(map[int]struct{}, len(p.Timeline.Windows))
	for i := range p.Timeline.Windows {
		w := &p.Timeline.Windows[i]
		path := fmt.Sprintf("timeline.windows[%d]", i)
		if w.Number <= 0 {
			return &Error{Code: CodeSchema, Message: "window number must be positive", Field: path + ".number", Version: version}
		}
		if _, ok := seenWindows[w.Number]; ok {
			return &Error{Code: CodeSchema, Message: "duplicate window number", Field: path + ".number", WindowNumber: w.Number, Version: version}
		}
		seenWindows[w.Number] = struct{}{}
		if w.EndMS < w.StartMS {
			return &Error{Code: CodeSchema, Message: "window end precedes start", Field: path + ".end_ms", WindowNumber: w.Number, Version: version}
		}
		if len(w.Samples) == 0 {
			return &Error{Code: CodeSchema, Message: "window has no sample times", Field: path + ".samples_ms", WindowNumber: w.Number, Version: version}
		}
		for j, t := range w.Samples {
			if t < w.StartMS || t > w.EndMS {
				return &Error{Code: CodeSchema, Message: "sample outside window", Field: fmt.Sprintf("%s.samples_ms[%d]", path, j), WindowNumber: w.Number, Version: version}
			}
			if t < 0 || t > p.Timeline.DurationMS {
				return &Error{Code: CodeSchema, Message: "sample outside timeline", Field: fmt.Sprintf("%s.samples_ms[%d]", path, j), WindowNumber: w.Number, Version: version}
			}
		}
	}
	if p.Timeline.DurationMS < 0 {
		return &Error{Code: CodeSchema, Message: "negative duration", Field: "timeline.duration_ms", Version: version}
	}
	return nil
}

func schemaError(version, field, message string) *Error {
	return &Error{Code: CodeSchema, Message: message, Field: field, Version: version}
}

func reqString(v *string, version, field string) (string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "", schemaError(version, field, "missing required string")
	}
	return *v, nil
}

func reqInt(v *int64, version, field string) (int64, error) {
	if v == nil {
		return 0, schemaError(version, field, "missing required integer")
	}
	return *v, nil
}

func reqVec(v []int64, n int, version, field string) (Vec3, error) {
	if len(v) != n {
		return Vec3{}, schemaError(version, field, fmt.Sprintf("expected integer vector of length %d", n))
	}
	out := Vec3{}
	for i := range out {
		out[i] = v[i]
	}
	return out, nil
}

func addTrace(p *Project, tr ConversionTrace) {
	if tr.TargetVersion == "" {
		tr.TargetVersion = InternalSchema
	}
	p.Traces = append(p.Traces, tr)
}

func traceExact(p *Project, fixtureID, cueID, field, version, source, internal, op string) {
	addTrace(p, ConversionTrace{
		FixtureID: fixtureID, CueID: cueID, Field: field, SourceVersion: version,
		SourceValue: source, InternalValue: internal, Operation: op, Rounding: string(RoundExact),
	})
}

func traceScaled(p *Project, fixtureID, cueID, field, version, source, internal, op string, rounding Rounding, remainder int64) {
	addTrace(p, ConversionTrace{
		FixtureID: fixtureID, CueID: cueID, Field: field, SourceVersion: version,
		SourceValue: source, InternalValue: internal, Operation: op,
		Rounding: string(rounding), Remainder: remainder,
	})
}

func parseHexRGB(value, version, field string) ([3]int64, error) {
	if len(value) != 7 || value[0] != '#' {
		return [3]int64{}, schemaError(version, field, "expected #RRGGBB")
	}
	var out [3]int64
	for i := 0; i < 3; i++ {
		var b int64
		for _, ch := range value[1+i*2 : 3+i*2] {
			b *= 16
			switch {
			case ch >= '0' && ch <= '9':
				b += int64(ch - '0')
			case ch >= 'a' && ch <= 'f':
				b += int64(ch-'a') + 10
			case ch >= 'A' && ch <= 'F':
				b += int64(ch-'A') + 10
			default:
				return [3]int64{}, schemaError(version, field, "invalid hex color")
			}
		}
		v, _, err := ByteToPermyriad(b)
		if err != nil {
			return [3]int64{}, err
		}
		out[i] = v
	}
	return out, nil
}

func hexRGB(rgb [3]int64) (string, error) {
	var out strings.Builder
	out.WriteByte('#')
	for _, channel := range rgb {
		b, _, err := PermyriadToByte(channel)
		if err != nil {
			return "", err
		}
		if b < 0 {
			b = 0
		}
		if b > 255 {
			b = 255
		}
		fmt.Fprintf(&out, "%02X", b)
	}
	return out.String(), nil
}

func sortProjectCues(p *Project) {
	sort.SliceStable(p.Cues, func(i, j int) bool {
		if p.Cues[i].StartMS != p.Cues[j].StartMS {
			return p.Cues[i].StartMS < p.Cues[j].StartMS
		}
		return p.Cues[i].ID < p.Cues[j].ID
	})
}
