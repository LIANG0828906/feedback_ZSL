package incremental

import (
	"encoding/json"

	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/hash"
	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/storage"
)

// versionMap 是区域参数/几何版本的有序映射快照。
type versionMap struct {
	Param map[string]int `json:"param"`
	Geom  map[string]int `json:"geom"`
}

// BuildMeta 计算当前工程的快照元数据。
func (o *Orchestrator) BuildMeta() storage.Meta {
	vm := versionMap{Param: map[string]int{}, Geom: map[string]int{}}
	for _, r := range o.project.Regions {
		vm.Param[r.ID] = r.ParamVersion
		vm.Geom[r.ID] = r.GeomVersion
	}
	paramJSON, _ := json.Marshal(vm.Param)
	geomJSON, _ := json.Marshal(vm.Geom)
	return storage.Meta{
		ProjectHash:   hash.Content(o.project),
		AlgoVersion:   engine.AlgorithmVersion,
		GeoScaleBits:  o.cfg.ScaleBits,
		ParamVersions: string(paramJSON),
		GeomVersions:  string(geomJSON),
	}
}

// SaveSnapshotMeta 在全量重算开始前写入快照元数据。
func (o *Orchestrator) SaveSnapshotMeta() error {
	if o.store == nil {
		return nil
	}
	return o.store.SaveMeta(o.BuildMeta())
}

// VerifySnapshot 校验磁盘快照与当前工程是否兼容。
// 任一不匹配都整体作废并返回 invalid=true，禁止混用旧窗口结果。
func (o *Orchestrator) VerifySnapshot() (valid bool, reason string, err error) {
	if o.store == nil {
		return true, "no-store", nil
	}
	meta, found, err := o.store.LoadMeta()
	if err != nil {
		return false, "storage-error", err
	}
	if !found {
		return false, "empty-snapshot", nil
	}
	current := o.BuildMeta()
	switch {
	case meta.ProjectHash != current.ProjectHash:
		return false, "project content hash mismatch", nil
	case meta.AlgoVersion != current.AlgoVersion:
		return false, "algorithm version mismatch", nil
	case meta.GeoScaleBits != current.GeoScaleBits:
		return false, "geo scale bits mismatch", nil
	case meta.ParamVersions != current.ParamVersions:
		return false, "region param version mismatch", nil
	case meta.GeomVersions != current.GeomVersions:
		return false, "region geom version mismatch", nil
	}
	return true, "ok", nil
}

// Recover 实现断点恢复：校验快照，不兼容则作废并全量重算；
// 兼容则直接复用持久化缓存做增量路径，已提交窗口不再重算。
func (o *Orchestrator) Recover() ([]model.Result, []model.Profile, string, error) {
	valid, reason, err := o.VerifySnapshot()
	if err != nil {
		return nil, nil, reason, err
	}
	if !valid {
		// 整体作废：清空所有窗口与对结果，禁止混用。
		if err := o.store.Reset(); err != nil {
			return nil, nil, reason, err
		}
		if err := o.SaveSnapshotMeta(); err != nil {
			return nil, nil, reason, err
		}
		results, profiles, err := o.FullRecompute()
		return results, profiles, reason, err
	}
	// 快照兼容：增量路径从持久化缓存命中恢复。
	results, profiles, err := o.Incremental()
	return results, profiles, reason, err
}

// LoadedWindows 透传已提交窗口序号。
func (o *Orchestrator) LoadedWindows() ([]int, error) {
	if o.store == nil {
		return nil, nil
	}
	return o.store.LoadedWindows()
}
