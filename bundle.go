package editorpick

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

type QueryFile struct {
	Schema           string        `json:"schema"`
	StructureVersion string        `json:"structure_version"`
	Precision        PrecisionSpec `json:"precision"`
	Seed             int64         `json:"seed"`
	Camera           Camera        `json:"camera"`
	Rays             []RayQuery    `json:"rays"`
	Boxes            []BoxQuery    `json:"boxes"`
	Points           []PointQuery  `json:"points"`
	EditSequence     []EditStep    `json:"edit_sequence"`
}

type EditStep struct {
	ID          string    `json:"id"`
	Description string    `json:"description,omitempty"`
	Selection   []string  `json:"selection"`
	Pivot       Vec3      `json:"pivot"`
	Ops         []PivotOp `json:"ops"`
	ExpectCode  string    `json:"expect_code,omitempty"`
	Checkpoint  bool      `json:"checkpoint,omitempty"`
}

func LoadQueryFile(path string) (*QueryFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "cannot read queries: " + err.Error()}
	}
	var q QueryFile
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "invalid query JSON: " + err.Error()}
	}
	if q.Schema != QuerySchemaV1 {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "unexpected query schema " + q.Schema}
	}
	if err := q.Precision.Compatible(); err != nil {
		return nil, err
	}
	return &q, nil
}

type RayGeometryTruth struct {
	GeometryIndex int         `json:"geometry_index"`
	NodeID        string      `json:"node_id"`
	Kind          string      `json:"kind"`
	Hit           bool        `json:"hit"`
	Distance      float64     `json:"distance,omitempty"`
	Point         *[3]float64 `json:"point,omitempty"`
}

type RayTruth struct {
	QueryID        string             `json:"query_id"`
	ExpectedStatus string             `json:"expected_status"`
	NodeID         string             `json:"node_id,omitempty"`
	GeometryIndex  int                `json:"geometry_index,omitempty"`
	Point          *[3]float64        `json:"point,omitempty"`
	PerGeometry    []RayGeometryTruth `json:"per_geometry"`
}

type BoxTruth struct {
	QueryID        string   `json:"query_id"`
	ExpectedStatus string   `json:"expected_status"`
	NodeIDs        []string `json:"node_ids"`
}

type PointTruth struct {
	QueryID        string   `json:"query_id"`
	ExpectedStatus string   `json:"expected_status"`
	NodeIDs        []string `json:"node_ids"`
}

type OperationTruth struct {
	StepID        string                 `json:"step_id"`
	ExpectCode    string                 `json:"expect_code,omitempty"`
	WorldMatrices map[string][16]float64 `json:"world_matrices"`
	WorldAABBs    map[string][6]float64  `json:"world_aabbs,omitempty"`
}

type TruthFile struct {
	Schema                string                 `json:"schema"`
	StructureVersion      string                 `json:"structure_version"`
	Precision             PrecisionSpec          `json:"precision"`
	Seed                  int64                  `json:"seed"`
	DeclaredLocalMatrices map[string][16]float64 `json:"declared_local_matrices"`
	RayTruths             []RayTruth             `json:"ray_truths"`
	BoxTruths             []BoxTruth             `json:"box_truths"`
	PointTruths           []PointTruth           `json:"point_truths"`
	OperationTruths       []OperationTruth       `json:"operation_truths"`
}

func LoadTruthFile(path string) (*TruthFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "cannot read truth: " + err.Error()}
	}
	var t TruthFile
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "invalid truth JSON: " + err.Error()}
	}
	if t.Schema != TruthSchemaV1 {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "unexpected truth schema " + t.Schema}
	}
	return &t, nil
}

type ManifestFile struct {
	Schema           string            `json:"schema"`
	Seed             int64             `json:"seed"`
	StructureVersion string            `json:"structure_version"`
	Precision        PrecisionSpec     `json:"precision"`
	Files            map[string]string `json:"files"`
}

func LoadManifest(path string) (*ManifestFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "cannot read manifest: " + err.Error()}
	}
	var m ManifestFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "invalid manifest JSON: " + err.Error()}
	}
	return &m, nil
}

// VerifyFileHashes checks the generator-written local file manifest.
func (m *ManifestFile) VerifyFileHashes(dir string) error {
	for name, expected := range m.Files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return &DomainError{Code: ESchema, ObjectID: name, Message: "manifest file missing: " + err.Error()}
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != expected {
			return &DomainError{Code: EHash, ObjectID: name, Message: "manifest file hash mismatch"}
		}
	}
	return nil
}
