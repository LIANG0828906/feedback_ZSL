package spatialpick

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// LoadSceneWithTruth reads, validates and only then constructs both documents.
// A non-nil error always means neither returned document is usable.
func LoadSceneWithTruth(scenePath, truthPath string) (*Scene, *Truth, error) {
	sceneBytes, err := os.ReadFile(scenePath)
	if err != nil {
		return nil, nil, &CodedError{Code: CodeSchema, Op: "load_scene", Field: scenePath, Cause: err}
	}
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		return nil, nil, &CodedError{Code: CodeSchema, Op: "load_truth", Field: truthPath, Cause: err}
	}
	return ParseSceneWithTruth(sceneBytes, truthBytes)
}

func ParseSceneWithTruth(sceneBytes, truthBytes []byte) (*Scene, *Truth, error) {
	sceneDoc, err := decodeJSONDocument(sceneBytes, "scene")
	if err != nil {
		return nil, nil, err
	}
	if err := validateSceneDocument(sceneDoc); err != nil {
		return nil, nil, err
	}
	var scene Scene
	if err := json.Unmarshal(sceneBytes, &scene); err != nil {
		return nil, nil, &CodedError{Code: CodeSchema, Op: "decode_scene", Message: err.Error(), Cause: err}
	}
	if err := validateDecodedScene(&scene); err != nil {
		return nil, nil, err
	}

	truthDoc, err := decodeJSONDocument(truthBytes, "truth")
	if err != nil {
		return nil, nil, err
	}
	if err := validateTruthDocument(truthDoc, &scene); err != nil {
		return nil, nil, err
	}
	var truth Truth
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		return nil, nil, &CodedError{Code: CodeSchema, Op: "decode_truth", Message: err.Error(), Cause: err}
	}
	if err := validateDecodedTruth(&scene, &truth); err != nil {
		return nil, nil, err
	}
	return &scene, &truth, nil
}

func decodeJSONDocument(data []byte, op string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &CodedError{Code: CodeSchema, Op: op, Message: "invalid JSON", Cause: err}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, coded(CodeSchema, op, "trailing JSON content")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, coded(CodeSchema, op, "document root must be an object")
	}
	return m, nil
}

