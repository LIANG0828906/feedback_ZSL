package editorpick

import (
	"math/big"
	"sort"
	"sync"
)

// Derived is the cached fixed-point world transform and conservative error
// envelope for one node.
type Derived struct {
	NodeID              string    `json:"node_id"`
	Depth               int       `json:"depth"`
	World               Mat4      `json:"world"`
	Inverse             Mat4      `json:"inverse"`
	AABB                AABB      `json:"aabb"`
	Sphere              Sphere    `json:"sphere"`
	MatrixError         [16]int64 `json:"matrix_error_units"`
	MaxMatrixErrorUnits int64     `json:"max_matrix_error_units"`
	PositionErrorUnits  int64     `json:"position_error_units"`
	Revision            int64     `json:"revision"`
	ContentHash         string    `json:"content_hash"`
}

type WorldGeometry struct {
	NodeID        string   `json:"node_id"`
	GeometryIndex int      `json:"geometry_index"`
	Vertices      []Vec3   `json:"vertices,omitempty"`
	Triangles     [][3]int `json:"triangles,omitempty"`
	MaxErrorUnits int64    `json:"max_error_units"`
}

// IndexEntry is one stable hierarchical acceleration entry per scene node.
type IndexEntry struct {
	NodeID        string `json:"node_id"`
	GeometryIndex int    `json:"geometry_index"`
	HasBounds     bool   `json:"has_bounds"`
	OwnBounds     AABB   `json:"own_bounds"`
	OwnSphere     Sphere `json:"own_sphere"`
	SubtreeBounds AABB   `json:"subtree_bounds"`
	SubtreeSphere Sphere `json:"subtree_sphere"`
	Revision      int64  `json:"revision"`
	ContentHash   string `json:"content_hash"`
}

type queryCacheEntry struct {
	key      string
	ray      *RayResult
	box      *BoxResult
	point    *PointResult
	depNodes []string
}

type Engine struct {
	mu              sync.RWMutex
	snapshotWriteMu sync.Mutex
	scene           *Scene
	derived         map[string]*Derived
	worldGeom       map[string]*WorldGeometry
	index           map[string]*IndexEntry
	graph           *DepGraph
	cache           map[string]*queryCacheEntry
	revision        int64
	lastReport      *InvalidationReport
}

func NewEngine(scene *Scene) (*Engine, error) {
	e := &Engine{scene: scene, revision: 1}
	if err := e.RebuildAll(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) Scene() *Scene { return e.scene }

func (e *Engine) Revision() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.revision
}

func (e *Engine) LastInvalidation() *InvalidationReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastReport
}

func (e *Engine) Derived(nodeID string) (*Derived, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.derived[nodeID]
	return d, ok
}

func (e *Engine) IndexEntry(nodeID string) (*IndexEntry, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	v, ok := e.index[nodeID]
	return v, ok
}

func (e *Engine) topoOrderLocked() ([]*Node, error) {
	byID := e.scene.NodeByID
	order := make([]*Node, 0, len(e.scene.Nodes))
	queued := make(map[string]bool, len(e.scene.Nodes))
	queue := make([]string, 0, len(e.scene.Nodes))
	roots := append([]string(nil), e.scene.Roots...)
	sort.Strings(roots)
	for _, id := range roots {
		if !queued[id] {
			queued[id] = true
			queue = append(queue, id)
		}
	}
	for head := 0; head < len(queue); head++ {
		id := queue[head]
		n, ok := byID[id]
		if !ok {
			return nil, newRef(id, "node missing during topological ordering")
		}
		order = append(order, n)
		children := append([]string(nil), e.scene.Children[id]...)
		sort.Strings(children)
		for _, child := range children {
			if !queued[child] {
				queued[child] = true
				queue = append(queue, child)
			}
		}
	}
	if len(order) != len(e.scene.Nodes) {
		return nil, newCycle("<forest>")
	}
	return order, nil
}

func (e *Engine) RebuildAll() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rebuildAllLocked()
}

