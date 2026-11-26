package projectio

import (
	"encoding/json"
	"fmt"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

type LoadResult struct {
	Project    *domain.Project `json:"project"`
	Conversion *conv.Recorder  `json:"conversion"`
}

// Load detects one of the three supported schema versions and maps it to the
// canonical spectral model.
func Load(data []byte) (*LoadResult, error) {
	rec := &conv.Recorder{}
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, e.DecodeError("unknown", "$", "invalid JSON: %v", err)
	}
	version := detectVersion(head)
	switch version {
	case domain.SchemaV1:
		return loadV1(data, rec)
	case domain.SchemaV2:
		return loadV2(data, rec)
	case domain.SchemaV3:
		return loadV3(data, rec)
	default:
		key, raw := detectVersionKey(head)
		val := ""
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &val)
		}
		if val != "" {
			version = val
		}
		return nil, e.DecodeError(version, "$."+key, "unsupported schema version %q", val)
	}
}

func detectVersion(head map[string]json.RawMessage) string {
	_, raw := detectVersionKey(head)
	var v string
	_ = json.Unmarshal(raw, &v)
	return v
}

func detectVersionKey(head map[string]json.RawMessage) (string, json.RawMessage) {
	for _, key := range []string{"schema", "format", "schemaVersion"} {
		if raw, ok := head[key]; ok {
			return key, raw
		}
	}
	return "schema/format/schemaVersion", nil
}

func mustValue[T any](v *T, version, path string) (T, error) {
	var zero T
	if v == nil {
		return zero, e.DecodeError(version, path, "required field is missing or null")
	}
	return *v, nil
}

func buildTreeFromBands(bands []domain.Band) []*domain.Group {
	root := &domain.Group{ID: "root", Name: "root", Path: []string{}}
	byPath := map[string]*domain.Group{"": root}
	for _, b := range bands {
		cur := root
		path := []string{}
		for _, seg := range b.GroupPath {
			path = append(path, seg)
			key := joinPath(path)
			g, ok := byPath[key]
			if !ok {
				g = &domain.Group{ID: "g-" + key, Name: seg, Path: append([]string(nil), path...)}
				byPath[key] = g
				cur.Children = append(cur.Children, g)
			}
			cur = g
		}
		cur.BandIDs = append(cur.BandIDs, b.ID)
	}
	return root.Children
}

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}

