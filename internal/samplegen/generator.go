package samplegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"editorpick"
)

const DefaultSeed int64 = 241125

type generator struct {
	seed      int64
	rng       *rand.Rand
	scene     editorpick.SceneFile
	queries   editorpick.QueryFile
	truth     editorpick.TruthFile
	locals    map[string]fMat4
	parents   map[string]string
	geomNode  []string
	raySpecs  []raySpec
	boxSpecs  []boxSpec
	pointSpec []pointSpec
}

type raySpec struct {
	id             string
	screen         [2]float64
	o, d           fVec3
	expectedStatus string
	expectedNode   string
}

type boxSpec struct {
	id             string
	r              editorpick.Rect
	expectedStatus string
}

type pointSpec struct {
	id             string
	p              fVec3
	expectedStatus string
}

func Generate(dir string, seed int64) error {
	g := &generator{
		seed:    seed,
		rng:     rand.New(rand.NewSource(seed)),
		locals:  map[string]fMat4{},
		parents: map[string]string{},
	}
	precision := editorpick.DefaultPrecision()
	g.scene = editorpick.SceneFile{
		Schema:           editorpick.SceneSchemaV1,
		StructureVersion: editorpick.StructureVersion,
		Precision:        precision,
		Seed:             seed,
	}
	g.queries = editorpick.QueryFile{
		Schema:           editorpick.QuerySchemaV1,
		StructureVersion: editorpick.StructureVersion,
		Precision:        precision,
		Seed:             seed,
	}
	g.truth = editorpick.TruthFile{
		Schema:                editorpick.TruthSchemaV1,
		StructureVersion:      editorpick.StructureVersion,
		Precision:             precision,
		Seed:                  seed,
		DeclaredLocalMatrices: map[string][16]float64{},
	}

	g.buildScene()
	g.buildRayQueries()
	g.buildBoxAndPointQueries()
	g.buildRayTruth()
	g.buildBoxTruth()
	g.buildPointTruth()
	g.buildEditSequence()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sceneBytes, err := writeJSON(filepath.Join(dir, "scene.json"), g.scene)
	if err != nil {
		return err
	}
	queryBytes, err := writeJSON(filepath.Join(dir, "queries.json"), g.queries)
	if err != nil {
		return err
	}
	truthBytes, err := writeJSON(filepath.Join(dir, "truth.json"), g.truth)
	if err != nil {
		return err
	}
	manifest := editorpick.ManifestFile{
		Schema:           "editorpick.manifest/v1",
		Seed:             seed,
		StructureVersion: editorpick.StructureVersion,
		Precision:        precision,
		Files: map[string]string{
			"scene.json":   hashBytes(sceneBytes),
			"queries.json": hashBytes(queryBytes),
			"truth.json":   hashBytes(truthBytes),
		},
	}
	if _, err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return err
	}
	return nil
}