func (e *Engine) rebuildAllLocked() error {
	e.revision++
	e.derived = make(map[string]*Derived, len(e.scene.Nodes))
	e.worldGeom = make(map[string]*WorldGeometry)
	e.index = make(map[string]*IndexEntry, len(e.scene.Nodes))
	e.cache = make(map[string]*queryCacheEntry)
	e.graph = NewDepGraph()

	for _, g := range e.scene.Geometries {
		e.graph.AddEdge(geometryObject(g.Index), "all-geometry", "geometry-asset")
	}
	for _, n := range e.scene.Nodes {
		e.graph.AddEdge(localObject(n.ID), worldObject(n.ID), "local-transform")
		if n.ParentID != "" {
			e.graph.AddEdge(worldObject(n.ParentID), worldObject(n.ID), "parent-transform")
		}
		e.graph.AddEdge(worldObject(n.ID), entryObject(n.ID), "world-transform")
		if n.GeometryIndex >= 0 {
			e.graph.AddEdge(geometryObject(n.GeometryIndex), entryObject(n.ID), "geometry-content")
		}
		if n.ParentID != "" {
			e.graph.AddEdge(entryObject(n.ID), entryObject(n.ParentID), "subtree-bounds")
		}
	}

	order, err := e.topoOrderLocked()
	if err != nil {
		return err
	}
	for _, n := range order {
		parent := (*Derived)(nil)
		if n.ParentID != "" {
			parent = e.derived[n.ParentID]
			if parent == nil {
				return newSchema(n.ID, "parent derived cache is missing")
			}
		}
		d, wg, entry, err := computeNode(e.scene, e.revision, n, parent)
		if err != nil {
			return err
		}
		e.derived[n.ID] = d
		if wg != nil {
			e.worldGeom[n.ID] = wg
		}
		e.index[n.ID] = entry
	}
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		e.recomputeSubtreeLocked(n.ID)
	}
	e.lastReport = &InvalidationReport{Revision: e.revision, Objects: []InvalidObject{{ObjectID: "<all>", Kind: "snapshot", Reason: "full-rebuild"}}}
	return nil
}

func localErrorMatrix() [16]int64 {
	var m [16]int64
	for i := range m {
		m[i] = 1 // <= 0.5 ULP, rounded up to a conservative whole ULP bound.
	}
	return m
}

