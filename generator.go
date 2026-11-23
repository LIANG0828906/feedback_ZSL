package spatialpick

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const (
	SampleSceneVersion = "deterministic-editor-v1"
	SamplePrecision    = 6
)

func GenerateSample() (*Scene, *Truth, error) {
	scene := &Scene{
		FormatVersion: FormatVersion,
		SceneVersion:  SampleSceneVersion,
		Precision:     SamplePrecision,
		Camera: Camera{
			ViewportWidth:  1280,
			ViewportHeight: 720,
			WorldHeight:    20,
			Eye:            V3(0, 0, 24),
			Right:          V3(1, 0, 0),
			Up:             V3(0, 1, 0),
			Forward:        V3(0, 0, -1),
		},
		RayQueries:   []RayQuery{},
		BoxQueries:   []BoxQuery{},
		PointQueries: []PointQuery{},
		EditSequence: []EditBatch{},
	}

	addGeometry := func(kind string, variant int) int {
		i := len(scene.Geometries)
		g := Geometry{ID: fmt.Sprintf("g%03d", i), Kind: kind}
		size := 1 + float64(variant%4)*0.06
		switch kind {
		case GeometryAABB:
			g.Min = V3(-0.18*size, -0.12*size, -0.09*size)
			g.Max = V3(0.18*size, 0.12*size, 0.09*size)
		case GeometrySphere:
			g.Center = V3(0, 0, 0)
			g.Radius = 0.14 * size
		case GeometryMesh:
			s := 0.22 * size
			g.Vertices = []Vec3{
				V3(-s, -0.75*s, 0),
				V3(s, -0.75*s, 0),
				V3(0, s, 0),
				V3(0, -0.1*s, 0.6*s),
			}
			g.Triangles = [][3]int{{0, 1, 2}, {0, 3, 1}, {1, 3, 2}, {2, 3, 0}}
		}
		scene.Geometries = append(scene.Geometries, g)
		return i
	}

	localFor := func(seed int, scale float64, offset Vec3) Mat4 {
		angle := float64(seed)*0.0031 + 0.002
		tiltX := float64(seed%7) * 0.00021
		tiltY := float64(seed%5) * 0.00017
		t := V3(0.048+offset.X, 0.0035*math.Sin(float64(seed)*0.73)+offset.Y, 0.0028*math.Cos(float64(seed)*0.41)+offset.Z)
		m := Mat4Multiply(TranslationMat4(t), RotationZMat4(angle))
		m = Mat4Multiply(m, RotationYMat4(tiltY))
		m = Mat4Multiply(m, RotationXMat4(tiltX))
		m = Mat4Multiply(m, ScaleMat4(V3(scale, scale, scale)))
		return roundMatrix(m, SamplePrecision)
	}

	const chainDepth = 48
	for i := 0; i < chainDepth; i++ {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("n%03d", i-1)
		}
		gidx := -1
		if i%2 == 0 {
			kind := []string{GeometryAABB, GeometrySphere, GeometryMesh}[(i/2)%3]
			gidx = addGeometry(kind, i/2)
		}
		scale := 0.9985 - float64(i)*0.00002
		scene.Nodes = append(scene.Nodes, Node{
			ID:            fmt.Sprintf("n%03d", i),
			ParentID:      parent,
			LocalMatrix:   matrixArray(localFor(i, scale, Vec3{})),
			GeometryIndex: gidx,
		})
	}

	branchBase := chainDepth
	branchParent := "n008"
	const branchDepth = 12
	for j := 0; j < branchDepth; j++ {
		id := fmt.Sprintf("n%03d", branchBase+j)
		parent := branchParent
		if j > 0 {
			parent = fmt.Sprintf("n%03d", branchBase+j-1)
		}
		gidx := -1
		if j%2 == 0 {
			kind := []string{GeometrySphere, GeometryMesh, GeometryAABB}[(j/2)%3]
			gidx = addGeometry(kind, 5+j/2)
		}
		m := localFor(100+j, 1.0, V3(0.06, 0.045, 0.025))
		scene.Nodes = append(scene.Nodes, Node{
			ID: id, ParentID: parent, LocalMatrix: matrixArray(m), GeometryIndex: gidx,
		})
	}

	independentID := "n060"
	ig := addGeometry(GeometrySphere, 9)
	scene.Nodes = append(scene.Nodes, Node{
		ID:            independentID,
		ParentID:      "",
		LocalMatrix:   matrixArray(roundMatrix(Mat4Multiply(TranslationMat4(V3(1.45, 1.15, 0.7)), ScaleMat4(V3(1.15, 1.15, 1.15))), SamplePrecision)),
		GeometryIndex: ig,
	})

	worlds, _, err := floatWorlds(scene)
	if err != nil {
		return nil, nil, err
	}
	pxPerWorld := scene.Camera.ViewportHeight / scene.Camera.WorldHeight
	project := func(p Vec3) Vec2 {
		d := p.Sub(scene.Camera.Eye)
		return Vec2{
			X: scene.Camera.ViewportWidth/2 + d.Dot(scene.Camera.Right)*pxPerWorld,
			Y: scene.Camera.ViewportHeight/2 - d.Dot(scene.Camera.Up)*pxPerWorld,
		}
	}
	nodeByID := make(map[string]Node, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodeByID[n.ID] = n
	}
	centerFor := func(nodeID string) (Vec3, float64, bool) {
		n := nodeByID[nodeID]
		if n.GeometryIndex < 0 {
			return Vec3{}, 0, false
		}
		g := scene.Geometries[n.GeometryIndex]
		sphere := generatorWorldSphere(g, worlds[nodeID])
		return sphere.Center, sphere.Radius, true
	}

	rayTargets := []string{"n002", "n010", "n018", "n026", "n034", "n042", "n048", "n052", independentID}
	rayIndex := 0
	for _, id := range rayTargets {
		c, _, ok := centerFor(id)
		if !ok {
			continue
		}
		p := project(c)
		if p.X < 8 || p.X > scene.Camera.ViewportWidth-8 || p.Y < 8 || p.Y > scene.Camera.ViewportHeight-8 {
			continue
		}
		scene.RayQueries = append(scene.RayQueries, RayQuery{ID: fmt.Sprintf("ray-%03d", rayIndex), Screen: p})
		rayIndex++
	}
	scene.RayQueries = append(scene.RayQueries,
		RayQuery{ID: fmt.Sprintf("ray-%03d", rayIndex), Screen: Vec2{X: 80, Y: 80}},
		RayQuery{ID: fmt.Sprintf("ray-%03d", rayIndex+1), Screen: Vec2{X: 1190, Y: 640}},
	)
	rayIndex += 2

	// Deliberately tangent sphere ray. The component must report E_AMBIG at
	// this exact boundary rather than choosing a side.
	if c, radius, ok := centerFor(independentID); ok {
		p := project(c)
		scene.RayQueries = append(scene.RayQueries, RayQuery{
			ID: "ray-ambig-sphere", Screen: Vec2{X: p.X, Y: p.Y - (radius-1e-6)*pxPerWorld},
		})
	}

	boxTargets := []string{"n002", "n018", "n034", independentID}
	for i, id := range boxTargets {
		n := nodeByID[id]
		if n.GeometryIndex < 0 {
			continue
		}
		b := generatorProjectedAABB(generatorWorldBounds(scene.Geometries[n.GeometryIndex], worlds[id]), scene.Camera)
		pad := 4.0
		scene.BoxQueries = append(scene.BoxQueries, BoxQuery{
			ID:   fmt.Sprintf("box-%03d", i),
			Rect: Rect{X0: clamp(b.X0-pad, 0, scene.Camera.ViewportWidth), Y0: clamp(b.Y0-pad, 0, scene.Camera.ViewportHeight), X1: clamp(b.X1+pad, 0, scene.Camera.ViewportWidth), Y1: clamp(b.Y1+pad, 0, scene.Camera.ViewportHeight)},
		})
	}
	scene.BoxQueries = append(scene.BoxQueries, BoxQuery{
		ID: "box-ambig-edge", Rect: func() Rect {
			n := nodeByID["n018"]
			b := generatorProjectedAABB(generatorWorldBounds(scene.Geometries[n.GeometryIndex], worlds["n018"]), scene.Camera)
			return Rect{X0: b.X1, Y0: clamp(b.Y0-6, 0, scene.Camera.ViewportHeight), X1: clamp(b.X1+80, 0, scene.Camera.ViewportWidth), Y1: clamp(b.Y1+6, 0, scene.Camera.ViewportHeight)}
		}(),
	})

	if c, radius, ok := centerFor(independentID); ok {
		scene.PointQueries = append(scene.PointQueries,
			PointQuery{ID: "point-inside", Point: c},
			PointQuery{ID: "point-ambig-sphere-surface", Point: c.Add(V3(radius-1e-6, 0, 0))},
		)
	}

	worldN14 := worlds["n014"].TransformPoint(Vec3{})
	scene.EditSequence = []EditBatch{
		{
			ID: "batch-001-translate", Selection: []string{"n006"}, Pivot: V3(0, 0, 0),
			Steps: []PivotStep{{Kind: StepTranslate, Value: V3(0.07, 0.03, 0.012)}},
		},
		{
			ID: "batch-002-rotate", Selection: []string{"n014"}, Pivot: worldN14,
			Steps: []PivotStep{{Kind: StepRotate, Value: V3(0, 0, 7)}},
		},
		{
			ID: "batch-003-scale", Selection: []string{"n022", "n040"}, Pivot: V3(0, 0, 0),
			Steps: []PivotStep{{Kind: StepScale, Value: V3(1.018, 0.985, 1.012)}},
		},
		{
			ID: "batch-004-mixed", Selection: []string{"n030", "n048"}, Pivot: V3(0.35, 0.15, 0),
			Steps: []PivotStep{
				{Kind: StepTranslate, Value: V3(-0.025, 0.018, 0.008)},
				{Kind: StepRotate, Value: V3(0, 0, -4.5)},
			},
		},
	}

	hash, err := ComputeSceneHash(scene)
	if err != nil {
		return nil, nil, err
	}
	scene.ContentHash = hash
	truth, err := generateTruth(scene, worlds)
	if err != nil {
		return nil, nil, err
	}
	return scene, truth, nil
}