func loadV1(data []byte, rec *conv.Recorder) (*LoadResult, error) {
	const ver = domain.SchemaV1
	var doc v1Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, e.DecodeError(ver, "$", "invalid %s document: %v", ver, err)
	}
	tr := []domain.Trace{}
	if doc.Schema != ver {
		return nil, e.DecodeError(ver, "$.schema", "unexpected schema identifier %q", doc.Schema)
	}
	seed, err := mustValue(doc.Seed, ver, "$.seed")
	if err != nil {
		return nil, err
	}
	if doc.Geometry == nil {
		return nil, e.DecodeError(ver, "$.geometry", "required field is missing")
	}
	w, err := mustValue(doc.Geometry.Width, ver, "$.geometry.width")
	if err != nil {
		return nil, err
	}
	h, err := mustValue(doc.Geometry.Height, ver, "$.geometry.height")
	if err != nil {
		return nil, err
	}
	bw, err := mustValue(doc.Geometry.BlockWidth, ver, "$.geometry.block_width")
	if err != nil {
		return nil, err
	}
	bh, err := mustValue(doc.Geometry.BlockHeight, ver, "$.geometry.block_height")
	if err != nil {
		return nil, err
	}
	if doc.Quantization == nil {
		return nil, e.DecodeError(ver, "$.quantization", "required field is missing")
	}
	scale, err := mustValue(doc.Quantization.PermilleMax, ver, "$.quantization.permille_max")
	if err != nil {
		return nil, err
	}
	if scale != conv.PermilleScale {
		return nil, e.DecodeError(ver, "$.quantization.permille_max", "v1 scale must be %d, got %d", conv.PermilleScale, scale)
	}
	exp, err := mustValue(doc.Exposure, ver, "$.exposure")
	if err != nil {
		return nil, err
	}
	refUS, err := mustValue(exp.ReferenceMicros, ver, "$.exposure.reference_micros")
	if err != nil {
		return nil, err
	}
	whitePermille, err := mustValue(doc.WhitePointPermille, ver, "$.white_point_permille")
	if err != nil {
		return nil, err
	}
	fullWell, err := mustValue(doc.FullWellDose, ver, "$.full_well_dose")
	if err != nil {
		return nil, err
	}
	if len(doc.Bands) == 0 {
		return nil, e.DecodeError(ver, "$.bands", "at least one band is required")
	}

	p := &domain.Project{
		ProjectID:      doc.ProjectCode,
		Name:           doc.ProjectName,
		SourceSchema:   ver,
		Seed:           seed,
		Width:          w,
		Height:         h,
		BlockWidth:     bw,
		BlockHeight:    bh,
		QuantScale:     domain.InternalQuantScale,
		ReferenceExpUS: refUS,
		FullWellDose:   fullWell,
	}
	if p.ProjectID == "" && doc.ProjectName != "" {
		p.ProjectID = "PRJ-" + doc.ProjectName
	}
	if p.ProjectID == "" {
		p.ProjectID = "imported-v1"
	}
	for i, v := range whitePermille {
		if doc.XMSWhitePointBP != nil {
			p.WhitePointBP = *doc.XMSWhitePointBP
		} else {
			bp, err := conv.PermilleToPermyriad(v, rec)
			if err != nil {
				return nil, err
			}
			p.WhitePointBP[i] = bp
		}
	}
	tr = append(tr, domain.Trace{Op: "convert_scale", Schema: ver, Path: "$.white_point_permille", OldValue: fmt.Sprint(whitePermille), NewValue: fmt.Sprint(p.WhitePointBP), Source: "permille*10=permyriad"})
	for i, ms := range exp.StackMicros {
		_ = ms
		p.Exposures = append(p.Exposures, domain.Exposure{
			ID:         fmt.Sprintf("exp-%d", i),
			ExposureUS: exp.StackMicros[i],
			Reference:  exp.StackMicros[i] == refUS,
		})
	}
	if len(p.Exposures) == 0 {
		p.Exposures = []domain.Exposure{{ID: "exp-ref", ExposureUS: refUS, Reference: true}}
	}

	for i, raw := range doc.Bands {
		base := fmt.Sprintf("$.bands[%d]", i)
		id, err := mustValue(raw.BandID, ver, base+".band_id")
		if err != nil {
			return nil, err
		}
		name, err := mustValue(raw.Name, ver, base+".name")
		if err != nil {
			return nil, err
		}
		center, err := mustValue(raw.CenterNM, ver, base+".center_nm")
		if err != nil {
			return nil, err
		}
		resp, err := mustValue(raw.ResponsePermille, ver, base+".response_permille")
		if err != nil {
			return nil, err
		}
		b := domain.Band{ID: id, Name: name, CenterNM: center, GainPPM: conv.GainPPMUnity}
		if raw.XMSResponseBP != nil {
			b.Response = *raw.XMSResponseBP
			tr = append(tr, domain.Trace{Op: "restore_extension", Schema: ver, Path: base + ".x_ms_response_bp", Source: "vendor-extension"})
		} else {
			for c, rv := range resp {
				b.Response[c], err = conv.PermilleToPermyriad(rv, rec)
				if err != nil {
					return nil, err
				}
			}
		}
		tr = append(tr, domain.Trace{Op: "convert_scale", Schema: ver, Path: base + ".response_permille", OldValue: fmt.Sprint(resp), NewValue: fmt.Sprint(b.Response), Source: "permille*10=response_bp"})
		if raw.XMSGainPPM != nil {
			b.GainPPM = *raw.XMSGainPPM
			b.GainExplicit = true
			tr = append(tr, domain.Trace{Op: "restore_extension", Schema: ver, Path: base + ".x_ms_gain_ppm", NewValue: fmt.Sprint(b.GainPPM), Source: "vendor-extension"})
		} else {
			tr = append(tr, domain.Trace{Op: "fill_default", Schema: ver, Path: base + ".gain_correction", OldValue: "<missing>", NewValue: fmt.Sprint(conv.GainPPMUnity), Source: "declared-default:gain.unity_ppm"})
		}
		if len(raw.XMSGroupPath) > 0 {
			b.GroupPath = append([]string(nil), raw.XMSGroupPath...)
			tr = append(tr, domain.Trace{Op: "restore_extension", Schema: ver, Path: base + ".x_ms_group_path", NewValue: joinPath(b.GroupPath), Source: "vendor-extension"})
		} else if raw.GroupName != "" {
			b.GroupPath = []string{raw.GroupName}
			tr = append(tr, domain.Trace{Op: "map_field", Schema: ver, Path: base + ".group_name", NewValue: joinPath(b.GroupPath), Source: "flat-group-column"})
		} else {
			b.GroupPath = []string{"Ungrouped"}
			tr = append(tr, domain.Trace{Op: "fill_default", Schema: ver, Path: "$.spectral_tree", OldValue: "<missing>", NewValue: "Ungrouped/" + id, Source: "declared-default:tree.ungrouped"})
		}
		p.Bands = append(p.Bands, b)
	}
	p.Tree = buildTreeFromBands(p.Bands)
	p.Traces = tr
	return &LoadResult{Project: p, Conversion: rec}, nil
}
