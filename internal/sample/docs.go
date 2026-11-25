package sample

import (
	"samplingkernel/internal/loader"
	"samplingkernel/internal/model"
)

func renderDocs(p *model.Project) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, err := loader.Render(p, version)
		if err != nil {
			return nil, err
		}
		out[version] = data
	}
	return out, nil
}