func generateTruth(scene *Scene, baseWorlds map[string]Mat4) (*Truth, error) {
	truth := &Truth{
		FormatVersion:     FormatVersion,
		SceneVersion:      scene.SceneVersion,
		Precision:         scene.Precision,
		ContentHash:       scene.ContentHash,
		TransformManifest: make([]MatrixManifest, 0, len(scene.Nodes)),
	}
	nodes := append([]Node(nil), scene.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		truth.TransformManifest = append(truth.TransformManifest, MatrixManifest{
			NodeID: n.ID, LocalDigest: LocalMatrixDigest(n.LocalMatrix, scene.Precision),
		})
	}
	truth.RayResults = generateRayTruth(scene, baseWorlds, scene.RayQueries)
	truth.BoxResults = generateBoxTruth(scene, baseWorlds, scene.BoxQueries)
	truth.PointResults = generatePointTruth(scene, baseWorlds, scene.PointQueries)

	worlds := make(map[string]Mat4, len(baseWorlds))
	for id, m := range baseWorlds {
		worlds[id] = m
	}
	locals := make(map[string]Mat4, len(nodes))
	for _, n := range nodes {
		locals[n.ID] = Mat4(n.LocalMatrix)
	}
	for _, batch := range scene.EditSequence {
		var err error
		locals, worlds, err = applyTruthBatch(scene, locals, worlds, batch)
		if err != nil {
			return nil, err
		}
		op := OperationTruth{BatchID: batch.ID}
		for _, id := range sortedNodeIDs(scene) {
			op.Matrices = append(op.Matrices, NodeMatrixTruth{NodeID: id, Matrix: matrixArray(worlds[id])})
		}
		truth.OperationResults = append(truth.OperationResults, op)
	}
	truth.FinalRayResults = generateRayTruth(scene, worlds, scene.RayQueries)
	truth.FinalBoxResults = generateBoxTruth(scene, worlds, scene.BoxQueries)
	truth.FinalPointResults = generatePointTruth(scene, worlds, scene.PointQueries)
	return truth, nil
}