func validateSceneDocument(doc map[string]any) error {
	if _, err := requireIntField(doc, "format_version", "scene.format_version"); err != nil {
		return err
	}
	if _, err := requireStringField(doc, "scene_version", "scene.scene_version"); err != nil {
		return err
	}
	precision, err := requireIntField(doc, "precision", "scene.precision")
	if err != nil {
		return err
	}
	if !validPrecision(int(precision)) {
		return schemaField("scene.precision", "precision must be in [0,9]", "")
	}
	if _, err := requireStringField(doc, "content_hash", "scene.content_hash"); err != nil {
		return err
	}

	camera, err := requireObjectField(doc, "camera", "scene.camera")
	if err != nil {
		return err
	}
	for _, key := range []string{"viewport_width", "viewport_height", "world_height", "eye", "right", "up", "forward"} {
		if _, err := requireField(camera, key, "scene.camera."+key); err != nil {
			return err
		}
	}
	for _, key := range []string{"viewport_width", "viewport_height", "world_height"} {
		v, err := requireFloatField(camera, key, "scene.camera."+key)
		if err != nil {
			return err
		}
		if v <= 0 {
			return schemaField("scene.camera."+key, "must be positive", "")
		}
	}
	for _, key := range []string{"eye", "right", "up", "forward"} {
		if _, err := requireVec(camera[key], "scene.camera."+key, 3); err != nil {
			return err
		}
	}

	geometriesAny, err := requireArrayField(doc, "geometries", "scene.geometries")
	if err != nil {
		return err
	}
	geometryIDs := make(map[string]int, len(geometriesAny))
	for i, ga := range geometriesAny {
		path := fmt.Sprintf("scene.geometries[%d]", i)
		g, err := asObject(ga, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(g, "id", path+".id")
		if err != nil {
			return err
		}
		if id == "" {
			return schemaField(path+".id", "must not be empty", "")
		}
		if _, exists := geometryIDs[id]; exists {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", NodeID: id, Field: path + ".id", Message: "duplicate geometry id"}
		}
		geometryIDs[id] = i
		kind, err := requireStringField(g, "kind", path+".kind")
		if err != nil {
			return err
		}
		switch kind {
		case GeometryAABB:
			minV, err := requireVecField(g, "min", path+".min")
			if err != nil {
				return err
			}
			maxV, err := requireVecField(g, "max", path+".max")
			if err != nil {
				return err
			}
			if minV.X > maxV.X || minV.Y > maxV.Y || minV.Z > maxV.Z {
				return schemaField(path+".max", "AABB max must be component-wise >= min", "")
			}
		case GeometrySphere:
			if _, err := requireVecField(g, "center", path+".center"); err != nil {
				return err
			}
			radius, err := requireFloatField(g, "radius", path+".radius")
			if err != nil {
				return err
			}
			if radius <= 0 {
				return schemaField(path+".radius", "must be positive", "")
			}
		case GeometryMesh:
			verts, err := requireArrayField(g, "vertices", path+".vertices")
			if err != nil {
				return err
			}
			if len(verts) < 3 {
				return schemaField(path+".vertices", "mesh requires at least three vertices", "")
			}
			for j, v := range verts {
				if _, err := requireVec(v, fmt.Sprintf("%s.vertices[%d]", path, j), 3); err != nil {
					return err
				}
			}
			tris, err := requireArrayField(g, "triangles", path+".triangles")
			if err != nil {
				return err
			}
			if len(tris) == 0 {
				return schemaField(path+".triangles", "mesh requires at least one triangle", "")
			}
			for j, triAny := range tris {
				tri, err := requireNumbers(triAny, fmt.Sprintf("%s.triangles[%d]", path, j), 3)
				if err != nil {
					return err
				}
				for k := 0; k < 3; k++ {
					idx := int(tri[k])
					if tri[k] != float64(idx) || idx < 0 || idx >= len(verts) {
						return &CodedError{
							Code: CodeRef, Op: "validate_scene", GeometryID: id,
							Field:   fmt.Sprintf("%s.triangles[%d][%d]", path, j, k),
							Message: "triangle vertex index is out of range",
						}
					}
				}
			}
		default:
			return schemaField(path+".kind", "unknown geometry kind", id)
		}
	}

	nodesAny, err := requireArrayField(doc, "nodes", "scene.nodes")
	if err != nil {
		return err
	}
	nodeIDs := make(map[string]int, len(nodesAny))
	parents := make([]string, len(nodesAny))
	for i, na := range nodesAny {
		path := fmt.Sprintf("scene.nodes[%d]", i)
		n, err := asObject(na, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(n, "id", path+".id")
		if err != nil {
			return err
		}
		if id == "" {
			return schemaField(path+".id", "must not be empty", "")
		}
		if _, exists := nodeIDs[id]; exists {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", NodeID: id, Field: path + ".id", Message: "duplicate node id"}
		}
		nodeIDs[id] = i
		parent, err := requireStringField(n, "parent_id", path+".parent_id")
		if err != nil {
			return attachNodeID(err, id)
		}
		parents[i] = parent
		m, err := requireArrayField(n, "local_matrix", path+".local_matrix")
		if err != nil {
			return attachNodeID(err, id)
		}
		if len(m) != 16 {
			return schemaField(path+".local_matrix", "must contain exactly 16 numbers", id)
		}
		for j, v := range m {
			f, ok := asFloat(v)
			if !ok || !finite(f) {
				return schemaField(fmt.Sprintf("%s.local_matrix[%d]", path, j), "must be a finite number", id)
			}
		}
		gidx, err := requireIntField(n, "geometry_index", path+".geometry_index")
		if err != nil {
			return attachNodeID(err, id)
		}
		if gidx < -1 || gidx >= int64(len(geometriesAny)) {
			return &CodedError{Code: CodeRef, Op: "validate_scene", NodeID: id, Field: path + ".geometry_index", Message: "geometry index is out of range"}
		}
	}
	for i, parent := range parents {
		if parent == "" {
			continue
		}
		if _, ok := nodeIDs[parent]; !ok {
			path := fmt.Sprintf("scene.nodes[%d].parent_id", i)
			return &CodedError{Code: CodeRef, Op: "validate_scene", NodeID: nodeIDAt(nodesAny, i), Field: path, Message: "parent node does not exist"}
		}
	}
	if err := detectCycle(nodesAny, nodeIDs, parents); err != nil {
		return err
	}

	if err := validateQueryArrays(doc, nodeIDs); err != nil {
		return err
	}
	if err := validateEditArray(doc, nodeIDs); err != nil {
		return err
	}
	return nil
}

func validateDecodedScene(sc *Scene) error {
	if sc.FormatVersion != FormatVersion {
		return schemaField("scene.format_version", fmt.Sprintf("unsupported format version %d", sc.FormatVersion), "")
	}
	if sc.SceneVersion == "" {
		return schemaField("scene.scene_version", "must not be empty", "")
	}
	if !validPrecision(sc.Precision) {
		return schemaField("scene.precision", "precision must be in [0,9]", "")
	}
	for i, n := range sc.Nodes {
		fm, err := QuantizeMatrix(Mat4(n.LocalMatrix), sc.Precision)
		if err != nil {
			var ce *CodedError
			if IsCoded(err, &ce) {
				ce.NodeID = n.ID
				ce.Field = fmt.Sprintf("scene.nodes[%d].local_matrix", i)
			}
			return err
		}
		if !fm.IsAffine(sc.Precision) {
			return schemaField(fmt.Sprintf("scene.nodes[%d].local_matrix", i), "matrix must be affine", n.ID)
		}
	}
	hashed, err := ComputeSceneHash(sc)
	if err != nil {
		return err
	}
	if sc.ContentHash != hashed {
		return schemaField("scene.content_hash", "declared content hash does not match scene content", "")
	}
	for _, key := range []string{"right", "up", "forward"} {
		var v Vec3
		switch key {
		case "right":
			v = sc.Camera.Right
		case "up":
			v = sc.Camera.Up
		default:
			v = sc.Camera.Forward
		}
		if math.Abs(v.Length()-1) > 1e-9 {
			return schemaField("scene.camera."+key, "camera basis vectors must be unit length", "")
		}
	}
	return nil
}

func validateTruthDocument(doc map[string]any, scene *Scene) error {
	for _, key := range []string{"format_version", "scene_version", "precision", "content_hash", "transform_manifest", "ray_results", "box_results", "point_results", "operation_results", "final_ray_results", "final_box_results", "final_point_results"} {
		if _, err := requireField(doc, key, "truth."+key); err != nil {
			return err
		}
	}
	if v, err := requireIntField(doc, "format_version", "truth.format_version"); err != nil {
		return err
	} else if v != FormatVersion {
		return schemaField("truth.format_version", "unsupported format version", "")
	}
	if v, err := requireStringField(doc, "scene_version", "truth.scene_version"); err != nil {
		return err
	} else if v != scene.SceneVersion {
		return schemaField("truth.scene_version", "does not match scene version", "")
	}
	if v, err := requireIntField(doc, "precision", "truth.precision"); err != nil {
		return err
	} else if int(v) != scene.Precision {
		return schemaField("truth.precision", "does not match scene precision", "")
	}
	if v, err := requireStringField(doc, "content_hash", "truth.content_hash"); err != nil {
		return err
	} else if v != scene.ContentHash {
		return schemaField("truth.content_hash", "does not match scene content hash", "")
	}
	manifest, err := requireArrayField(doc, "transform_manifest", "truth.transform_manifest")
	if err != nil {
		return err
	}
	if len(manifest) != len(scene.Nodes) {
		return schemaField("truth.transform_manifest", "must contain one entry per scene node", "")
	}
	for i, ma := range manifest {
		path := fmt.Sprintf("truth.transform_manifest[%d]", i)
		m, err := asObject(ma, path)
		if err != nil {
			return err
		}
		if _, err := requireStringField(m, "node_id", path+".node_id"); err != nil {
			return err
		}
		if _, err := requireStringField(m, "local_digest", path+".local_digest"); err != nil {
			return err
		}
	}
	nodeSet := make(map[string]struct{}, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodeSet[n.ID] = struct{}{}
	}
	querySets := map[string]map[string]struct{}{
		"ray_results":         queryIDSet(rayQueryIDs(scene.RayQueries)),
		"box_results":         queryIDSet(boxQueryIDs(scene.BoxQueries)),
		"point_results":       queryIDSet(pointQueryIDs(scene.PointQueries)),
		"final_ray_results":   queryIDSet(rayQueryIDs(scene.RayQueries)),
		"final_box_results":   queryIDSet(boxQueryIDs(scene.BoxQueries)),
		"final_point_results": queryIDSet(pointQueryIDs(scene.PointQueries)),
	}
	for key, allowed := range querySets {
		arr, _ := doc[key].([]any)
		seen := make(map[string]struct{}, len(arr))
		for i, qa := range arr {
			path := fmt.Sprintf("truth.%s[%d]", key, i)
			q, err := asObject(qa, path)
			if err != nil {
				return err
			}
			id, err := requireStringField(q, "query_id", path+".query_id")
			if err != nil {
				return err
			}
			if _, ok := allowed[id]; !ok {
				return &CodedError{Code: CodeRef, Op: "validate_truth", QueryID: id, Field: path + ".query_id", Message: "truth references unknown query"}
			}
			if _, dup := seen[id]; dup {
				return &CodedError{Code: CodeSchema, Op: "validate_truth", QueryID: id, Field: path + ".query_id", Message: "duplicate truth query id"}
			}
			seen[id] = struct{}{}
			if _, err := requireStringField(q, "error_code", path+".error_code"); err != nil {
				return err
			}
			if key == "ray_results" || key == "final_ray_results" {
				if err := validateTruthRayShape(q, path); err != nil {
					return err
				}
			} else {
				if err := validateTruthHitSetShape(q, path); err != nil {
					return err
				}
			}
		}
		if len(seen) != len(allowed) {
			return schemaField("truth."+key, "truth query set does not match scene query set", "")
		}
	}
	ops, err := requireArrayField(doc, "operation_results", "truth.operation_results")
	if err != nil {
		return err
	}
	if len(ops) != len(scene.EditSequence) {
		return schemaField("truth.operation_results", "must contain one entry per edit batch", "")
	}
	for i, oa := range ops {
		path := fmt.Sprintf("truth.operation_results[%d]", i)
		o, err := asObject(oa, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(o, "batch_id", path+".batch_id")
		if err != nil {
			return err
		}
		if id != scene.EditSequence[i].ID {
			return &CodedError{Code: CodeSchema, Op: "validate_truth", BatchID: id, Field: path + ".batch_id", Message: "operation truth order does not match edit sequence"}
		}
		matrices, err := requireArrayField(o, "matrices", path+".matrices")
		if err != nil {
			return err
		}
		for j, ma := range matrices {
			mpath := fmt.Sprintf("%s.matrices[%d]", path, j)
			m, err := asObject(ma, mpath)
			if err != nil {
				return err
			}
			if _, err := requireStringField(m, "node_id", mpath+".node_id"); err != nil {
				return err
			}
			if _, err := requireNumbersField(m, "matrix", mpath+".matrix", 16); err != nil {
				return err
			}
		}
	}
	_ = nodeSet
	return nil
}

func validateTruthRayShape(q map[string]any, path string) error {
	geometries, err := requireArrayField(q, "geometries", path+".geometries")
	if err != nil {
		return err
	}
	for i, ga := range geometries {
		gpath := fmt.Sprintf("%s.geometries[%d]", path, i)
		g, err := asObject(ga, gpath)
		if err != nil {
			return err
		}
		if _, err := requireStringField(g, "node_id", gpath+".node_id"); err != nil {
			return err
		}
		if _, err := requireStringField(g, "geometry_id", gpath+".geometry_id"); err != nil {
			return err
		}
		if _, err := requireField(g, "hit", gpath+".hit"); err != nil {
			return err
		}
		if _, ok := g["hit"].(bool); !ok {
			return schemaField(gpath+".hit", "must be a boolean", "")
		}
	}
	if firstAny, ok := q["first"]; ok {
		first, err := asObject(firstAny, path+".first")
		if err != nil {
			return err
		}
		if err := validateTruthHitObject(first, path+".first"); err != nil {
			return err
		}
	}
	return validateTruthAmbiguityShape(q, path)
}

func validateTruthHitSetShape(q map[string]any, path string) error {
	hits, err := requireArrayField(q, "hits", path+".hits")
	if err != nil {
		return err
	}
	for i, h := range hits {
		if _, ok := h.(string); !ok {
			return schemaField(fmt.Sprintf("%s.hits[%d]", path, i), "must be a node id string", "")
		}
	}
	return validateTruthAmbiguityShape(q, path)
}

func validateTruthHitObject(h map[string]any, path string) error {
	if _, err := requireStringField(h, "node_id", path+".node_id"); err != nil {
		return err
	}
	if _, err := requireStringField(h, "geometry_id", path+".geometry_id"); err != nil {
		return err
	}
	if _, err := requireFloatField(h, "t", path+".t"); err != nil {
		return err
	}
	if _, err := requireVecField(h, "point", path+".point"); err != nil {
		return err
	}
	return nil
}

func validateTruthAmbiguityShape(q map[string]any, path string) error {
	v, ok := q["ambiguities"]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return schemaField(path+".ambiguities", "must be an array", "")
	}
	for i, aa := range arr {
		apath := fmt.Sprintf("%s.ambiguities[%d]", path, i)
		a, err := asObject(aa, apath)
		if err != nil {
			return err
		}
		for _, key := range []string{"query_id", "node_id", "geometry_id", "reason"} {
			if _, err := requireStringField(a, key, apath+"."+key); err != nil {
				return err
			}
		}
		if _, err := requireIntField(a, "depth", apath+".depth"); err != nil {
			return err
		}
		if _, err := requireFloatField(a, "bound", apath+".bound"); err != nil {
			return err
		}
	}
	return nil
}

func validateDecodedTruth(scene *Scene, truth *Truth) error {
	if truth.ContentHash != scene.ContentHash || truth.SceneVersion != scene.SceneVersion || truth.Precision != scene.Precision {
		return schemaField("truth", "truth header does not match scene", "")
	}
	byID := make(map[string]Node, len(scene.Nodes))
	for _, n := range scene.Nodes {
		byID[n.ID] = n
	}
	manifestSeen := make(map[string]struct{}, len(truth.TransformManifest))
	for i, entry := range truth.TransformManifest {
		path := fmt.Sprintf("truth.transform_manifest[%d]", i)
		n, ok := byID[entry.NodeID]
		if !ok {
			return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: entry.NodeID, Field: path + ".node_id", Message: "unknown node"}
		}
		if _, dup := manifestSeen[entry.NodeID]; dup {
			return &CodedError{Code: CodeSchema, Op: "validate_truth", NodeID: entry.NodeID, Field: path + ".node_id", Message: "duplicate transform manifest entry"}
		}
		manifestSeen[entry.NodeID] = struct{}{}
		want := LocalMatrixDigest(n.LocalMatrix, scene.Precision)
		if entry.LocalDigest != want {
			return &CodedError{Code: CodeSchema, Op: "validate_truth", NodeID: entry.NodeID, Field: path + ".local_digest", Message: "declared local matrix digest does not match scene"}
		}
	}
	if len(manifestSeen) != len(byID) {
		return schemaField("truth.transform_manifest", "manifest does not cover every node", "")
	}
	return validateTruthReferences(scene, truth)
}

func validateTruthReferences(scene *Scene, truth *Truth) error {
	nodeByID := make(map[string]Node, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodeByID[n.ID] = n
	}
	geometryByID := make(map[string]Geometry, len(scene.Geometries))
	for _, g := range scene.Geometries {
		geometryByID[g.ID] = g
	}
	checkRef := func(nodeID, geometryID, field string) error {
		n, ok := nodeByID[nodeID]
		if !ok {
			return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: nodeID, Field: field + ".node_id", Message: "truth references unknown node"}
		}
		if geometryID != "" {
			if _, ok := geometryByID[geometryID]; !ok {
				return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: nodeID, GeometryID: geometryID, Field: field + ".geometry_id", Message: "truth references unknown geometry"}
			}
			if n.GeometryIndex >= 0 && scene.Geometries[n.GeometryIndex].ID != geometryID {
				return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: nodeID, GeometryID: geometryID, Field: field + ".geometry_id", Message: "geometry does not belong to node"}
			}
		}
		return nil
	}
	checkAmbig := func(path string, a Ambiguity) error {
		if err := checkRef(a.NodeID, a.GeometryID, path); err != nil {
			return err
		}
		if a.Depth < 0 {
			return schemaField(path+".depth", "depth must be non-negative", a.NodeID)
		}
		return nil
	}
	checkRays := func(prefix string, rays []RayTruth) error {
		for i, r := range rays {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			if r.First != nil {
				if err := checkRef(r.First.NodeID, r.First.GeometryID, path+".first"); err != nil {
					return err
				}
			}
			for j, gh := range r.Geometries {
				if err := checkRef(gh.NodeID, gh.GeometryID, fmt.Sprintf("%s.geometries[%d]", path, j)); err != nil {
					return err
				}
			}
			for j, a := range r.Ambiguities {
				if err := checkAmbig(fmt.Sprintf("%s.ambiguities[%d]", path, j), a); err != nil {
					return err
				}
			}
		}
		return nil
	}
	checkBoxes := func(prefix string, boxes []BoxTruth) error {
		for i, b := range boxes {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			for j, id := range b.Hits {
				if _, ok := nodeByID[id]; !ok {
					return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: id, Field: fmt.Sprintf("%s.hits[%d]", path, j), Message: "truth references unknown node"}
				}
			}
			for j, a := range b.Ambiguities {
				if err := checkAmbig(fmt.Sprintf("%s.ambiguities[%d]", path, j), a); err != nil {
					return err
				}
			}
		}
		return nil
	}
	checkPoints := func(prefix string, points []PointTruth) error {
		for i, pt := range points {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			for j, id := range pt.Hits {
				if _, ok := nodeByID[id]; !ok {
					return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: id, Field: fmt.Sprintf("%s.hits[%d]", path, j), Message: "truth references unknown node"}
				}
			}
			for j, a := range pt.Ambiguities {
				if err := checkAmbig(fmt.Sprintf("%s.ambiguities[%d]", path, j), a); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := checkRays("truth.ray_results", truth.RayResults); err != nil {
		return err
	}
	if err := checkRays("truth.final_ray_results", truth.FinalRayResults); err != nil {
		return err
	}
	if err := checkBoxes("truth.box_results", truth.BoxResults); err != nil {
		return err
	}
	if err := checkBoxes("truth.final_box_results", truth.FinalBoxResults); err != nil {
		return err
	}
	if err := checkPoints("truth.point_results", truth.PointResults); err != nil {
		return err
	}
	if err := checkPoints("truth.final_point_results", truth.FinalPointResults); err != nil {
		return err
	}
	for i, op := range truth.OperationResults {
		seen := make(map[string]struct{}, len(op.Matrices))
		for j, nm := range op.Matrices {
			if _, ok := nodeByID[nm.NodeID]; !ok {
				return &CodedError{Code: CodeRef, Op: "validate_truth", NodeID: nm.NodeID, Field: fmt.Sprintf("truth.operation_results[%d].matrices[%d].node_id", i, j), Message: "truth references unknown node"}
			}
			if _, dup := seen[nm.NodeID]; dup {
				return &CodedError{Code: CodeSchema, Op: "validate_truth", NodeID: nm.NodeID, Field: fmt.Sprintf("truth.operation_results[%d].matrices[%d].node_id", i, j), Message: "duplicate node matrix"}
			}
			seen[nm.NodeID] = struct{}{}
		}
		if len(seen) != len(scene.Nodes) {
			return schemaField(fmt.Sprintf("truth.operation_results[%d].matrices", i), "must contain one matrix per node", "")
		}
	}
	return nil
}

func validateQueryArrays(doc map[string]any, nodeIDs map[string]int) error {
	rays, err := requireArrayField(doc, "ray_queries", "scene.ray_queries")
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	for i, qa := range rays {
		path := fmt.Sprintf("scene.ray_queries[%d]", i)
		q, err := asObject(qa, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(q, "id", path+".id")
		if err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", QueryID: id, Field: path + ".id", Message: "duplicate query id"}
		}
		seen[id] = struct{}{}
		if _, err := requireNumbersField(q, "screen", path+".screen", 2); err != nil {
			return err
		}
	}
	boxes, err := requireArrayField(doc, "box_queries", "scene.box_queries")
	if err != nil {
		return err
	}
	for i, qa := range boxes {
		path := fmt.Sprintf("scene.box_queries[%d]", i)
		q, err := asObject(qa, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(q, "id", path+".id")
		if err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", QueryID: id, Field: path + ".id", Message: "duplicate query id"}
		}
		seen[id] = struct{}{}
		if _, err := requireNumbersField(q, "rect", path+".rect", 4); err != nil {
			return err
		}
	}
	points, err := requireArrayField(doc, "point_queries", "scene.point_queries")
	if err != nil {
		return err
	}
	for i, qa := range points {
		path := fmt.Sprintf("scene.point_queries[%d]", i)
		q, err := asObject(qa, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(q, "id", path+".id")
		if err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", QueryID: id, Field: path + ".id", Message: "duplicate query id"}
		}
		seen[id] = struct{}{}
		if _, err := requireVecField(q, "point", path+".point"); err != nil {
			return err
		}
	}
	_ = nodeIDs
	return nil
}

func validateEditArray(doc map[string]any, nodeIDs map[string]int) error {
	edits, err := requireArrayField(doc, "edit_sequence", "scene.edit_sequence")
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(edits))
	for i, ea := range edits {
		path := fmt.Sprintf("scene.edit_sequence[%d]", i)
		e, err := asObject(ea, path)
		if err != nil {
			return err
		}
		id, err := requireStringField(e, "id", path+".id")
		if err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return &CodedError{Code: CodeSchema, Op: "validate_scene", BatchID: id, Field: path + ".id", Message: "duplicate batch id"}
		}
		seen[id] = struct{}{}
		selection, err := requireArrayField(e, "selection", path+".selection")
		if err != nil {
			return err
		}
		if len(selection) == 0 {
			return &CodedError{Code: CodeTransaction, Op: "validate_scene", BatchID: id, Field: path + ".selection", Message: "selection must not be empty"}
		}
		selSeen := make(map[string]struct{}, len(selection))
		for j, s := range selection {
			nid, ok := s.(string)
			if !ok {
				return schemaField(fmt.Sprintf("%s.selection[%d]", path, j), "must be a string", "")
			}
			if _, ok := nodeIDs[nid]; !ok {
				return &CodedError{Code: CodeRef, Op: "validate_scene", BatchID: id, NodeID: nid, Field: fmt.Sprintf("%s.selection[%d]", path, j), Message: "unknown selected node"}
			}
			if _, dup := selSeen[nid]; dup {
				return &CodedError{Code: CodeTransaction, Op: "validate_scene", BatchID: id, NodeID: nid, Field: fmt.Sprintf("%s.selection[%d]", path, j), Message: "duplicate selected node"}
			}
			selSeen[nid] = struct{}{}
		}
		if _, err := requireVecField(e, "pivot", path+".pivot"); err != nil {
			return err
		}
		steps, err := requireArrayField(e, "steps", path+".steps")
		if err != nil {
			return err
		}
		if len(steps) == 0 {
			return &CodedError{Code: CodeTransaction, Op: "validate_scene", BatchID: id, Field: path + ".steps", Message: "batch requires at least one step"}
		}
		for j, sa := range steps {
			spath := fmt.Sprintf("%s.steps[%d]", path, j)
			s, err := asObject(sa, spath)
			if err != nil {
				return err
			}
			kind, err := requireStringField(s, "kind", spath+".kind")
			if err != nil {
				return err
			}
			if kind != StepTranslate && kind != StepRotate && kind != StepScale {
				return &CodedError{Code: CodeSchema, Op: "validate_scene", BatchID: id, Field: spath + ".kind", Message: "unknown step kind"}
			}
			if _, err := requireVecField(s, "value", spath+".value"); err != nil {
				return err
			}
			if kind == StepScale {
				v := s["value"].([]any)
				for k := 0; k < 3; k++ {
					f, _ := asFloat(v[k])
					if f == 0 {
						return &CodedError{Code: CodeTransaction, Op: "validate_scene", BatchID: id, Field: fmt.Sprintf("%s.value[%d]", spath, k), Message: "scale component must not be zero"}
					}
				}
			}
		}
	}
	return nil
}

