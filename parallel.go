package editorpick

import "sync"

// RebuildAllParallel computes nodes at the same hierarchy depth on worker
// goroutines. Parents from earlier depths are immutable during a depth wave,
// so independent branches and nodes are safe to derive concurrently.
func (e *Engine) RebuildAllParallel() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.revision++
	derived := make(map[string]*Derived, len(e.scene.Nodes))
	worldGeom := make(map[string]*WorldGeometry)
	index := make(map[string]*IndexEntry, len(e.scene.Nodes))
	cache := make(map[string]*queryCacheEntry)
	graph := NewDepGraph()

	for _, g := range e.scene.Geometries {
		graph.AddEdge(geometryObject(g.Index), "all-geometry", "geometry-asset")
	}
	for _, n := range e.scene.Nodes {
		graph.AddEdge(localObject(n.ID), worldObject(n.ID), "local-transform")
		if n.ParentID != "" {
			graph.AddEdge(worldObject(n.ParentID), worldObject(n.ID), "parent-transform")
		}
		graph.AddEdge(worldObject(n.ID), entryObject(n.ID), "world-transform")
		if n.GeometryIndex >= 0 {
			graph.AddEdge(geometryObject(n.GeometryIndex), entryObject(n.ID), "geometry-content")
		}
		if n.ParentID != "" {
			graph.AddEdge(entryObject(n.ID), entryObject(n.ParentID), "subtree-bounds")
		}
	}

	order, err := e.topoOrderLocked()
	if err != nil {
		return err
	}
	maxDepth := 0
	byDepth := map[int][]*Node{}
	for _, n := range order {
		byDepth[n.Depth] = append(byDepth[n.Depth], n)
		if n.Depth > maxDepth {
			maxDepth = n.Depth
		}
	}

	type result struct {
		d   *Derived
		wg  *WorldGeometry
		ent *IndexEntry
		err error
	}
	for depth := 0; depth <= maxDepth; depth++ {
		nodes := byDepth[depth]
		results := make([]result, len(nodes))
		var wg sync.WaitGroup
		for i, n := range nodes {
			var parent *Derived
			if n.ParentID != "" {
				parent = derived[n.ParentID]
				if parent == nil {
					return newSchema(n.ID, "parallel parent derived cache missing")
				}
			}
			wg.Add(1)
			go func(i int, n *Node, parent *Derived) {
				defer wg.Done()
				d, world, ent, err := computeNode(e.scene, e.revision, n, parent)
				results[i] = result{d: d, wg: world, ent: ent, err: err}
			}(i, n, parent)
		}
		wg.Wait()
		for _, r := range results {
			if r.err != nil {
				return r.err
			}
			derived[r.d.NodeID] = r.d
			if r.wg != nil {
				worldGeom[r.d.NodeID] = r.wg
			}
			index[r.d.NodeID] = r.ent
		}
	}

	e.derived = derived
	e.worldGeom = worldGeom
	e.index = index
	e.cache = cache
	e.graph = graph
	for i := len(order) - 1; i >= 0; i-- {
		e.recomputeSubtreeLocked(order[i].ID)
	}
	e.lastReport = &InvalidationReport{Revision: e.revision, Objects: []InvalidObject{{ObjectID: "<all>", Kind: "snapshot", Reason: "parallel-full-rebuild"}}}
	return nil
}