func propagateMatrixError(p Mat4, pe [16]int64, l Mat4, le [16]int64) [16]int64 {
	var out [16]int64
	for row := 0; row < 4; row++ {
		for col := 0; col < 4; col++ {
			var numerator big.Int
			for k := 0; k < 4; k++ {
				pik := big.NewInt(absInt64(int64(p[row*4+k])))
				lek := big.NewInt(le[k*4+col])
				lkj := big.NewInt(absInt64(int64(l[k*4+col])))
				peik := big.NewInt(pe[row*4+k])
				term1 := new(big.Int).Mul(pik, lek)
				term2 := new(big.Int).Mul(lkj, peik)
				term3 := new(big.Int).Mul(new(big.Int).Set(peik), big.NewInt(le[k*4+col]))
				numerator.Add(&numerator, term1)
				numerator.Add(&numerator, term2)
				numerator.Add(&numerator, term3)
			}
			units, ok := ceilQuo(&numerator, FixedScale)
			if !ok || units < 0 {
				units = 1 << 62
			}
			out[row*4+col] = units + 1 // one fixed matrix-product rounding bound.
		}
	}
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func pointErrorUnits(m Mat4, me [16]int64, p Vec3) int64 {
	homogeneous := [4]int64{int64(p.X), int64(p.Y), int64(p.Z), FixedScale}
	var maxUnits int64 = 1
	for row := 0; row < 3; row++ {
		var numerator big.Int
		for col := 0; col < 4; col++ {
			term := big.NewInt(me[row*4+col])
			term.Mul(term, big.NewInt(absInt64(homogeneous[col])))
			numerator.Add(&numerator, term)
		}
		units, ok := ceilQuo(&numerator, FixedScale)
		if !ok || units > maxUnits {
			maxUnits = units
		}
	}
	return maxUnits + 1 // TransformPoint rounding.
}

func computeNode(scene *Scene, revision int64, n *Node, parent *Derived) (*Derived, *WorldGeometry, *IndexEntry, error) {
	world := n.Local
	merr := localErrorMatrix()
	depth := 0
	if parent != nil {
		var err error
		world, err = parent.World.Mul(n.Local)
		if err != nil {
			return nil, nil, nil, newSchema(n.ID, "fixed point overflow while propagating transform")
		}
		merr = propagateMatrixError(parent.World, parent.MatrixError, n.Local, localErrorMatrix())
		depth = parent.Depth + 1
	}
	inv, err := world.Inverse()
	if err != nil {
		return nil, nil, nil, &DomainError{Code: ETxn, NodeID: n.ID, Message: "world matrix is singular"}
	}
	maxErr := int64(0)
	for _, v := range merr {
		if v > maxErr {
			maxErr = v
		}
	}
	d := &Derived{
		NodeID:              n.ID,
		Depth:               depth,
		World:               world,
		Inverse:             inv,
		MatrixError:         merr,
		MaxMatrixErrorUnits: maxErr,
		Revision:            revision,
	}

	entry := &IndexEntry{NodeID: n.ID, GeometryIndex: n.GeometryIndex, Revision: revision}
	var wg *WorldGeometry
	if n.GeometryIndex >= 0 && n.GeometryIndex < len(scene.Geometries) {
		g := scene.Geometries[n.GeometryIndex]
		bounds, posErr, err := transformLocalBounds(world, merr, g.LocalAABB)
		if err != nil {
			return nil, nil, nil, newSchema(n.ID, err.Error())
		}
		d.AABB = bounds
		d.Sphere = boundingSphereFromAABB(bounds)
		d.PositionErrorUnits = posErr
		entry.GeometryIndex = n.GeometryIndex
		entry.HasBounds = true
		entry.OwnBounds = bounds.Expand(Fixed(posErr))
		entry.OwnSphere = boundingSphereFromAABB(entry.OwnBounds)

		wg = &WorldGeometry{NodeID: n.ID, GeometryIndex: n.GeometryIndex, MaxErrorUnits: posErr}
		if g.Kind == GeometryMesh {
			wg.Vertices = make([]Vec3, len(g.Vertices))
			var maxVertexErr int64
			for i, v := range g.Vertices {
				wv, err := world.TransformPoint(v)
				if err != nil {
					return nil, nil, nil, newSchema(n.ID, "fixed point overflow transforming mesh vertex")
				}
				wg.Vertices[i] = wv
				ve := pointErrorUnits(world, merr, v)
				if ve > maxVertexErr {
					maxVertexErr = ve
				}
			}
			wg.MaxErrorUnits = maxVertexErr
			wg.Triangles = append([][3]int(nil), g.Triangles...)
			d.PositionErrorUnits = maxVertexErr
			entry.OwnSphere = boundingSphereFromAABB(entry.OwnBounds)
			wg.MaxErrorUnits = maxVertexErr
		}
	} else {
		// Nodes without geometry still need a valid neutral entry. Hierarchy
		// bounds are assembled from descendants.
		entry.OwnBounds = AABB{}
		entry.OwnSphere = Sphere{}
		entry.HasBounds = false
	}
	entry.SubtreeBounds = entry.OwnBounds
	entry.SubtreeSphere = entry.OwnSphere
	hash, _ := canonicalHash(struct {
		World  Mat4
		Error  [16]int64
		Bounds AABB
	}{d.World, d.MatrixError, entry.OwnBounds})
	d.ContentHash = hash
	entry.ContentHash = hash
	return d, wg, entry, nil
}

func transformLocalBounds(world Mat4, merr [16]int64, b AABB) (AABB, int64, error) {
	corners := []Vec3{
		{b.Min.X, b.Min.Y, b.Min.Z}, {b.Max.X, b.Min.Y, b.Min.Z},
		{b.Min.X, b.Max.Y, b.Min.Z}, {b.Max.X, b.Max.Y, b.Min.Z},
		{b.Min.X, b.Min.Y, b.Max.Z}, {b.Max.X, b.Min.Y, b.Max.Z},
		{b.Min.X, b.Max.Y, b.Max.Z}, {b.Max.X, b.Max.Y, b.Max.Z},
	}
	var out AABB
	var maxErr int64 = 1
	for i, c := range corners {
		p, err := world.TransformPoint(c)
		if err != nil {
			return AABB{}, 0, err
		}
		pe := pointErrorUnits(world, merr, c)
		if pe > maxErr {
			maxErr = pe
		}
		if i == 0 {
			out.Min, out.Max = p, p
		} else {
			out.Min = Vec3{minFixed(out.Min.X, p.X), minFixed(out.Min.Y, p.Y), minFixed(out.Min.Z, p.Z)}
			out.Max = Vec3{maxFixed(out.Max.X, p.X), maxFixed(out.Max.Y, p.Y), maxFixed(out.Max.Z, p.Z)}
		}
	}
	return out, maxErr, nil
}

func boundingSphereFromAABB(b AABB) Sphere {
	cx := (b.Min.X + b.Max.X) / 2
	cy := (b.Min.Y + b.Max.Y) / 2
	cz := (b.Min.Z + b.Max.Z) / 2
	fx := maxFixed(absFixed(b.Max.X-cx), absFixed(cx-b.Min.X)) + 1
	fy := maxFixed(absFixed(b.Max.Y-cy), absFixed(cy-b.Min.Y)) + 1
	fz := maxFixed(absFixed(b.Max.Z-cz), absFixed(cz-b.Min.Z)) + 1
	var sum big.Int
	for _, v := range []Fixed{fx, fy, fz} {
		term := big.NewInt(int64(v))
		sum.Add(&sum, new(big.Int).Mul(term, term))
	}
	r := upperSqrtBig(&sum)
	return Sphere{Center: Vec3{cx, cy, cz}, Radius: Fixed(r)}
}

func upperSqrtBig(v *big.Int) int64 {
	if v.Sign() <= 0 {
		return 0
	}
	var floor big.Int
	floor.Sqrt(v)
	r := floor.Int64()
	var sq big.Int
	sq.Mul(big.NewInt(r), big.NewInt(r))
	if sq.Cmp(v) < 0 {
		r++
	}
	return r
}

func (e *Engine) recomputeSubtreeLocked(nodeID string) {
	entry := e.index[nodeID]
	if entry == nil {
		return
	}
	has := false
	if entry.GeometryIndex >= 0 {
		entry.SubtreeBounds = entry.OwnBounds
		entry.SubtreeSphere = entry.OwnSphere
		has = true
	} else {
		entry.SubtreeBounds = AABB{}
		entry.SubtreeSphere = Sphere{}
	}
	for _, childID := range e.scene.Children[nodeID] {
		child := e.index[childID]
		if child == nil {
			continue
		}
		if !child.HasBounds {
			continue
		}
		if !has {
			entry.SubtreeBounds = child.SubtreeBounds
			entry.SubtreeSphere = child.SubtreeSphere
			has = true
		} else {
			entry.SubtreeBounds = entry.SubtreeBounds.Union(child.SubtreeBounds)
			entry.SubtreeSphere = entry.SubtreeSphere.UnionSphere(child.SubtreeSphere)
		}
	}
	if !has {
		entry.SubtreeBounds = AABB{}
		entry.SubtreeSphere = Sphere{}
	}
	entry.HasBounds = has
}

func (s Sphere) UnionSphere(o Sphere) Sphere {
	// Conservative enclosing sphere from the two center/radius circles. The
	// result is intentionally slightly enlarged by one fixed ULP.
	dx := o.Center.X - s.Center.X
	dy := o.Center.Y - s.Center.Y
	dz := o.Center.Z - s.Center.Z
	dist := Fixed(upperSqrtBig(squaresBig(dx, dy, dz)))
	if dist+s.Radius <= o.Radius {
		return o
	}
	if dist+o.Radius <= s.Radius {
		return s
	}
	radius := (dist + s.Radius + o.Radius) / 2
	ratioNum := radius - s.Radius
	var cx, cy, cz Fixed
	if dist > 0 {
		cx = s.Center.X + Fixed(int64(dx)*int64(ratioNum)/int64(dist))
		cy = s.Center.Y + Fixed(int64(dy)*int64(ratioNum)/int64(dist))
		cz = s.Center.Z + Fixed(int64(dz)*int64(ratioNum)/int64(dist))
	} else {
		cx, cy, cz = s.Center.X, s.Center.Y, s.Center.Z
	}
	return Sphere{Center: Vec3{cx, cy, cz}, Radius: radius + 1}
}

func squaresBig(x, y, z Fixed) *big.Int {
	var sum big.Int
	for _, v := range []Fixed{x, y, z} {
		term := big.NewInt(int64(v))
		sum.Add(&sum, new(big.Int).Mul(term, term))
	}
	return &sum
}