func detectCycle(nodes []any, nodeIDs map[string]int, parents []string) error {
	const (
		unseen = 0
		active = 1
		done   = 2
	)
	state := make([]int, len(parents))
	var visit func(int, []int) error
	visit = func(i int, stack []int) error {
		if state[i] == done {
			return nil
		}
		if state[i] == active {
			id := nodeIDAt(nodes, i)
			return &CodedError{Code: CodeCycle, Op: "validate_scene", NodeID: id, Field: fmt.Sprintf("scene.nodes[%d].parent_id", i), Message: "parent relationship contains a cycle"}
		}
		state[i] = active
		if p := parents[i]; p != "" {
			if err := visit(nodeIDs[p], append(stack, i)); err != nil {
				return err
			}
		}
		state[i] = done
		return nil
	}
	for i := range parents {
		if err := visit(i, nil); err != nil {
			return err
		}
	}
	return nil
}

func requireField(m map[string]any, key, path string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, schemaField(path, "required field is missing", "")
	}
	return v, nil
}

func requireObjectField(m map[string]any, key, path string) (map[string]any, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return nil, err
	}
	return asObject(v, path)
}

func requireArrayField(m map[string]any, key, path string) ([]any, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return nil, err
	}
	a, ok := v.([]any)
	if !ok {
		return nil, schemaField(path, "must be an array", "")
	}
	return a, nil
}

