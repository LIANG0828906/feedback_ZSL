package bake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"terrain-bake/internal/atomicfs"
	"terrain-bake/project"
)

type RenderBlock struct {
	BlockID        string         `json:"blockId"`
	Col            int            `json:"col"`
	Row            int            `json:"row"`
	Status         string         `json:"status"`
	CacheHit       bool           `json:"cacheHit"`
	LastBakeMS     float64        `json:"lastBakeMs"`
	ArtifactFile   string         `json:"artifactFile,omitempty"`
	ArtifactSHA256 string         `json:"artifactSha256,omitempty"`
	CacheKey       string         `json:"cacheKey"`
	Dependencies   []string       `json:"dependencies"`
	VertexCount    int            `json:"vertexCount"`
	InstanceCount  int            `json:"instanceCount"`
	Bounds         Bounds         `json:"bounds"`
	Seams          SeamStats      `json:"seams"`
	MaterialRatios []float64      `json:"materialRatios"`
	LastError      *project.Error `json:"lastError,omitempty"`
}

type RenderManifest struct {
	SchemaVersion        int           `json:"schemaVersion"`
	ContentHash          string        `json:"contentHash"`
	BakeAlgorithmVersion string        `json:"bakeAlgorithmVersion"`
	BakeParamsVersion    string        `json:"bakeParamsVersion"`
	BakeParamsHash       string        `json:"bakeParamsHash"`
	Generation           uint64        `json:"generation"`
	CacheSnapshotPath    string        `json:"cacheSnapshotPath"`
	CacheSnapshotHash    string        `json:"cacheSnapshotHash"`
	Blocks               []RenderBlock `json:"blocks"`
}

func (m *Manager) writeBuildOutputsWith(write SnapshotWriteResult) *project.Error {
	status := m.Status()
	blockDir := filepath.Join(m.opts.OutputDir, "blocks")
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		return project.NewError(project.CodeCache, "", "outputDir", "create output directory: %v", err)
	}
	manifest := RenderManifest{
		SchemaVersion:        1,
		ContentHash:          status.ContentHash,
		BakeAlgorithmVersion: status.AlgorithmVersion,
		BakeParamsVersion:    status.ParamsVersion,
		BakeParamsHash:       status.ParamsHash,
		Generation:           status.Generation,
		CacheSnapshotPath:    status.CacheSnapshotPath,
		CacheSnapshotHash:    write.Hash,
	}
	for _, st := range status.States {
		a := m.Artifact(st.BlockID)
		rec := RenderBlock{
			BlockID:        st.BlockID,
			Col:            st.Col,
			Row:            st.Row,
			Status:         st.Status,
			CacheHit:       st.CacheHit,
			LastBakeMS:     st.LastBakeMS,
			ArtifactSHA256: st.ArtifactHash,
			Dependencies:   st.Dependencies,
			VertexCount:    st.VertexCount,
			InstanceCount:  st.InstanceCount,
			Bounds:         st.Bounds,
			Seams:          st.Seams,
			MaterialRatios: st.MaterialRatios,
			LastError:      st.LastError,
			CacheKey: CacheKey(status.ContentHash, status.ParamsVersion, status.ParamsHash,
				st.BlockID, st.Dependencies),
		}
		if a == nil {
			path := filepath.Join(blockDir, st.BlockID+".json")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return project.NewError(project.CodeCache, st.BlockID, "artifactFile", "remove stale output: %v", err)
			}
			manifest.Blocks = append(manifest.Blocks, rec)
			continue
		}
		rel := filepath.ToSlash(filepath.Join("blocks", st.BlockID+".json"))
		rec.ArtifactFile = rel
		rec.ArtifactSHA256 = a.Hash()
		if err := atomicfs.WriteFile(filepath.Join(m.opts.OutputDir, rel), a.CanonicalBytes(), 0o644); err != nil {
			return project.NewError(project.CodeCache, st.BlockID, "artifactFile", "write artifact atomically: %v", err)
		}
		manifest.Blocks = append(manifest.Blocks, rec)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return project.NewError(project.CodeCache, "", "outputManifest", "encode render manifest: %v", err)
	}
	data = append(data, '\n')
	if err := atomicfs.WriteFile(filepath.Join(m.opts.OutputDir, "manifest.json"), data, 0o644); err != nil {
		return project.NewError(project.CodeCache, "", "outputManifest", "write render manifest atomically: %v", err)
	}
	return nil
}

func (m *Manager) OutputPath() string {
	return m.opts.OutputDir
}

func (m *Manager) CachePath() string {
	return m.snapshot.Path
}

func (m *Manager) Describe() string {
	s := m.Status()
	return fmt.Sprintf("generation=%d artifacts=%d content=%s cache=%s", s.Generation, s.ArtifactCount, s.ContentHash, s.CacheReason)
}
