package editorpick

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	SceneSchemaV1    = "editorpick.scene/v1"
	QuerySchemaV1    = "editorpick.queries/v1"
	TruthSchemaV1    = "editorpick.truth/v1"
	SnapshotSchemaV1 = "editorpick.snapshot/v1"
	// StructureVersion changes whenever the serialized structure or derivation
	// contract changes in an incompatible way.
	StructureVersion = "struct-2026-11-25-v1"
)

type PrecisionSpec struct {
	Kind             string  `json:"kind"`
	FractionalDigits int     `json:"fractional_digits"`
	Scale            int64   `json:"scale"`
	Step             float64 `json:"step"`
}

func DefaultPrecision() PrecisionSpec {
	return PrecisionSpec{Kind: "decimal", FractionalDigits: 6, Scale: FixedScale, Step: 1.0 / float64(FixedScale)}
}

func (p PrecisionSpec) Compatible() error {
	if p.Kind != "decimal" || p.FractionalDigits != 6 || p.Scale != FixedScale {
		return &DomainError{Code: EVersion, ObjectID: "precision", Message: fmt.Sprintf("unsupported precision declaration %+v", p)}
	}
	return nil
}

type Vec3DTO struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3DTO) toVec3(nodeID string) (Vec3, error) {
	if !isFinite(v.X) || !isFinite(v.Y) || !isFinite(v.Z) {
		return Vec3{}, newSchema(nodeID, "vector components must be finite")
	}
	return Vec3FromFloat(v.X, v.Y, v.Z), nil
}

type NodeDTO struct {
	ID            *string   `json:"id"`
	ParentID      *string   `json:"parent_id"`
	Matrix        []float64 `json:"matrix"`
	GeometryIndex *int      `json:"geometry_index,omitempty"`
}

type GeometryDTO struct {
	Kind     string      `json:"kind"`
	Min      *Vec3DTO    `json:"min,omitempty"`
	Max      *Vec3DTO    `json:"max,omitempty"`
	Center   *Vec3DTO    `json:"center,omitempty"`
	Radius   float64     `json:"radius,omitempty"`
	Vertices [][]float64 `json:"vertices,omitempty"`
	Indices  []int       `json:"indices,omitempty"`
}

type SceneFile struct {
	Schema           string        `json:"schema"`
	StructureVersion string        `json:"structure_version"`
	Precision        PrecisionSpec `json:"precision"`
	Seed             int64         `json:"seed"`
	Nodes            []NodeDTO     `json:"nodes"`
	Geometries       []GeometryDTO `json:"geometries"`
}

type GeometryKind string

const (
	GeometryAABB   GeometryKind = "aabb"
	GeometrySphere GeometryKind = "sphere"
	GeometryMesh   GeometryKind = "mesh"
)

type Geometry struct {
	Index      int
	Kind       GeometryKind
	LocalAABB  AABB
	Center     Vec3
	Radius     Fixed
	Vertices   []Vec3
	Triangles  [][3]int
	SourceHash string
}

type Node struct {
	ID            string
	ParentID      string
	Local         Mat4
	GeometryIndex int
	Depth         int
}

type Scene struct {
	Schema           string
	StructureVersion string
	Precision        PrecisionSpec
	Seed             int64
	Nodes            []*Node
	NodeByID         map[string]*Node
	Children         map[string][]string
	Roots            []string
	Geometries       []Geometry
	sourceHash       string
}

func LoadSceneFile(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: ESchema, ObjectID: path, Message: "cannot read scene: " + err.Error()}
	}
	return ParseScene(data)
}