func requireStringField(m map[string]any, key, path string) (string, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", schemaField(path, "must be a string", "")
	}
	return s, nil
}

func requireIntField(m map[string]any, key, path string) (int64, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return 0, err
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaField(path, "must be an integer", "")
	}
	i, err := n.Int64()
	if err != nil {
		return 0, schemaField(path, "must be an integer", "")
	}
	return i, nil
}

func requireFloatField(m map[string]any, key, path string) (float64, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return 0, err
	}
	f, ok := asFloat(v)
	if !ok || !finite(f) {
		return 0, schemaField(path, "must be a finite number", "")
	}
	return f, nil
}

func requireVecField(m map[string]any, key, path string) (Vec3, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return Vec3{}, err
	}
	return requireVec(v, path, 3)
}

func requireNumbersField(m map[string]any, key, path string, n int) ([]float64, error) {
	v, err := requireField(m, key, path)
	if err != nil {
		return nil, err
	}
	return requireNumbers(v, path, n)
}

func requireNumbers(v any, path string, n int) ([]float64, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, schemaField(path, fmt.Sprintf("must be an array of %d numbers", n), "")
	}
	if len(a) != n {
		return nil, schemaField(path, fmt.Sprintf("must contain exactly %d numbers", n), "")
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		f, ok := asFloat(a[i])
		if !ok || !finite(f) {
			return nil, schemaField(fmt.Sprintf("%s[%d]", path, i), "must be a finite number", "")
		}
		out[i] = f
	}
	return out, nil
}