func generateRayTruth(scene *Scene, worlds map[string]Mat4, queries []RayQuery) []RayTruth {
	out := make([]RayTruth, 0, len(queries))
	for _, q := range queries {
		ray := generatorRay(scene.Camera, q.Screen)
		rt := RayTruth{QueryID: q.ID, Geometries: []GeometryHitTruth{}}
		var first *HitTruth
		for _, n := range sortedNodes(scene) {
			if n.GeometryIndex < 0 {
				continue
			}
			g := scene.Geometries[n.GeometryIndex]
			hit, t, ambig, bound := truthIntersect(g, worlds[n.ID], ray, depthOf(scene, n.ID), scene.Precision)
			ght := GeometryHitTruth{NodeID: n.ID, GeometryID: g.ID, Hit: hit}
			if hit {
				p := ray.Origin.Add(ray.Direction.Mul(t))
				ght.T, ght.Point = t, p
				h := HitTruth{NodeID: n.ID, GeometryID: g.ID, T: t, Point: p}
				if first == nil || h.T < first.T || (h.T == first.T && h.NodeID < first.NodeID) {
					c := h
					first = &c
				}
			}
			rt.Geometries = append(rt.Geometries, ght)
			for _, reason := range ambig {
				rt.Ambiguities = append(rt.Ambiguities, Ambiguity{
					QueryID: q.ID, NodeID: n.ID, GeometryID: g.ID,
					Depth: depthOf(scene, n.ID), Reason: reason, Bound: bound,
				})
			}
		}
		rt.First = first
		sortAmbiguities(rt.Ambiguities)
		if len(rt.Ambiguities) > 0 {
			rt.ErrorCode = CodeAmbiguous
		}
		out = append(out, rt)
	}
	return out
}

