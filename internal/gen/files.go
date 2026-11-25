package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"featurekernel/internal/schema"
)

type Manifest struct {
	SchemaVersion string          `json:"schema_version"`
	Seed          int64           `json:"seed"`
	BatchCount    int             `json:"batch_count"`
	Batches       []BatchManifest `json:"batches"`
}

type BatchManifest struct {
	BatchID     string            `json:"batch_id"`
	ProjectID   string            `json:"project_id"`
	SampleFiles map[string]string `json:"sample_files"`
	TruthFile   string            `json:"truth_file"`
	SHA256      map[string]string `json:"sha256"`
}

func GenerateTree(root string, seed int64, batchCount int) (*Manifest, error) {
	if batchCount <= 0 {
		batchCount = DefaultBatchCount
	}
	sampleDir := filepath.Join(root, "samples")
	truthDir := filepath.Join(root, "truth")
	checkpointDir := filepath.Join(root, "checkpoints")
	for _, dir := range []string{sampleDir, truthDir, checkpointDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	manifest := &Manifest{SchemaVersion: "manifest.v1", Seed: seed, BatchCount: batchCount, Batches: []BatchManifest{}}
	for i := 0; i < batchCount; i++ {
		project, err := BuildProject(seed, i)
		if err != nil {
			return nil, err
		}
		truth, err := BuildTruth(project)
		if err != nil {
			return nil, err
		}
		batch := BatchManifest{
			BatchID: project.BatchID, ProjectID: project.ProjectID,
			SampleFiles: map[string]string{}, SHA256: map[string]string{},
		}
		for _, version := range []string{"v1", "v2", "v3"} {
			data, err := schema.Encode(project, version)
			if err != nil {
				return nil, err
			}
			name := project.BatchID + "." + version + ".json"
			relative := filepath.Join("samples", name)
			if err := os.WriteFile(filepath.Join(root, relative), data, 0o644); err != nil {
				return nil, err
			}
			batch.SampleFiles[version] = relative
			batch.SHA256[relative] = hashBytes(data)
		}
		truthData, err := json.MarshalIndent(truth, "", "  ")
		if err != nil {
			return nil, err
		}
		truthData = append(truthData, '\n')
		truthRelative := filepath.Join("truth", project.BatchID+".truth.json")
		if err := os.WriteFile(filepath.Join(root, truthRelative), truthData, 0o644); err != nil {
			return nil, err
		}
		batch.TruthFile = truthRelative
		batch.SHA256[truthRelative] = hashBytes(truthData)
		manifest.Batches = append(manifest.Batches, batch)
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestData = append(manifestData, '\n')
	manifestRelative := "manifest.json"
	if err := os.WriteFile(filepath.Join(root, manifestRelative), manifestData, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func SortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
