package projectio

import (
	"encoding/json"
	"fmt"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

type ExportResult struct {
	Schema     string             `json:"schema"`
	Data       []byte             `json:"-"`
	LossReport *domain.LossReport `json:"loss_report,omitempty"`
	Conversion *conv.Recorder     `json:"conversion,omitempty"`
}

func Export(p *domain.Project, target string) (*ExportResult, error) {
	switch target {
	case domain.SchemaV1:
		return exportV1(p)
	case domain.SchemaV2:
		return exportV2(p)
	case domain.SchemaV3:
		return exportV3(p)
	default:
		return nil, e.DecodeError(target, "$.schema", "unknown export schema %q", target)
	}
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}

func exportV1(p *domain.Project) (*ExportResult, error) {
	rec := &conv.Recorder{}
	loss := &domain.LossReport{Code: string(e.Lossy), From: "internal-canonical", To: domain.SchemaV1}
	doc := v1Document{
		Schema:      domain.SchemaV1,
		ProjectCode: p.ProjectID,
		ProjectName: p.Name,
		Seed:        &p.Seed,
		Geometry: &struct {
			Width       *int `json:"width"`
			Height      *int `json:"height"`
			BlockWidth  *int `json:"block_width"`
			BlockHeight *int `json:"block_height"`
		}{Width: &p.Width, Height: &p.Height, BlockWidth: &p.BlockWidth, BlockHeight: &p.BlockHeight},
		Quantization: &struct {
			PermilleMax *int64  `json:"permille_max"`
			Rounding    *string `json:"rounding"`
		}{PermilleMax: &[]int64{conv.PermilleScale}[0], Rounding: &[]string{"floor"}[0]},
		Exposure: &struct {
			ReferenceMicros *int64  `json:"reference_micros"`
			StackMicros     []int64 `json:"stack_micros"`
		}{ReferenceMicros: &p.ReferenceExpUS},
		WhitePointPermille: &[3]int64{},
		XMSWhitePointBP:    &p.WhitePointBP,
		FullWellDose:       &p.FullWellDose,
	}
	doc.Exposure.StackMicros = make([]int64, 0, len(p.Exposures))
	for _, ex := range p.Exposures {
		doc.Exposure.StackMicros = append(doc.Exposure.StackMicros, ex.ExposureUS)
	}
	for i, v := range p.WhitePointBP {
		pv, _, err := conv.PermyriadToPermille(v, rec)
		if err != nil {
			return nil, err
		}
		doc.WhitePointPermille[i] = pv
	}
	for i, b := range p.Bands {
		band := v1Band{BandID: &b.ID, Name: &b.Name, CenterNM: &b.CenterNM, ResponsePermille: &[3]int64{}}
		responseExt := b.Response
		band.XMSResponseBP = &responseExt
		for c, rv := range b.Response {
			pv, _, err := conv.PermyriadToPermille(rv, rec)
			if err != nil {
				return nil, err
			}
			band.ResponsePermille[c] = pv
		}
		if len(b.GroupPath) > 0 {
			band.GroupName = b.GroupPath[0]
			path := append([]string(nil), b.GroupPath...)
			band.XMSGroupPath = path
			loss.Items = append(loss.Items, domain.LossItem{
				Path: fmt.Sprintf("$.bands[%d].group_path", i), Kind: "nested_spectral_tree",
				Value: joinPath(b.GroupPath), Action: "flatten_to_flat_band_table",
				PreservedIn: fmt.Sprintf("$.bands[%d].x_ms_group_path", i),
				Reason:      "v1 supports one flat band table and one optional group_name column",
			})
		}
		if b.GainExplicit {
			gain := b.GainPPM
			band.XMSGainPPM = &gain
			loss.Items = append(loss.Items, domain.LossItem{
				Path: fmt.Sprintf("$.bands[%d].gain_correction", i), Kind: "gain_fixup",
				Value: fmt.Sprint(b.GainPPM), Action: "omit_standard_field_and_apply_declared_unity_default",
				PreservedIn: fmt.Sprintf("$.bands[%d].x_ms_gain_ppm", i),
				Reason:      "v1 has no optional band gain correction field",
			})
		}
		doc.Bands = append(doc.Bands, band)
	}
	if len(loss.Items) > 0 {
		loss.RecomputeDigest()
	} else {
		loss = nil
	}
	return &ExportResult{Schema: domain.SchemaV1, Data: mustJSON(doc), LossReport: loss, Conversion: rec}, nil
}

func exportV2(p *domain.Project) (*ExportResult, error) {
	rec := &conv.Recorder{}
	loss := &domain.LossReport{Code: string(e.Lossy), From: "internal-canonical", To: domain.SchemaV2}
	id := p.ProjectID
	title := p.Name
	doc := v2Document{
		Format: domain.SchemaV2,
		Metadata: &struct {
			ProjectID string `json:"project_id"`
			Title     string `json:"title"`
			Seed      *int64 `json:"seed"`
		}{ProjectID: id, Title: title, Seed: &p.Seed},
		Canvas: &struct {
			Width      *int `json:"width_px"`
			Height     *int `json:"height_px"`
			TileWidth  *int `json:"tile_width_px"`
			TileHeight *int `json:"tile_height_px"`
		}{Width: &p.Width, Height: &p.Height, TileWidth: &p.BlockWidth, TileHeight: &p.BlockHeight},
		QuantSpec: &struct {
			BasisMax *int64  `json:"basis_max"`
			Truncate *string `json:"truncation_direction"`
		}{BasisMax: &[]int64{conv.PermyriadScale}[0], Truncate: &[]string{"floor_nonnegative"}[0]},
		ExposureMetadata: &struct {
			ReferenceMS    *int64  `json:"reference_ms"`
			StackMS        []int64 `json:"stack_ms"`
			XMSReferenceUS *int64  `json:"x_ms_reference_us,omitempty"`
			XMSStackUS     []int64 `json:"x_ms_stack_us,omitempty"`
		}{},
		WhitePointBP: &p.WhitePointBP,
		FullWellDose: &p.FullWellDose,
	}
	refMS, refRem, err := conv.MicroToMilli(p.ReferenceExpUS, rec)
	if err != nil {
		return nil, err
	}
	doc.ExposureMetadata.ReferenceMS = &refMS
	refUS := p.ReferenceExpUS
	doc.ExposureMetadata.XMSReferenceUS = &refUS
	if refRem != 0 {
		loss.Items = append(loss.Items, domain.LossItem{Path: "$.exposure_metadata.reference_ms", Kind: "exposure_unit_remainder", Value: fmt.Sprintf("%dus rem%d", p.ReferenceExpUS, refRem), Action: "floor_us_to_ms_and_preserve_us_extension", PreservedIn: "$.exposure_metadata.x_ms_reference_us"})
	}
	for _, ex := range p.Exposures {
		ms, rem, err := conv.MicroToMilli(ex.ExposureUS, rec)
		if err != nil {
			return nil, err
		}
		doc.ExposureMetadata.StackMS = append(doc.ExposureMetadata.StackMS, ms)
		doc.ExposureMetadata.XMSStackUS = append(doc.ExposureMetadata.XMSStackUS, ex.ExposureUS)
		if rem != 0 {
			loss.Items = append(loss.Items, domain.LossItem{Path: "$.exposure_metadata.stack_ms", Kind: "exposure_unit_remainder", Value: fmt.Sprintf("%dus rem%d", ex.ExposureUS, rem), Action: "floor_us_to_ms_and_preserve_us_extension", PreservedIn: "$.exposure_metadata.x_ms_stack_us"})
		}
	}
	for i, b := range p.Bands {
		band := v2Band{BandID: &b.ID, Label: &b.Name, CenterNanometers: &b.CenterNM, TargetResponseBP: &b.Response}
		if len(b.GroupPath) > 0 {
			band.SpectralFamily = b.GroupPath[0]
			if len(b.GroupPath) > 1 {
				band.SpectralSubfamily = b.GroupPath[1]
			}
			if len(b.GroupPath) > 2 {
				band.XMSGroupPath = append([]string(nil), b.GroupPath...)
				loss.Items = append(loss.Items, domain.LossItem{Path: fmt.Sprintf("$.spectral_bands[%d].group_path", i), Kind: "nested_spectral_tree", Value: joinPath(b.GroupPath), Action: "retain_first_two_flat_columns_and_preserve_full_path", PreservedIn: fmt.Sprintf("$.spectral_bands[%d].x_ms_group_path", i)})
			}
		}
		if b.GainExplicit {
			gp, rem, err := conv.PPMGainToPermille(b.GainPPM, rec)
			if err != nil {
				return nil, err
			}
			band.GainCorrectionPermille = &gp
			fullPPM := b.GainPPM
			band.XMSGainPPM = &fullPPM
			if rem != 0 {
				loss.Items = append(loss.Items, domain.LossItem{Path: fmt.Sprintf("$.spectral_bands[%d].gain_correction_permille", i), Kind: "gain_precision_remainder", Value: fmt.Sprintf("%dppm rem%d", b.GainPPM, rem), Action: "floor_ppm_to_permille_and_preserve_ppm_extension", PreservedIn: fmt.Sprintf("$.spectral_bands[%d].x_ms_gain_ppm", i)})
			}
		}
		doc.SpectralBands = append(doc.SpectralBands, band)
	}
	if len(loss.Items) > 0 {
		loss.RecomputeDigest()
	} else {
		loss = nil
	}
	return &ExportResult{Schema: domain.SchemaV2, Data: mustJSON(doc), LossReport: loss, Conversion: rec}, nil
}

func exportV3(p *domain.Project) (*ExportResult, error) {
	expGroups := make([]v3ExposureGroup, 0, len(p.Exposures))
	tree := buildV3Tree(p.Tree, p.Bands)
	if len(tree) == 0 {
		tree = buildV3Tree(buildTreeFromBands(p.Bands), p.Bands)
	}
	for _, ex := range p.Exposures {
		ex := ex
		gid := ex.ID
		us := ex.ExposureUS
		expGroups = append(expGroups, v3ExposureGroup{ExposureID: &gid, ExposureMicros: &us, Reference: ex.Reference || us == p.ReferenceExpUS, SpectralTree: tree})
	}
	if len(expGroups) == 0 {
		gid := "exp-ref"
		us := p.ReferenceExpUS
		expGroups = append(expGroups, v3ExposureGroup{ExposureID: &gid, ExposureMicros: &us, Reference: true, SpectralTree: tree})
	}
	seed := p.Seed
	fullWell := p.FullWellDose
	den := conv.PermyriadScale
	rounding := "floor_nonnegative"
	doc := v3Document{
		SchemaVersion: domain.SchemaV3, CompositionID: p.ProjectID, DisplayName: p.Name, RandomSeed: &seed,
		Canvas: &struct {
			W     *int `json:"w"`
			H     *int `json:"h"`
			TileW *int `json:"tileW"`
			TileH *int `json:"tileH"`
		}{W: &p.Width, H: &p.Height, TileW: &p.BlockWidth, TileH: &p.BlockHeight},
		Quantization: &struct {
			Denominator *int64  `json:"denominator"`
			Rounding    *string `json:"rounding"`
		}{Denominator: &den, Rounding: &rounding},
		WhitePoint: &struct {
			XYZBP *[3]int64 `json:"xyzBp"`
		}{XYZBP: &p.WhitePointBP},
		FullWellDose:   &fullWell,
		ExposureGroups: expGroups,
	}
	return &ExportResult{Schema: domain.SchemaV3, Data: mustJSON(doc)}, nil
}

func buildV3Tree(groups []*domain.Group, bands []domain.Band) []v3Node {
	byID := map[string]domain.Band{}
	for _, b := range bands {
		byID[b.ID] = b
	}
	out := make([]v3Node, 0, len(groups))
	for _, g := range groups {
		node := v3Node{NodeID: g.ID, Label: g.Name, Kind: "group"}
		node.Children = buildV3Tree(g.Children, bands)
		for _, id := range g.BandIDs {
			b, ok := byID[id]
			if !ok {
				continue
			}
			id := b.ID
			name := b.Name
			center := b.CenterNM
			gain := b.GainPPM
			resp := b.Response
			node.Children = append(node.Children, v3Node{
				NodeID: "leaf-" + id, Label: b.Name, Kind: "leaf",
				Band: &v3Leaf{BandID: &id, Name: &name, CenterNM: &center, GainPPM: &gain, ResponseBP: &resp},
			})
		}
		out = append(out, node)
	}
	return out
}