func generateBoxTruth(scene *Scene, worlds map[string]Mat4, queries []BoxQuery) []BoxTruth {
	out := make([]BoxTruth, 0, len(queries))
	ppw := scene.Camera.ViewportHeight / scene.Camera.WorldHeight
	for _, q := range queries {
		bt := BoxTruth{QueryID: q.ID, Hits: []string{}}
		rect := q.Rect.Normalized()
		for _, n := range sortedNodes(scene) {
			if n.GeometryIndex < 0 {
				continue
			}
			g := scene.Geometries[n.GeometryIndex]
			b := generatorProjectedAABB(generatorWorldBounds(g, worlds[n.ID]), scene.Camera)
			ox := math.Min(b.X1, rect.X1) - math.Max(b.X0, rect.X0)
			oy := math.Min(b.Y1, rect.Y1) - math.Max(b.Y0, rect.Y0)
			eps := truthScreenEps(scene, depthOf(scene, n.ID), g, ppw)
			if ox >= 0 && oy >= 0 {
				bt.Hits = append(bt.Hits, n.ID)
				if math.Min(ox, oy) <= eps {
					bt.Ambiguities = append(bt.Ambiguities, Ambiguity{
						QueryID: q.ID, NodeID: n.ID, GeometryID: g.ID,
						Depth: depthOf(scene, n.ID), Reason: "frame_overlap_at_error_bound", Bound: math.Min(ox, oy),
					})
				}
			} else if math.Max(-ox, -oy) <= eps {
				bt.Ambiguities = append(bt.Ambiguities, Ambiguity{
					QueryID: q.ID, NodeID: n.ID, GeometryID: g.ID,
					Depth: depthOf(scene, n.ID), Reason: "frame_gap_at_error_bound", Bound: math.Max(-ox, -oy),
				})
			}
		}
		sort.Strings(bt.Hits)
		sortAmbiguities(bt.Ambiguities)
		if len(bt.Ambiguities) > 0 {
			bt.ErrorCode = CodeAmbiguous
		}
		out = append(out, bt)
	}
	return out
}

func generatePointTruth(scene *Scene, worlds map[string]Mat4, queries []PointQuery) []PointTruth {
	out := make([]PointTruth, 0, len(queries))
	for _, q := range queries {
		pt := PointTruth{QueryID: q.ID, Hits: []string{}}
		for _, n := range sortedNodes(scene) {
			if n.GeometryIndex < 0 {
				continue
			}
			g := scene.Geometries[n.GeometryIndex]
			inside, ambig, bound := truthContains(g, worlds[n.ID], q.Point, depthOf(scene, n.ID), scene.Precision)
			if inside {
				pt.Hits = append(pt.Hits, n.ID)
			}
			if ambig {
				pt.Ambiguities = append(pt.Ambiguities, Ambiguity{
					QueryID: q.ID, NodeID: n.ID, GeometryID: g.ID,
					Depth: depthOf(scene, n.ID), Reason: "point_near_boundary", Bound: bound,
				})
			}
		}
		sort.Strings(pt.Hits)
		sortAmbiguities(pt.Ambiguities)
		if len(pt.Ambiguities) > 0 {
			pt.ErrorCode = CodeAmbiguous
		}
		out = append(out, pt)
	}
	return out
}