func writeJSON(path string, v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return data, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (g *generator) addNode(id, parent string, m fMat4, geom *editorpick.GeometryDTO) {
	dto := editorpick.NodeDTO{ID: &id, Matrix: append([]float64(nil), m[:]...)}
	if parent != "" {
		p := parent
		dto.ParentID = &p
		g.parents[id] = parent
	}
	if geom != nil {
		idx := len(g.scene.Geometries)
		g.scene.Geometries = append(g.scene.Geometries, *geom)
		dto.GeometryIndex = &idx
		g.geomNode = append(g.geomNode, id)
	}
	g.scene.Nodes = append(g.scene.Nodes, dto)
	g.locals[id] = m
	g.truth.DeclaredLocalMatrices[id] = [16]float64(m)
}

func (g *generator) buildScene() {
	g.addNode("root", "", fIdentity(), nil)

	aabbGeom := &editorpick.GeometryDTO{
		Kind: "aabb",
		Min:  &editorpick.Vec3DTO{X: -1, Y: -1, Z: -1},
		Max:  &editorpick.Vec3DTO{X: 1, Y: 1, Z: 1},
	}
	g.addNode("aabb-node", "root", fTranslation(fv(-4, 0, 0)), aabbGeom)

	sphereGeom := &editorpick.GeometryDTO{Kind: "sphere", Center: &editorpick.Vec3DTO{X: 0, Y: 0, Z: 0}, Radius: 1.2}
	g.addNode("sphere-node", "root", fIdentity(), sphereGeom)

	meshGeom := &editorpick.GeometryDTO{
		Kind: "mesh",
		Vertices: [][]float64{
			{-1.2, -1, 0}, {1.2, -1, 0}, {0, 1.4, 0.25},
		},
		Indices: []int{0, 1, 2},
	}
	g.addNode("mesh-node", "root", fTranslation(fv(4, 0, 0)), meshGeom)

	g.addNode("chain-00", "root", fTranslation(fv(0, 3.0, 0)), nil)
	for i := 1; i < 24; i++ {
		id := fmt.Sprintf("chain-%02d", i)
		parent := fmt.Sprintf("chain-%02d", i-1)
		m := fTranslation(fv(0.07, 0.14, 0))
		if i%6 == 0 {
			m = fMul(m, fRotationZ(0.0)) // deterministic marker; exact zero rotation keeps truth simple.
		}
		g.addNode(id, parent, m, nil)
	}
	chainGeom := &editorpick.GeometryDTO{
		Kind: "aabb",
		Min:  &editorpick.Vec3DTO{X: -0.45, Y: -0.45, Z: -0.45},
		Max:  &editorpick.Vec3DTO{X: 0.45, Y: 0.45, Z: 0.45},
	}
	last := "chain-23"
	idx := len(g.scene.Geometries)
	g.scene.Geometries = append(g.scene.Geometries, *chainGeom)
	g.scene.Nodes[len(g.scene.Nodes)-1].GeometryIndex = &idx
	g.geomNode = append(g.geomNode, last)

	g.addNode("filler-root", "root", fIdentity(), nil)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("filler-%02d", i)
		x := 18 + g.rng.Float64()*5
		y := -6 + g.rng.Float64()*12
		z := -2 + g.rng.Float64()*4
		half := 0.2 + g.rng.Float64()*0.25
		geom := &editorpick.GeometryDTO{
			Kind: "aabb",
			Min:  &editorpick.Vec3DTO{X: -half, Y: -half, Z: -half},
			Max:  &editorpick.Vec3DTO{X: half, Y: half, Z: half},
		}
		g.addNode(id, "filler-root", fTranslation(fv(x, y, z)), geom)
	}
}

func (g *generator) worldMatrix(id string) fMat4 {
	m := g.locals[id]
	parent := g.parents[id]
	if parent == "" {
		return m
	}
	return fMul(g.worldMatrix(parent), m)
}

func translationOf(m fMat4) fVec3 { return fVec3{X: m[3], Y: m[7], Z: m[11]} }

func (g *generator) addRay(spec raySpec) {
	g.raySpecs = append(g.raySpecs, spec)
	g.queries.Rays = append(g.queries.Rays, editorpick.RayQuery{
		ID:     spec.id,
		Screen: g.queries.Camera.WorldToScreen(spec.o.X, spec.o.Y),
		Origin: editorpick.Vec3FromFloat(spec.o.X, spec.o.Y, spec.o.Z),
		Dir:    editorpick.Vec3FromFloat(spec.d.X, spec.d.Y, spec.d.Z),
	})
}

func (g *generator) buildRayQueries() {
	g.queries.Camera = editorpick.DefaultCamera()
	d := fv(0, 0, 1)
	g.addRay(raySpec{id: "ray-aabb-center", screen: [2]float64{-4, 0}, o: fv(-4, 0, -10), d: d, expectedStatus: "hit", expectedNode: "aabb-node"})
	g.addRay(raySpec{id: "ray-sphere-center", screen: [2]float64{0, 0}, o: fv(0, 0, -10), d: d, expectedStatus: "hit", expectedNode: "sphere-node"})
	g.addRay(raySpec{id: "ray-mesh-center", screen: [2]float64{4, 0}, o: fv(4, 0, -10), d: d, expectedStatus: "hit", expectedNode: "mesh-node"})

	chainWorld := translationOf(g.worldMatrix("chain-23"))
	g.addRay(raySpec{id: "ray-chain-center", screen: [2]float64{chainWorld.X, chainWorld.Y}, o: fv(chainWorld.X, chainWorld.Y, -10), d: d, expectedStatus: "hit", expectedNode: "chain-23"})
	g.addRay(raySpec{id: "ray-empty", screen: [2]float64{-12, -12}, o: fv(-12, -12, -10), d: d, expectedStatus: "miss"})

	g.addRay(raySpec{id: "ray-aabb-edge-ambig", screen: [2]float64{-2.999999, 0}, o: fv(-2.999999, 0, -10), d: d, expectedStatus: editorpick.EAmbiguous, expectedNode: "aabb-node"})
	g.addRay(raySpec{id: "ray-sphere-edge-ambig", screen: [2]float64{1.200001, 0}, o: fv(1.200001, 0, -10), d: d, expectedStatus: editorpick.EAmbiguous, expectedNode: "sphere-node"})
	g.addRay(raySpec{id: "ray-mesh-edge-ambig", screen: [2]float64{4, -1.000001}, o: fv(4, -1.000001, -10), d: d, expectedStatus: editorpick.EAmbiguous, expectedNode: "mesh-node"})

	chainMaxX := chainWorld.X + 0.45
	g.addRay(raySpec{id: "ray-chain-edge-ambig", screen: [2]float64{chainMaxX + 0.000001, chainWorld.Y}, o: fv(chainMaxX+0.000001, chainWorld.Y, -10), d: d, expectedStatus: editorpick.EAmbiguous, expectedNode: "chain-23"})
}

