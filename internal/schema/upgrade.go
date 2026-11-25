package schema

import (
	"fmt"

	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

// Upgrade applies explicit, version-by-version migrations. Each defaulted field
// is recorded with its old value and the declared default source.
func Upgrade(p *model.Project, target int) (*model.UpgradeResult, error) {
	if p == nil {
		return nil, kerr.New(kerr.CodeVersion, "project", "nil project")
	}
	if target < 1 || target > 3 {
		return nil, kerr.New(kerr.CodeVersion, "format_version", fmt.Sprintf("unknown target version %d", target))
	}
	if target < p.SourceVersion {
		return nil, kerr.New(kerr.CodeVersion, "format_version", "Upgrade only migrates forward; use Marshal for downgrade/export")
	}
	res := &model.UpgradeResult{Project: p.Clone()}
	for res.Project.SourceVersion < target {
		switch res.Project.SourceVersion {
		case 1:
			applyV1ToV2(res)
		case 2:
			applyV2ToV3(res)
		default:
			return nil, kerr.New(kerr.CodeVersion, "format_version", fmt.Sprintf("unknown source version %d", res.Project.SourceVersion))
		}
	}
	return res, nil
}

func applyV1ToV2(res *model.UpgradeResult) {
	p := res.Project
	oldNorm := "per_layer"
	if p.NormMode == "" {
		p.NormMode = "global"
	}
	res.Traces = append(res.Traces, model.TransformTrace{
		Path:        "normalization",
		OldValue:    oldNorm,
		DefaultFrom: "v2.default:normalization=global",
	})
	res.Traces = append(res.Traces, model.TransformTrace{
		Path:        "slopeCapDeg",
		OldValue:    "<absent>",
		DefaultFrom: "v2.default:slopeCapDeg=disabled",
	})
	for i := range p.Layers {
		layer := &p.Layers[i]
		if layer.Curve != nil && layer.Curve.Driver == "" {
			layer.Curve.Driver = "height"
			res.Traces = append(res.Traces, model.TransformTrace{
				Path:        layerPath(i, "driver"),
				OldValue:    "<absent>",
				DefaultFrom: "v2.default:driver=height",
			})
		}
		if layer.SlopeCapURad < 0 {
			layer.SlopeCapURad = 0
		}
	}
	p.SourceVersion = 2
}

func applyV2ToV3(res *model.UpgradeResult) {
	p := res.Project
	for i := range p.Layers {
		layer := &p.Layers[i]
		res.Traces = append(res.Traces, model.TransformTrace{
			Path:        layerPath(i, "noise"),
			OldValue:    "<absent>",
			DefaultFrom: "v3.default:noise=disabled(amplitude=0)",
		})
		res.Traces = append(res.Traces, model.TransformTrace{
			Path:        layerPath(i, "slopeCapDeg"),
			OldValue:    "<project-level>",
			DefaultFrom: "v3.default:per-layer slope cap inherited from project cap",
		})
		if layer.Noise == nil {
			layer.Noise = nil
		}
	}
	p.SourceVersion = 3
}