func truthIntersect(g Geometry, world Mat4, ray Ray, depth, precision int) (bool, float64, []string, float64) {
	inv, ok := Mat4InverseAffine(world)
	if !ok {
		return false, 0, nil, 0
	}
	o := inv.TransformPoint(ray.Origin)
	d := inv.TransformVector(ray.Direction)
	size := generatorLocalBounds(g).Diagonal()
	bound := math.Pow10(-precision) * float64(depth+2) * 2 * (3 + 3*size)
	var hit bool
	var t float64
	var ambig []string
	switch g.Kind {
	case GeometryAABB:
		hit, t, ambig = truthRayAABB(o, d, g.Min, g.Max, ray, bound)
	case GeometrySphere:
		hit, t, ambig = truthRaySphere(o, d, g.Center, g.Radius, ray, bound)
	case GeometryMesh:
		hit, t, ambig = truthRayMesh(o, d, g, ray, bound)
	}
	return hit, t, dedupeStrings(ambig), bound
}

func truthRayAABB(o, d, min, max Vec3, ray Ray, bound float64) (bool, float64, []string) {
	tmin, tmax := math.Inf(-1), math.Inf(1)
	for axis := 0; axis < 3; axis++ {
		oa, da := o.Component(axis), d.Component(axis)
		lo, hi := min.Component(axis), max.Component(axis)
		if math.Abs(da) < 1e-18 {
			if oa < lo || oa > hi {
				return false, 0, nil
			}
			continue
		}
		t1, t2 := (lo-oa)/da, (hi-oa)/da
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		tmin, tmax = math.Max(tmin, t1), math.Min(tmax, t2)
		if tmin > tmax {
			return false, 0, nil
		}
	}
	if tmax < 0 {
		return false, 0, nil
	}
	t := math.Max(0, tmin)
	var ambig []string
	inside := o.X >= min.X && o.X <= max.X && o.Y >= min.Y && o.Y <= max.Y && o.Z >= min.Z && o.Z <= max.Z
	if (tmax-tmin)*ray.Direction.Length() <= bound {
		ambig = append(ambig, "grazing_aabb")
	}
	if !inside && t*ray.Direction.Length() <= bound {
		ambig = append(ambig, "ray_starts_on_aabb_surface")
	}
	if distanceToAABB(o, min, max) <= bound {
		ambig = append(ambig, "ray_origin_near_aabb_boundary")
	}
	return true, t, ambig
}

func truthRaySphere(o, d, center Vec3, radius float64, ray Ray, bound float64) (bool, float64, []string) {
	oc := o.Sub(center)
	a := d.Dot(d)
	if a < 1e-24 {
		return false, 0, nil
	}
	b := 2 * oc.Dot(d)
	c := oc.Dot(oc) - radius*radius
	disc := b*b - 4*a*c
	rayDistance := math.Sqrt(math.Max(0, oc.Dot(oc)-(oc.Dot(d)*oc.Dot(d))/a))
	if disc < 0 {
		if math.Abs(rayDistance-radius) <= bound {
			return false, 0, []string{"sphere_tangency_boundary"}
		}
		return false, 0, nil
	}
	sq := math.Sqrt(math.Max(0, disc))
	t0, t1 := (-b-sq)/(2*a), (-b+sq)/(2*a)
	if t0 > t1 {
		t0, t1 = t1, t0
	}
	if t1 < 0 {
		return false, 0, nil
	}
	t := math.Max(0, t0)
	var ambig []string
	if math.Abs(rayDistance-radius) <= bound {
		ambig = append(ambig, "sphere_tangency_boundary")
	}
	if (t1-t0)*ray.Direction.Length() <= bound {
		ambig = append(ambig, "sphere_tangency_boundary")
	}
	if math.Abs(o.Sub(center).Length()-radius) <= bound {
		ambig = append(ambig, "ray_origin_near_sphere_surface")
	}
	return true, t, ambig
}

