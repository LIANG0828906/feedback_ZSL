package gen

import (
	"skillsched/internal/model"
	"skillsched/internal/schema"
)

// WireSet 是同一规则工程在三个 schema 版本下的序列化描述。
type WireSet struct {
	V1      []byte
	V2      []byte
	V3      []byte
	Reports map[model.Version]*schema.Report
}

// ExportWires 把内部工程导出为三版本 JSON。
func (sc *Scenario) ExportWires() (*WireSet, error) {
	v1, r1, err := schema.Export(sc.Rules, model.V1)
	if err != nil {
		return nil, err
	}
	v2, r2, err := schema.Export(sc.Rules, model.V2)
	if err != nil {
		return nil, err
	}
	v3, r3, err := schema.Export(sc.Rules, model.V3)
	if err != nil {
		return nil, err
	}
	return &WireSet{
		V1: v1, V2: v2, V3: v3,
		Reports: map[model.Version]*schema.Report{model.V1: r1, model.V2: r2, model.V3: r3},
	}, nil
}
