package projectio

import (
	"encoding/json"
	"fmt"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

func loadV2(data []byte, rec *conv.Recorder) (*LoadResult, error) {
	const ver = domain.SchemaV2
	var doc v2Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, e.DecodeError(ver, "$", "invalid %s document: %v", ver, err)
	}
	if doc.Format != ver {
		return nil, e.DecodeError(ver, "$.format", "unexpected format %q", doc.Format)
	}
	if doc.Metadata == nil {
		return nil, e.DecodeError(ver, "$.metadata", "required field is missing")
	}
	seed, err := mustValue(doc.Metadata.Seed, ver, "$.metadata.seed")
	if err != nil {
		return nil, err
	}
	if doc.Canvas == nil {
		return nil, e.DecodeError(ver, "$.canvas", "required field is missing")
	}
	w, err := mustValue(doc.Canvas.Width, ver, "$.canvas.width_px")
	if err != nil {
		return nil, err
	}
	h, err := mustValue(doc.Canvas.Height, ver, "$.canvas.height_px")
	if err != nil {
		return nil, err
	}
	bw, err := mustValue(doc.Canvas.TileWidth, ver, "$.canvas.tile_width_px")
	if err != nil {
		return nil, err
	}
	bh, err := mustValue(doc.Canvas.TileHeight, ver, "$.canvas.tile_height_px")
	if err != nil {
		return nil, err
	}
	if doc.QuantSpec == nil {
		return nil, e.DecodeError(ver, "$.quant_spec", "required field is missing")
	}
	basis, err := mustValue(doc.QuantSpec.BasisMax, ver, "$.quant_spec.basis_max")
	if err != nil {
		return nil, err
	}
	if basis != conv.PermyriadScale {
		return nil, e.DecodeError(ver, "$.quant_spec.basis_max", "v2 basis must be %d, got %d", conv.PermyriadScale, basis)
	}
	em, err := mustValue(doc.ExposureMetadata, ver, "$.exposure_metadata")
	if err != nil {
		return nil, err
	}
	refMS, err := mustValue(em.ReferenceMS, ver, "$.exposure_metadata.reference_ms")
	if err != nil {
		return nil, err
	}
	refUS, err := conv.MilliToMicro(refMS)
	if err != nil {
		return nil, err
	}
	if em.XMSReferenceUS != nil {
		refUS = *em.XMSReferenceUS
	}
	white, err := mustValue(doc.WhitePointBP, ver, "$.white_point_bp")
	if err != nil {
		return nil, err
	}
	fullWell, err := mustValue(doc.FullWellDose, ver, "$.full_well_dose")
	if err != nil {
		return nil, err
	}
	if len(doc.SpectralBands) == 0 {
		return nil, e.DecodeError(ver, "$.spectral_bands", "at least one band is required")
	}

	p := &domain.Project{
		ProjectID:      doc.Metadata.ProjectID,
		Name:           doc.Metadata.Title,
		SourceSchema:   ver,
		Seed:           seed,
		Width:          w,
		Height:         h,
		BlockWidth:     bw,
		BlockHeight:    bh,
		QuantScale:     domain.InternalQuantScale,
		ReferenceExpUS: refUS,
		FullWellDose:   fullWell,
		WhitePointBP:   white,
	}
	for i, ms := range em.StackMS {
		us, err := conv.MilliToMicro(ms)
		if err != nil {
			return nil, err
		}
		if i < len(em.XMSStackUS) {
			us = em.XMSStackUS[i]
		}
		p.Exposures = append(p.Exposures, domain.Exposure{ID: fmt.Sprintf("exp-%d", i), ExposureUS: us, Reference: us == refUS})
	}
	if len(em.StackMS) == 0 && len(em.XMSStackUS) > 0 {
		for i, us := range em.XMSStackUS {
			p.Exposures = append(p.Exposures, domain.Exposure{ID: fmt.Sprintf("exp-%d", i), ExposureUS: us, Reference: us == refUS})
		}
	}
	if len(p.Exposures) == 0 {
		p.Exposures = []domain.Exposure{{ID: "exp-ref", ExposureUS: refUS, Reference: true}}
	}

	tr := []domain.Trace{{Op: "convert_unit", Schema: ver, Path: "$.exposure_metadata.reference_ms", OldValue: fmt.Sprint(refMS) + "ms", NewValue: fmt.Sprint(refUS) + "us", Source: "integer*1000"}}
	for i, raw := range doc.SpectralBands {
		base := fmt.Sprintf("$.spectral_bands[%d]", i)
		id, err := mustValue(raw.BandID, ver, base+".band_id")
		if err != nil {
			return nil, err
		}
		label, err := mustValue(raw.Label, ver, base+".label")
		if err != nil {
			return nil, err
		}
		center, err := mustValue(raw.CenterNanometers, ver, base+".center_nanometers")
		if err != nil {
			return nil, err
		}
		resp, err := mustValue(raw.TargetResponseBP, ver, base+".target_response_bp")
		if err != nil {
			return nil, err
		}
		b := domain.Band{ID: id, Name: label, CenterNM: center, Response: resp}
		if raw.XMSGainPPM != nil {
			b.GainPPM = *raw.XMSGainPPM
			b.GainExplicit = true
			tr = append(tr, domain.Trace{Op: "restore_extension", Schema: ver, Path: base + ".x_ms_gain_ppm", NewValue: fmt.Sprint(b.GainPPM), Source: "vendor-extension"})
		} else if raw.GainCorrectionPermille != nil {
			b.GainPPM, err = conv.PermilleGainToPPM(raw.GainCorrectionPermille)
			if err != nil {
				return nil, err
			}
			b.GainExplicit = true
			tr = append(tr, domain.Trace{Op: "convert_scale", Schema: ver, Path: base + ".gain_correction_permille", OldValue: fmt.Sprint(*raw.GainCorrectionPermille), NewValue: fmt.Sprint(b.GainPPM), Source: "permille*1000=ppm"})
		} else {
			b.GainPPM = conv.GainPPMUnity
			tr = append(tr, domain.Trace{Op: "fill_default", Schema: ver, Path: base + ".gain_correction_permille", OldValue: "<missing>", NewValue: fmt.Sprint(b.GainPPM), Source: "declared-default:gain.unity_ppm"})
		}
		switch {
		case len(raw.XMSGroupPath) > 0:
			b.GroupPath = append([]string(nil), raw.XMSGroupPath...)
		case raw.SpectralFamily != "" && raw.SpectralSubfamily != "":
			b.GroupPath = []string{raw.SpectralFamily, raw.SpectralSubfamily}
		case raw.SpectralFamily != "":
			b.GroupPath = []string{raw.SpectralFamily}
		default:
			b.GroupPath = []string{"Ungrouped"}
			tr = append(tr, domain.Trace{Op: "fill_default", Schema: ver, Path: "$.spectral_tree", OldValue: "<missing>", NewValue: "Ungrouped/" + id, Source: "declared-default:tree.ungrouped"})
		}
		p.Bands = append(p.Bands, b)
	}
	p.Tree = buildTreeFromBands(p.Bands)
	p.Traces = tr
	return &LoadResult{Project: p, Conversion: rec}, nil
}