func truthRayMesh(o, d Vec3, g Geometry, ray Ray, bound float64) (bool, float64, []string) {
	best := math.Inf(1)
	var ambig []string
	for _, tri := range g.Triangles {
		v0 := g.Vertices[tri[0]]
		e1 := g.Vertices[tri[1]].Sub(v0)
		e2 := g.Vertices[tri[2]].Sub(v0)
		p := d.Cross(e2)
		det := e1.Dot(p)
		if math.Abs(det) < 1e-18 {
			continue
		}
		inv := 1 / det
		tv := o.Sub(v0)
		u := tv.Dot(p) * inv
		q := tv.Cross(e1)
		v := d.Dot(q) * inv
		t := e2.Dot(q) * inv
		if u < -1e-12 || v < -1e-12 || u+v > 1+1e-12 || t < 0 {
			continue
		}
		if t < best {
			best = t
		}
		if math.Abs(u) <= bound || math.Abs(v) <= bound || math.Abs(1-u-v) <= bound {
			ambig = append(ambig, "mesh_triangle_edge")
		}
		if t*ray.Direction.Length() <= bound {
			ambig = append(ambig, "ray_starts_on_mesh_surface")
		}
	}
	if math.IsInf(best, 1) {
		return false, 0, nil
	}
	return true, best, dedupeStrings(ambig)
}

func truthContains(g Geometry, world Mat4, p Vec3, depth, precision int) (bool, bool, float64) {
	inv, ok := Mat4InverseAffine(world)
	if !ok {
		return false, false, 0
	}
	lp := inv.TransformPoint(p)
	bound := math.Pow10(-precision) * float64(depth+2) * 2 * (3 + generatorLocalBounds(g).Diagonal())
	switch g.Kind {
	case GeometryAABB:
		inside, ambig, margin := generatorPointInAABB(lp, g.Min, g.Max, bound)
		if inside && !ambig {
			return true, false, 0
		}
		return inside, ambig, margin
	case GeometrySphere:
		margin := math.Abs(lp.Sub(g.Center).Length() - g.Radius)
		return lp.Sub(g.Center).Length() < g.Radius, margin <= bound, margin
	case GeometryMesh:
		b := generatorLocalBounds(g)
		inside, ambig, margin := generatorPointInAABB(lp, b.Min, b.Max, bound)
		if inside && !ambig {
			return true, false, 0
		}
		return inside, ambig, margin
	}
	return false, false, 0
}

func generatorPointInAABB(p, min, max Vec3, bound float64) (bool, bool, float64) {
	inside := p.X >= min.X && p.X <= max.X && p.Y >= min.Y && p.Y <= max.Y && p.Z >= min.Z && p.Z <= max.Z
	if inside {
		margin := math.Min(
			math.Min(p.X-min.X, max.X-p.X),
			math.Min(math.Min(p.Y-min.Y, max.Y-p.Y), math.Min(p.Z-min.Z, max.Z-p.Z)),
		)
		return true, margin <= bound, margin
	}
	gap := math.Max(math.Max(min.X-p.X, p.X-max.X), math.Max(math.Max(min.Y-p.Y, p.Y-max.Y), math.Max(min.Z-p.Z, p.Z-max.Z)))
	return false, gap <= bound, gap
}

func applyTruthBatch(scene *Scene, locals map[string]Mat4, worlds map[string]Mat4, batch EditBatch) (map[string]Mat4, map[string]Mat4, error) {
	delta := IdentityMat4()
	for _, step := range batch.Steps {
		switch step.Kind {
		case StepTranslate:
			delta = Mat4Multiply(TranslationMat4(step.Value), delta)
		case StepRotate:
			d := Mat4Multiply(TranslationMat4(batch.Pivot), Mat4Multiply(RotationEulerDegrees(step.Value), TranslationMat4(batch.Pivot.Mul(-1))))
			delta = Mat4Multiply(d, delta)
		case StepScale:
			d := Mat4Multiply(TranslationMat4(batch.Pivot), Mat4Multiply(ScaleMat4(step.Value), TranslationMat4(batch.Pivot.Mul(-1))))
			delta = Mat4Multiply(d, delta)
		}
	}
	nodeMap := make(map[string]Node, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodeMap[n.ID] = n
	}
	nextLocals := make(map[string]Mat4, len(locals))
	for k, v := range locals {
		nextLocals[k] = v
	}
	for _, id := range batch.Selection {
		newWorld := Mat4Multiply(delta, worlds[id])
		parentWorld := IdentityMat4()
		if p := nodeMap[id].ParentID; p != "" {
			parentWorld = worlds[p]
		}
		inv, ok := Mat4InverseAffine(parentWorld)
		if !ok {
			return nil, nil, fmt.Errorf("singular parent for %s", id)
		}
		nextLocals[id] = roundMatrix(Mat4Multiply(inv, newWorld), scene.Precision)
	}
	nextWorlds, _, err := floatWorldsWithLocals(scene, nextLocals)
	if err != nil {
		return nil, nil, err
	}
	return nextLocals, nextWorlds, nil
}