func (g *generator) addBox(spec boxSpec) {
	g.boxSpecs = append(g.boxSpecs, spec)
	g.queries.Boxes = append(g.queries.Boxes, editorpick.BoxQuery{ID: spec.id, Rect: spec.r})
}

func (g *generator) addPoint(spec pointSpec) {
	g.pointSpec = append(g.pointSpec, spec)
	g.queries.Points = append(g.queries.Points, editorpick.PointQuery{ID: spec.id, Point: editorpick.Vec3FromFloat(spec.p.X, spec.p.Y, spec.p.Z)})
}

func rectFromFloats(x1, y1, x2, y2 float64) editorpick.Rect {
	return editorpick.Rect{
		MinX: editorpick.FixedFromFloat(x1), MinY: editorpick.FixedFromFloat(y1),
		MaxX: editorpick.FixedFromFloat(x2), MaxY: editorpick.FixedFromFloat(y2),
	}
}

func (g *generator) buildBoxAndPointQueries() {
	g.addBox(boxSpec{id: "box-principals", r: rectFromFloats(-5.5, -1.6, 5.5, 1.9), expectedStatus: "hit"})
	g.addBox(boxSpec{id: "box-aabb-only", r: rectFromFloats(-5.3, -1.3, -2.7, 1.3), expectedStatus: "hit"})
	g.addBox(boxSpec{id: "box-sphere-only", r: rectFromFloats(-1.6, -1.6, 1.6, 1.6), expectedStatus: "hit"})
	chain := translationOf(g.worldMatrix("chain-23"))
	g.addBox(boxSpec{id: "box-chain", r: rectFromFloats(chain.X-0.8, chain.Y-0.8, chain.X+0.8, chain.Y+0.8), expectedStatus: "hit"})
	g.addBox(boxSpec{id: "box-empty", r: rectFromFloats(20, 20, 22, 22), expectedStatus: "miss"})

	g.addPoint(pointSpec{id: "point-aabb-inside", p: fv(-4, 0, 0), expectedStatus: "hit"})
	g.addPoint(pointSpec{id: "point-sphere-inside", p: fv(0, 0, 0), expectedStatus: "hit"})
	g.addPoint(pointSpec{id: "point-aabb-edge-ambig", p: fv(-2.999999, 0, 0), expectedStatus: editorpick.EAmbiguous})
	g.addPoint(pointSpec{id: "point-empty", p: fv(-12, -12, 0), expectedStatus: "miss"})
}

func vec3DTOToF(v *editorpick.Vec3DTO) fVec3 {
	if v == nil {
		return fVec3{}
	}
	return fv(v.X, v.Y, v.Z)
}