func ParseScene(data []byte) (*Scene, error) {
	var file SceneFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, &DomainError{Code: ESchema, NodeID: "<document>", Message: "invalid scene JSON: " + err.Error()}
	}
	if file.Schema != SceneSchemaV1 {
		return nil, newSchema("<document>", "unexpected schema %q", file.Schema)
	}
	if file.StructureVersion != StructureVersion {
		return nil, &DomainError{Code: EVersion, ObjectID: "structure_version", Message: "unsupported structure version " + file.StructureVersion}
	}
	if err := file.Precision.Compatible(); err != nil {
		return nil, err
	}
	if len(file.Nodes) == 0 {
		return nil, newSchema("<document>", "scene must contain at least one node")
	}

	scene := &Scene{
		Schema:           file.Schema,
		StructureVersion: file.StructureVersion,
		Precision:        file.Precision,
		Seed:             file.Seed,
		NodeByID:         make(map[string]*Node, len(file.Nodes)),
		Children:         make(map[string][]string),
	}

	geometries, err := parseGeometries(file.Geometries)
	if err != nil {
		return nil, err
	}
	scene.Geometries = geometries

	for i := range file.Nodes {
		dto := file.Nodes[i]
		nodeLabel := fmt.Sprintf("nodes[%d]", i)
		if dto.ID == nil || strings.TrimSpace(*dto.ID) == "" {
			return nil, newSchema(nodeLabel, "missing required field id")
		}
		id := strings.TrimSpace(*dto.ID)
		nodeLabel = id
		if _, exists := scene.NodeByID[id]; exists {
			return nil, newSchema(id, "duplicate node id")
		}
		if len(dto.Matrix) != 16 {
			return nil, newSchema(id, "matrix must contain exactly 16 finite numbers")
		}
		matrix := [16]float64{}
		for j, v := range dto.Matrix {
			if !isFinite(v) {
				return nil, newSchema(id, "matrix[%d] is not finite", j)
			}
			matrix[j] = v
		}
		node := &Node{ID: id, Local: Mat4FromFloat64(matrix), GeometryIndex: -1}
		if dto.ParentID != nil {
			node.ParentID = strings.TrimSpace(*dto.ParentID)
		}
		if dto.GeometryIndex != nil {
			idx := *dto.GeometryIndex
			if idx < 0 || idx >= len(scene.Geometries) {
				return nil, newRef(id, "geometry_index %d is outside 0..%d", idx, len(scene.Geometries)-1)
			}
			node.GeometryIndex = idx
		}
		scene.Nodes = append(scene.Nodes, node)
		scene.NodeByID[id] = node
	}

	for _, node := range scene.Nodes {
		if node.ParentID == "" {
			continue
		}
		parent, ok := scene.NodeByID[node.ParentID]
		if !ok {
			return nil, newRef(node.ID, "parent_id %q does not exist", node.ParentID)
		}
		_ = parent
	}

	if err := detectCycles(scene); err != nil {
		return nil, err
	}

	for _, node := range scene.Nodes {
		if node.ParentID == "" {
			scene.Roots = append(scene.Roots, node.ID)
		} else {
			scene.Children[node.ParentID] = append(scene.Children[node.ParentID], node.ID)
		}
	}
	assignDepths(scene)

	hash := sha256.Sum256(data)
	scene.sourceHash = hex.EncodeToString(hash[:])
	return scene, nil
}