func floatWorlds(scene *Scene) (map[string]Mat4, []string, error) {
	locals := make(map[string]Mat4, len(scene.Nodes))
	for _, n := range scene.Nodes {
		locals[n.ID] = Mat4(n.LocalMatrix)
	}
	return floatWorldsWithLocals(scene, locals)
}

func floatWorldsWithLocals(scene *Scene, locals map[string]Mat4) (map[string]Mat4, []string, error) {
	nodeMap := make(map[string]Node, len(scene.Nodes))
	children := make(map[string][]string)
	var roots []string
	for _, n := range scene.Nodes {
		nodeMap[n.ID] = n
		if n.ParentID == "" {
			roots = append(roots, n.ID)
		} else {
			children[n.ParentID] = append(children[n.ParentID], n.ID)
		}
	}
	sort.Strings(roots)
	for k := range children {
		sort.Strings(children[k])
	}
	worlds := make(map[string]Mat4, len(scene.Nodes))
	var order []string
	var visit func(string) error
	visit = func(id string) error {
		n := nodeMap[id]
		if n.ParentID == "" {
			worlds[id] = locals[id]
		} else {
			parent, ok := worlds[n.ParentID]
			if !ok {
				return fmt.Errorf("parent %s not computed for %s", n.ParentID, id)
			}
			worlds[id] = floatMul4(parent, locals[id])
		}
		order = append(order, id)
		for _, c := range children[id] {
			if err := visit(c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := visit(r); err != nil {
			return nil, nil, err
		}
	}
	return worlds, order, nil
}

func floatMul4(a, b Mat4) Mat4 {
	var out Mat4
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			var s float64
			for k := 0; k < 4; k++ {
				s += a[i*4+k] * b[k*4+j]
			}
			out[i*4+j] = s
		}
	}
	return out
}

func generatorRay(c Camera, screen Vec2) Ray {
	ppw := c.ViewportHeight / c.WorldHeight
	wx := (screen.X - c.ViewportWidth/2) / ppw
	wy := (c.ViewportHeight/2 - screen.Y) / ppw
	dir := c.Forward
	if n, ok := dir.Normalize(); ok {
		dir = n
	}
	return Ray{Origin: c.Eye.Add(c.Right.Mul(wx)).Add(c.Up.Mul(wy)), Direction: dir}
}

func depthOf(scene *Scene, nodeID string) int {
	nodeMap := make(map[string]Node, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodeMap[n.ID] = n
	}
	depth := 0
	for nodeMap[nodeID].ParentID != "" {
		nodeID = nodeMap[nodeID].ParentID
		depth++
	}
	return depth
}