func loadV3(data []byte, rec *conv.Recorder) (*LoadResult, error) {
	const ver = domain.SchemaV3
	var doc v3Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, e.DecodeError(ver, "$", "invalid %s document: %v", ver, err)
	}
	if doc.SchemaVersion != ver {
		return nil, e.DecodeError(ver, "$.schemaVersion", "unexpected schemaVersion %q", doc.SchemaVersion)
	}
	seed, err := mustValue(doc.RandomSeed, ver, "$.randomSeed")
	if err != nil {
		return nil, err
	}
	if doc.Canvas == nil {
		return nil, e.DecodeError(ver, "$.canvas", "required field is missing")
	}
	w, err := mustValue(doc.Canvas.W, ver, "$.canvas.w")
	if err != nil {
		return nil, err
	}
	h, err := mustValue(doc.Canvas.H, ver, "$.canvas.h")
	if err != nil {
		return nil, err
	}
	bw, err := mustValue(doc.Canvas.TileW, ver, "$.canvas.tileW")
	if err != nil {
		return nil, err
	}
	bh, err := mustValue(doc.Canvas.TileH, ver, "$.canvas.tileH")
	if err != nil {
		return nil, err
	}
	if doc.Quantization == nil {
		return nil, e.DecodeError(ver, "$.quantization", "required field is missing")
	}
	den, err := mustValue(doc.Quantization.Denominator, ver, "$.quantization.denominator")
	if err != nil {
		return nil, err
	}
	if den != conv.PermyriadScale {
		return nil, e.DecodeError(ver, "$.quantization.denominator", "v3 denominator must be %d, got %d", conv.PermyriadScale, den)
	}
	if doc.WhitePoint == nil || doc.WhitePoint.XYZBP == nil {
		return nil, e.DecodeError(ver, "$.whitePoint.xyzBp", "required field is missing")
	}
	fullWell, err := mustValue(doc.FullWellDose, ver, "$.fullWellDose")
	if err != nil {
		return nil, err
	}
	if len(doc.ExposureGroups) == 0 {
		return nil, e.DecodeError(ver, "$.exposureGroups", "at least one exposure group is required")
	}

	p := &domain.Project{
		ProjectID:    doc.CompositionID,
		Name:         doc.DisplayName,
		SourceSchema: ver,
		Seed:         seed,
		Width:        w,
		Height:       h,
		BlockWidth:   bw,
		BlockHeight:  bh,
		QuantScale:   domain.InternalQuantScale,
		FullWellDose: fullWell,
		WhitePointBP: *doc.WhitePoint.XYZBP,
	}
	tr := []domain.Trace{}
	tr = append(tr,
		domain.Trace{Op: "map_field", Schema: ver, Path: "$.schemaVersion", NewValue: "source_schema=" + ver, Description: "retain source version identifier"},
		domain.Trace{Op: "map_field", Schema: ver, Path: "$.canvas.w/h/tileW/tileH", NewValue: "width/height/block_width/block_height", Source: "camelCase geometry mapping"},
		domain.Trace{Op: "convert_scale", Schema: ver, Path: "$.quantization.denominator", OldValue: fmt.Sprint(den), NewValue: fmt.Sprint(p.QuantScale), Source: "already permyriad"},
		domain.Trace{Op: "map_field", Schema: ver, Path: "$.exposureGroups[*].exposureMicros", NewValue: "exposures[].exposure_us/reference_exposure_us", Source: "microsecond identity"},
		domain.Trace{Op: "map_field", Schema: ver, Path: "$.exposureGroups[*].spectralTree", NewValue: "bands[].group_path + tree", Source: "walk nested group/leaf nodes"},
	)
	bandSeen := map[string]bool{}
	var canonicalTree []v3Node
	for gi, group := range doc.ExposureGroups {
		gpath := fmt.Sprintf("$.exposureGroups[%d]", gi)
		expID, err := mustValue(group.ExposureID, ver, gpath+".exposureId")
		if err != nil {
			return nil, err
		}
		us, err := mustValue(group.ExposureMicros, ver, gpath+".exposureMicros")
		if err != nil {
			return nil, err
		}
		p.Exposures = append(p.Exposures, domain.Exposure{ID: expID, ExposureUS: us, Reference: group.Reference})
		if group.Reference {
			p.ReferenceExpUS = us
		}
		if len(group.SpectralTree) == 0 {
			return nil, e.DecodeError(ver, gpath+".spectralTree", "spectral tree is empty")
		}
		if group.Reference || canonicalTree == nil {
			canonicalTree = group.SpectralTree
		}
		var walk func(nodes []v3Node, prefix []string) error
		walk = func(nodes []v3Node, prefix []string) error {
			for ni, node := range nodes {
				nodePath := fmt.Sprintf("%s.spectralTree[%d]", gpath, ni)
				if node.Kind == "group" || node.Band == nil {
					next := append(append([]string(nil), prefix...), node.Label)
					if len(node.Children) == 0 {
						return e.DecodeError(ver, nodePath+".children", "group node has no children")
					}
					if err := walk(node.Children, next); err != nil {
						return err
					}
					continue
				}
				leafPath := nodePath + ".band"
				id, err := mustValue(node.Band.BandID, ver, leafPath+".bandId")
				if err != nil {
					return err
				}
				if bandSeen[id] {
					continue
				}
				name, err := mustValue(node.Band.Name, ver, leafPath+".name")
				if err != nil {
					return err
				}
				center, err := mustValue(node.Band.CenterNM, ver, leafPath+".centerNm")
				if err != nil {
					return err
				}
				gain, err := mustValue(node.Band.GainPPM, ver, leafPath+".gainPpm")
				if err != nil {
					return err
				}
				resp, err := mustValue(node.Band.ResponseBP, ver, leafPath+".responseBp")
				if err != nil {
					return err
				}
				bandSeen[id] = true
				p.Bands = append(p.Bands, domain.Band{
					ID: id, Name: name, CenterNM: center, GainPPM: gain, GainExplicit: true,
					Response: resp, GroupPath: append([]string(nil), prefix...),
				})
				tr = append(tr, domain.Trace{Op: "map_field", Schema: ver, Path: leafPath + ".band", NewValue: "bands[" + id + "]", Source: "nested spectral leaf"})
				return nil
			}
			return nil
		}
		if err := walk(group.SpectralTree, nil); err != nil {
			return nil, err
		}
	}
	if p.ReferenceExpUS == 0 && len(p.Exposures) > 0 {
		p.ReferenceExpUS = p.Exposures[0].ExposureUS
	}
	if len(p.Bands) == 0 {
		return nil, e.DecodeError(ver, "$.exposureGroups[*].spectralTree", "no band leaves found")
	}
	_ = canonicalTree
	p.Tree = buildTreeFromBands(p.Bands)
	p.Traces = tr
	return &LoadResult{Project: p, Conversion: rec}, nil
}

// MarshalDocument is a small helper for diagnostics and tests.
func MarshalDocument(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, e.New(e.Decode, "marshal schema document: %v", err)
	}
	return b, nil
}