func parseGeometries(dtos []GeometryDTO) ([]Geometry, error) {
	out := make([]Geometry, 0, len(dtos))
	for i, dto := range dtos {
		g := Geometry{Index: i}
		label := fmt.Sprintf("geometry[%d]", i)
		switch GeometryKind(dto.Kind) {
		case GeometryAABB:
			g.Kind = GeometryAABB
			if dto.Min == nil || dto.Max == nil {
				return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "aabb requires min and max"}
			}
			min, err := dto.Min.toVec3(label)
			if err != nil {
				return nil, err
			}
			max, err := dto.Max.toVec3(label)
			if err != nil {
				return nil, err
			}
			g.LocalAABB = NewAABB(min, max)
			if !g.LocalAABB.Valid() {
				return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "aabb bounds are invalid"}
			}
		case GeometrySphere:
			g.Kind = GeometrySphere
			if dto.Center == nil {
				return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "sphere requires center"}
			}
			center, err := dto.Center.toVec3(label)
			if err != nil {
				return nil, err
			}
			if !isFinite(dto.Radius) || dto.Radius < 0 {
				return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "sphere radius must be finite and non-negative"}
			}
			g.Center = center
			g.Radius = FixedFromFloat(dto.Radius)
			g.LocalAABB = AABB{
				Min: Vec3{center.X - g.Radius, center.Y - g.Radius, center.Z - g.Radius},
				Max: Vec3{center.X + g.Radius, center.Y + g.Radius, center.Z + g.Radius},
			}
		case GeometryMesh:
			g.Kind = GeometryMesh
			if len(dto.Vertices) < 3 || len(dto.Indices) < 3 || len(dto.Indices)%3 != 0 {
				return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "mesh requires vertices and triangle index triples"}
			}
			g.Vertices = make([]Vec3, len(dto.Vertices))
			var bounds *AABB
			for vi, raw := range dto.Vertices {
				if len(raw) != 3 || !allFinite(raw) {
					return nil, &DomainError{Code: ESchema, ObjectID: fmt.Sprintf("%s.vertices[%d]", label, vi), Message: "vertex must contain three finite coordinates"}
				}
				v := Vec3FromFloat(raw[0], raw[1], raw[2])
				g.Vertices[vi] = v
				if bounds == nil {
					b := AABB{Min: v, Max: v}
					bounds = &b
				} else {
					*bounds = bounds.Union(AABB{Min: v, Max: v})
				}
			}
			maxIndex := len(dto.Vertices) - 1
			for ii, idx := range dto.Indices {
				if idx < 0 || idx > maxIndex {
					return nil, &DomainError{Code: ERef, ObjectID: label, Message: fmt.Sprintf("index %d at positions[%d] is outside vertices", idx, ii)}
				}
			}
			for t := 0; t < len(dto.Indices)/3; t++ {
				a, b, c := dto.Indices[t*3], dto.Indices[t*3+1], dto.Indices[t*3+2]
				g.Triangles = append(g.Triangles, [3]int{a, b, c})
			}
			g.LocalAABB = *bounds
		default:
			return nil, &DomainError{Code: ESchema, ObjectID: label, Message: "unknown geometry kind " + dto.Kind}
		}
		sum, err := canonicalHash(g)
		if err != nil {
			return nil, err
		}
		g.SourceHash = sum
		out = append(out, g)
	}
	return out, nil
}

func allFinite(values []float64) bool {
	for _, v := range values {
		if !isFinite(v) {
			return false
		}
	}
	return true
}

func detectCycles(scene *Scene) error {
	state := make(map[string]int, len(scene.Nodes)) // 0 unvisited, 1 visiting, 2 done
	var stack []string
	var dfs func(id string) error
	dfs = func(id string) error {
		switch state[id] {
		case 1:
			return newCycle(id)
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		node := scene.NodeByID[id]
		if node.ParentID != "" {
			if err := dfs(node.ParentID); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(scene.Nodes))
	for _, n := range scene.Nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == 0 {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func assignDepths(scene *Scene) {
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		n := scene.NodeByID[id]
		n.Depth = depth
		for _, child := range scene.Children[id] {
			walk(child, depth+1)
		}
	}
	for _, root := range scene.Roots {
		walk(root, 0)
	}
}

func canonicalHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Scene) SourceHash() string {
	if s.sourceHash != "" {
		return s.sourceHash
	}
	h, _ := canonicalHash(struct {
		StructureVersion string
		Precision        PrecisionSpec
		Nodes            []*Node
		Geometries       []Geometry
	}{s.StructureVersion, s.Precision, s.Nodes, s.Geometries})
	s.sourceHash = h
	return h
}

func (s *Scene) clone() *Scene {
	copyScene := &Scene{
		Schema:           s.Schema,
		StructureVersion: s.StructureVersion,
		Precision:        s.Precision,
		Seed:             s.Seed,
		NodeByID:         make(map[string]*Node, len(s.Nodes)),
		Children:         make(map[string][]string, len(s.Children)),
		Roots:            append([]string(nil), s.Roots...),
		Geometries:       append([]Geometry(nil), s.Geometries...),
		sourceHash:       s.sourceHash,
	}
	for _, n := range s.Nodes {
		cp := *n
		copyScene.Nodes = append(copyScene.Nodes, &cp)
		copyScene.NodeByID[cp.ID] = &cp
	}
	for parent, children := range s.Children {
		copyScene.Children[parent] = append([]string(nil), children...)
	}
	return copyScene
}
