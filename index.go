package spatialpick

import (
	"math"
	"sort"
)

type BVHNode struct {
	Bounds AABB     `json:"bounds"`
	Left   *BVHNode `json:"left,omitempty"`
	Right  *BVHNode `json:"right,omitempty"`
	Entry  int      `json:"entry,omitempty"`
	Leaf   bool     `json:"leaf"`
}

type IndexEntry struct {
	NodeID         string      `json:"node_id"`
	GeometryID     string      `json:"geometry_id"`
	GeometryIndex  int         `json:"geometry_index"`
	Depth          int         `json:"depth"`
	Version        uint64      `json:"version"`
	LocalVersion   uint64      `json:"local_version"`
	World          FixedMatrix `json:"world"`
	WorldInverse   FixedMatrix `json:"world_inverse"`
	WorldFloat     Mat4        `json:"world_float"`
	WorldInvFloat  Mat4        `json:"world_inv_float"`
	AABB           AABB        `json:"aabb"`
	Sphere         Sphere      `json:"sphere"`
	Geometry       Geometry    `json:"geometry"`
	TransformError float64     `json:"transform_error"`
	GeometryError  float64     `json:"geometry_error"`
	Quantum        float64     `json:"quantum"`
	Failed         bool        `json:"failed"`
	Failure        string      `json:"failure,omitempty"`
}

// Index is immutable after publication. Queries may run concurrently on one
// Index while an editor prepares the next Index.
type Index struct {
	Version       uint64       `json:"version"`
	Entries       []IndexEntry `json:"entries"`
	Root          *BVHNode     `json:"root,omitempty"`
	MaxErrorBound float64      `json:"max_error_bound"`
}

func MakeIndexEntry(d *Derived, g *Geometry, precision int) IndexEntry {
	e := IndexEntry{
		NodeID:         d.NodeID,
		GeometryID:     d.GeometryID,
		GeometryIndex:  d.GeometryIndex,
		Depth:          d.Depth,
		Version:        d.Version,
		LocalVersion:   d.LocalVersion,
		World:          d.World,
		WorldInverse:   d.WorldInverse,
		WorldFloat:     d.WorldFloat,
		WorldInvFloat:  d.WorldInvFloat,
		AABB:           d.AABB,
		Sphere:         d.Sphere,
		TransformError: d.TransformError,
		GeometryError:  d.GeometryError,
		Quantum:        math.Pow10(-precision),
		Failed:         d.Failed,
		Failure:        d.Failure,
	}
	if g != nil {
		e.Geometry = *g
	}
	return e
}

func BuildIndex(entries []IndexEntry, version uint64) *Index {
	es := append([]IndexEntry(nil), entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].NodeID < es[j].NodeID })
	idx := &Index{Version: version, Entries: es}
	if len(es) > 0 {
		order := make([]int, len(es))
		for i := range order {
			order[i] = i
		}
		idx.Root = buildBVH(es, order)
	}
	for i := range es {
		if es[i].GeometryError > idx.MaxErrorBound {
			idx.MaxErrorBound = es[i].GeometryError
		}
	}
	return idx
}

func buildBVH(entries []IndexEntry, order []int) *BVHNode {
	if len(order) == 1 {
		return &BVHNode{Bounds: entries[order[0]].AABB, Leaf: true, Entry: order[0]}
	}
	centroidBounds := EmptyAABB()
	for _, i := range order {
		c := entries[i].AABB.Center()
		centroidBounds = centroidBounds.Union(AABB{Min: c, Max: c})
	}
	ext := centroidBounds.Size()
	axis := 0
	if ext.Y > ext.X && ext.Y >= ext.Z {
		axis = 1
	} else if ext.Z > ext.X && ext.Z > ext.Y {
		axis = 2
	}
	sort.Slice(order, func(i, j int) bool {
		a := entries[order[i]].AABB.Center().Component(axis)
		b := entries[order[j]].AABB.Center().Component(axis)
		if a != b {
			return a < b
		}
		return entries[order[i]].NodeID < entries[order[j]].NodeID
	})
	mid := len(order) / 2
	left := buildBVH(entries, order[:mid])
	right := buildBVH(entries, order[mid:])
	return &BVHNode{
		Bounds: left.Bounds.Union(right.Bounds),
		Left:   left,
		Right:  right,
		Entry:  -1,
	}
}

func (idx *Index) FindEntry(nodeID string) (int, bool) {
	if idx == nil {
		return 0, false
	}
	i := sort.Search(len(idx.Entries), func(i int) bool { return idx.Entries[i].NodeID >= nodeID })
	return i, i < len(idx.Entries) && idx.Entries[i].NodeID == nodeID
}

// UpdateIndexEntries replaces selected transform entries and rebuilds only the
// BVH paths from the changed leaves to the root. Unchanged subtrees are shared
// with the previous immutable index.
func UpdateIndexEntries(old *Index, changed map[string]IndexEntry, version uint64) *Index {
	if old == nil {
		list := make([]IndexEntry, 0, len(changed))
		for _, e := range changed {
			list = append(list, e)
		}
		return BuildIndex(list, version)
	}
	entries := append([]IndexEntry(nil), old.Entries...)
	root := old.Root
	foundAll := true
	for nodeID, entry := range changed {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].NodeID >= nodeID })
		if i >= len(entries) || entries[i].NodeID != nodeID {
			foundAll = false
			break
		}
		entries[i] = entry
		updated := updateBVHLeaf(root, i, entry.AABB)
		if updated == nil {
			foundAll = false
			break
		}
		root = updated
	}
	if !foundAll {
		return BuildIndex(entries, version)
	}
	idx := &Index{Version: version, Entries: entries, Root: root}
	for i := range entries {
		if entries[i].GeometryError > idx.MaxErrorBound {
			idx.MaxErrorBound = entries[i].GeometryError
		}
	}
	return idx
}

func updateBVHLeaf(n *BVHNode, entry int, bounds AABB) *BVHNode {
	if n == nil {
		return nil
	}
	if n.Leaf {
		if n.Entry != entry {
			return nil
		}
		return &BVHNode{Bounds: bounds, Leaf: true, Entry: entry}
	}
	if left := updateBVHLeaf(n.Left, entry, bounds); left != nil {
		return &BVHNode{Bounds: left.Bounds.Union(n.Right.Bounds), Left: left, Right: n.Right, Entry: -1}
	}
	if right := updateBVHLeaf(n.Right, entry, bounds); right != nil {
		return &BVHNode{Bounds: n.Left.Bounds.Union(right.Bounds), Left: n.Left, Right: right, Entry: -1}
	}
	return nil
}