func sortedNodes(scene *Scene) []Node {
	out := append([]Node(nil), scene.Nodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedNodeIDs(scene *Scene) []string {
	nodes := sortedNodes(scene)
	out := make([]string, len(nodes))
	for i := range nodes {
		out[i] = nodes[i].ID
	}
	return out
}

func truthScreenEps(scene *Scene, depth int, g Geometry, ppw float64) float64 {
	q := math.Pow10(-scene.Precision)
	return (q*float64(depth+2)*2*(3+generatorLocalBounds(g).Diagonal()) + 1e-9) * ppw
}

func roundMatrix(m Mat4, precision int) Mat4 {
	s := math.Pow10(precision)
	var out Mat4
	for i, v := range m {
		out[i] = math.Round(v*s) / s
	}
	out[12], out[13], out[14], out[15] = 0, 0, 0, 1
	return out
}

func matrixArray(m Mat4) [16]float64 {
	var out [16]float64
	copy(out[:], m[:])
	return out
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func generatorLocalBounds(g Geometry) AABB {
	switch g.Kind {
	case GeometryAABB:
		return AABB{Min: g.Min, Max: g.Max}
	case GeometrySphere:
		r := V3(g.Radius, g.Radius, g.Radius)
		return AABB{Min: g.Center.Sub(r), Max: g.Center.Add(r)}
	case GeometryMesh:
		if len(g.Vertices) == 0 {
			return AABB{}
		}
		a := AABB{Min: g.Vertices[0], Max: g.Vertices[0]}
		for _, v := range g.Vertices[1:] {
			a.Min = a.Min.Min(v)
			a.Max = a.Max.Max(v)
		}
		return a
	default:
		return AABB{}
	}
}

func generatorWorldBounds(g Geometry, world Mat4) AABB {
	lb := generatorLocalBounds(g)
	corners := []Vec3{
		V3(lb.Min.X, lb.Min.Y, lb.Min.Z), V3(lb.Max.X, lb.Min.Y, lb.Min.Z),
		V3(lb.Min.X, lb.Max.Y, lb.Min.Z), V3(lb.Max.X, lb.Max.Y, lb.Min.Z),
		V3(lb.Min.X, lb.Min.Y, lb.Max.Z), V3(lb.Max.X, lb.Min.Y, lb.Max.Z),
		V3(lb.Min.X, lb.Max.Y, lb.Max.Z), V3(lb.Max.X, lb.Max.Y, lb.Max.Z),
	}
	var out AABB
	first := true
	for _, c := range corners {
		p := world.TransformPoint(c)
		if first {
			out = AABB{Min: p, Max: p}
			first = false
		} else {
			out.Min = out.Min.Min(p)
			out.Max = out.Max.Max(p)
		}
	}
	return out
}

func generatorWorldSphere(g Geometry, world Mat4) Sphere {
	if g.Kind != GeometrySphere {
		a := generatorWorldBounds(g, world)
		return Sphere{Center: a.Center(), Radius: a.Diagonal() / 2}
	}
	center := world.TransformPoint(g.Center)
	sx := V3(world[0], world[4], world[8]).Length()
	sy := V3(world[1], world[5], world[9]).Length()
	sz := V3(world[2], world[6], world[10]).Length()
	return Sphere{Center: center, Radius: g.Radius * math.Max(sx, math.Max(sy, sz))}
}

func generatorProjectedAABB(a AABB, camera Camera) Rect {
	ppw := camera.ViewportHeight / camera.WorldHeight
	corners := []Vec3{
		V3(a.Min.X, a.Min.Y, a.Min.Z), V3(a.Max.X, a.Min.Y, a.Min.Z),
		V3(a.Min.X, a.Max.Y, a.Min.Z), V3(a.Max.X, a.Max.Y, a.Min.Z),
		V3(a.Min.X, a.Min.Y, a.Max.Z), V3(a.Max.X, a.Min.Y, a.Max.Z),
		V3(a.Min.X, a.Max.Y, a.Max.Z), V3(a.Max.X, a.Max.Y, a.Max.Z),
	}
	out := Rect{X0: math.Inf(1), Y0: math.Inf(1), X1: math.Inf(-1), Y1: math.Inf(-1)}
	for _, c := range corners {
		d := c.Sub(camera.Eye)
		x := camera.ViewportWidth/2 + d.Dot(camera.Right)*ppw
		y := camera.ViewportHeight/2 - d.Dot(camera.Up)*ppw
		out.X0 = math.Min(out.X0, x)
		out.Y0 = math.Min(out.Y0, y)
		out.X1 = math.Max(out.X1, x)
		out.Y1 = math.Max(out.Y1, y)
	}
	return out
}

// WriteSample generates the paired sample and truth files. Files are written
// with a temp-file + rename sequence so readers never observe a partial JSON
// document.
func WriteSample(dir string) (*Scene, *Truth, error) {
	scene, truth, err := GenerateSample()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	sceneBytes, err := json.MarshalIndent(scene, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	truthBytes, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	sceneBytes = append(sceneBytes, '\n')
	truthBytes = append(truthBytes, '\n')
	if err := atomicWriteFile(filepath.Join(dir, "truth.json"), truthBytes, 0o644); err != nil {
		return nil, nil, err
	}
	if err := atomicWriteFile(filepath.Join(dir, "scene.json"), sceneBytes, 0o644); err != nil {
		return nil, nil, err
	}
	return scene, truth, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}