func geometryLocalAABB(d editorpick.GeometryDTO) (fVec3, fVec3) {
	switch d.Kind {
	case "aabb":
		return vec3DTOToF(d.Min), vec3DTOToF(d.Max)
	case "sphere":
		c := vec3DTOToF(d.Center)
		return fv(c.X-d.Radius, c.Y-d.Radius, c.Z-d.Radius), fv(c.X+d.Radius, c.Y+d.Radius, c.Z+d.Radius)
	default:
		var mn, mx fVec3
		for i, raw := range d.Vertices {
			p := fv(raw[0], raw[1], raw[2])
			if i == 0 {
				mn, mx = p, p
			} else {
				mn = fv(minFloat(mn.X, p.X), minFloat(mn.Y, p.Y), minFloat(mn.Z, p.Z))
				mx = fv(maxFloat(mx.X, p.X), maxFloat(mx.Y, p.Y), maxFloat(mx.Z, p.Z))
			}
		}
		return mn, mx
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (g *generator) rayGeometry(ray raySpec, geomIndex int) (editorpick.RayGeometryTruth, *fRayHit) {
	dto := g.scene.Geometries[geomIndex]
	nodeID := g.geomNode[geomIndex]
	m := g.worldMatrix(nodeID)
	t := translationOf(m)
	localO := ray.o.sub(t)
	var hit fRayHit
	switch dto.Kind {
	case "aabb":
		hit = fRayAABB(localO, ray.d, vec3DTOToF(dto.Min), vec3DTOToF(dto.Max))
	case "sphere":
		hit = fRaySphere(localO, ray.d, vec3DTOToF(dto.Center), dto.Radius)
	case "mesh":
		for ti := 0; ti < len(dto.Indices)/3; ti++ {
			ia, ib, ic := dto.Indices[ti*3], dto.Indices[ti*3+1], dto.Indices[ti*3+2]
			rawA, rawB, rawC := dto.Vertices[ia], dto.Vertices[ib], dto.Vertices[ic]
			a := fv(rawA[0], rawA[1], rawA[2])
			b := fv(rawB[0], rawB[1], rawB[2])
			c := fv(rawC[0], rawC[1], rawC[2])
			candidate := fRayTriangle(localO, ray.d, a, b, c)
			if candidate.hit && (!hit.hit || candidate.t < hit.t) {
				hit = candidate
			}
		}
	}
	truth := editorpick.RayGeometryTruth{GeometryIndex: geomIndex, NodeID: nodeID, Kind: dto.Kind, Hit: hit.hit}
	if hit.hit {
		worldPoint := hit.point.add(t)
		arr := [3]float64{worldPoint.X, worldPoint.Y, worldPoint.Z}
		truth.Point = &arr
		truth.Distance = hit.t * ray.d.dot(ray.d)
		truth.Distance = sqrtFloat(ray.d.dot(ray.d)) * hit.t
	}
	return truth, &hit
}

func sqrtFloat(v float64) float64 {
	return math.Sqrt(v)
}

func (g *generator) buildRayTruth() {
	for _, spec := range g.raySpecs {
		rt := editorpick.RayTruth{QueryID: spec.id, ExpectedStatus: spec.expectedStatus, NodeID: spec.expectedNode, PerGeometry: []editorpick.RayGeometryTruth{}}
		var bestT float64
		var hasBest bool
		for i := range g.scene.Geometries {
			gt, hit := g.rayGeometry(spec, i)
			rt.PerGeometry = append(rt.PerGeometry, gt)
			if hit.hit && (!hasBest || hit.t < bestT) {
				hasBest = true
				bestT = hit.t
				rt.NodeID = gt.NodeID
				rt.GeometryIndex = gt.GeometryIndex
				rt.Point = gt.Point
			}
		}
		g.truth.RayTruths = append(g.truth.RayTruths, rt)
	}
}

func (g *generator) buildBoxTruth() {
	for _, spec := range g.boxSpecs {
		bt := editorpick.BoxTruth{QueryID: spec.id, ExpectedStatus: spec.expectedStatus, NodeIDs: []string{}}
		for i, dto := range g.scene.Geometries {
			nodeID := g.geomNode[i]
			m := g.worldMatrix(nodeID)
			mnLocal, mxLocal := geometryLocalAABB(dto)
			mn, mx := fTransformAABB(m, mnLocal, mxLocal)
			if floatRectIntersects(spec.r, mn, mx) {
				bt.NodeIDs = append(bt.NodeIDs, nodeID)
			}
		}
		sort.Strings(bt.NodeIDs)
		g.truth.BoxTruths = append(g.truth.BoxTruths, bt)
	}
}

func floatRectIntersects(r editorpick.Rect, mn, mx fVec3) bool {
	return r.MinX.Float64() <= mx.X && r.MaxX.Float64() >= mn.X &&
		r.MinY.Float64() <= mx.Y && r.MaxY.Float64() >= mn.Y
}

func (g *generator) buildPointTruth() {
	for _, spec := range g.pointSpec {
		pt := editorpick.PointTruth{QueryID: spec.id, ExpectedStatus: spec.expectedStatus, NodeIDs: []string{}}
		for i, dto := range g.scene.Geometries {
			if spec.expectedStatus == editorpick.EAmbiguous {
				continue
			}
			nodeID := g.geomNode[i]
			m := g.worldMatrix(nodeID)
			mnLocal, mxLocal := geometryLocalAABB(dto)
			mn, mx := fTransformAABB(m, mnLocal, mxLocal)
			if spec.p.X >= mn.X && spec.p.X <= mx.X && spec.p.Y >= mn.Y && spec.p.Y <= mx.Y && spec.p.Z >= mn.Z && spec.p.Z <= mx.Z {
				pt.NodeIDs = append(pt.NodeIDs, nodeID)
			}
		}
		sort.Strings(pt.NodeIDs)
		g.truth.PointTruths = append(g.truth.PointTruths, pt)
	}
}

func (g *generator) buildEditSequence() {
	steps := []struct {
		step       editorpick.EditStep
		kind       string
		vector     fVec3
		angle      float64
		apply      bool
		checkpoint bool
	}{
		{
			step: editorpick.EditStep{
				ID: "step-translate-aabb", Description: "translate AABB node in a batch",
				Selection: []string{"aabb-node"}, Pivot: editorpick.Vec3FromFloat(-4, 0, 0),
				Ops: []editorpick.PivotOp{{Type: "translate", Vector: editorpick.Vec3FromFloat(0.6, 0, 0)}},
			},
			kind: "translate", vector: fv(0.6, 0, 0), apply: true,
		},
		{
			step: editorpick.EditStep{
				ID: "step-rotate-principals", Description: "rotate sphere and mesh around shared pivot",
				Selection: []string{"sphere-node", "mesh-node"}, Pivot: editorpick.Vec3FromFloat(0, 0, 0),
				Ops:        []editorpick.PivotOp{{Type: "rotate", Axis: "z", Angle: 25 * 3.141592653589793 / 180}},
				Checkpoint: true,
			},
			kind: "rotate", angle: 25 * 3.141592653589793 / 180, apply: true, checkpoint: true,
		},
		{
			step: editorpick.EditStep{
				ID: "step-scale-aabb-pivot", Description: "scale around translated AABB center",
				Selection: []string{"aabb-node"}, Pivot: editorpick.Vec3FromFloat(-3.4, 0, 0),
				Ops: []editorpick.PivotOp{{Type: "scale", Vector: editorpick.Vec3FromFloat(1.3, 1.3, 1)}},
			},
			kind: "scale", vector: fv(1.3, 1.3, 1), apply: true,
		},
		{
			step: editorpick.EditStep{
				ID: "step-invalid-zero-scale", Description: "zero scale must abort with E_TXN",
				Selection: []string{"sphere-node"}, Pivot: editorpick.Vec3FromFloat(0, 0, 0),
				Ops:        []editorpick.PivotOp{{Type: "scale", Vector: editorpick.Vec3FromFloat(0, 1, 1)}},
				ExpectCode: editorpick.ETxn,
			},
			kind: "scale", vector: fv(0, 1, 1), apply: false,
		},
		{
			step: editorpick.EditStep{
				ID: "step-rotate-deep-branch", Description: "rotate chain root and propagate 24 levels",
				Selection: []string{"chain-00"}, Pivot: editorpick.Vec3FromFloat(0, 3.0, 0),
				Ops: []editorpick.PivotOp{{Type: "rotate", Axis: "z", Angle: 15 * 3.141592653589793 / 180}},
			},
			kind: "rotate", angle: 15 * 3.141592653589793 / 180, apply: true,
		},
	}

	for _, item := range steps {
		g.queries.EditSequence = append(g.queries.EditSequence, item.step)
		if item.apply {
			delta := fPivot(fVec3FromEditor(item.step.Pivot), item.kind, item.vector, item.angle)
			for _, id := range item.step.Selection {
				oldWorld := g.worldMatrix(id)
				newWorld := fMul(delta, oldWorld)
				if g.parents[id] == "" || g.parents[id] == "root" {
					g.locals[id] = newWorld
				}
			}
		}
		truth := editorpick.OperationTruth{StepID: item.step.ID, ExpectCode: item.step.ExpectCode, WorldMatrices: map[string][16]float64{}, WorldAABBs: map[string][6]float64{}}
		for _, n := range g.scene.Nodes {
			truth.WorldMatrices[*n.ID] = [16]float64(g.worldMatrix(*n.ID))
		}
		for geomIndex, nodeID := range g.geomNode {
			dto := g.scene.Geometries[geomIndex]
			mnLocal, mxLocal := geometryLocalAABB(dto)
			mn, mx := fTransformAABB(g.worldMatrix(nodeID), mnLocal, mxLocal)
			truth.WorldAABBs[nodeID] = [6]float64{mn.X, mn.Y, mn.Z, mx.X, mx.Y, mx.Z}
		}
		g.truth.OperationTruths = append(g.truth.OperationTruths, truth)
	}
}

func fVec3FromEditor(v editorpick.Vec3) fVec3 {
	return fv(v.X.Float64(), v.Y.Float64(), v.Z.Float64())
}