func requireVec(v any, path string, n int) (Vec3, error) {
	a, ok := v.([]any)
	if !ok {
		return Vec3{}, schemaField(path, fmt.Sprintf("must be an array of %d numbers", n), "")
	}
	if len(a) != n {
		return Vec3{}, schemaField(path, fmt.Sprintf("must contain exactly %d numbers", n), "")
	}
	var out Vec3
	for i := 0; i < n; i++ {
		f, ok := asFloat(a[i])
		if !ok || !finite(f) {
			return Vec3{}, schemaField(fmt.Sprintf("%s[%d]", path, i), "must be a finite number", "")
		}
		switch i {
		case 0:
			out.X = f
		case 1:
			out.Y = f
		case 2:
			if n == 3 {
				out.Z = f
			}
		}
	}
	return out, nil
}

func asObject(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaField(path, "must be an object", "")
	}
	return m, nil
}

func asFloat(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func schemaField(path, message, nodeID string) *CodedError {
	return &CodedError{Code: CodeSchema, Op: "validate", NodeID: nodeID, Field: path, Message: message}
}

func attachNodeID(err error, nodeID string) error {
	var ce *CodedError
	if IsCoded(err, &ce) && ce.NodeID == "" {
		ce.NodeID = nodeID
	}
	return err
}

func IsCoded(err error, target **CodedError) bool {
	if err == nil {
		return false
	}
	ce, ok := err.(*CodedError)
	if ok && target != nil {
		*target = ce
	}
	return ok
}

func nodeIDAt(nodes []any, i int) string {
	if i < 0 || i >= len(nodes) {
		return ""
	}
	if m, ok := nodes[i].(map[string]any); ok {
		if s, ok := m["id"].(string); ok {
			return s
		}
	}
	return ""
}

func rayQueryIDs(qs []RayQuery) []string {
	out := make([]string, len(qs))
	for i := range qs {
		out[i] = qs[i].ID
	}
	return out
}

func boxQueryIDs(qs []BoxQuery) []string {
	out := make([]string, len(qs))
	for i := range qs {
		out[i] = qs[i].ID
	}
	return out
}

func pointQueryIDs(qs []PointQuery) []string {
	out := make([]string, len(qs))
	for i := range qs {
		out[i] = qs[i].ID
	}
	return out
}

func queryIDSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func sortedStringsFromSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
