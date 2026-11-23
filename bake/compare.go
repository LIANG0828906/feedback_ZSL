package bake

import (
	"bytes"
	"encoding/json"
	"fmt"

	"terrain-bake/project"
)

var artifactComponentOrder = []string{
	"positions",
	"normals",
	"indices",
	"materialWeights",
	"materialRatios",
	"instances",
	"bounds",
	"seams",
	"artifact",
}

type DiffReport struct {
	Equal        bool   `json:"equal"`
	Compared     int    `json:"compared"`
	FirstBlock   string `json:"firstBlock,omitempty"`
	ArtifactType string `json:"artifactType,omitempty"`
	Message      string `json:"message"`
}

// ArtifactComponentBytes returns canonical bytes for a single product type.
func ArtifactComponentBytes(a *Artifact, typ string) []byte {
	if a == nil {
		return nil
	}
	var v any
	switch typ {
	case "positions":
		v = a.Positions
	case "normals":
		v = a.Normals
	case "indices":
		v = a.Indices
	case "materialWeights":
		v = a.MaterialWeights
	case "materialRatios":
		v = a.MaterialRatios
	case "instances":
		v = a.Instances
	case "bounds":
		v = a.Bounds
	case "seams":
		v = a.Seams
	case "artifact":
		v = a
	default:
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// CompareArtifacts locates the first product-type difference in deterministic
// block order and returns E_DIFF with block and product type details.
func CompareArtifacts(actual, expected map[string]*Artifact, order []string) (*DiffReport, *project.Error) {
	report := &DiffReport{Message: "artifacts are byte-identical"}
	for _, id := range order {
		a, aok := actual[id]
		b, bok := expected[id]
		if !aok || !bok {
			report.Equal = false
			report.FirstBlock = id
			report.ArtifactType = "artifact"
			report.Message = "artifact missing on one side"
			return report, diffError(report)
		}
		report.Compared++
		if bytes.Equal(a.CanonicalBytes(), b.CanonicalBytes()) {
			continue
		}
		for _, typ := range artifactComponentOrder {
			if bytes.Equal(ArtifactComponentBytes(a, typ), ArtifactComponentBytes(b, typ)) {
				continue
			}
			report.Equal = false
			report.FirstBlock = id
			report.ArtifactType = typ
			report.Message = fmt.Sprintf("first byte difference in block %s/%s", id, typ)
			return report, diffError(report)
		}
		report.Equal = false
		report.FirstBlock = id
		report.ArtifactType = "artifact"
		report.Message = fmt.Sprintf("artifact bytes differ for block %s", id)
		return report, diffError(report)
	}
	report.Equal = true
	return report, nil
}

func diffError(report *DiffReport) *project.Error {
	return project.NewError(project.CodeDiff, report.FirstBlock, "artifactType:"+report.ArtifactType, "%s", report.Message).
		WithDetail("artifactType", report.ArtifactType).
		WithDetail("firstBlock", report.FirstBlock)
}
